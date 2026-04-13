package procworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

const MaxStdOutSize = 1024 * 1024

// Input contains information about the subprocess to run. Task
// values should be JSON-encoded versions of this structure.
type Input struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Outbox string   `json:"outbox"`
	Errbox string   `json:"errbox"`

	// Directory names for stdout and stderr. If empty, outputs as string into
	// Stdout and Stderr fields. If specified, creates a random file
	// name.stdout/stderr file.
	Outdir string `json:"outdir"`
	Errdir string `json:"errdir"`

	// Err is the most recent error, in case this is moved to a failure queue
	// before a process can start.
	Err string `json:"err"`
}

// JSON creates JSON from the input.
func (p *Input) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// AsOutput creates an output structure from the input, for filling in results.
func (p *Input) AsOutput() *Output {
	out := &Output{
		Cmd:    p.Cmd,
		Env:    p.Env,
		Dir:    p.Dir,
		Outdir: p.Outdir,
		Errdir: p.Errdir,
	}

	ts := strings.Replace(time.Now().UTC().Format("20060102-150405.000"), ".", "", -1)

	if p.Outdir != "" {
		out.Outfile = filepath.Join(p.Outdir, fmt.Sprintf("%s.STDOUT", ts))
	}

	if p.Errdir != "" {
		out.Errfile = filepath.Join(p.Errdir, fmt.Sprintf("%s.STDERR", ts))
	}

	return out
}

// Output contains results from the subprocess that was run. Task
// values pushed to the outbox will contain this as a JSON-encoded string.
type Output struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Err    string   `json:"err"`
	Outdir string   `json:"outdir"`
	Errdir string   `json:"errdir"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`

	// If Outfile and Errfile were specified in the input, this contains
	// the final full pathnames of those files.
	Outfile string `json:"outfile"`
	Errfile string `json:"errfile"`
}

// JSON creates JSON from the output.
func (p *Output) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// TeeWriter writes to two writers what is written to it.
type TeeWriter struct {
	writers []io.Writer
}

// NewTeeWriter crates a new TeeWriter with the given writers to fan input out to.
func NewTeeWriter(writers ...io.Writer) *TeeWriter {
	return &TeeWriter{writers: writers}
}

// Write forwards the data written to it to the two writers within.
func (w *TeeWriter) Write(data []byte) (int, error) {
	for _, writer := range w.writers {
		n, err := writer.Write(data)
		if err != nil {
			return n, err
		}
	}
	return len(data), nil
}

// Worker provides the execution logic.
type Worker struct {
}

// Option defines options that can be passed to Run.
type Option func(*Worker, *[]worker.Option[Input], *[]worker.RunOption)

// WithQueues sets the queues to collect tasks from.
func WithQueues(qs ...string) Option {
	return func(pw *Worker, wo *[]worker.Option[Input], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.Watching(qs...))
	}
}

// WithLease sets the lease duration for claimed tasks.
func WithLease(lease time.Duration) Option {
	return func(pw *Worker, wo *[]worker.Option[Input], ro *[]worker.RunOption) {
		*ro = append(*ro, worker.WithLease(lease))
	}
}

// WithWorkerOption allows passing core worker options directly.
func WithWorkerOption(opt worker.Option[Input]) Option {
	return func(pw *Worker, wo *[]worker.Option[Input], ro *[]worker.RunOption) {
		*wo = append(*wo, opt)
	}
}

// WithRunOption allows passing core run options directly.
func WithRunOption(opt worker.RunOption) Option {
	return func(pw *Worker, wo *[]worker.Option[Input], ro *[]worker.RunOption) {
		*ro = append(*ro, opt)
	}
}

