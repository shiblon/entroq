package httpworker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

const (
	// DefaultMaxBodySize is the maximum response body size captured by default (1MB).
	DefaultMaxBodySize = 1024 * 1024
)

// Request contains details about an HTTP request to execute.
type Request struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Header  map[string][]string `json:"header"`
	Body    []byte              `json:"body"`
	Outbox  string              `json:"outbox"`
	Errbox  string              `json:"errbox"`
	Timeout time.Duration       `json:"timeout"`
}

// Response contains results from an executed HTTP request.
type Response struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       []byte              `json:"body"`
	Error      string              `json:"error,omitempty"`
	Elapsed    time.Duration       `json:"elapsed"`
}

// Worker provides logic for executing HTTP requests.
type Worker struct {
	client *http.Client
}

// Option defines configuration for the HTTP worker.
type Option func(*Worker, *[]worker.RunOption, *runOpt)

type runOpt struct {
}

// WithClient sets the HTTP client to use for requests.
func WithClient(client *http.Client) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		w.client = client
	}
}

// WithRunOption allows passing core worker.RunOption directly.
func WithRunOption(opt worker.RunOption) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, opt)
	}
}

// Watching specifies the queues to watch.
func Watching(qs ...string) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, worker.Watching(qs...))
	}
}

// WithLease sets the lease duration.
func WithLease(d time.Duration) Option {
	return func(w *Worker, wo *[]worker.RunOption, ro *runOpt) {
		*wo = append(*wo, worker.WithLease(d))
	}
}

// New creates a new HTTP worker.
func New() *Worker {
	return &Worker{
		client: http.DefaultClient,
	}
}

// Run starts the HTTP worker.
func (hw *Worker) Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	ro := &runOpt{}
	var workerRunOpts []worker.RunOption

	for _, opt := range opts {
		opt(hw, &workerRunOpts, ro)
	}

	handler := func(ctx context.Context, task *entroq.Task, reqSpec Request, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
		outbox := reqSpec.Outbox
		if outbox == "" {
			outbox = task.Queue + "/done"
		}
		errbox := reqSpec.Errbox
		if errbox == "" {
			errbox = task.Queue + "/error"
		}

		if reqSpec.Method == "" {
			reqSpec.Method = http.MethodGet
		}

		execCtx := ctx
		if reqSpec.Timeout > 0 {
			var cancel context.CancelFunc
			execCtx, cancel = context.WithTimeout(ctx, reqSpec.Timeout)
			defer cancel()
		}

		start := time.Now()
		resp, err := hw.doRequest(execCtx, reqSpec)
		elapsed := time.Since(start)

		if err != nil {
			resp = &Response{
				Error:   err.Error(),
				Elapsed: elapsed,
			}
			return []entroq.ModifyArg{
				task.Delete(),
				entroq.InsertingInto(errbox, entroq.WithValue(resp)),
			}, nil
		}

		resp.Elapsed = elapsed
		return []entroq.ModifyArg{
			task.Delete(),
			entroq.InsertingInto(outbox, entroq.WithValue(resp)),
		}, nil
	}

	return worker.New(eq, worker.WithDoModify(handler)).Run(ctx, workerRunOpts...)
}

func (hw *Worker) doRequest(ctx context.Context, spec Request) (*Response, error) {
	var body io.Reader
	if len(spec.Body) > 0 {
		body = bytes.NewReader(spec.Body)
	}

	req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL, body)
	if err != nil {
		return nil, fmt.Errorf("httpworker: new request: %w", err)
	}

	for k, vs := range spec.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := hw.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpworker: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxBodySize))
	if err != nil {
		return nil, fmt.Errorf("httpworker: read body: %w", err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       respBody,
	}, nil
}

// Run creates and runs an HTTP worker in a single call.
func Run(ctx context.Context, eq *entroq.EntroQ, opts ...Option) error {
	return New().Run(ctx, eq, opts...)
}
