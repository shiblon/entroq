package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const resultSchemaVersion = 1

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type loadConfig struct {
	Mode           string        `json:"mode"`
	AuthzStrategy  string        `json:"authz_strategy"`
	AuthzProfile   string        `json:"authz_profile"`
	Sample         int           `json:"sample"`
	URL            string        `json:"url"`
	Host           string        `json:"host,omitempty"`
	Concurrency    int           `json:"concurrency"`
	TargetRPS      float64       `json:"target_rps,omitempty"`
	Duration       time.Duration `json:"-"`
	DurationText   string        `json:"duration"`
	Warmup         time.Duration `json:"-"`
	WarmupText     string        `json:"warmup"`
	RequestTimeout time.Duration `json:"-"`
	TimeoutText    string        `json:"request_timeout"`
	PayloadBytes   int           `json:"payload_bytes"`
	ExpectedStatus int           `json:"expected_status"`
	MetricInterval time.Duration `json:"-"`
	MetricText     string        `json:"metric_interval"`
	Metrics        []metricSpec  `json:"metrics"`
}

type metricSpec struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type latencySummary struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

type sampleResult struct {
	SchemaVersion     int              `json:"schema_version"`
	StartedAt         time.Time        `json:"started_at"`
	Config            loadConfig       `json:"config"`
	ElapsedSeconds    float64          `json:"elapsed_seconds"`
	Requests          int64            `json:"requests"`
	Completed         int64            `json:"completed"`
	Failures          int64            `json:"failures"`
	InvalidResponses  int64            `json:"invalid_responses"`
	Throughput        float64          `json:"throughput_rps"`
	Latency           latencySummary   `json:"latency"`
	FailureExamples   []string         `json:"failure_examples,omitempty"`
	MetricSnapshots   []metricSnapshot `json:"metric_snapshots"`
	MetricScrapeError int              `json:"metric_scrape_errors"`
}

type metricSnapshot struct {
	OffsetMillis int64  `json:"offset_ms"`
	Source       string `json:"source"`
	Body         string `json:"body,omitempty"`
	Error        string `json:"error,omitempty"`
}

type phaseResult struct {
	elapsed   time.Duration
	requests  int64
	completed int64
	failures  int64
	invalid   int64
	latencies []time.Duration
	examples  []string
}

type sampleError struct {
	err error
}

func (e sampleError) Error() string { return e.err.Error() }
func (e sampleError) Silent() bool  { return true }

