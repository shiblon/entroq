// Package eqmr is an EXPERIMENTAL MapReduce built entirely on EntroQ task
// queues and documents. Its API, doc layout, and queue layout may change or be
// removed without a migration path. Do not depend on the on-queue or in-doc
// representations as a stable interchange format.
//
// # Shape
//
// A run is described by a Config and driven by three kinds of worker, each of
// which may run in its own process or pod:
//
//   - MapperWorker watches {Prefix}/map, claims one input split, runs the
//     Mapper over every KV in it, optionally applies a Combiner, partitions the
//     emitted keys into Config.ReduceShards buckets, and writes one spill doc
//     per non-empty bucket.
//   - ReducerWorker watches {Prefix}/reduce, claims every spill doc for one
//     partition, merges their sorted runs by key, runs the Reducer once per
//     key, and writes one result doc for the partition.
//   - ControlWorker watches {Prefix}/control, which holds exactly one
//     self-requeueing task carrying the phase state. EntroQ's claim guarantees
//     a single controller acts at a time, so any number of control pods may
//     run, and a crashed one is replaced once its claim is released.
//
// # Why the phase barriers are safe
//
// Each phase ends when a class of doc is exhausted, and each worker deletes its
// input doc in the same Modify that writes its output docs. So "no split docs
// remain" atomically implies "every spill doc exists", and "no spill docs
// remain" implies "every result doc exists". No coordinator state is needed for
// the barrier, and a worker that dies mid-task simply loses its claim.
//
// # Keys
//
// Doc keys here are ordinary readable text: "split/000007", "spill/000003",
// "result/000003". Arbitrary map keys are NOT doc keys. They travel as []byte
// inside doc content, where encoding/json represents them losslessly as base64.
// This matters because a doc key must survive both PostgreSQL TEXT (valid
// UTF-8, no NUL) and a JSON string (valid UTF-8), and encoding a byte string to
// fit those would make every key opaque in psql and in logs.
package eqmr

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shiblon/entroq"
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
)

// Config describes one MapReduce run. Both shard counts are required: they are
// baked into the doc layout when the map phase writes spills and cannot be
// changed mid-run, so defaulting them would silently pick a layout the caller
// never chose.
type Config struct {
	// Prefix is the doc namespace and the root of every queue name for this
	// run. It must be unique per run, valid UTF-8, NUL-free, and at most
	// MaxNamespaceBytes bytes. Required.
	Prefix string

	// MapShards is the number of input splits. Input KVs are divided into this
	// many split docs, each of which becomes one map task. Required, > 0.
	//
	// A split doc holds all of its KVs in a single doc value, so
	// len(input)/MapShards must comfortably fit the backend's message size
	// limit (10MB by default for the gRPC service).
	MapShards int

	// ReduceShards is the number of reduce partitions, and therefore the number
	// of reduce tasks the run creates. Map output keys are assigned to
	// partitions by ShardForKey. This is the reducer pool size. Required, > 0.
	ReduceShards int

	// Lease is the task and doc claim duration for every worker in the run.
	// Zero means DefaultLease.
	Lease time.Duration

	// ControlInterval paces the control loop: how long the controller waits
	// between phase-barrier checks while holding its claim. It is a loop
	// cadence, not a task schedule, so it should be a human-scale interval
	// rather than a fine-grained tick. Zero means DefaultControlInterval.
	ControlInterval time.Duration

	// StallTimeout fails the run when a phase reports no progress for this
	// long: no change in queue depth and no change in barrier state. Negative
	// disables stall detection entirely. Zero means DefaultStallTimeout.
	StallTimeout time.Duration
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
	return out
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

func (c *Config) validate() error {
	if c.Prefix == "" {
		return fmt.Errorf("eqmr: Config.Prefix is required")
	}
	if err := ValidText("Config.Prefix", c.Prefix, MaxNamespaceBytes); err != nil {
		return fmt.Errorf("eqmr: %w", err)
	}
	if c.MapShards <= 0 {
		return fmt.Errorf("eqmr: Config.MapShards must be set explicitly and be positive, got %d", c.MapShards)
	}
	if c.ReduceShards <= 0 {
		return fmt.Errorf("eqmr: Config.ReduceShards must be set explicitly and be positive, got %d", c.ReduceShards)
	}
	return nil
}

// Controller holds the queue and doc layout for one run and drives its phases.
// It is safe to construct one in every process that participates in the run:
// it carries no run state of its own, because all of it lives in EntroQ.
type Controller struct {
	client *entroq.EntroQ
	cfg    Config
}

// New creates a Controller for a run described by cfg. It returns an error
// rather than defaulting when a required field is missing.
func New(eq *entroq.EntroQ, cfg Config) (*Controller, error) {
	if eq == nil {
		return nil, fmt.Errorf("eqmr: nil client")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Controller{client: eq, cfg: cfg.withDefaults()}, nil
}

// Config returns the effective configuration, with defaults applied.
func (c *Controller) Config() Config { return c.cfg }

// DocNS is the doc namespace holding every split, spill, and result doc.
func (c *Controller) DocNS() string { return c.cfg.Prefix }

// MapQ is the queue of input-split tasks, watched by mapper workers.
func (c *Controller) MapQ() string { return c.cfg.Prefix + "/map" }

// ReduceQ is the queue of partition tasks, watched by reducer workers.
func (c *Controller) ReduceQ() string { return c.cfg.Prefix + "/reduce" }

// ControlQ holds the single self-requeueing control task.
func (c *Controller) ControlQ() string { return c.cfg.Prefix + "/control" }

// ErrQ is the quarantine queue for tasks that exhausted their attempts. A
// non-empty ErrQ fails the run: a MapReduce whose parts did not all succeed
// cannot make any claim about the correctness of its output.
func (c *Controller) ErrQ() string { return c.cfg.Prefix + "/err" }

// errQMap sends every worker's failures to the single run quarantine queue, so
// the controller has exactly one place to look.
func (c *Controller) errQMap(string) string { return c.ErrQ() }

// Doc key layout. These are readable on purpose: they are what shows up in psql
// and in `eqc` output when a run needs debugging.
func splitDocKey(n int) string  { return fmt.Sprintf("split/%06d", n) }
func spillDocKey(p int) string  { return fmt.Sprintf("spill/%06d", p) }
func resultDocKey(p int) string { return fmt.Sprintf("result/%06d", p) }

// resultDocID is a deterministic document id for a partition's output.
//
// It exists so that two workers racing the same partition cannot both write a
// result. An insert carrying an explicit id is rejected when that id already
// exists, which gives the empty-partition case the exclusion that a non-empty
// one gets from deleting its spill documents. Ids are capped at 64 bytes; this
// is 13.
func resultDocID(p int) string { return fmt.Sprintf("result-%06d", p) }

// Key range prefixes, and the exclusive upper bounds for scanning them. Every
// prefix ends in '/' (0x2F), so replacing that byte with '0' (0x30) yields the
// next string after every key under the prefix.
const (
	splitPrefix  = "split/"
	spillPrefix  = "spill/"
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
}

// asQuery reads the doc group by primary key without claiming it. Reading
// rather than claiming is what allows two workers to process the same unit
// concurrently; exclusion happens when they commit.
func (r docRef) asQuery() *entroq.DocQuery {
	return &entroq.DocQuery{Namespace: r.NS, KeyExact: r.Key}
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