// doWork is the internal handler that matches worker.DoModifyRun.
func (pw *Worker) doWork(ctx context.Context, t *entroq.Task, input Input, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
	outbox := input.Outbox
	if outbox == "" {
		outbox = t.Queue + "/done"
	}

	errbox := input.Errbox
	if errbox == "" {
		errbox = t.Queue + "/error"
	}

	if len(input.Cmd) == 0 {
		log.Print("Empty command")
		return []entroq.ModifyArg{
			t.Delete(),
			entroq.InsertingInto(outbox, entroq.WithRawValue(input.AsOutput().JSON())),
		}, nil
	}

	cmd := exec.CommandContext(ctx, input.Cmd[0], input.Cmd[1:]...)
	if len(input.Env) != 0 {
		cmd.Env = append(os.Environ(), input.Env...)
	}
	cmd.Dir = input.Dir

	output := input.AsOutput()
	outWriters := []io.Writer{os.Stdout}
	errWriters := []io.Writer{os.Stderr}

	// Create output buffers if needed, otherwise open files.
	var (
		outbuf *bytes.Buffer
		errbuf *bytes.Buffer
	)

	if output.Outfile == "" {
		outbuf = new(bytes.Buffer)
		outWriters = append(outWriters, outbuf)
	} else {
		if err := os.MkdirAll(filepath.Dir(output.Outfile), 0775); err != nil {
			return nil, fmt.Errorf("creating stdout dir %q: %w", output.Outfile, err)
		}
		outFile, err := os.Create(output.Outfile)
		if err != nil {
			return nil, fmt.Errorf("creating stdout file %q: %w", output.Outfile, err)
		}
		defer outFile.Close()
		outWriters = append(outWriters, outFile)
	}

	if output.Errfile == "" {
		errbuf = new(bytes.Buffer)
		errWriters = append(errWriters, errbuf)
	} else {
		if err := os.MkdirAll(filepath.Dir(output.Errfile), 0775); err != nil {
			return nil, fmt.Errorf("creating stdout dir %q: %w", output.Errfile, err)
		}
		errFile, err := os.Create(output.Errfile)
		if err != nil {
			return nil, fmt.Errorf("creating stderr file %q: %w", output.Errfile, err)
		}
		defer errFile.Close()
		errWriters = append(errWriters, errFile)
	}

	cmd.Stdout = NewTeeWriter(outWriters...)
	cmd.Stderr = NewTeeWriter(errWriters...)

	destQueue := outbox

	if err := cmd.Run(); err != nil {
		destQueue = errbox
		output.Err = err.Error()
		_, ok := err.(*exec.ExitError)
		if !ok {
			log.Printf("Non-exit error: %v", err)
			return []entroq.ModifyArg{
				t.Delete(),
				entroq.InsertingInto(errbox+"/failed-start", entroq.WithRawValue(output.JSON())),
			}, nil
		}
	}

	if outbuf != nil {
		output.Stdout = outbuf.String()
		if len(output.Stdout) > MaxStdOutSize {
			output.Stdout = "<truncated...>\n" + output.Stdout[len(output.Stdout)-MaxStdOutSize:]
		}
	}
	if errbuf != nil {
		output.Stderr = errbuf.String()
		if len(output.Stderr) > MaxStdOutSize {
			output.Stderr = "<truncated...>\n" + output.Stderr[len(output.Stderr)-MaxStdOutSize:]
		}
	}

	return []entroq.ModifyArg{
		t.Delete(),
		entroq.InsertingInto(destQueue, entroq.WithRawValue(output.JSON())),
	}, nil
}

// New creates a new proc worker ready to be configured.
func New(opts ...Option) *Worker {
	pw := &Worker{}
	return pw
}

// Run creates and runs a proc worker in a single one-shot call.
func Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	pw := New()
	var workerOpts []worker.Option[Input]
	var runOpts []worker.RunOption

	// Default: use the struct's doWork method.
	workerOpts = append(workerOpts, worker.WithDoModify[Input](pw.doWork))

	for _, opt := range opts {
		opt(pw, &workerOpts, &runOpts)
	}

	return worker.New(eq, workerOpts...).Run(ctx, runOpts...)
}
