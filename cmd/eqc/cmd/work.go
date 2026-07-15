// Copyright © 2026 Chris Monson <shiblon@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/spf13/cobra"
)

const (
	maxWorkErrBytes          = 32 * 1024
	defaultMaxWorkOutputSize = 10 * 1024 * 1024
)

var flagWork = struct {
	queues         []string
	outQueue       string
	command        string
	outIn          time.Duration
	recurIn        time.Duration
	retryIn        time.Duration
	maxAttempts    int32
	errorQueue     string
	lease          time.Duration
	maxOutputBytes int64
}{}

func init() {
	rootCmd.AddCommand(workCmd)

	flags := workCmd.Flags()
	flags.StringArrayVarP(&flagWork.queues, "queue", "q", nil, "Queue to claim from. Required, can be repeated to claim from one of several queues.")
	if err := workCmd.MarkFlagRequired("queue"); err != nil {
		panic(err)
	}
	flags.StringVarP(&flagWork.outQueue, "out-queue", "Q", "", "Queue where stdout JSONL records are inserted. Empty stdout is always allowed.")
	flags.StringVarP(&flagWork.command, "command", "c", "", "Run the given command string with bash -c. Mutually exclusive with direct command arguments.")
	flags.DurationVarP(&flagWork.outIn, "in", "i", 0, "Relative arrival delay for stdout-created tasks, e.g. 5m. Requires --out-queue.")
	flags.DurationVar(&flagWork.recurIn, "recur-in", 0, "After success, reinsert the input value into the claimed task's queue with this relative arrival delay.")
	flags.DurationVar(&flagWork.retryIn, "retry-in", worker.DefaultRetryDelay, "Relative delay before retrying a failed input task.")
	flags.Int32Var(&flagWork.maxAttempts, "max-attempts", 0, "Maximum attempts before moving the input task to the error queue. The default 0 means unlimited retries.")
	flags.StringVar(&flagWork.errorQueue, "error-queue", "", "Queue for exhausted or non-retriable failed tasks. Defaults to <input>/err.")
	flags.DurationVar(&flagWork.lease, "lease", entroq.DefaultClaimDuration, "Claim lease and renewal interval.")
	flags.Int64Var(&flagWork.maxOutputBytes, "max-output-bytes", defaultMaxWorkOutputSize, "Maximum stdout bytes to capture from the command. Use 0 for unlimited.")
}

