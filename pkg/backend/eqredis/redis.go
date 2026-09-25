// Package eqredis implements an entroq.Backend using Redis.
//
// Data model:
//
//	{eq}:q:{name}           -- ZSET: member=taskID, score=atUnixMilli (task index per queue)
//	{eq}:t:{id}             -- Hash: task fields (id, queue, value, at, created, modified, claimant, version, claims, attempt, err)
//	{eq}:qs                 -- Set: active queue names (maintained lazily; GC removes empties)
//	{eq}:qsclaimed:{name}   -- ZSET: claimed task IDs, score=atUnixMilli; ZCOUNT >now gives exact claimed count
//	{eq}:d:{ns}/{id}        -- Hash: doc fields (namespace, id, version, claimant, at, key_primary, key_secondary, content, created, modified)
//	{eq}:dnsidx:{ns}        -- ZSET: doc namespace index, member=keyPrimary\x00keySecondary\x00id, score=0
//	{eq}:ns                 -- Set: active namespace names (maintained lazily; GC removes empties)
//	{eq}:nsclaimed:{ns}     -- ZSET: claimed doc IDs, score=atUnixMilli; ZCOUNT >now gives exact claimed count
//
// Key hash tags:
//
//	All keys share the hash tag "{eq}", which forces Redis cluster mode to assign
//	them all to the same hash slot. This is required for WATCH/MULTI/EXEC to work
//	correctly: a transaction that watches keys across multiple slots is not
//	supported by Redis cluster. The cost is that entroq data occupies exactly one
//	slot (one primary shard), so the backend does not scale horizontally with
//	cluster size -- but it does gain the operational benefits of cluster mode
//	(managed services, HA, automatic failover).
//
// Atomicity:
//
//	TryClaim and Modify use WATCH + MULTI/EXEC (optimistic locking).
//	Since Redis is single-threaded, MULTI/EXEC blocks are atomic; WATCH detects
//	concurrent modification and triggers a retry in Go.
//
// Notifications:
//
//	SubQ is used for in-process "something changed" signals. A readiness loop
//	(WithReadinessInterval) periodically counts available tasks in queues that
//	have waiters and wakes them, which covers tasks that become ready with the
//	passage of time and changes made by other processes sharing the Redis.
//	Without it, those are found only by the claim poll (ClaimQuery.PollTime).
package eqredis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/gcmetrics"
	"github.com/shiblon/entroq/pkg/subq"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	// claimWindow is the number of candidates fetched from a queue ZSET during
	// TryClaim. A larger window spreads collisions when many clients claim
	// simultaneously at the cost of slightly looser At-ordering.
	claimWindow = 20

	// keyPrefix is prepended to all Redis keys owned by this backend.
	// The "{eq}" hash tag forces all keys to the same cluster slot; see package doc.
	keyPrefix = "{eq}:"
)

// key helpers -- all key construction goes through these functions.

func taskKey(id string) string {
	return keyPrefix + "t:" + id
}

func queueKey(name string) string {
	return keyPrefix + "q:" + name
}

func qsclaimedKey(name string) string {
	return keyPrefix + "qsclaimed:" + name
}

// nsclaimedKey is the per-doc claim set from before doc groups had locks. Only
// migrateDocLocks uses it, to remove it.
func nsclaimedKey(ns string) string {
	return keyPrefix + "nsclaimed:" + ns
}

// docKeyEscaper percent-escapes "%" and "/" in a namespace, so the first "/"
// in a doc key always ends the namespace and no two namespaces share an
// encoding. The id follows unescaped and may contain "/".
var docKeyEscaper = strings.NewReplacer("%", "%25", "/", "%2F")

func docKey(namespace, id string) string {
	return keyPrefix + "d:" + docKeyEscaper.Replace(namespace) + "/" + id
}

// legacyDocKey is the doc key encoding before "%" was escaped, under which
// namespaces "a/b" and "a%2Fb" shared keys. Only migrateDocKeys uses it.
func legacyDocKey(namespace, id string) string {
	return keyPrefix + "d:" + strings.ReplaceAll(namespace, "/", "%2F") + "/" + id
}

const queuesKey = keyPrefix + "qs"
const namespacesKey = keyPrefix + "ns"

