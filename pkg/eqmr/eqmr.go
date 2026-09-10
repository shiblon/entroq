// Package eqmr is an EXPERIMENTAL MapReduce built entirely on EntroQ task
// queues and documents. Its API and its queue and document layouts may change
// or be removed without a migration path. Do not treat what it writes as a
// stable interchange format.
//
// # Roles
//
// A run has three roles, each of which may be its own process or pod, and each
// with its own scaling property:
//
//   - Mappers (RunMapper) claim one input split, run the Mapper over every pair
//     in it, optionally apply a Combiner, assign each emitted key to a
//     partition, and write one map-output document per non-empty partition.
//     Scale them freely.
//   - Reducers (RunReducer) claim every map-output document for one partition,
//     merge those sorted runs by key, run the Reducer once per key, and write
//     the partition's result. There is one unit per partition, so scale to the
//     partition count.
//   - Controllers (RunController) drive the phase machine. The control queue
//     holds exactly one task, so a claim makes exactly one controller act at a
//     time; run as many replicas as you like for failover.
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
// A mapper pod needs the run prefix and nothing else. The partition count it
// must agree on arrives with each task:
//
//	ctrl, err := eqmr.New(eq, os.Getenv("MR_PREFIX"))
//	if err != nil { return err }
//	for {
//		if err := ctrl.RunMapper(ctx, myMapper); err != nil {
//			log.Printf("mapper exited: %v", err) // restart: see RunMapper
//		}
//	}
//
// Shard counts are required by Setup, which is where the layout is fixed. See
// the examples for a controller pod and for combiners.
//
// # Completion
//
// Each worker deletes its input document in the same Modify that writes its
// output, so document state alone says how far a run has got. No split
// documents remain exactly when every map-output document exists, and every
// partition writes a result, empty ones included, so counting results measures
// the reduce phase. Progress reports both; no queue is consulted to decide
// completion.
//
// # Text
//
// Keys and values are strings, and must be valid UTF-8 with no NUL. Content is
// JSONB in PostgreSQL, which rejects NUL, and a JSON string cannot represent
// invalid UTF-8. ValidateText states the rule, and Setup and every emit apply
// it, so a violation fails where it was produced. Encode binary keys in the job
// that needs them. Document keys are readable text: "split/000007",
// "mapout/000003", "result/000003".
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

// Limits mirrored from the backend schema. Note that the PostgreSQL CHECK
// constraints use length(), which counts CHARACTERS, while the btree index they
// feed is bounded in BYTES. Go's len() counts bytes, so validating here is
// strictly the safer of the two and rejects multi-byte keys that Postgres would
// accept but that consume more index space than intended.
const (
	// MaxNamespaceBytes bounds Config.Prefix, which becomes the doc namespace.
	MaxNamespaceBytes = 1024
	// MaxDocKeyBytes bounds doc primary and secondary keys.
	MaxDocKeyBytes = 256
)

// Defaults applied when a Config leaves the corresponding field zero. Shard
// counts are deliberately absent: they change the on-doc layout and cannot be
// altered once a run has started, so they must always be stated.
const (
	DefaultLease           = 30 * time.Second
	DefaultControlInterval = time.Second
	DefaultStallTimeout    = 5 * time.Minute

	// DefaultMaxClaims bounds how many times a unit may be claimed before it is
	// quarantined, which fails the run.
	//
	// A claim count covers both work that kills workers and ordinary
	// infrastructure churn such as rolling deploys and evictions, so the bound
	// has to survive a handful of the latter while still catching the former.
	// Ten does. Raise it for a cluster that churns more; WithMaxClaims takes a
	// negative value for no bound at all.
	DefaultMaxClaims int32 = 10
)