var workCmd = &cobra.Command{
	Use:   "work -q QUEUE [-q QUEUE...] [-Q OUT] (-c COMMAND | -- COMMAND [ARG...])",
	Short: "Process tasks with a local command using JSON stdin and JSONL stdout.",
	Long: `Claim tasks from one or more EntroQ queues and run a local command for
each task.

The claimed task value is written to the command's stdin as one JSON value
followed by a newline. Stdout is parsed as JSONL: each non-blank line is one
JSON task value to insert into --out-queue. Logs should be written to stderr.

On success, the input task is deleted, all stdout tasks are inserted into the
single output queue, and --recur-in can insert a fresh delayed copy of the input
task back into the queue it was claimed from. These changes happen in one
Modify. On failure, stdout is ignored and the input task is retried or moved to
the error queue.

A command is always required. Use "-- cat" for an explicit identity worker
that copies input task values to the output queue.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := newWorkConfig(cmd, args)
		if err != nil {
			return err
		}
		queues, err := cmd.Flags().GetStringArray("queue")
		if err != nil {
			return fmt.Errorf("read --queue: %w", err)
		}
		lease, err := cmd.Flags().GetDuration("lease")
		if err != nil {
			return fmt.Errorf("read --lease: %w", err)
		}

		var workerOpts []worker.Option[json.RawMessage]
		workerOpts = append(workerOpts, worker.WithDoModify(cfg.doWork))

		w := worker.New(eq, workerOpts...)
		return w.Run(context.Background(),
			worker.Watching(queues...),
			worker.WithLease(lease),
		)
	},
}

type workConfig struct {
	command        []string
	outQueue       string
	outIn          time.Duration
	recurIn        time.Duration
	retryIn        time.Duration
	maxAttempts    int32
	errorQueue     string
	maxOutputBytes int64
}

func newWorkConfig(cmd *cobra.Command, args []string) (*workConfig, error) {
	commandFlag, err := cmd.Flags().GetString("command")
	if err != nil {
		return nil, fmt.Errorf("read --command: %w", err)
	}
	outQueue, err := cmd.Flags().GetString("out-queue")
	if err != nil {
		return nil, fmt.Errorf("read --out-queue: %w", err)
	}
	outIn, err := cmd.Flags().GetDuration("in")
	if err != nil {
		return nil, fmt.Errorf("read --in: %w", err)
	}
	recurIn, err := cmd.Flags().GetDuration("recur-in")
	if err != nil {
		return nil, fmt.Errorf("read --recur-in: %w", err)
	}
	retryIn, err := cmd.Flags().GetDuration("retry-in")
	if err != nil {
		return nil, fmt.Errorf("read --retry-in: %w", err)
	}
	maxAttempts, err := cmd.Flags().GetInt32("max-attempts")
	if err != nil {
		return nil, fmt.Errorf("read --max-attempts: %w", err)
	}
	errorQueue, err := cmd.Flags().GetString("error-queue")
	if err != nil {
		return nil, fmt.Errorf("read --error-queue: %w", err)
	}
	lease, err := cmd.Flags().GetDuration("lease")
	if err != nil {
		return nil, fmt.Errorf("read --lease: %w", err)
	}
	maxOutputBytes, err := cmd.Flags().GetInt64("max-output-bytes")
	if err != nil {
		return nil, fmt.Errorf("read --max-output-bytes: %w", err)
	}

	if commandFlag != "" && len(args) != 0 {
		return nil, fmt.Errorf("use either -c or direct command arguments, not both")
	}
	if commandFlag == "" && len(args) == 0 {
		return nil, fmt.Errorf("no work command specified")
	}
	if maxAttempts < 0 {
		return nil, fmt.Errorf("--max-attempts must be >= 0")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("--lease must be > 0")
	}
	if retryIn < 0 {
		return nil, fmt.Errorf("--retry-in must be >= 0")
	}
	if outIn != 0 && outQueue == "" {
		return nil, fmt.Errorf("--in requires --out-queue")
	}
	if recurIn < 0 {
		return nil, fmt.Errorf("--recur-in must be >= 0")
	}
	if maxOutputBytes < 0 {
		return nil, fmt.Errorf("--max-output-bytes must be >= 0")
	}

	command := args
	if commandFlag != "" {
		command = []string{"bash", "-c", commandFlag}
	}
	return &workConfig{
		command:        command,
		outQueue:       outQueue,
		outIn:          outIn,
		recurIn:        recurIn,
		retryIn:        retryIn,
		maxAttempts:    maxAttempts,
		errorQueue:     errorQueue,
		maxOutputBytes: maxOutputBytes,
	}, nil
}

func (cfg *workConfig) doWork(ctx context.Context, task *entroq.Task, value json.RawMessage, _ []*entroq.Doc) (*worker.Result, error) {
	result := cfg.runCommand(ctx, task, value)
	if result.err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return worker.Modify(cfg.retryArg(task, workErrorMessage("command failed", result.stderr, result.err))), nil
	}
	if result.stdoutExceeded {
		return worker.Modify(cfg.moveArg(task, workErrorMessage(fmt.Sprintf("stdout exceeded --max-output-bytes=%d", cfg.maxOutputBytes), result.stderr, nil))), nil
	}

	outputs, err := parseJSONLines(result.stdout)
	if err != nil {
		return worker.Modify(cfg.moveArg(task, workErrorMessage("parse stdout JSONL", result.stderr, err))), nil
	}
	if len(outputs) != 0 && cfg.outQueue == "" {
		return nil, worker.FatalErrorf("%s", workErrorMessage("stdout produced tasks but --out-queue is not set", result.stderr, nil))
	}

	modArgs := []entroq.ModifyArg{task.Delete()}
	for _, output := range outputs {
		insArgs := []entroq.InsertArg{entroq.WithRawValue(output)}
		if cfg.outIn != 0 {
			insArgs = append(insArgs, entroq.WithArrivalTimeIn(cfg.outIn))
		}
		modArgs = append(modArgs, entroq.InsertingInto(cfg.outQueue, insArgs...))
	}
	if cfg.recurIn != 0 {
		modArgs = append(modArgs, entroq.InsertingInto(task.Queue,
			entroq.WithRawValue(append(json.RawMessage(nil), value...)),
			entroq.WithArrivalTimeIn(cfg.recurIn),
		))
	}
	return worker.Modify(modArgs...), nil
}

func (cfg *workConfig) errorQueueFor(task *entroq.Task) string {
	if cfg.errorQueue != "" {
		return cfg.errorQueue
	}
	return worker.DefaultErrQMap(task.Queue)
}

func (cfg *workConfig) retryArg(task *entroq.Task, msg string) entroq.ModifyArg {
	return task.RetryOrQuarantine(msg, cfg.errorQueueFor(task), cfg.maxAttempts, entroq.ArrivalTimeBy(cfg.retryIn))
}

func (cfg *workConfig) moveArg(task *entroq.Task, msg string) entroq.ModifyArg {
	return task.Quarantine(msg, cfg.errorQueueFor(task))
}

type workCommandResult struct {
	stdout         []byte
	stderr         []byte
	stdoutExceeded bool
	err            error
}

func (cfg *workConfig) runCommand(ctx context.Context, task *entroq.Task, value json.RawMessage) workCommandResult {
	cmd := exec.CommandContext(ctx, cfg.command[0], cfg.command[1:]...)
	cmd.Env = append(os.Environ(), workTaskEnv(task)...)

	stdin := value
	if len(stdin) == 0 {
		stdin = json.RawMessage("null")
	}
	cmd.Stdin = bytes.NewReader(append(append([]byte(nil), stdin...), '\n'))

	stdout := &cappedBuffer{max: cfg.maxOutputBytes}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	err := cmd.Run()
	return workCommandResult{
		stdout:         stdout.Bytes(),
		stderr:         stderr.Bytes(),
		stdoutExceeded: stdout.exceeded,
		err:            err,
	}
}

func workTaskEnv(task *entroq.Task) []string {
	return []string{
		"ENTROQ_TASK_ID=" + task.ID,
		"ENTROQ_TASK_QUEUE=" + task.Queue,
		"ENTROQ_TASK_VERSION=" + strconv.Itoa(int(task.Version)),
		"ENTROQ_TASK_CLAIMANT=" + task.Claimant,
		"ENTROQ_TASK_CLAIMS=" + strconv.Itoa(int(task.Claims)),
		"ENTROQ_TASK_ATTEMPT=" + strconv.Itoa(int(task.Attempt)),
		"ENTROQ_TASK_ERR=" + task.Err,
	}
}

type cappedBuffer struct {
	buf      bytes.Buffer
	max      int64
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.max == 0 {
		_, err := b.buf.Write(p)
		return n, err
	}

	remaining := b.max - int64(b.buf.Len())
	if remaining <= 0 {
		b.exceeded = true
		return n, nil
	}
	if int64(len(p)) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, err := b.buf.Write(p)
	return n, err
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *cappedBuffer) String() string {
	return b.buf.String()
}

func parseJSONLines(out []byte) ([]json.RawMessage, error) {
	var values []json.RawMessage
	for i, line := range bytes.Split(out, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, fmt.Errorf("line %d is not valid JSON: %q", i+1, string(line))
		}
		values = append(values, append(json.RawMessage(nil), line...))
	}
	return values, nil
}

func workErrorMessage(prefix string, stderr []byte, err error) string {
	var parts []string
	parts = append(parts, prefix)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			parts = append(parts, fmt.Sprintf("exit code %d", exitErr.ExitCode()))
		} else {
			parts = append(parts, err.Error())
		}
	}
	if trimmed := strings.TrimSpace(truncateWorkErr(stderr)); trimmed != "" {
		parts = append(parts, "stderr: "+trimmed)
	}
	return strings.Join(parts, ": ")
}

func truncateWorkErr(stderr []byte) string {
	if len(stderr) <= maxWorkErrBytes {
		return string(stderr)
	}
	return "<truncated...>\n" + string(stderr[len(stderr)-maxWorkErrBytes:])
}
