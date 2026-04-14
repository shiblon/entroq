package cmd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"

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