// Config carries a whole run configuration at once, for callers who would
// rather build a struct than a list of options. Pass it with WithConfig.
//
// Every field has a matching option, and options are the primary form. A zero
// field is left alone, so a partially filled Config merges with other options.
type Config struct {
	// MapShards is the number of input splits. Input pairs are divided into
	// this many split documents, each becoming one map task.
	//
	// A split document holds all of its pairs in one value, so
	// len(input)/MapShards must comfortably fit the backend's message size
	// limit (10MB by default for the gRPC service).
	MapShards int

	// ReduceShards is the number of reduce partitions, and therefore the number
	// of reduce tasks a run creates. Intermediate keys are assigned to
	// partitions by ShardForKey.
	ReduceShards int

	// Lease is the task claim duration for every worker in the run.
	Lease time.Duration

	// ControlInterval paces the control loop.
	ControlInterval time.Duration

	// StallTimeout fails a run whose phase stops making progress. Negative
	// disables stall detection.
	StallTimeout time.Duration

	// MaxClaims bounds how many times a unit may be claimed before it is
	// quarantined. Negative means unlimited.
	MaxClaims int32
}

// Option configures a Controller. See New.
type Option func(*Config)

// WithConfig merges a whole Config into the configuration. Zero fields are left
// alone, so it can be combined with other options in any order.
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
		if cfg.ControlInterval != 0 {
			c.ControlInterval = cfg.ControlInterval
		}
		if cfg.StallTimeout != 0 {
			c.StallTimeout = cfg.StallTimeout
		}
		if cfg.MaxClaims != 0 {
			c.MaxClaims = cfg.MaxClaims
		}
	}
}

// WithMapShards sets the number of input splits.
//
// Required by Setup, and only by Setup: it fixes the document layout at the
// moment a run is created. Worker processes never need it, so a mapper or
// reducer pod constructs its Controller without one.
func WithMapShards(n int) Option {
	return func(c *Config) { c.MapShards = n }
}

// WithReduceShards sets the number of reduce partitions.
//
// Required by Setup, and only by Setup. Mappers read the number from their own
// task, so mapper processes need no configuration of their own.
func WithReduceShards(n int) Option {
	return func(c *Config) { c.ReduceShards = n }
}

// WithLease sets the task claim duration for workers in this run.
//
// It has to outlast a single unit of work. A reduce unit is a whole partition,
// so its duration scales with how much the map phase sent there rather than
// with a fixed per-record cost; size this against the largest partition you
// expect, not the average. Renewal happens at half the lease, and a unit whose
// renewal slips is reclaimed mid-work and eventually quarantined.
func WithLease(d time.Duration) Option {
	return func(c *Config) { c.Lease = d }
}

// WithControlInterval paces the control loop: how long the controller waits
// between phase-barrier checks while holding its claim. It is a loop cadence
// rather than a task schedule, so a human-scale interval suits it.
func WithControlInterval(d time.Duration) Option {
	return func(c *Config) { c.ControlInterval = d }
}

// WithStallTimeout fails a run whose current phase reports no progress for this
// long. A negative duration disables stall detection.
func WithStallTimeout(d time.Duration) Option {
	return func(c *Config) { c.StallTimeout = d }
}

// WithMaxClaims bounds how many times a unit may be claimed before it is
// quarantined, which fails the run. Negative means unlimited, reproducing the
// pkg/worker default; see DefaultMaxClaims for why this package takes a
// position where a general worker cannot.
func WithMaxClaims(n int32) Option {
	return func(c *Config) { c.MaxClaims = n }
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Lease == 0 {
		out.Lease = DefaultLease
	}
	if out.ControlInterval == 0 {
		out.ControlInterval = DefaultControlInterval
	}
	if out.StallTimeout == 0 {
		out.StallTimeout = DefaultStallTimeout
	}
	if out.MaxClaims == 0 {
		out.MaxClaims = DefaultMaxClaims
	}
	return out
}

