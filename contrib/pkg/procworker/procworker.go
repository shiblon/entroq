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
)

// SubprocessInput contains information about the subprocess to run. Task
// values should be JSON-encoded versions of this structure.
type SubprocessInput struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Outbox string   `json:"outbox"`
	Errbox string   `json:"errbox"`

	// Err is the most recent error, in case this is moved to a failure queue
	// before a process can start.
	Err string `json:"err"`
}

// JSON creates JSON from the input.
func (p *SubprocessInput) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// AsOutput creates an output structure from the input, for filling in results.
func (p *SubprocessInput) AsOutput() *SubprocessOutput {
	return &SubprocessOutput{
		Cmd: p.Cmd,
		Env: p.Env,
		Dir: p.Dir,
	}
}

// SubprocessOutput contains results from the subprocess that was run. Task
// values pushed to the outbox will contain this as a JSON-encoded string.
type SubprocessOutput struct {
	Cmd    []string `json:"cmd"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Err    string   `json:"err"`
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
		log.Printf("Error unmarshaling value: %v", err)
		return []entroq.ModifyArg{
			t.AsChange(entroq.QueueTo(t.Queue + "/failed-parse")),
		}, nil
	}

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
			t.AsDeletion(),
			entroq.InsertingInto(outbox, entroq.WithValue(input.AsOutput().JSON())),
		}, nil
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

	output := input.AsOutput()

	destQueue := outbox

	if err := cmd.Run(); err != nil {
		destQueue = errbox
		output.Err = err.Error()
		_, ok := err.(*exec.ExitError)
		if !ok {
			log.Printf("Non-exit error: %v", err)
			return []entroq.ModifyArg{
				t.AsDeletion(),
				entroq.InsertingInto(errbox+"/failed-start", entroq.WithValue(output.JSON())),
			}, nil
		}
	}

	output.Stdout = outbuf.String()
	output.Stderr = errbuf.String()

	return []entroq.ModifyArg{
		t.AsDeletion(),
		entroq.InsertingInto(destQueue, entroq.WithValue(output.JSON())),
	}, nil
}
