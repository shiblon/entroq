package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	eqcBin  string
	svcAddr string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	svcAddr = fmt.Sprintf("localhost:%d", lis.Addr().(*net.TCPAddr).Port)

	svc, err := eqsvcgrpc.New(ctx, eqmem.Opener())
	if err != nil {
		fmt.Fprintf(os.Stderr, "eqsvcgrpc: %v\n", err)
		os.Exit(1)
	}
	defer svc.Close()

	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	hpb.RegisterHealthServer(s, health.NewServer())
	go s.Serve(lis) //nolint:errcheck

	tmp, err := os.MkdirTemp("", "eqc-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	eqcBin = tmp + "/eqc"
	build := exec.Command("go", "build", "-o", eqcBin, "github.com/shiblon/entroq/cmd/eqc")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build eqc: %s: %v\n", out, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// run executes the eqc binary with the given args, returning combined output.
func run(args ...string) ([]byte, error) {
	allArgs := append([]string{"--svcaddr=" + svcAddr}, args...)
	cmd := exec.Command(eqcBin, allArgs...)
	return cmd.Output()
}

func mustRun(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := run(args...)
	if err != nil {
		t.Fatalf("eqc %v: %v\noutput: %s", args, err, out)
	}
	return out
}

func TestInsAndTs(t *testing.T) {
	queue := "test/ins-ts"

	out := mustRun(t, "ins", "-q", queue, "-v", `{"hello":"world"}`)

	var inserted []*entroq.Task
	if err := json.Unmarshal(out, &inserted); err != nil {
		t.Fatalf("parse inserted: %v\noutput: %s", err, out)
	}
	if len(inserted) != 1 {
		t.Fatalf("expected 1 inserted task, got %d", len(inserted))
	}

	out = mustRun(t, "ts", "-q", queue)

	// ts outputs one JSON object per line.
	var tasks []*entroq.Task
	for _, line := range splitLines(out) {
		var task entroq.Task
		if err := json.Unmarshal(line, &task); err != nil {
			t.Fatalf("parse task line: %v\nline: %s", err, line)
		}
		tasks = append(tasks, &task)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Queue != queue {
		t.Errorf("queue: got %q, want %q", tasks[0].Queue, queue)
	}
}

func TestDocPutAndDocs(t *testing.T) {
	ns := "test-docput-and-docs"

	out := mustRun(t, "doc-put", "-n", ns, "-k", "mykey", "-v", `{"x":42}`)

	var inserted []*entroq.Doc
	if err := json.Unmarshal(out, &inserted); err != nil {
		t.Fatalf("parse doc-put output: %v\noutput: %s", err, out)
	}
	if len(inserted) != 1 {
		t.Fatalf("expected 1 inserted doc, got %d", len(inserted))
	}
	if inserted[0].Key != "mykey" {
		t.Errorf("key: got %q, want %q", inserted[0].Key, "mykey")
	}

	out = mustRun(t, "docs", "-n", ns)

	// docs outputs one JSON object per line.
	var docs []*entroq.Doc
	for _, line := range splitLines(out) {
		var d entroq.Doc
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("parse doc line: %v\nline: %s", err, line)
		}
		docs = append(docs, &d)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if docs[0].Key != "mykey" {
		t.Errorf("key: got %q, want %q", docs[0].Key, "mykey")
	}
}

func TestDocsByID(t *testing.T) {
	ns := "test-docs-by-id"

	out := mustRun(t, "doc-put", "-n", ns, "-k", "a", "-v", `"one"`)
	var inserted []*entroq.Doc
	if err := json.Unmarshal(out, &inserted); err != nil {
		t.Fatalf("parse doc-put: %v", err)
	}
	id := inserted[0].ID

	// Fetch by ID - should return exactly the one we inserted.
	out = mustRun(t, "docs", "-n", ns, "-i", id)
	var docs []*entroq.Doc
	for _, line := range splitLines(out) {
		var d entroq.Doc
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("parse doc: %v", err)
		}
		docs = append(docs, &d)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc by ID, got %d", len(docs))
	}
	if docs[0].ID != id {
		t.Errorf("id: got %q, want %q", docs[0].ID, id)
	}
}

func TestDocRm(t *testing.T) {
	ns := "test-doc-rm"

	out := mustRun(t, "doc-put", "-n", ns, "-k", "gone", "-v", `null`)
	var inserted []*entroq.Doc
	if err := json.Unmarshal(out, &inserted); err != nil {
		t.Fatalf("parse doc-put: %v", err)
	}
	id := inserted[0].ID

	mustRun(t, "doc-rm", "-n", ns, "-i", id)

	out = mustRun(t, "docs", "-n", ns)
	if len(splitLines(out)) != 0 {
		t.Fatalf("expected no docs after rm, got: %s", out)
	}
}

func TestDocKeyRange(t *testing.T) {
	ns := "test-doc-key-range"

	for _, k := range []string{"a", "b", "c", "d"} {
		mustRun(t, "doc-put", "-n", ns, "-k", k)
	}

	// [b, d) should return b and c.
	out := mustRun(t, "docs", "-n", ns, "-k", "b", "-K", "d")
	var docs []*entroq.Doc
	for _, line := range splitLines(out) {
		var d entroq.Doc
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("parse doc: %v", err)
		}
		docs = append(docs, &d)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs in range [b,d), got %d: %s", len(docs), out)
	}
	if docs[0].Key != "b" || docs[1].Key != "c" {
		t.Errorf("keys: got %q, %q; want b, c", docs[0].Key, docs[1].Key)
	}
}