// ValidateText reports whether s is usable as a MapReduce key or value: valid
// UTF-8, with no NUL.
//
// Keys and values live inside document content, which is JSONB in PostgreSQL,
// and JSONB rejects \u0000 ("unsupported Unicode escape sequence"). A JSON
// string cannot represent invalid UTF-8 at all, and Go's encoding/json
// substitutes U+FFFD for it silently.
//
// Setup and every emit apply this, so a violation fails where it was produced.
// Encode binary keys or values in the job that needs them.
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
// free of NUL, and within maxBytes. Both restrictions are real. PostgreSQL TEXT
// in a UTF-8 database rejects invalid sequences and NUL outright, and a JSON
// string cannot represent invalid UTF-8 at all (encoding/json silently
// substitutes U+FFFD rather than failing, so an unvalidated key corrupts on the
// way through the wire rather than erroring at the point of the mistake).
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

// Controller holds the queue and doc layout for one run and drives its phases.
// It is safe to construct one in every process that participates in the run:
// it carries no run state of its own, because all of it lives in EntroQ.
type Controller struct {
	client *entroq.EntroQ
	cfg    Config
	prefix string
}

// New creates a Controller for the run under prefix.
//
// prefix is the document namespace and the root of every queue name, and it is
// the only thing a participating process cannot work out for itself. It must be
// unique per run, valid UTF-8, NUL-free, and at most MaxNamespaceBytes bytes.
//
// Shard counts are options rather than parameters because only Setup needs
// them. A mapper or reducer pod says
//
//	ctrl, err := eqmr.New(eq, prefix)
//
// and nothing more: the layout it must agree on travels with its work.
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
	return &Controller{client: eq, cfg: cfg.withDefaults(), prefix: prefix}, nil
}

// validateForSetup checks the fields only Setup requires. A Controller built for
// a worker role legitimately has neither shard count, so these are not checked
// at construction.
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

// DocNS is the doc namespace holding every split, map output, and result doc.
func (c *Controller) DocNS() string { return c.prefix }

// MapQ is the queue of input-split tasks, watched by mapper workers.
func (c *Controller) MapQ() string { return c.prefix + "/map" }

// ReduceQ is the queue of partition tasks, watched by reducer workers.
func (c *Controller) ReduceQ() string { return c.prefix + "/reduce" }

// ControlQ holds the single self-requeueing control task.
func (c *Controller) ControlQ() string { return c.prefix + "/control" }

// ErrQ is the quarantine queue for tasks that exhausted their attempts. A
// non-empty ErrQ fails the run: a MapReduce whose parts did not all succeed
// cannot make any claim about the correctness of its output.
func (c *Controller) ErrQ() string { return c.prefix + "/err" }

// errQMap sends every worker's failures to the single run quarantine queue, so
// the controller has exactly one place to look.
func (c *Controller) errQMap(string) string { return c.ErrQ() }

// Doc key layout. These are readable on purpose: they are what shows up in psql
// and in `eqc` output when a run needs debugging.
func splitDocKey(n int) string  { return fmt.Sprintf("split/%06d", n) }
func mapOutDocKey(p int) string { return fmt.Sprintf("mapout/%06d", p) }
func resultDocKey(p int) string { return fmt.Sprintf("result/%06d", p) }

// resultDocID is a deterministic document id for a partition's output.
//
// It exists so that two workers racing the same partition cannot both write a
// result. An insert carrying an explicit id is rejected when that id already
// exists, which gives the empty-partition case the exclusion that a non-empty
// one gets from deleting its map output documents. Ids are capped at 64 bytes; this
// is 13.
func resultDocID(p int) string { return fmt.Sprintf("result-%06d", p) }

// Key range prefixes, and the exclusive upper bounds for scanning them. Every
// prefix ends in '/' (0x2F), so replacing that byte with '0' (0x30) yields the
// next string after every key under the prefix.
const (
	splitPrefix  = "split/"
	mapOutPrefix = "mapout/"
	resultPrefix = "result/"
)

// prefixEnd returns the exclusive upper bound for a scan over keys beginning
// with prefix. It requires an ASCII prefix ending in '/', which every layout
// constant above satisfies.
func prefixEnd(prefix string) string {
	if prefix == "" || prefix[len(prefix)-1] != '/' {
		panic(fmt.Sprintf("eqmr: key prefix %q must end in '/'", prefix))
	}
	return prefix[:len(prefix)-1] + "0"
}

