// Package eqmr is an EXPERIMENTAL MapReduce on EntroQ. Its API and its queue
// and document layouts may change without a migration path.
//
// # Roles
//
// A run has three roles, each its own process or pod if you like:
//
//   - Mappers (RunMapper) run the Mapper over one split, partition and sort
//     what it emits into immutable runs, and commit a finish task naming them.
//   - Reducers (RunReducer) merge one partition's runs, run the Reducer once
//     per key, stream the results into a staged attempt at the run's Sink, and
//     commit a finish task naming it. There is one unit per partition with data.
//   - Controllers (RunController) react to finish tasks and alone write run
//     state. Run any number of replicas.
//
// # Getting started
//
// One process, start to finish:
//
//	ctrl, err := eqmr.New(eq, "/wordcount/run-1",
//		eqmr.WithMapShards(8), eqmr.WithReduceShards(4))
//	if err != nil { return err }
//	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer,
//		eqmr.RunOptions{Mappers: 4, Reducers: 2}); err != nil {
//		return err
//	}
//	results, err := ctrl.Results(ctx)
//
// A mapper pod needs only the run prefix; the partition count arrives with
// each task:
//
//	ctrl, err := eqmr.New(eq, os.Getenv("MR_PREFIX"))
//	if err != nil { return err }
//	for {
//		if err := ctrl.RunMapper(ctx, myMapper); err != nil {
//			log.Printf("mapper exited: %v", err) // restart: see RunMapper
//		}
//	}
//
// # Completion
//
// The controller accepts the first finish for each unit while claiming only
// that unit's input, and deletes the input in the Modify that records the
// acceptance. That Modify also commits a check task. A check looks, without a
// claim, for any of its phase's inputs; one that finds none claims the run's
// state doc, checks again, and moves the run on. The last acceptance of a
// phase always commits a check that sees the phase complete.
//
// # Output
//
// Output goes to a Sink, which owns its format and readiness. The controller
// commits one staged attempt per partition and calls Sink.Finish before the
// run is done. Without WithSink, output goes to the run's document sink, which
// Results reads.
//
// # Text
//
// Keys and values are strings, and must be valid UTF-8 with no NUL, which
// PostgreSQL JSONB and JSON strings require. ValidateText is the rule; Setup,
// every emit and combine, and the document sink apply it. Document keys are
// readable: "state", "split/000007", "mapout/000003", "result/000003", and,
// for the document store and sink, "run/..." and "output/...".
package eqmr

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Limits mirrored from the backend schema, in bytes, which is stricter than
// PostgreSQL's character count.
const (
	// MaxNamespaceBytes bounds Config.Prefix, which becomes the doc namespace.
	MaxNamespaceBytes = 1024
	// MaxDocKeyBytes bounds doc primary and secondary keys.
	MaxDocKeyBytes = 256
)

// Defaults for zero Config fields. Shard counts have none; they must always be
// stated.
const (
	DefaultLease = 30 * time.Second

	// DefaultMaxClaims bounds how many times a unit may be claimed before it is
	// quarantined, which fails the run. Claims also count ordinary churn such
	// as rolling deploys, so raise it for a cluster that churns more.
	DefaultMaxClaims int32 = 10
)

// Config is a whole run configuration, passed with WithConfig. Every field has
// a matching option, and a zero field is left alone.
type Config struct {
	// MapShards is the number of input splits, each one split doc and one map
	// task. A split doc must fit the backend message limit (10MB by default
	// for the gRPC service).
	MapShards int

	// ReduceShards is the number of reduce partitions. Keys are assigned to
	// them by ShardForKey.
	ReduceShards int

	// Lease is the task claim duration for every worker in the run.
	Lease time.Duration

	// MaxClaims bounds how many times a unit may be claimed before it is
	// quarantined. Negative means unlimited.
	MaxClaims int32

	// IntermediateStore holds the run's intermediate data; nil means the run's
	// document store. See WithIntermediateStore.
	IntermediateStore IntermediateStore

	// Sink receives the run's output; nil means the run's document sink. See
	// WithSink.
	Sink Sink
}

