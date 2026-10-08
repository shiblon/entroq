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
//   - Mappers (RunMapper) claim one map task, run the Mapper over every pair
//     in its split, optionally apply a Combiner, assign each emitted key to a
//     partition, write bounded immutable runs, and commit a finish task naming
//     them. Scale them freely.
//   - Reducers (RunReducer) open every published run for one partition, merge
//     their record streams by key, run the Reducer once per key, stream each
//     result into a staged attempt at the run's Sink, and commit a finish task
//     naming the attempt. There is one unit per partition with data, so scale
//     to the partition count.
//   - Controllers (RunController) are a set of workers reacting to finish
//     tasks. They alone write run state: they accept the first finish for each
//     unit, publish its runs or commit its output, collect what losers and
//     consumed partitions leave behind, tell the Sink when the output is
//     complete, sweep a run once it has ended, and judge quarantined tasks.
//     Run as many replicas as you like.
//
// # Output
//
// Output goes to a Sink, which owns its format, naming, and readiness. The run
// only hands it values: a reducer stages an attempt at a partition, the
// controller commits exactly one attempt per partition, and when every
// partition is committed the controller calls Sink.Finish before the run is
// done. Without WithSink, output goes to the run's own document sink, meant
// for small jobs, which Results reads back.
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
// Workers never write documents. Each commits a finish task in the same Modify
// that deletes its own task, so a finish exists exactly when the work it
// describes is done. A controller worker accepts the first finish for each
// unit while holding a claim on that unit's input alone, deleting the input in
// the Modify that records the acceptance, so acceptances of different units
// never wait on one another.
//
// That Modify also commits a check task. A check reads, cheaply and without a
// claim, whether its phase's inputs are all gone; usually they are not, and it
// simply retires. One that finds them gone claims the run's state doc, checks
// again under the claim, and moves the run on: to the reduce phase, fanning
// out a reduce task per partition that received data, or to done. Because each
// check is created by the commit it follows, the check committed by whichever
// acceptance is truly last is certain to see its phase complete, and the run
// as a whole is serialized only at phase boundaries.
//
// # Text
//
// Keys and values are strings, and must be valid UTF-8 with no NUL. Content is
// JSONB in PostgreSQL, which rejects NUL, and a JSON string cannot represent
// invalid UTF-8. ValidateText states the rule, and Setup, every emit, every
// combine, and the document sink apply it, so a violation fails where it was
// produced. Encode binary keys in the job that needs them. Document keys are
// readable text: "state", "split/000007", "mapout/000003", "result/000003",
// and, for the document store and sink, "run/..." and "output/...".
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
	DefaultLease = 30 * time.Second

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

	// MaxClaims bounds how many times a unit may be claimed before it is
	// quarantined. Negative means unlimited.
	MaxClaims int32

	// IntermediateStore holds the run's intermediate data. Setup records it in
	// the run, and every process that maps, reduces, or controls the run must be
	// configured with it. Nil means the run's own document store.
	IntermediateStore IntermediateStore

	// Sink receives the run's output. Setup records it in the run, and every
	// process that reduces or controls the run must be configured with it.
	// Nil means the run's own document sink, which Results can read back.
	Sink Sink
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

// WithIntermediateStore keeps the run's intermediate data in store instead of
// the run's document store. See NewFileStore.
//
// Setup records the store's descriptor in the run, so every process that maps,
// reduces, or controls the run needs this option too, with a store that
// reports the same descriptor; one that cannot resolve it exits rather than
// read or write elsewhere.
func WithIntermediateStore(store IntermediateStore) Option {
	return func(c *Config) { c.IntermediateStore = store }
}