// docRef identifies a doc by namespace and primary key. It is the value of a
// map task, and it is what lets a worker claim its input before working.
type docRef struct {
	NS  string `json:"ns"`
	Key string `json:"key"`
	// ReduceShards is the partition count for the run, stamped by Setup. A
	// mapper reads it here to decide which partition each emitted key belongs
	// to, so mapper processes need no configuration of their own and no two
	// can disagree about it.
	ReduceShards int `json:"reduce_shards"`
}

// asQuery reads the doc group by primary key without claiming it. Reading
// rather than claiming is what allows two workers to process the same unit
// concurrently; exclusion happens when they commit.
func (r docRef) asQuery() *entroq.DocQuery {
	return &entroq.DocQuery{Namespace: r.NS, KeyExact: r.Key}
}

// RunMapper runs a mapper for this run until ctx ends or the worker exits. It
// watches the map queue with the run's lease and claim ceiling.
//
// A Mapper that returns an error kills the worker, which is the MapReduce
// contract: a run whose map calls failed cannot support a claim about its
// output. Restart it and let the pipeline decide. The task is reclaimed when
// its lease expires, and a record that kills every worker exhausts the claim
// ceiling and fails the run.
func (c *Controller) RunMapper(ctx context.Context, mapFn Mapper, opts ...MapperOption) error {
	return c.MapperWorker(mapFn, opts...).Run(ctx, c.workerRunOptions(c.MapQ())...)
}

// RunReducer runs a reducer for this run until ctx ends or the worker exits.
// Restart it on return, as with RunMapper.
func (c *Controller) RunReducer(ctx context.Context, reduceFn Reducer) error {
	return c.ReducerWorker(reduceFn).Run(ctx, c.workerRunOptions(c.ReduceQ())...)
}

// RunController runs the phase machine for this run.
//
// Run as many as you like. The control queue holds exactly one task, so EntroQ's
// claim guarantees a single controller acts at a time, and a replica that dies
// is replaced once its claim is released.
func (c *Controller) RunController(ctx context.Context) error {
	return c.ControlWorker().Run(ctx, c.controlRunOptions()...)
}

// workerRunOptions is the run configuration shared by mappers and reducers.
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

// controlRunOptions deliberately carries no claim ceiling. The control task is
// claimed once per tick for the life of a run, so any ceiling would quarantine
// the controller after seconds of healthy operation and leave the run with
// nothing driving it.
func (c *Controller) controlRunOptions() []worker.RunOption {
	return []worker.RunOption{
		worker.Watching(c.ControlQ()),
		worker.WithLease(c.cfg.Lease),
	}
}

// countDocs counts the docs under a key prefix. It lists metadata only, so the
// cost is one scan of the key range with no content transferred; a run with a
// very large MapShards pays for that scan on every control tick, which is the
// price of a doc-based barrier that also reports progress.
func (c *Controller) countDocs(ctx context.Context, prefix string) (int, error) {
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  c.DocNS(),
		KeyStart:   prefix,
		KeyEnd:     prefixEnd(prefix),
		OmitValues: true,
	})
	if err != nil {
		return 0, fmt.Errorf("count %q docs: %w", prefix, err)
	}
	return len(docs), nil
}

// claimedTasks counts the tasks currently claimed across the run's work queues.
// It is how "slow" is told apart from "stalled": documents are no longer
// claimed by workers, so a held task is the only evidence that something is
// actively being worked on.
func (c *Controller) claimedTasks(ctx context.Context) (int, error) {
	stats, err := c.client.QueueStats(ctx, entroq.MatchExact(c.MapQ(), c.ReduceQ()))
	if err != nil {
		return 0, fmt.Errorf("claimed task check: %w", err)
	}
	n := 0
	for _, s := range stats {
		n += s.Claimed
	}
	return n, nil
}