// Option configures a Controller. See New.
type Option func(*Config)

// WithConfig merges cfg into the configuration, leaving zero fields alone.
func WithConfig(cfg Config) Option {
	return func(c *Config) {
		if cfg.MapShards != 0 {
			c.MapShards = cfg.MapShards
		}
		if cfg.ReduceShards != 0 {
			c.ReduceShards = cfg.ReduceShards
		}
		if cfg.Lease != 0 {
			c.Lease = cfg.Lease
		}
		if cfg.MaxClaims != 0 {
			c.MaxClaims = cfg.MaxClaims
		}
		if cfg.IntermediateStore != nil {
			c.IntermediateStore = cfg.IntermediateStore
		}
		if cfg.Sink != nil {
			c.Sink = cfg.Sink
		}
	}
}

// WithMapShards sets the number of input splits. Setup requires it.
func WithMapShards(n int) Option {
	return func(c *Config) { c.MapShards = n }
}

// WithReduceShards sets the number of reduce partitions. Setup requires it.
func WithReduceShards(n int) Option {
	return func(c *Config) { c.ReduceShards = n }
}

// WithLease sets the task claim duration for workers in this run. Renewal
// happens at half the lease, so a renewal must complete within the other half;
// a reduce unit is a whole partition, so size it for the largest.
func WithLease(d time.Duration) Option {
	return func(c *Config) { c.Lease = d }
}

// WithIntermediateStore keeps the run's intermediate data in store; see
// NewFileStore. Setup records its descriptor, and every process that maps,
// reduces, or controls the run must be configured with a store that reports
// the same one.
func WithIntermediateStore(store IntermediateStore) Option {
	return func(c *Config) { c.IntermediateStore = store }
}

// WithSink sends the run's output to sink. Setup records its Descriptor, and
// every process that reduces or controls the run must be configured with a sink
// that reports the same one.
func WithSink(sink Sink) Option {
	return func(c *Config) { c.Sink = sink }
}

// WithMaxClaims bounds how many times a unit may be claimed before it is
// quarantined, which fails the run. Negative means unlimited.
func WithMaxClaims(n int32) Option {
	return func(c *Config) { c.MaxClaims = n }
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Lease == 0 {
		out.Lease = DefaultLease
	}
	if out.MaxClaims == 0 {
		out.MaxClaims = DefaultMaxClaims
	}
	return out
}

// ValidateText reports whether s is usable as a MapReduce key or value: valid
// UTF-8, with no NUL. PostgreSQL JSONB rejects NUL, and Go's encoding/json
// silently replaces invalid UTF-8.
func ValidateText(what, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8 (encode it, with base64 or similar, if it is genuinely binary)", what)
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s contains a NUL byte, which PostgreSQL JSONB cannot store (encode it if it is genuinely binary)", what)
	}
	return nil
}

// ValidText reports whether s is usable as a doc namespace or key: valid UTF-8,
// free of NUL, and within maxBytes.
func ValidText(what, s string, maxBytes int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8: %q", what, s)
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s contains a NUL byte: %q", what, s)
	}
	if len(s) > maxBytes {
		return fmt.Errorf("%s is %d bytes, limit is %d: %q", what, len(s), maxBytes, s)
	}
	return nil
}

// Controller is one process's handle on a run: its layout, and the stores and
// sinks this process can reach. Run state lives in EntroQ, so every
// participating process constructs its own.
type Controller struct {
	client *entroq.EntroQ
	cfg    Config
	prefix string

	// stores and sinks are what this process can reach, by descriptor.
	stores intermediateStores
	sinks  sinks
	// store and sink are the ones Setup records for a new run.
	store IntermediateStore
	sink  Sink
}