func TestWorkDirectCommandCopiesTaskValue(t *testing.T) {
	in := uniqueQueue(t, "in")
	outQ := uniqueQueue(t, "out")

	mustRun(t, "ins", "-q", in, "-v", `{"hello":"world"}`)
	work := startRun(t, "work", "-q", in, "-Q", outQ, "--", "cat")
	defer work.stop(t)

	waitFor(t, "output task", work, func() bool {
		return len(tasksInQueue(t, outQ)) == 1
	})

	assertQueueEmpty(t, in)
	tasks := tasksInQueue(t, outQ)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 output task, got %d", len(tasks))
	}
	if got := string(tasks[0].Value); got != `{"hello":"world"}` {
		t.Fatalf("output value: got %s", got)
	}
}

func TestWorkCommandJSONLFanout(t *testing.T) {
	in := uniqueQueue(t, "in")
	outQ := uniqueQueue(t, "out")

	mustRun(t, "ins", "-q", in, "-v", `{"ignored":true}`)
	work := startRun(t, "work", "-q", in, "-Q", outQ, "-c", `printf '{"a":1}\n{"b":2}\n'`)
	defer work.stop(t)

	waitFor(t, "fanout output tasks", work, func() bool {
		return len(tasksInQueue(t, outQ)) == 2
	})

	assertQueueEmpty(t, in)
	tasks := tasksInQueue(t, outQ)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 output tasks, got %d", len(tasks))
	}
	got := map[string]bool{}
	for _, task := range tasks {
		got[string(task.Value)] = true
	}
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		if !got[want] {
			t.Fatalf("missing output value %s in %#v", want, got)
		}
	}
}

func TestWorkRepeatableInputQueuesShareOutputQueue(t *testing.T) {
	inA := uniqueQueue(t, "in-a")
	inB := uniqueQueue(t, "in-b")
	outQ := uniqueQueue(t, "out")

	mustRun(t, "ins", "-q", inA, "-v", `{"from":"a"}`)
	mustRun(t, "ins", "-q", inB, "-v", `{"from":"b"}`)
	work := startRun(t, "work", "-q", inA, "-q", inB, "-Q", outQ, "--", "cat")
	defer work.stop(t)

	waitFor(t, "two output tasks", work, func() bool {
		return len(tasksInQueue(t, outQ)) == 2
	})

	assertQueueEmpty(t, inA)
	assertQueueEmpty(t, inB)
	got := map[string]bool{}
	for _, task := range tasksInQueue(t, outQ) {
		got[string(task.Value)] = true
	}
	for _, want := range []string{`{"from":"a"}`, `{"from":"b"}`} {
		if !got[want] {
			t.Fatalf("missing output value %s in %#v", want, got)
		}
	}
}