// EQRedis implements entroq.Backend using Redis.
type EQRedis struct {
	client    *redis.Client
	nw        entroq.NotifyWaiter
	stopGC    context.CancelFunc
	gcDone    chan struct{}
	gcMetrics *gcmetrics.Metrics

	stopReadiness context.CancelFunc
	readinessDone chan struct{}
}

type redisOptions struct {
	addr        string
	password    string
	db          int
	nw          entroq.NotifyWaiter
	gcInterval  time.Duration
	gcBatchSize int
	mp          metric.MeterProvider

	readinessInterval time.Duration
}

// RedisOpt configures the Redis backend.
type RedisOpt func(*redisOptions)

// WithAddr sets the Redis server address (default "localhost:6379").
func WithAddr(addr string) RedisOpt {
	return func(o *redisOptions) {
		o.addr = addr
	}
}

// WithPassword sets the Redis AUTH password.
func WithPassword(pwd string) RedisOpt {
	return func(o *redisOptions) {
		o.password = pwd
	}
}

// WithRedisDB selects the Redis logical database index (default 0).
func WithRedisDB(db int) RedisOpt {
	return func(o *redisOptions) {
		o.db = db
	}
}

// WithNotifyWaiter sets the SubQ notifier used for claim wakeups.
func WithNotifyWaiter(nw entroq.NotifyWaiter) RedisOpt {
	return func(o *redisOptions) {
		o.nw = nw
	}
}

// WithMeterProvider sets the OTel MeterProvider used for GC telemetry (metrics
// are reported under the "entroq.redis" meter). Defaults to a noop provider.
func WithMeterProvider(mp metric.MeterProvider) RedisOpt {
	return func(o *redisOptions) {
		o.mp = mp
	}
}

// Opener returns a BackendOpener for use with entroq.New.
func Opener(opts ...RedisOpt) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return Open(ctx, opts...)
	}
}

// Open creates a new EQRedis backend, connecting to Redis with the given options.
// A background GC goroutine is started automatically and runs until ctx is canceled.
func Open(ctx context.Context, opts ...RedisOpt) (*EQRedis, error) {
	o := &redisOptions{
		addr:        "localhost:6379",
		gcInterval:  defaultGCInterval,
		gcBatchSize: defaultGCBatchSize,

		readinessInterval: DefaultReadinessInterval,
	}
	for _, opt := range opts {
		opt(o)
	}

	client := redis.NewClient(&redis.Options{
		Addr:     o.addr,
		Password: o.password,
		DB:       o.db,
	})

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("eqredis open: ping redis: %w", err)
	}
	if err := migrateDocKeys(ctx, client); err != nil {
		client.Close()
		return nil, fmt.Errorf("eqredis open: migrate doc keys: %w", err)
	}
	if err := migrateDocLocks(ctx, client); err != nil {
		client.Close()
		return nil, fmt.Errorf("eqredis open: migrate doc locks: %w", err)
	}
	if err := migrateDocFields(ctx, client); err != nil {
		client.Close()
		return nil, fmt.Errorf("eqredis open: migrate doc fields: %w", err)
	}

	nw := o.nw
	if nw == nil {
		nw = subq.New()
	}

	mp := o.mp
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	gcMetrics, err := gcmetrics.New(mp.Meter("entroq.redis"))
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("eqredis open: gc metrics: %w", err)
	}

	// GC's context is rooted at context.Background(), NOT the constructor's ctx:
	// the loop's lifetime is the backend's, ended by Close, whereas the
	// constructor ctx scopes only construction (a caller may bound Open with a
	// timeout and defer cancel()). Close cancels it and waits for it to exit
	// before closing the client, so the loop never touches a closed connection.
	gcCtx, gcCancel := context.WithCancel(context.Background())
	b := &EQRedis{
		client:    client,
		nw:        nw,
		stopGC:    gcCancel,
		gcDone:    make(chan struct{}),
		gcMetrics: gcMetrics,
	}
	// GC is a first-class, always-on backend behavior.
	go func() {
		defer close(b.gcDone)
		b.runGCLoop(gcCtx, o.gcInterval, o.gcBatchSize)
	}()
	// The readiness loop shares GC's lifetime. It needs to know who is
	// waiting; a custom NotifyWaiter that cannot say leaves claim polling as the
	// only wakeup for tasks that become ready over time.
	if lc, ok := nw.(entroq.ListenerCounter); ok && o.readinessInterval > 0 {
		readinessCtx, stop := context.WithCancel(context.Background())
		b.stopReadiness = stop
		b.readinessDone = make(chan struct{})
		go func() {
			defer close(b.readinessDone)
			b.runReadinessLoop(readinessCtx, lc, o.readinessInterval)
		}()
	}
	return b, nil
}