// New creates a Controller for the run under prefix, which is the document
// namespace and the root of every queue name. It must be unique per run, valid
// UTF-8, NUL-free, and at most MaxNamespaceBytes bytes. Worker processes need
// no options; the layout travels with their tasks.
func New(eq *entroq.EntroQ, prefix string, opts ...Option) (*Controller, error) {
	if eq == nil {
		return nil, fmt.Errorf("eqmr: nil client")
	}
	if prefix == "" {
		return nil, fmt.Errorf("eqmr: prefix is required")
	}
	if err := ValidText("prefix", prefix, MaxNamespaceBytes); err != nil {
		return nil, fmt.Errorf("eqmr: %w", err)
	}
	cfg := new(Config)
	for _, opt := range opts {
		opt(cfg)
	}
	c := &Controller{client: eq, cfg: cfg.withDefaults(), prefix: prefix}

	docStore := c.documentStore()
	storeList := []IntermediateStore{docStore}
	c.store = docStore
	if cfg.IntermediateStore != nil {
		c.store = cfg.IntermediateStore
		if cfg.IntermediateStore.descriptor() != docStore.descriptor() {
			storeList = append(storeList, cfg.IntermediateStore)
		}
	}
	stores, err := newIntermediateStores(storeList...)
	if err != nil {
		return nil, err
	}
	docSink := newDocumentSink(eq, c.DocNS())
	registered := []Sink{docSink}
	c.sink = docSink
	if cfg.Sink != nil {
		c.sink = cfg.Sink
		if cfg.Sink.Descriptor() != docSink.Descriptor() {
			registered = append(registered, cfg.Sink)
		}
	}
	sinks, err := newSinks(registered...)
	if err != nil {
		return nil, err
	}
	c.stores, c.sinks = stores, sinks
	return c, nil
}

// validateForSetup checks the fields only Setup requires.
func (c *Config) validateForSetup() error {
	if c.MapShards <= 0 {
		return fmt.Errorf("eqmr: MapShards must be set with WithMapShards; it fixes how input is divided when a run is created and cannot change afterwards")
	}
	if c.ReduceShards <= 0 {
		return fmt.Errorf("eqmr: ReduceShards must be set with WithReduceShards; it fixes the document layout when the map phase writes its output and cannot change mid-run")
	}
	return nil
}

// Config returns the effective configuration, with defaults applied.
func (c *Controller) Config() Config { return c.cfg }

// Prefix returns the document namespace and queue root for this run.
func (c *Controller) Prefix() string { return c.prefix }

// DocNS is the run's doc namespace: its state doc, splits, pointers, and result
// markers, and whatever the document store and sink keep.
func (c *Controller) DocNS() string { return c.prefix }

// MapQ is the queue of input-split tasks, watched by mapper workers.
func (c *Controller) MapQ() string { return c.prefix + "/map" }

// ReduceQ is the queue of partition tasks, watched by reducer workers.
func (c *Controller) ReduceQ() string { return c.prefix + "/reduce" }

// MapDoneQ holds mapper finish tasks, watched by the controller.
func (c *Controller) MapDoneQ() string { return c.prefix + "/map-done" }

// MapCheckQ holds checks for the end of the map phase, watched by the
// controller.
func (c *Controller) MapCheckQ() string { return c.prefix + "/map-check" }

// ReduceCheckQ holds checks for the end of the reduce phase, watched by the
// controller.
func (c *Controller) ReduceCheckQ() string { return c.prefix + "/reduce-check" }

// ReduceDoneQ holds reducer finish tasks, watched by the controller.
func (c *Controller) ReduceDoneQ() string { return c.prefix + "/reduce-done" }

// GCQ holds collection tasks, watched by the controller.
func (c *Controller) GCQ() string { return c.prefix + "/gc" }

// QuarantineQ keeps failed tasks, with their errors, for inspection.
func (c *Controller) QuarantineQ() string { return c.prefix + "/quarantine" }

// errQSuffix makes a queue's error queue, as worker.DefaultErrQMap does. Each
// worker quarantines to its own inbox's, so the queue says what failed.
const errQSuffix = "/err"

