package entroq

import (
	"context"
	"time"
)

// Client is what a consumer of EntroQ speaks through: everything needed to
// claim work, hold it, commit it, and read state, as ONE claimant.
//
// A claimant is a consumer, not a process. Two things working concurrently are
// two consumers even inside one process, and they must not share a claimant:
// doc sets exclude by claimant, so two workers under one claimant can claim
// each other's sets and the exclusion silently stops excluding. Tasks are the
// same -- a claimant check is what keeps one holder from writing another's
// task. So anything running several consumers on one connection gives each its
// own Client, with As.
//
// *EntroQ is a Client, holding things as its own ID. As returns another, over
// the same connection, holding things as a different claimant.
//
// KEEPING THIS SMALL IS DELIBERATE. Adding a method here breaks every fake a
// caller wrote for its tests, which is the cost of handing an interface out
// rather than accepting one. The methods are the ones a worker and a worker's
// handler need; something rarer belongs in a second, small interface that
// callers type-assert for, the way io.ReaderFrom sits beside io.Reader, rather
// than growing this one.
//
// Close is deliberately absent: a scoped client does not own the connection it
// speaks through, and several share one. Close the *EntroQ.
//
// Renew, RenewFor and RenewAllFor are also absent. They name an absolute hold,
// which predates arrivals; UpdateArrival says how much LONGER to hold and lets
// the backend resolve that against its own clock.
type Client interface {
	// Claiming and holding. Each of these records this client's claimant as
	// the holder, which is the whole reason this interface exists.
	Claim(ctx context.Context, opts ...ClaimOpt) (*Task, error)
	TryClaim(ctx context.Context, opts ...ClaimOpt) (*Task, error)
	ClaimDocs(ctx context.Context, args ...DocClaimArg) ([]*DocSet, error)
	Modify(ctx context.Context, args ...ModifyArg) (*ModifyResponse, error)
	UpdateArrival(ctx context.Context, entries ...*ArrivalEntry) (*ModifyResponse, error)

	// Reading. None of these records a holder, so a claimant does not come
	// into them.
	Docs(ctx context.Context, rq *DocQuery) ([]*Doc, error)
	Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error)
	Queues(ctx context.Context, opts ...QueuesOpt) (map[string]int, error)
	Time(ctx context.Context) (time.Time, error)

	// ID is the claimant this client holds things as.
	ID() string
	// GenID returns a fresh random ID, for naming a task or doc.
	GenID() string
	// As returns another client over the same connection, holding what it
	// claims as claimant.
	As(claimant string) Client
}

// Compile-time proof that the real client is one, so a caller can pass
// *EntroQ wherever a Client is wanted.
var _ Client = (*EntroQ)(nil)

// As returns a client over this connection that holds what it claims as
// claimant, leaving this one's own ID untouched.
//
// Use it to give each concurrent consumer its own claimant while sharing one
// connection: worker.New takes a Client, and a worker's Run scopes itself with
// this.
func (c *EntroQ) As(claimant string) Client {
	return &scopedClient{eq: c, claimant: claimant}
}

// scopedClient is a Client that holds everything as one claimant, over a
// connection it does not own. It is unexported because it adds nothing to the
// interface: there is one name for this idea, and it is Client.
type scopedClient struct {
	eq       *EntroQ
	claimant string
}

var _ Client = (*scopedClient)(nil)

// As returns a SIBLING over the same connection, never a client wrapping this
// one.
//
// Nesting would invert the claimant, silently and only for writes: each scoped
// client appends its own claimant to the arguments it passes down, so a client
// wrapping a client would leave the INNER claimant last, and last wins. Every
// scopedClient is one hop from the connection, so that cannot arise.
func (c *scopedClient) As(claimant string) Client {
	return &scopedClient{eq: c.eq, claimant: claimant}
}

// ID is the claimant, which is what this client holds things as.
func (c *scopedClient) ID() string { return c.claimant }

func (c *scopedClient) GenID() string { return c.eq.GenID() }

// Modify applies a modification as this client's claimant.
//
// The claimant is appended LAST, so it wins over anything the caller passed.
// That is deliberate: the claimant is what holds the lease the modification
// depends on, so honoring a caller's own ModifyAs would commit under an
// identity that holds nothing. Appending also leaves WithModification's
// documented behavior intact -- it ignores a Modification's Claimant rather
// than copying it -- so neither order can drop this one.
func (c *scopedClient) Modify(ctx context.Context, args ...ModifyArg) (*ModifyResponse, error) {
	return c.eq.Modify(ctx, append(args, ModifyAs(c.claimant))...)
}

func (c *scopedClient) Claim(ctx context.Context, opts ...ClaimOpt) (*Task, error) {
	return c.eq.Claim(ctx, append(opts, WithClaimant(c.claimant))...)
}

func (c *scopedClient) TryClaim(ctx context.Context, opts ...ClaimOpt) (*Task, error) {
	return c.eq.TryClaim(ctx, append(opts, WithClaimant(c.claimant))...)
}

func (c *scopedClient) ClaimDocs(ctx context.Context, args ...DocClaimArg) ([]*DocSet, error) {
	return c.eq.ClaimDocs(ctx, append(args, ClaimingSetsAs(c.claimant))...)
}

// UpdateArrival renews or releases what this claimant holds. It goes through
// this client's own Modify, not the connection's, because an arrival names
// what the claimant holds and must be written as that claimant.
func (c *scopedClient) UpdateArrival(ctx context.Context, entries ...*ArrivalEntry) (*ModifyResponse, error) {
	var mod Modification
	Arriving(entries...)(&mod)
	if len(mod.Arrives) == 0 && len(mod.DocArrives) == 0 {
		return nil, InvalidArgumentf("update arrival: no tasks or doc sets")
	}
	return c.Modify(ctx, Arriving(entries...))
}

func (c *scopedClient) Docs(ctx context.Context, rq *DocQuery) ([]*Doc, error) {
	return c.eq.Docs(ctx, rq)
}

func (c *scopedClient) Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error) {
	return c.eq.Tasks(ctx, queue, opts...)
}

func (c *scopedClient) Queues(ctx context.Context, opts ...QueuesOpt) (map[string]int, error) {
	return c.eq.Queues(ctx, opts...)
}

func (c *scopedClient) Time(ctx context.Context) (time.Time, error) {
	return c.eq.Time(ctx)
}