func load(args []string) error {
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	var metricArgs stringList
	cfg := loadConfig{}
	flags.StringVar(&cfg.Mode, "mode", "", "result label, usually direct or mesh")
	flags.StringVar(&cfg.AuthzStrategy, "authz-strategy", "", "EntroQ authorization strategy used by the deployment")
	flags.StringVar(&cfg.AuthzProfile, "authz-profile", "", "authorization benchmark profile, such as none, full, or allow-all")
	flags.IntVar(&cfg.Sample, "sample", 0, "one-based sample number")
	flags.StringVar(&cfg.URL, "url", "", "HTTP endpoint to load")
	flags.StringVar(&cfg.Host, "host", "", "optional HTTP Host override")
	flags.IntVar(&cfg.Concurrency, "concurrency", 8, "concurrent request workers")
	flags.Float64Var(&cfg.TargetRPS, "target-rps", 0, "total offered requests per second; zero runs without pacing")
	flags.DurationVar(&cfg.Duration, "duration", 15*time.Second, "measured duration")
	flags.DurationVar(&cfg.Warmup, "warmup", 3*time.Second, "unmeasured warm-up duration")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", 10*time.Second, "per-request timeout")
	flags.IntVar(&cfg.PayloadBytes, "payload-bytes", 1024, "request body size")
	flags.IntVar(&cfg.ExpectedStatus, "expected-status", http.StatusOK, "HTTP status counted as a successful response")
	flags.DurationVar(&cfg.MetricInterval, "metric-interval", time.Second, "metrics scrape interval")
	flags.Var(&metricArgs, "metric", "metric source as name=url; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	for _, value := range metricArgs {
		spec, err := parseMetricSpec(value)
		if err != nil {
			return err
		}
		cfg.Metrics = append(cfg.Metrics, spec)
	}
	cfg.DurationText = cfg.Duration.String()
	cfg.WarmupText = cfg.Warmup.String()
	cfg.TimeoutText = cfg.RequestTimeout.String()
	cfg.MetricText = cfg.MetricInterval.String()

	payload := makePayload(cfg.PayloadBytes)
	transport := &http.Transport{
		MaxIdleConns:        cfg.Concurrency * 2,
		MaxIdleConnsPerHost: cfg.Concurrency,
		IdleConnTimeout:     30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}

	if cfg.Warmup > 0 {
		warmup := runPhase(client, cfg, payload, cfg.Warmup)
		if warmup.failures > 0 || warmup.invalid > 0 || warmup.completed == 0 {
			return fmt.Errorf("warm-up failed: completed=%d failures=%d invalid=%d: %v",
				warmup.completed, warmup.failures, warmup.invalid, warmup.examples)
		}
	}

	started := time.Now().UTC()
	sampler := newMetricSampler(cfg.Metrics, cfg.MetricInterval, started)
	sampler.start()
	phase := runPhase(client, cfg, payload, cfg.Duration)
	snapshots := sampler.stop()

	result := sampleResult{
		SchemaVersion:    resultSchemaVersion,
		StartedAt:        started,
		Config:           cfg,
		ElapsedSeconds:   phase.elapsed.Seconds(),
		Requests:         phase.requests,
		Completed:        phase.completed,
		Failures:         phase.failures,
		InvalidResponses: phase.invalid,
		Latency:          summarizeLatencies(phase.latencies),
		FailureExamples:  phase.examples,
		MetricSnapshots:  snapshots,
	}
	if result.ElapsedSeconds > 0 {
		result.Throughput = float64(result.Completed) / result.ElapsedSeconds
	}
	for _, snapshot := range snapshots {
		if snapshot.Error != "" {
			result.MetricScrapeError++
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	missedTarget := cfg.TargetRPS > 0 && result.Throughput < cfg.TargetRPS*0.9
	if result.Completed == 0 || result.Failures > 0 || result.InvalidResponses > 0 || result.MetricScrapeError > 0 || missedTarget {
		return sampleError{err: fmt.Errorf("sample failed: completed=%d failures=%d invalid=%d metric_errors=%d target_rps=%.2f achieved_rps=%.2f",
			result.Completed, result.Failures, result.InvalidResponses, result.MetricScrapeError, cfg.TargetRPS, result.Throughput)}
	}
	return nil
}

func (c *loadConfig) validate() error {
	if c.Mode == "" || c.AuthzStrategy == "" || c.AuthzProfile == "" || c.URL == "" {
		return fmt.Errorf("mode, authz-strategy, authz-profile, and url are required")
	}
	if c.Sample < 1 || c.Concurrency < 1 || c.TargetRPS < 0 || c.Duration <= 0 || c.RequestTimeout <= 0 || c.PayloadBytes < 1 || c.ExpectedStatus < 100 || c.ExpectedStatus > 599 || c.MetricInterval <= 0 {
		return fmt.Errorf("sample, concurrency, duration, request-timeout, payload-bytes, expected-status, and metric-interval must be valid positive values")
	}
	if c.TargetRPS > float64(time.Second) {
		return fmt.Errorf("target-rps is too large to represent with nanosecond pacing")
	}
	if c.Warmup < 0 {
		return fmt.Errorf("warmup must not be negative")
	}
	return nil
}

func makePayload(size int) []byte {
	if size == 1 {
		return []byte("0")
	}
	payload := make([]byte, size)
	payload[0] = '"'
	for i := 1; i < size-1; i++ {
		payload[i] = 'x'
	}
	payload[size-1] = '"'
	return payload
}

func parseMetricSpec(value string) (metricSpec, error) {
	name, url, ok := strings.Cut(value, "=")
	if !ok || name == "" || url == "" {
		return metricSpec{}, fmt.Errorf("metric must be name=url, got %q", value)
	}
	return metricSpec{Name: name, URL: url}, nil
}

func runPhase(client *http.Client, cfg loadConfig, payload []byte, duration time.Duration) phaseResult {
	started := time.Now()
	stopAt := started.Add(duration)
	ctx, cancel := context.WithDeadline(context.Background(), stopAt)
	defer cancel()

	var ticks <-chan time.Time
	var ticker *time.Ticker
	if cfg.TargetRPS > 0 {
		interval := time.Duration(float64(time.Second) / cfg.TargetRPS)
		ticker = time.NewTicker(interval)
		ticks = ticker.C
		defer ticker.Stop()
	}

	results := make(chan phaseResult, cfg.Concurrency)
	var workers sync.WaitGroup
	for range cfg.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			var local phaseResult
			for {
				if ticks == nil {
					if !time.Now().Before(stopAt) {
						break
					}
				} else {
					select {
					case <-ctx.Done():
						results <- local
						return
					case <-ticks:
					}
				}
				requestStarted := time.Now()
				req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(payload))
				if err != nil {
					local.failures++
					addExample(&local.examples, err.Error())
					break
				}
				req.Header.Set("Content-Type", "application/json")
				if cfg.Host != "" {
					req.Host = cfg.Host
				}
				local.requests++
				resp, err := client.Do(req)
				if err != nil {
					local.failures++
					addExample(&local.examples, err.Error())
					continue
				}
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(len(payload))+1))
				closeErr := resp.Body.Close()
				if readErr != nil || closeErr != nil || resp.StatusCode != cfg.ExpectedStatus {
					local.failures++
					addExample(&local.examples, fmt.Sprintf("status=%d read=%v close=%v", resp.StatusCode, readErr, closeErr))
					continue
				}
				if cfg.ExpectedStatus == http.StatusOK && !bytes.Equal(body, payload) {
					local.invalid++
					addExample(&local.examples, fmt.Sprintf("response bytes=%d want=%d", len(body), len(payload)))
					continue
				}
				local.completed++
				local.latencies = append(local.latencies, time.Since(requestStarted))
			}
			results <- local
		}()
	}
	workers.Wait()
	close(results)

	combined := phaseResult{elapsed: time.Since(started)}
	for local := range results {
		combined.requests += local.requests
		combined.completed += local.completed
		combined.failures += local.failures
		combined.invalid += local.invalid
		combined.latencies = append(combined.latencies, local.latencies...)
		for _, example := range local.examples {
			addExample(&combined.examples, example)
		}
	}
	return combined
}