// errQueues lists the error queues of the run's worker queues. The error
// worker quarantines to QuarantineQ.
func (c *Controller) errQueues() []string {
	inboxes := []string{c.MapQ(), c.ReduceQ(), c.MapDoneQ(), c.MapCheckQ(), c.ReduceDoneQ(), c.ReduceCheckQ(), c.GCQ()}
	qs := make([]string, len(inboxes))
	for i, q := range inboxes {
		qs[i] = q + errQSuffix
	}
	return qs
}

// Doc keys, readable in psql and eqc output.
func splitDocKey(n int) string { return fmt.Sprintf("%s%06d", splitPrefix, n) }

// splitDocID is a deterministic split doc id, so a retried Setup recognizes
// staged splits.
func splitDocID(n int) string   { return fmt.Sprintf("split-%06d", n) }
func mapOutDocKey(p int) string { return fmt.Sprintf("%s%06d", mapOutPrefix, p) }
func resultDocKey(p int) string { return fmt.Sprintf("%s%06d", resultPrefix, p) }

// mapOutSecondaryKey names run i of the split keyed splitKey. A partition's
// pointers share a primary key, so that claiming it takes the partition; this
// keeps each (key, secondary) pair unique.
func mapOutSecondaryKey(splitKey string, i int) string {
	return fmt.Sprintf("%s/%04d", splitKey, i)
}

// resultDocID is the id of the marker that a partition's output is committed.
// Being explicit, it makes a second marker for the partition an error.
func resultDocID(p int) string { return fmt.Sprintf("result-%06d", p) }

// Key prefixes. Each ends in '/', so replacing it with '0' bounds a scan.
const (
	splitPrefix  = "split/"
	mapOutPrefix = "mapout/"
	resultPrefix = "result/"
)

// prefixEnd returns the exclusive upper bound for a scan over keys beginning
// with prefix, which must end in '/'.
func prefixEnd(prefix string) string {
	if prefix == "" || prefix[len(prefix)-1] != '/' {
		panic(fmt.Sprintf("eqmr: key prefix %q must end in '/'", prefix))
	}
	return prefix[:len(prefix)-1] + "0"
}

// docRef identifies a doc by namespace and primary key. It is the value of a
// map task: the split the mapper reads.
type docRef struct {
	NS  string `json:"ns"`
	Key string `json:"key"`
	// ReduceShards is the run's partition count, stamped by Setup.
	ReduceShards int `json:"reduce_shards"`
	// Store is the run's intermediate store, stamped by Setup.
	Store Descriptor `json:"store,omitzero"`
}

// asQuery reads the doc group by primary key, without claiming it.
func (r docRef) asQuery() *entroq.DocQuery {
	return &entroq.DocQuery{Namespace: r.NS, KeyExact: r.Key}
}

// RunMapper runs a mapper for this run until ctx ends or the worker exits. A
// Mapper error exits the worker; restart it. The task is reclaimed when its
// lease expires, and the claim ceiling fails a run whose record always fails.
func (c *Controller) RunMapper(ctx context.Context, mapFn Mapper, opts ...MapperOption) error {
	return c.MapperWorker(mapFn, opts...).Run(ctx, c.workerRunOptions(c.MapQ())...)
}

// RunReducer runs a reducer for this run until ctx ends or the worker exits;
// restart it, as with RunMapper.
func (c *Controller) RunReducer(ctx context.Context, reduceFn Reducer) error {
	return c.ReducerWorker(reduceFn).Run(ctx, c.workerRunOptions(c.ReduceQ())...)
}

// workerRunOptions configures mappers and reducers.
func (c *Controller) workerRunOptions(queue string) []worker.RunOption {
	opts := []worker.RunOption{
		worker.Watching(queue),
		worker.WithLease(c.cfg.Lease),
	}
	if c.cfg.MaxClaims > 0 {
		opts = append(opts, worker.WithMaxClaims(c.cfg.MaxClaims))
	}
	return opts
}
