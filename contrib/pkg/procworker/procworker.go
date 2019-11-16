// Package procworker implements a worker that reads a subprocess specification
// task, executes it, and puts results into an outbox.
//
// Basically, it runs what you ask it to run, and pushes results where you
// want them.
package procworker

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"

	"entrogo.com/entroq"
	"github.com/pkg/errors"
)

// SubprocessInput contains information about the subprocess to run. Task
// values should be JSON-encoded versions of this structure.
type SubprocessInput struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Outbox string   `json:"outbox"`
}

// JSON creates JSON from the input.
func (p *SubprocessInput) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// SubprocessOutput contains results from the subprocess that was run. Task
// values pushed to the outbox will contain this as a JSON-encoded string.
type SubprocessOutput struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Error  string   `json:"statusMessage"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

// JSON creates JSON from the input.
func (p *SubprocessOutput) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// Run is a function that can be passed to a worker's Run method. The worker
// will call this for each task it wishes to process. It pulls subprocess call
// information from its input task, including which outbox to write results to.
// If no outbox is specified, the input task's queue name is suffixed with
// "/done" to produce one.
func Run(ctx context.Context, t *entroq.Task) ([]entroq.ModifyArg, error) {
	input := new(SubprocessInput)
	if err := json.Unmarshal(t.Value, input); err != nil {
		return nil, errors.Wrap(err, "unmarshal value in worker")
	}

	if len(input.Cmd) == 0 {
		return nil, errors.New("No command specified in input task")
	}

	outbox := input.Outbox
	if outbox == "" {
		outbox = t.Queue + "/done"
	}

	cmd := exec.CommandContext(ctx, input.Cmd[0], input.Cmd[1:]...)
	if len(input.Env) != 0 {
		cmd.Env = append(os.Environ(), input.Env...)
	}
	cmd.Dir = input.Dir

	outbuf := new(bytes.Buffer)
	errbuf := new(bytes.Buffer)

	cmd.Stdout = outbuf
	cmd.Stderr = errbuf

	output := &SubprocessOutput{
		Cmd: input.Cmd,
		Env: input.Env,
		Dir: input.Dir,
	}

	if err := cmd.Run(); err != nil {
		_, ok := err.(*exec.ExitError)
		if !ok {
			return nil, errors.Wrap(err, "non-exit error on process run")
		}
		output.Error = err.Error()
	}

	output.Stdout = outbuf.String()
	output.Stderr = errbuf.String()

	return []entroq.ModifyArg{
		t.AsDeletion(),
		entroq.InsertingInto(outbox, entroq.WithValue(output.JSON())),
	}, nil
}