func addExample(examples *[]string, value string) {
	if len(*examples) < 5 {
		*examples = append(*examples, value)
	}
}

func summarizeLatencies(values []time.Duration) latencySummary {
	if len(values) == 0 {
		return latencySummary{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return latencySummary{
		P50: percentileMillis(values, 0.50),
		P95: percentileMillis(values, 0.95),
		P99: percentileMillis(values, 0.99),
		Max: float64(values[len(values)-1]) / float64(time.Millisecond),
	}
}

func percentileMillis(values []time.Duration, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return float64(values[index]) / float64(time.Millisecond)
}

type metricSampler struct {
	specs    []metricSpec
	interval time.Duration
	started  time.Time
	client   *http.Client
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	samples  []metricSnapshot
}

func newMetricSampler(specs []metricSpec, interval time.Duration, started time.Time) *metricSampler {
	return &metricSampler{
		specs:    specs,
		interval: interval,
		started:  started,
		client:   &http.Client{Timeout: 2 * time.Second},
		done:     make(chan struct{}),
	}
}

func (s *metricSampler) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.collect(ctx)
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.collect(ctx)
			}
		}
	}()
}

func (s *metricSampler) stop() []metricSnapshot {
	s.cancel()
	<-s.done
	s.collect(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]metricSnapshot(nil), s.samples...)
}

func (s *metricSampler) collect(ctx context.Context) {
	for _, spec := range s.specs {
		snapshot := metricSnapshot{OffsetMillis: time.Since(s.started).Milliseconds(), Source: spec.Name}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, nil)
		if err == nil {
			var resp *http.Response
			resp, err = s.client.Do(req)
			if err == nil {
				var body []byte
				body, err = io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil && resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("status %s", resp.Status)
				}
				if err == nil {
					snapshot.Body = string(body)
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			snapshot.Error = err.Error()
		}
		s.mu.Lock()
		s.samples = append(s.samples, snapshot)
		s.mu.Unlock()
	}
}