// WithSink sends the run's output to sink instead of the run's document sink.
//
// Setup records the sink's Descriptor in the run, so every process that
// reduces or controls the run needs this option too, with a sink that reports
// the same descriptor; one that cannot resolve it exits rather than write
// elsewhere. Mapper processes do not need it.
func WithSink(sink Sink) Option {
	return func(c *Config) { c.Sink = sink }
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

	// stores and sinks are what this process can reach, by descriptor.
	stores intermediateStores
	sinks  sinks
	// store and sink are the ones Setup records for a new run.
	store IntermediateStore
	sink  Sink
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

// DocNS is the doc namespace holding the run's coordination state: the state
// doc and every split, map-output pointer, and result marker, plus whatever the
// document-backed store and sink keep there when they are in use. Only
// controller workers write coordination state, apart from Setup.
func (c *Controller) DocNS() string { return c.prefix }

// MapQ is the queue of input-split tasks, watched by mapper workers.
func (c *Controller) MapQ() string { return c.prefix + "/map" }

// ReduceQ is the queue of partition tasks, watched by reducer workers.
func (c *Controller) ReduceQ() string { return c.prefix + "/reduce" }

// MapDoneQ holds mapper finish tasks, watched by the controller.
func (c *Controller) MapDoneQ() string { return c.prefix + "/map-done" }

// MapCheckQ holds checks for the end of the map phase, one committed with each
// accepted map finish, watched by the controller.
func (c *Controller) MapCheckQ() string { return c.prefix + "/map-check" }

// ReduceCheckQ holds checks for the end of the reduce phase, one committed
// with each accepted reduce finish, watched by the controller.
func (c *Controller) ReduceCheckQ() string { return c.prefix + "/reduce-check" }

// ReduceDoneQ holds reducer finish tasks, watched by the controller.
func (c *Controller) ReduceDoneQ() string { return c.prefix + "/reduce-done" }

// GCQ holds requests to delete intermediate runs, watched by the controller.
func (c *Controller) GCQ() string { return c.prefix + "/gc" }

// QuarantineQ holds tasks the controller judged to be genuine failures. They
// are kept, with their errors, for inspection.
func (c *Controller) QuarantineQ() string { return c.prefix + "/quarantine" }

// errQSuffix is appended to a queue name to get its error queue, matching
// worker.DefaultErrQMap. Every worker in a run quarantines to its own inbox's
// error queue, so the controller can tell from the queue alone what failed.
const errQSuffix = "/err"

// errQueues lists the error queue of every queue in the run that a worker
// watches, except the error worker's own: it quarantines to QuarantineQ.
func (c *Controller) errQueues() []string {
	inboxes := []string{c.MapQ(), c.ReduceQ(), c.MapDoneQ(), c.MapCheckQ(), c.ReduceDoneQ(), c.ReduceCheckQ(), c.GCQ()}
	qs := make([]string, len(inboxes))
	for i, q := range inboxes {
		qs[i] = q + errQSuffix
	}
	return qs
}

// Doc key layout. These are readable on purpose: they are what shows up in psql
// and in `eqc` output when a run needs debugging.
func splitDocKey(n int) string { return fmt.Sprintf("%s%06d", splitPrefix, n) }

// splitDocID is a deterministic id for a split doc, so that a retried Setup
// recognizes the splits an earlier attempt already staged.
func splitDocID(n int) string   { return fmt.Sprintf("split-%06d", n) }
func mapOutDocKey(p int) string { return fmt.Sprintf("%s%06d", mapOutPrefix, p) }
func resultDocKey(p int) string { return fmt.Sprintf("%s%06d", resultPrefix, p) }

// mapOutSecondaryKey tells apart the pointers that share a partition's key:
// run i of the split keyed splitKey. Every pointer for a partition shares its
// primary key, so claiming that key takes the whole partition, and the
// secondary key keeps each (key, secondary) pair unique. A split is accepted
// once, so its runs' secondary keys never collide.
func mapOutSecondaryKey(splitKey string, i int) string {
	return fmt.Sprintf("%s/%04d", splitKey, i)
}

// resultDocID is a deterministic document id for the marker that a partition's
// output is committed. The output itself lives in the sink.
//
// The controller already accepts exactly one finish per partition, by claiming
// its pointer docs and deleting them as it records the acceptance. The explicit id
// makes a second marker for the same partition an error rather than a silent
// duplicate should that ever fail. Ids are capped at 64 bytes; this is 13.
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
// map task: the split the mapper reads.
type docRef struct {
	NS  string `json:"ns"`
	Key string `json:"key"`
	// ReduceShards is the partition count for the run, stamped by Setup. A
	// mapper reads it here to decide which partition each emitted key belongs
	// to, so mapper processes need no configuration of their own and no two
	// can disagree about it.
	ReduceShards int `json:"reduce_shards"`
	// Store is the intermediate store for the run, also stamped by Setup, so
	// every mapper writes where the run's reducers will read. A process that
	// cannot resolve it exits rather than write somewhere else.
	Store Descriptor `json:"store,omitzero"`
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