func TestWorkEmptyOutputDeletesInput(t *testing.T) {
	in := uniqueQueue(t, "in")

	mustRun(t, "ins", "-q", in, "-v", `{"ok":true}`)
	work := startRun(t, "work", "-q", in, "--", "true")
	defer work.stop(t)

	waitFor(t, "input queue to empty", work, func() bool {
		return len(tasksInQueue(t, in)) == 0
	})
}

func TestWorkInAppliesToOutputTasks(t *testing.T) {
	in := uniqueQueue(t, "in")
	outQ := uniqueQueue(t, "out")
	delay := time.Hour
	before := time.Now()

	mustRun(t, "ins", "-q", in, "-v", `{"ok":true}`)
	work := startRun(t, "work", "-q", in, "-Q", outQ, "--in", delay.String(), "-c", `printf '{}\n'`)
	defer work.stop(t)

	waitFor(t, "delayed output task", work, func() bool {
		return len(tasksInQueue(t, outQ)) == 1
	})

	tasks := tasksInQueue(t, outQ)
	if tasks[0].At.Before(before.Add(delay - time.Second)) {
		t.Fatalf("arrival time is too early: got %s, want about %s", tasks[0].At.Format(time.RFC3339Nano), before.Add(delay).Format(time.RFC3339Nano))
	}
}

func TestWorkFailureRetriesByDefault(t *testing.T) {
	in := uniqueQueue(t, "in")
	retryDelay := time.Hour
	before := time.Now()

	mustRun(t, "ins", "-q", in, "-v", `{"fail":true}`)
	work := startRun(t, "work", "-q", in, "--retry-in", retryDelay.String(), "-c", `echo nope >&2; exit 7`)
	defer work.stop(t)

	waitFor(t, "retried input task", work, func() bool {
		tasks := tasksInQueue(t, in)
		return len(tasks) == 1 && tasks[0].Attempt > 0
	})

	tasks := tasksInQueue(t, in)
	if got := string(tasks[0].Value); got != `{"fail":true}` {
		t.Fatalf("input value: got %s", got)
	}
	if tasks[0].At.Before(before.Add(retryDelay - time.Second)) {
		t.Fatalf("retry arrival time is too early: got %s, want about %s", tasks[0].At.Format(time.RFC3339Nano), before.Add(retryDelay).Format(time.RFC3339Nano))
	}
	assertQueueEmpty(t, in+"/err")
}

func TestWorkMaxAttemptsMovesInputToErrorQueue(t *testing.T) {
	in := uniqueQueue(t, "in")

	mustRun(t, "ins", "-q", in, "-v", `{"fail":true}`)
	work := startRun(t, "work", "-q", in, "--max-attempts", "1", "-c", `exit 7`)
	defer work.stop(t)

	waitFor(t, "task in error queue", work, func() bool {
		return len(tasksInQueue(t, in+"/err")) == 1
	})

	tasks := tasksInQueue(t, in+"/err")
	if got := string(tasks[0].Value); got != `{"fail":true}` {
		t.Fatalf("error value: got %s", got)
	}
	if tasks[0].Attempt != 1 {
		t.Fatalf("attempt: got %d, want 1", tasks[0].Attempt)
	}
	assertQueueEmpty(t, in)
}

func TestWorkInvalidJSONOutputMovesInputToErrorQueue(t *testing.T) {
	in := uniqueQueue(t, "in")
	outQ := uniqueQueue(t, "out")

	mustRun(t, "ins", "-q", in, "-v", `{"bad":"stdout"}`)
	work := startRun(t, "work", "-q", in, "-Q", outQ, "-c", `printf 'not-json\n'`)
	defer work.stop(t)

	waitFor(t, "invalid JSON task in error queue", work, func() bool {
		return len(tasksInQueue(t, in+"/err")) == 1
	})

	assertQueueEmpty(t, in)
	assertQueueEmpty(t, outQ)
}