// Close stops the GC goroutine, waits for it to exit, and closes the underlying
// Redis connection.
func (e *EQRedis) Close() error {
	e.stopGC()
	<-e.gcDone
	if e.stopReadiness != nil {
		e.stopReadiness()
		<-e.readinessDone
	}
	return e.client.Close()
}

// Time returns the current time in UTC.
func (e *EQRedis) Time(_ context.Context) (time.Time, error) {
	return time.Now().UTC(), nil
}

// taskFields is the canonical set of Hash fields for a task.
// Stored as strings; parsed on read.
type taskFields struct {
	ID       string
	Queue    string
	Value    json.RawMessage
	AtMs     int64 // unix milliseconds
	Created  int64 // unix milliseconds
	Modified int64 // unix milliseconds
	Claimant string
	Version  int32
	Claims   int32
	Attempt  int32
	Err      string
}

// toTask converts taskFields to an entroq.Task.
func (f *taskFields) toTask() *entroq.Task {
	return &entroq.Task{
		ID:       f.ID,
		Queue:    f.Queue,
		At:       time.UnixMilli(f.AtMs).UTC(),
		Created:  time.UnixMilli(f.Created).UTC(),
		Modified: time.UnixMilli(f.Modified).UTC(),
		Claimant: f.Claimant,
		Value:    json.RawMessage(f.Value),
		Version:  f.Version,
		Claims:   f.Claims,
		Attempt:  f.Attempt,
		Err:      f.Err,
	}
}

// taskToMap converts taskFields to a Redis HSET argument map.
func (f *taskFields) toMap() map[string]any {
	return map[string]any{
		"id":       f.ID,
		"queue":    f.Queue,
		"value":    string(f.Value),
		"at":       strconv.FormatInt(f.AtMs, 10),
		"created":  strconv.FormatInt(f.Created, 10),
		"modified": strconv.FormatInt(f.Modified, 10),
		"claimant": f.Claimant,
		"version":  strconv.FormatInt(int64(f.Version), 10),
		"claims":   strconv.FormatInt(int64(f.Claims), 10),
		"attempt":  strconv.FormatInt(int64(f.Attempt), 10),
		"err":      f.Err,
	}
}

// parseTaskFields reads a HGETALL result (alternating field/value strings) into taskFields.
func parseTaskFields(vals map[string]string) (*taskFields, error) {
	parseInt := func(s string) (int64, error) {
		if s == "" {
			return 0, nil
		}
		return strconv.ParseInt(s, 10, 64)
	}

	at, err := parseInt(vals["at"])
	if err != nil {
		return nil, fmt.Errorf("parse task at: %w", err)
	}
	created, err := parseInt(vals["created"])
	if err != nil {
		return nil, fmt.Errorf("parse task created: %w", err)
	}
	modified, err := parseInt(vals["modified"])
	if err != nil {
		return nil, fmt.Errorf("parse task modified: %w", err)
	}
	version, err := parseInt(vals["version"])
	if err != nil {
		return nil, fmt.Errorf("parse task version: %w", err)
	}
	claims, err := parseInt(vals["claims"])
	if err != nil {
		return nil, fmt.Errorf("parse task claims: %w", err)
	}
	attempt, err := parseInt(vals["attempt"])
	if err != nil {
		return nil, fmt.Errorf("parse task attempt: %w", err)
	}

	var rawValue json.RawMessage
	if v := vals["value"]; v != "" {
		rawValue = json.RawMessage(v)
	}

	return &taskFields{
		ID:       vals["id"],
		Queue:    vals["queue"],
		Value:    rawValue,
		AtMs:     at,
		Created:  created,
		Modified: modified,
		Claimant: vals["claimant"],
		Version:  int32(version),
		Claims:   int32(claims),
		Attempt:  int32(attempt),
		Err:      vals["err"],
	}, nil
}
