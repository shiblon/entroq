package entroq

import (
	"context"
	"fmt"
	"time"
)

// TasksQuery holds information for a tasks query.
type TasksQuery struct {
	Queue    string
	Claimant string
	Limit    int
	IDs      []string

	// OmitValues specifies that only metadata should be returned.
	// Backends are not required to honor this flag, though any
	// service receiving it in a request should ensure that values
	// are not passed over the wire.
	OmitValues bool
}

// QueuesQuery modifies a queue listing request.
type QueuesQuery struct {
	// MatchPrefix specifies allowable prefix matches. If empty, limitations
	// are not set based on prefix matching. All prefix match conditions are ORed.
	// If both this and MatchExact are empty or nil, no limitations are set on
	// queue name: all will be returned.
	MatchPrefix []string

	// MatchExact specifies allowable exact matches. If empty, limitations are
	// not set on exact queue names.
	MatchExact []string

	// Limit specifies an upper bound on the number of queue names returned.
	Limit int
}

func newQueuesQuery(opts ...QueuesOpt) *QueuesQuery {
	q := new(QueuesQuery)
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// QueueStat holds high-level information about a queue.
// Note that available + claimed may not add up to size. This is because a task
// can be unavailable (AT in the future) without being claimed by anyone.
type QueueStat struct {
	Name      string `json:"name"`      // The queue name.
	Size      int    `json:"size"`      // The total number of tasks.
	Claimed   int    `json:"claimed"`   // The number of currently claimed tasks.
	Available int    `json:"available"` // The number of available tasks.

	MaxClaims int `json:"maxClaims"` // The maximum number of claims for a task in the queue.
}

// QueuesFromStats can be used for converting the new QueueStats to the old
// Queues output, making it easier on backend implementers to just define one
// function (similar to how WaitTryClaim or PollTryClaim can make implementing
// Claim in terms of TryClaim easier).
func QueuesFromStats(stats map[string]*QueueStat, err error) (map[string]int, error) {
	if err != nil {
		return nil, err
	}
	qs := make(map[string]int)
	for k, v := range stats {
		qs[k] = v.Size
	}
	return qs, nil
}

// Queues returns a mapping from all queue names to their task counts.
func (c *EntroQ) Queues(ctx context.Context, opts ...QueuesOpt) (map[string]int, error) {
	query := newQueuesQuery(opts...)
	return c.backend.Queues(ctx, query)
}

// QueueStats returns a mapping from queue names to task stats.
func (c *EntroQ) QueueStats(ctx context.Context, opts ...QueuesOpt) (map[string]*QueueStat, error) {
	query := newQueuesQuery(opts...)
	return c.backend.QueueStats(ctx, query)
}

// QueuesEmpty indicates whether the specified task queues are all empty. If no
// options are specified, returns an error.
func (c *EntroQ) QueuesEmpty(ctx context.Context, opts ...QueuesOpt) (bool, error) {
	if len(opts) == 0 {
		return false, fmt.Errorf("empty check: no queue options specified")
	}

	qs, err := c.Queues(ctx, opts...)
	if err != nil {
		return false, fmt.Errorf("empty check: %w", err)
	}
	for _, size := range qs {
		if size > 0 {
			return false, nil
		}
	}
	return true, nil
}

// WaitQueuesEmpty does a poll-and-wait strategy to block until the queue query returns empty.
func (c *EntroQ) WaitQueuesEmpty(ctx context.Context, opts ...QueuesOpt) error {
	for {
		empty, err := c.QueuesEmpty(ctx, opts...)
		if err != nil {
			return fmt.Errorf("wait empty: %w", err)
		}
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait empty: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// Tasks returns a slice of all tasks in the given queue.
func (c *EntroQ) Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error) {
	query := &TasksQuery{
		Queue: queue,
	}
	for _, opt := range opts {
		opt(c, query)
	}

	return c.backend.Tasks(ctx, query)
}

// TasksOpt is an option that can be passed into Tasks to control what it returns.
type TasksOpt func(*EntroQ, *TasksQuery)

// LimitSelf only returns self-claimed tasks or expired tasks.
func LimitSelf() TasksOpt {
	return func(c *EntroQ, q *TasksQuery) {
		q.Claimant = c.ClientID
	}
}

// LimitClaimant only returns tasks with the given claimant, or expired tasks.
func LimitClaimant(id string) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.Claimant = id
	}
}

// LimitTasks sets the limit on the number of tasks to return. A value <= 0 indicates "no limit".
func LimitTasks(limit int) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.Limit = limit
	}
}

// OmitValues tells a tasks query to only return metadata, not values.
func OmitValues() TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.OmitValues = true
	}
}

// WithTaskID adds a task ID to the set of IDs that can be returned in a task
// query. The default is "all that match other specs" if no IDs are specified.
// Note that versions are not part of the ID.
func WithTaskID(ids ...string) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.IDs = append(q.IDs, ids...)
	}
}

// QueuesOpt modifies how queue requests are made.
type QueuesOpt func(*QueuesQuery)

// MatchPrefix adds allowable prefix matches for a queue listing.
func MatchPrefix(prefixes ...string) QueuesOpt {
	return func(q *QueuesQuery) {
		q.MatchPrefix = append(q.MatchPrefix, prefixes...)
	}
}

// MatchExact adds an allowable exact match for a queue listing.
func MatchExact(matches ...string) QueuesOpt {
	return func(q *QueuesQuery) {
		q.MatchExact = append(q.MatchExact, matches...)
	}
}

// LimitQueues sets the limit on the number of queues that are returned.
func LimitQueues(limit int) QueuesOpt {
	return func(q *QueuesQuery) {
		q.Limit = limit
	}
}