func TestWorkRecurInReinsertsFreshTask(t *testing.T) {
	in := uniqueQueue(t, "in")
	delay := time.Hour
	before := time.Now()

	out := mustRun(t, "ins", "-q", in, "-v", `{"cron":true}`)
	var inserted []*entroq.Task
	if err := json.Unmarshal(out, &inserted); err != nil {
		t.Fatalf("parse inserted: %v\noutput: %s", err, out)
	}
	work := startRun(t, "work", "-q", in, "--recur-in", delay.String(), "--", "true")
	defer work.stop(t)

	waitFor(t, "recurring task", work, func() bool {
		tasks := tasksInQueue(t, in)
		return len(tasks) == 1 && tasks[0].ID != inserted[0].ID
	})

	tasks := tasksInQueue(t, in)
	if got := string(tasks[0].Value); got != `{"cron":true}` {
		t.Fatalf("recurring value: got %s", got)
	}
	if tasks[0].Attempt != 0 {
		t.Fatalf("recurring attempt: got %d, want 0", tasks[0].Attempt)
	}
	if tasks[0].Claims != 0 {
		t.Fatalf("recurring claims: got %d, want 0", tasks[0].Claims)
	}
	if tasks[0].At.Before(before.Add(delay - time.Second)) {
		t.Fatalf("recurring arrival time is too early: got %s, want about %s", tasks[0].At.Format(time.RFC3339Nano), before.Add(delay).Format(time.RFC3339Nano))
	}
}

func TestWorkMaxOutputBytesMovesInputToErrorQueue(t *testing.T) {
	in := uniqueQueue(t, "in")
	outQ := uniqueQueue(t, "out")

	mustRun(t, "ins", "-q", in, "-v", `{"too":"large"}`)
	work := startRun(t, "work", "-q", in, "-Q", outQ, "--max-output-bytes", "3", "-c", `printf '{"a":1}\n'`)
	defer work.stop(t)

	waitFor(t, "oversized output task in error queue", work, func() bool {
		return len(tasksInQueue(t, in+"/err")) == 1
	})

	assertQueueEmpty(t, in)
	assertQueueEmpty(t, outQ)
}

// splitLines returns non-empty lines from output as byte slices.
func splitLines(out []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range out {
		if c == '\n' {
			line := out[start:i]
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if tail := out[start:]; len(tail) > 0 {
		lines = append(lines, tail)
	}
	return lines
}

func tasksInQueue(t *testing.T, queue string) []*entroq.Task {
	t.Helper()

	out := mustRun(t, "ts", "-q", queue)
	var tasks []*entroq.Task
	for _, line := range splitLines(out) {
		var task entroq.Task
		if err := json.Unmarshal(line, &task); err != nil {
			t.Fatalf("parse task line: %v\nline: %s", err, line)
		}
		tasks = append(tasks, &task)
	}
	return tasks
}

func assertQueueEmpty(t *testing.T, queue string) {
	t.Helper()

	if tasks := tasksInQueue(t, queue); len(tasks) != 0 {
		t.Fatalf("expected queue %q to be empty, got %d tasks", queue, len(tasks))
	}
}

func uniqueQueue(t *testing.T, leaf string) string {
	t.Helper()

	return fmt.Sprintf("test/%s/%d/%s", t.Name(), time.Now().UnixNano(), leaf)
}

type runningCmd struct {
	cancel context.CancelFunc
	done   chan error
	stdout bytes.Buffer
	stderr bytes.Buffer
	args   []string
}

func startRun(t *testing.T, args ...string) *runningCmd {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	allArgs := append([]string{"--svcaddr=" + svcAddr}, args...)
	cmd := exec.CommandContext(ctx, eqcBin, allArgs...)

	r := &runningCmd{
		cancel: cancel,
		done:   make(chan error, 1),
		args:   args,
	}
	cmd.Stdout = &r.stdout
	cmd.Stderr = &r.stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start eqc %v: %v", args, err)
	}
	go func() { r.done <- cmd.Wait() }()
	return r
}

func (r *runningCmd) stop(t *testing.T) {
	t.Helper()

	if r.done == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out stopping eqc %v\nstdout: %s\nstderr: %s", r.args, r.stdout.String(), r.stderr.String())
	}
}

func waitFor(t *testing.T, desc string, r *runningCmd, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		select {
		case err := <-r.done:
			r.done = nil
			t.Fatalf("eqc %v exited before %s: %v\nstdout: %s\nstderr: %s", r.args, desc, err, r.stdout.String(), r.stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\nstdout: %s\nstderr: %s", desc, r.stdout.String(), r.stderr.String())
}
