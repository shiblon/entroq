// Package procworker implements a worker that reads a subprocess specification
// task, executes it, and puts results into an outbox.
//
// Basically, it runs what you ask it to run, and pushes results where you
// want them.
//
// Because of this, YOU SHOULD NEVER USE THIS. Really. Just don't. It's super
// dangerous - anyone that has access to push tasks into your queue can make
// you run arbitrary things as your process user. That's horrible and bad and
// scary, even in a controlled environment.
//
// Containers do not make this better, at least not better enough.
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

	"entrogo.com/entroq"
	pkgerrors "github.com/pkg/errors"
)

const MaxStdOutSize = 1024 * 1024

// SubprocessInput contains information about the subprocess to run. Task
// values should be JSON-encoded versions of this structure.
type SubprocessInput struct {
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
func (p *SubprocessInput) JSON() []byte {
	out, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return out
}

// AsOutput creates an output structure from the input, for filling in results.
func (p *SubprocessInput) AsOutput() *SubprocessOutput {
	out := &SubprocessOutput{
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

// SubprocessOutput contains results from the subprocess that was run. Task
// values pushed to the outbox will contain this as a JSON-encoded string.
type SubprocessOutput struct {
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
	// TODO: use the input and fill these outputs.
}

// JSON creates JSON from the input.
func (p *SubprocessOutput) JSON() []byte {
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
			return nil, pkgerrors.Wrapf(err, "creating stdout dir %q", output.Outfile)
		}
		outFile, err := os.Create(output.Outfile)
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "creating stdout file %q", output.Outfile)
		}
		defer outFile.Close()
		outWriters = append(outWriters, outFile)
	}

	if output.Errfile == "" {
		errbuf = new(bytes.Buffer)
		errWriters = append(errWriters, errbuf)
	} else {
		if err := os.MkdirAll(filepath.Dir(output.Errfile), 0775); err != nil {
			return nil, pkgerrors.Wrapf(err, "creating stdout dir %q", output.Errfile)
		}
		errFile, err := os.Create(output.Errfile)
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "creating stderr file %q", output.Errfile)
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
				t.AsDeletion(),
				entroq.InsertingInto(errbox+"/failed-start", entroq.WithValue(output.JSON())),
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
		t.AsDeletion(),
		entroq.InsertingInto(destQueue, entroq.WithValue(output.JSON())),
	}, nil
}
