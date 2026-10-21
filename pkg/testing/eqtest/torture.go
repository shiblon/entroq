package eqtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// TortureEnv selects the size Torture runs at: small, large, or huge. Unset,
// Torture skips.
const TortureEnv = "ENTROQ_TORTURE"

// TortureConfig sizes a torture run.
//
// A run is Jobs jobs, each a chain of Rounds rounds, and each round a fan-out
// of FanOut pieces followed by a fan-in. Every round commits FanOut+2 times
// before faults, so a run commits about Jobs*Rounds*(FanOut+2) times.
type TortureConfig struct {
	Jobs     int
	Rounds   int
	FanOut   int
	Accounts int // ledger docs that pieces credit; fewer means more contention
	Shards   int // piece queues; piece workers watch all of them
	Workers  int // workers per stage; the piece stage gets twice as many

	// Lease is the worker lease. A crashed or killed worker's task and docs
	// come back after it expires, so it paces recovery.
	Lease time.Duration

	// FaultRate is the chance that each phase of a task (take, work, commit)
	// suffers an injected fault.
	FaultRate float64

	// KillEvery is the mean interval between killing a random worker
	// mid-flight. Zero kills none.
	KillEvery time.Duration

	// Timeout bounds the whole run.
	Timeout time.Duration
}

// TortureSizes are the sizes TortureEnv names. Small takes seconds and checks
// the harness; large and huge take minutes and are the stress.
var TortureSizes = map[string]TortureConfig{
	"small": {
		Jobs: 12, Rounds: 3, FanOut: 8, Accounts: 4, Shards: 3, Workers: 3,
		Lease: 500 * time.Millisecond, FaultRate: 0.05, KillEvery: 100 * time.Millisecond,
		Timeout: 2 * time.Minute,
	},
	"large": {
		Jobs: 100, Rounds: 10, FanOut: 32, Accounts: 16, Shards: 4, Workers: 8,
		Lease: time.Second, FaultRate: 0.03, KillEvery: 25 * time.Millisecond,
		Timeout: 30 * time.Minute,
	},
	"huge": {
		Jobs: 300, Rounds: 20, FanOut: 64, Accounts: 32, Shards: 8, Workers: 16,
		Lease: time.Second, FaultRate: 0.02, KillEvery: 20 * time.Millisecond,
		Timeout: 2 * time.Hour,
	},
}

// Torture runs TortureWith at the size TortureEnv names, and skips when it
// names none.
func Torture(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	size := os.Getenv(TortureEnv)
	if size == "" {
		t.Skipf("set %s=small|large|huge to run the torture test", TortureEnv)
	}
	cfg, ok := TortureSizes[size]
	if !ok {
		t.Fatalf("%s=%q: want small, large, or huge", TortureEnv, size)
	}
	TortureWith(cfg)(ctx, t, client, qPrefix)
}

// TortureWith returns a Tester that drives the backend through the patterns
// real pipelines use, while injecting faults all along the take, work, and
// commit path, and then checks that nothing was lost, duplicated, or left
// behind.
//
// Each round of a job is a split, a fan-out, and a fan-in:
//
//   - A split task deletes itself, inserts FanOut piece tasks across the
//     shard queues, and creates the round's join doc, all in one commit.
//   - A piece claims the join doc and a random account doc, credits the
//     account, counts itself into the join doc, and writes a result doc. The
//     piece that counts the join doc down to zero also inserts the merge task.
//   - The merge claims the join doc, reads the round's results by key range,
//     checks them, deletes them and the join doc, and inserts the next round's
//     split, or the job's done task after the last round.
//
// Faults: a take or work phase can crash the worker, a work phase can retry,
// quarantine, or outlive its lease, and a commit can fail on a stale
// dependency, which rolls back everything else it carried. A medic returns
// quarantined tasks to their queues. Workers are killed at random and
// restarted, and hog workers hold account docs to force doc contention. Every
// participant is a worker; the client is used directly only to seed the jobs
// and check the end state. Job keys differ only in case
// and punctuation, so a doc range that does not compare bytes reads another
// job's results.
//
// The run fails if any commit sees state that should be impossible, if it
// stops making progress, or if at the end any job is missing, any task or
// doc is left over, or the ledger does not balance.
func TortureWith(cfg TortureConfig) Tester {
	return func(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		tt := newTorture(cfg, client, qPrefix)
		tt.run(ctx, t)
	}
}

type tortureRound struct {
	Job   int `json:"job"`
	Round int `json:"round"`
}

type torturePiece struct {
	Job   int `json:"job"`
	Round int `json:"round"`
	Piece int `json:"piece"`
}

type tortureJoin struct {
	Remaining int `json:"remaining"`
	Sum       int `json:"sum"`
}

type tortureResult struct {
	Piece int `json:"piece"`
	Value int `json:"value"`
}

// tortureCounts tallies what a run injected and survived.
type tortureCounts struct {
	commits, takeCrash, workRetry, workMove, workCrash, workOverrun,
	commitConflict, unplannedDep, hogged, kills, restarts, healed atomic.Int64
}

type torture struct {
	cfg TortureConfig
	eq  *entroq.EntroQ
	id  string

	splitQ, mergeQ, doneQ, hogQ, violationQ string
	pieceQs                                 []string
	joinNS, resultNS, acctNS, chaosNS       string

	// chaos is never changed, so depending on a later version of it always
	// fails a commit.
	chaos *entroq.Doc

	mu         sync.Mutex
	rng        *rand.Rand
	violations []string
	holds      map[[2]string]*tortureHold
	committed  map[string]int32 // task ID to the version that committed it

	counts tortureCounts
}

func newTorture(cfg TortureConfig, eq *entroq.EntroQ, qPrefix string) *torture {
	id := entroq.GenHex16()
	p := path.Join(qPrefix, "torture", id)
	tt := &torture{
		cfg:        cfg,
		eq:         eq,
		id:         id,
		splitQ:     p + "/split",
		mergeQ:     p + "/merge",
		doneQ:      p + "/done",
		hogQ:       p + "/hog",
		violationQ: p + "/violation",
		joinNS:     p + "/join",
		resultNS:   p + "/result",
		acctNS:     p + "/acct",
		chaosNS:    p + "/chaos",
		rng:        rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x7047)),
		holds:      make(map[[2]string]*tortureHold),
		committed:  make(map[string]int32),
	}
	for i := range cfg.Shards {
		tt.pieceQs = append(tt.pieceQs, fmt.Sprintf("%s/piece/%d", p, i))
	}
	return tt
}

// jobKey names job j. Every four jobs share a number and differ only in case
// and punctuation, which a byte-order key range keeps apart and a locale
// collation does not.
func jobKey(j int) string {
	forms := [...]string{"Job-%05d", "job_%05d", "JOB.%05d", "job-%05d"}
	return fmt.Sprintf(forms[j%len(forms)], j/len(forms))
}

func (tt *torture) joinKey(j, r int) string      { return fmt.Sprintf("%s/%03d", jobKey(j), r) }
func (tt *torture) resultPrefix(j, r int) string { return tt.joinKey(j, r) + "/" }
func (tt *torture) acctKey(a int) string         { return fmt.Sprintf("acct/%03d", a) }

func (tt *torture) taskID(kind string, parts ...int) string {
	var b strings.Builder
	b.WriteString(tt.id + "." + kind)
	for _, p := range parts {
		fmt.Fprintf(&b, ".%d", p)
	}
	return b.String()
}

// value is what piece i of round r of job j credits.
func value(j, r, i int) int { return (j+1)*(r+1) + i }

// roundSum is what all of a round's pieces credit together.
func (tt *torture) roundSum(j, r int) int {
	f := tt.cfg.FanOut
	return f*(j+1)*(r+1) + f*(f-1)/2
}

func (tt *torture) chance(p float64) bool {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.rng.Float64() < p
}

func (tt *torture) intN(n int) int {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.rng.IntN(n)
}

func (tt *torture) violate(format string, args ...any) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.violations = append(tt.violations, fmt.Sprintf(format, args...))
}

// tortureHold records which worker incarnation took a doc, until when its
// claim lasts, and how far its commit has got.
type tortureHold struct {
	owner int64
	doc   string
	until time.Time
	state holdState

	// stolenBy is the worker that took the doc while this hold's commit was in
	// flight: legitimate if the commit landed, which released the doc, and a
	// violation if it failed, which did not.
	stolenBy int64
}

type holdState int

const (
	holding holdState = iota
	committing
	released
)

// holdSlack absorbs clock skew between the backend, which stamps claim
// expiry, and this process, which reads it.
const holdSlack = 100 * time.Millisecond

// holdRef is one worker's hold on the docs it took for one task.
type holdRef struct {
	tt    *torture
	holds []*tortureHold
}

// hold records that worker owner took docs, and reports a violation if
// another worker's claim on one of them is still live. A worker may take again
// what it already holds.
func (tt *torture) hold(owner int64, docs []*entroq.Doc) *holdRef {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	now := time.Now()
	ref := &holdRef{tt: tt}
	for _, d := range docs {
		k := [2]string{d.Namespace, d.ID}
		if h, ok := tt.holds[k]; ok && h.owner != owner && now.Before(h.until) {
			switch h.state {
			case holding:
				tt.violations = append(tt.violations, fmt.Sprintf(
					"doc %s taken by worker %d while worker %d holds it until %v", h.doc, owner, h.owner, h.until))
			case committing:
				h.stolenBy = owner
			}
		}
		h := &tortureHold{owner: owner, doc: d.Namespace + "/" + d.ID, until: d.At.Add(-holdSlack)}
		tt.holds[k] = h
		ref.holds = append(ref.holds, h)
	}
	return ref
}

// committing marks the docs as released by a commit now in flight.
func (r *holdRef) committing() { r.set(committing) }

// landed marks the docs released for good.
func (r *holdRef) landed() { r.set(released) }

// failed puts the docs back in hand, since a failed commit releases nothing,
// and reports a violation for any doc another worker took meanwhile.
func (r *holdRef) failed() {
	if r == nil {
		return
	}
	r.tt.mu.Lock()
	defer r.tt.mu.Unlock()
	for _, h := range r.holds {
		if h.stolenBy != 0 {
			r.tt.violations = append(r.tt.violations, fmt.Sprintf(
				"doc %s taken by worker %d while worker %d held it through a commit that failed", h.doc, h.stolenBy, h.owner))
		}
		h.state = holding
	}
}

func (r *holdRef) set(state holdState) {
	if r == nil {
		return
	}
	r.tt.mu.Lock()
	defer r.tt.mu.Unlock()
	for _, h := range r.holds {
		h.state = state
	}
}

// quarantine sends a task that saw impossible state where the medic will not
// return it from, so the run cannot finish and the end checks report it.
func (tt *torture) quarantine(format string, args ...any) error {
	tt.violate(format, args...)
	return worker.MoveErrorf(format, args...).To(tt.violationQ)
}

// take wraps a doc request with the take phase's fault: a crash after the task
// is claimed and before its docs are. The take phase runs again on every doc
// contention retry, so it crashes at a tenth of the fault rate.
func take[T any](tt *torture, claims func(T) []*entroq.DocClaim) worker.TakeRun[T] {
	return func(_ context.Context, _ *entroq.Task, v T) ([]*entroq.DocClaim, error) {
		if tt.chance(tt.cfg.FaultRate / 10) {
			tt.counts.takeCrash.Add(1)
			return nil, errors.New("torture: injected crash in take")
		}
		if claims == nil {
			return nil, nil
		}
		return claims(v), nil
	}
}

// work is the work phase's faults: a retry, a quarantine, a crash, or, more
// rarely, outliving the lease so that only renewal keeps the task.
func (tt *torture) work(ctx context.Context) error {
	if tt.chance(tt.cfg.FaultRate / 10) {
		tt.counts.workOverrun.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tt.cfg.Lease * 3 / 2):
		}
	}
	if !tt.chance(tt.cfg.FaultRate) {
		return nil
	}
	switch tt.intN(3) {
	case 0:
		tt.counts.workRetry.Add(1)
		return worker.RetryErrorf("torture: injected retry").After(time.Duration(tt.intN(20)) * time.Millisecond)
	case 1:
		tt.counts.workMove.Add(1)
		return worker.MoveErrorf("torture: injected move")
	default:
		tt.counts.workCrash.Add(1)
		return errors.New("torture: injected crash in work")
	}
}

// commit builds the result a handler returns, with the commit phase's fault: a
// dependency on a version of the chaos doc that does not exist, which fails the
// whole commit. Half of those retry; the rest wait out the lease. held is what
// the handler took, which a commit that lands releases.
func (tt *torture) commit(task *entroq.Task, held *holdRef, args ...entroq.ModifyArg) *worker.Result {
	planted := tt.chance(tt.cfg.FaultRate)
	if planted {
		tt.counts.commitConflict.Add(1)
		args = append(args, entroq.NewDocID(tt.chaosNS, tt.chaos.ID, tt.chaos.Version+1).Depend())
	}
	held.committing()
	return worker.Modify(args...).
		OnSuccess(func(context.Context) error {
			if planted {
				tt.violate("a commit depending on a missing doc version succeeded")
			}
			held.landed()
			tt.landed(task)
			tt.counts.commits.Add(1)
			return nil
		}).
		OnDependency(func(_ context.Context, de *entroq.DependencyError) error {
			held.failed()
			if !planted {
				// A lease lost under load is legitimate; count it.
				tt.counts.unplannedDep.Add(1)
				return nil
			}
			if tt.chance(0.5) {
				return worker.RetryErrorf("torture: injected conflict").After(time.Millisecond)
			}
			return nil
		})
}

// landed records that task committed, and reports a violation if it already
// had: a task's commit deletes it, so a second commit means two workers both
// finished it.
func (tt *torture) landed(task *entroq.Task) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if v, ok := tt.committed[task.ID]; ok {
		tt.violations = append(tt.violations, fmt.Sprintf(
			"task %s committed twice, from v%d and from v%d (attempt %d, claims %d)", task.ID, v, task.Version, task.Attempt, task.Claims))
	}
	tt.committed[task.ID] = task.Version
}

func docIn(docs []*entroq.Doc, ns string) []*entroq.Doc {
	var in []*entroq.Doc
	for _, d := range docs {
		if d.Namespace == ns {
			in = append(in, d)
		}
	}
	return in
}

func (tt *torture) splitWorker() *worker.Worker[tortureRound] {
	return worker.New(tt.eq,
		worker.WithTakeDocs(take[tortureRound](tt, nil)),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, v tortureRound, _ []*entroq.Doc) (*worker.Result, error) {
			if err := tt.work(ctx); err != nil {
				return nil, err
			}
			args := []entroq.ModifyArg{
				task.Delete(),
				entroq.PuttingDocInto(tt.joinNS,
					entroq.WithIDKeys(tt.taskID("join", v.Job, v.Round), tt.joinKey(v.Job, v.Round), ""),
					entroq.WithContent(tortureJoin{Remaining: tt.cfg.FanOut})),
			}
			for i := range tt.cfg.FanOut {
				args = append(args, entroq.InsertingInto(tt.pieceQs[i%len(tt.pieceQs)],
					entroq.WithID(tt.taskID("piece", v.Job, v.Round, i)),
					entroq.WithValue(torturePiece{Job: v.Job, Round: v.Round, Piece: i})))
			}
			return tt.commit(task, nil, args...), nil
		}))
}

func (tt *torture) pieceWorker(owner int64) *worker.Worker[torturePiece] {
	return worker.New(tt.eq,
		worker.WithTakeDocs(take(tt, func(v torturePiece) []*entroq.DocClaim {
			return []*entroq.DocClaim{
				entroq.ClaimKey(tt.joinNS, tt.joinKey(v.Job, v.Round)),
				entroq.ClaimKey(tt.acctNS, tt.acctKey(tt.intN(tt.cfg.Accounts))),
			}
		})),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, v torturePiece, docs []*entroq.Doc) (*worker.Result, error) {
			held := tt.hold(owner, docs)
			joins, accts := docIn(docs, tt.joinNS), docIn(docs, tt.acctNS)
			if len(joins) != 1 || len(accts) != 1 {
				return nil, tt.quarantine("piece %v took %d join and %d account docs, want 1 each", v, len(joins), len(accts))
			}
			var join tortureJoin
			if err := joins[0].ContentAs(&join); err != nil {
				return nil, tt.quarantine("piece %v: join doc: %v", v, err)
			}
			if join.Remaining <= 0 {
				return nil, tt.quarantine("piece %v found its join counted down to %d", v, join.Remaining)
			}
			var balance int
			if err := accts[0].ContentAs(&balance); err != nil {
				return nil, tt.quarantine("piece %v: account doc: %v", v, err)
			}
			if err := tt.work(ctx); err != nil {
				return nil, err
			}
			val := value(v.Job, v.Round, v.Piece)
			join.Remaining--
			join.Sum += val
			args := []entroq.ModifyArg{
				task.Delete(),
				joins[0].Change(entroq.WithContent(join)),
				accts[0].Change(entroq.WithContent(balance + val)),
				entroq.PuttingDocInto(tt.resultNS,
					entroq.WithIDKeys(tt.taskID("result", v.Job, v.Round, v.Piece), fmt.Sprintf("%s%05d", tt.resultPrefix(v.Job, v.Round), v.Piece), ""),
					entroq.WithContent(tortureResult{Piece: v.Piece, Value: val})),
			}
			if join.Remaining == 0 {
				args = append(args, entroq.InsertingInto(tt.mergeQ,
					entroq.WithID(tt.taskID("merge", v.Job, v.Round)),
					entroq.WithValue(tortureRound{Job: v.Job, Round: v.Round})))
			}
			return tt.commit(task, held, args...), nil
		}))
}

func (tt *torture) mergeWorker(owner int64) *worker.Worker[tortureRound] {
	return worker.New(tt.eq,
		worker.WithTakeDocs(take(tt, func(v tortureRound) []*entroq.DocClaim {
			return []*entroq.DocClaim{entroq.ClaimKey(tt.joinNS, tt.joinKey(v.Job, v.Round))}
		})),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, v tortureRound, docs []*entroq.Doc) (*worker.Result, error) {
			held := tt.hold(owner, docs)
			if len(docs) != 1 {
				return nil, tt.quarantine("merge %v took %d join docs, want 1", v, len(docs))
			}
			var join tortureJoin
			if err := docs[0].ContentAs(&join); err != nil {
				return nil, tt.quarantine("merge %v: join doc: %v", v, err)
			}
			want := tt.roundSum(v.Job, v.Round)
			if join.Remaining != 0 || join.Sum != want {
				return nil, tt.quarantine("merge %v found join %+v, want 0 remaining and sum %d", v, join, want)
			}
			prefix := tt.resultPrefix(v.Job, v.Round)
			results, err := tt.eq.Docs(ctx, &entroq.DocQuery{
				Namespace: tt.resultNS,
				KeyStart:  prefix,
				KeyEnd:    strings.TrimSuffix(prefix, "/") + "0",
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				return nil, worker.RetryErrorf("torture: list results: %v", err)
			}
			seen := make(map[int]bool, len(results))
			sum := 0
			for _, d := range results {
				var r tortureResult
				if err := d.ContentAs(&r); err != nil {
					return nil, tt.quarantine("merge %v: result %s: %v", v, d.Key, err)
				}
				if !strings.HasPrefix(d.Key, prefix) || seen[r.Piece] {
					return nil, tt.quarantine("merge %v read result %q (piece %d) from range %q", v, d.Key, r.Piece, prefix)
				}
				seen[r.Piece] = true
				sum += r.Value
			}
			if len(results) != tt.cfg.FanOut || sum != want {
				return nil, tt.quarantine("merge %v read %d results summing to %d, want %d summing to %d", v, len(results), sum, tt.cfg.FanOut, want)
			}
			if err := tt.work(ctx); err != nil {
				return nil, err
			}
			args := []entroq.ModifyArg{task.Delete(), docs[0].Delete()}
			for _, d := range results {
				args = append(args, d.Delete())
			}
			if next := v.Round + 1; next < tt.cfg.Rounds {
				args = append(args, entroq.InsertingInto(tt.splitQ,
					entroq.WithID(tt.taskID("split", v.Job, next)),
					entroq.WithValue(tortureRound{Job: v.Job, Round: next})))
			} else {
				args = append(args, entroq.InsertingInto(tt.doneQ,
					entroq.WithID(tt.taskID("done", v.Job)),
					entroq.WithValue(tortureRound{Job: v.Job, Round: next})))
			}
			return tt.commit(task, held, args...), nil
		}))
}

// medicWorker returns quarantined tasks to the queue they failed in, unless
// they failed because a doc they required was missing, which here means state
// went wrong.
func (tt *torture) medicWorker() *worker.Worker[struct{}] {
	return worker.New(tt.eq,
		worker.WithDoModify(func(_ context.Context, task *entroq.Task, _ struct{}, _ []*entroq.Doc) (*worker.Result, error) {
			if strings.Contains(task.Err, "required doc missing") {
				return nil, tt.quarantine("task %s in %s lost a doc it required", task.ID, task.Queue)
			}
			home := strings.TrimSuffix(task.Queue, "/err")
			return worker.Modify(task.Change(entroq.QueueTo(home), entroq.ArrivalTimeBy(0))).
				OnSuccess(func(context.Context) error {
					tt.counts.healed.Add(1)
					return nil
				}), nil
		}))
}

// hogs is how many hog workers, and hog tasks, a run has.
func (tt *torture) hogs() int { return max(2, tt.cfg.Accounts/4) }

// hogWorker holds a random account doc for a tenth of a lease, then releases
// it by committing it unchanged and requeues itself, so pieces meet account
// docs claimed by another worker.
func (tt *torture) hogWorker(owner int64) *worker.Worker[struct{}] {
	return worker.New(tt.eq,
		worker.WithTakeDocs(func(context.Context, *entroq.Task, struct{}) ([]*entroq.DocClaim, error) {
			return []*entroq.DocClaim{entroq.ClaimKey(tt.acctNS, tt.acctKey(tt.intN(tt.cfg.Accounts)))}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, _ struct{}, docs []*entroq.Doc) (*worker.Result, error) {
			held := tt.hold(owner, docs)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(tt.cfg.Lease / 10):
			}
			args := []entroq.ModifyArg{task.Change(entroq.ArrivalTimeBy(tt.cfg.Lease / 20))}
			for _, d := range docs {
				args = append(args, d.Change())
			}
			held.committing()
			return worker.Modify(args...).OnSuccess(func(context.Context) error {
				held.landed()
				tt.counts.hogged.Add(1)
				return nil
			}).OnDependency(func(context.Context, *entroq.DependencyError) error {
				held.failed()
				return nil
			}), nil
		}))
}

// tortureCrew runs workers that restart when they exit, each incarnation under
// its own context so that one can be killed mid-flight, and under its own
// number so that doc holds can be attributed to it.
type tortureCrew struct {
	ctx   context.Context
	wg    sync.WaitGroup
	mu    sync.Mutex
	kills []context.CancelFunc
	next  atomic.Int64
}

func (c *tortureCrew) start(counts *tortureCounts, run func(ctx context.Context, incarnation int64) error) {
	c.mu.Lock()
	i := len(c.kills)
	c.kills = append(c.kills, func() {})
	c.mu.Unlock()
	c.wg.Go(func() {
		for c.ctx.Err() == nil {
			ctx, cancel := context.WithCancel(c.ctx)
			c.mu.Lock()
			c.kills[i] = cancel
			c.mu.Unlock()
			if err := run(ctx, c.next.Add(1)); err != nil && c.ctx.Err() == nil {
				counts.restarts.Add(1)
			}
			cancel()
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func (c *tortureCrew) killOne(i int) {
	c.mu.Lock()
	kill := c.kills[i%len(c.kills)]
	c.mu.Unlock()
	kill()
}

func (tt *torture) setup(ctx context.Context) error {
	resp, err := tt.eq.Modify(ctx, entroq.PuttingDocInto(tt.chaosNS,
		entroq.WithIDKeys(tt.id+".chaos", "chaos", ""), entroq.WithContent(0)))
	if err != nil {
		return fmt.Errorf("create chaos doc: %w", err)
	}
	tt.chaos = resp.InsertedDocs[0]

	var args []entroq.ModifyArg
	for a := range tt.cfg.Accounts {
		args = append(args, entroq.PuttingDocInto(tt.acctNS,
			entroq.WithIDKeys(tt.taskID("acct", a), tt.acctKey(a), ""), entroq.WithContent(0)))
	}
	for range tt.hogs() {
		args = append(args, entroq.InsertingInto(tt.hogQ))
	}
	for j := range tt.cfg.Jobs {
		args = append(args, entroq.InsertingInto(tt.splitQ,
			entroq.WithID(tt.taskID("split", j, 0)),
			entroq.WithValue(tortureRound{Job: j})))
	}
	for len(args) > 0 {
		n := min(len(args), 100)
		if _, err := tt.eq.Modify(ctx, args[:n]...); err != nil {
			return fmt.Errorf("set up jobs: %w", err)
		}
		args = args[n:]
	}
	return nil
}

func (tt *torture) run(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := tt.setup(ctx); err != nil {
		t.Fatalf("torture: %v", err)
	}
	started := time.Now()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	crew := &tortureCrew{ctx: runCtx}
	opts := func(qs ...string) []worker.RunOption {
		return []worker.RunOption{
			worker.Watching(qs...),
			worker.WithLease(tt.cfg.Lease),
			worker.WithBaseRetryDelay(5 * time.Millisecond),
		}
	}
	var errQs []string
	for _, q := range append([]string{tt.splitQ, tt.mergeQ}, tt.pieceQs...) {
		errQs = append(errQs, worker.DefaultErrQMap(q))
	}
	for range tt.cfg.Workers {
		crew.start(&tt.counts, func(ctx context.Context, _ int64) error { return tt.splitWorker().Run(ctx, opts(tt.splitQ)...) })
		crew.start(&tt.counts, func(ctx context.Context, n int64) error { return tt.mergeWorker(n).Run(ctx, opts(tt.mergeQ)...) })
		for range 2 {
			crew.start(&tt.counts, func(ctx context.Context, n int64) error { return tt.pieceWorker(n).Run(ctx, opts(tt.pieceQs...)...) })
		}
	}
	crew.start(&tt.counts, func(ctx context.Context, _ int64) error { return tt.medicWorker().Run(ctx, opts(errQs...)...) })
	for range tt.hogs() {
		crew.start(&tt.counts, func(ctx context.Context, n int64) error { return tt.hogWorker(n).Run(ctx, opts(tt.hogQ)...) })
	}

	var bg sync.WaitGroup
	defer bg.Wait()
	defer stop()
	if tt.cfg.KillEvery > 0 {
		bg.Go(func() { tt.killer(runCtx, crew) })
	}

	waitErr := tt.waitDone(runCtx, t)
	stop()
	crew.wg.Wait()
	bg.Wait()

	c := &tt.counts
	t.Logf("torture: %d jobs x %d rounds x %d pieces in %v: %d commits; injected %d take crashes, %d work retries, %d moves (%d healed), %d work crashes, %d overruns, %d commit conflicts; %d unplanned dependency failures, %d hog holds, %d kills, %d restarts",
		tt.cfg.Jobs, tt.cfg.Rounds, tt.cfg.FanOut, time.Since(started).Round(time.Millisecond), c.commits.Load(),
		c.takeCrash.Load(), c.workRetry.Load(), c.workMove.Load(), c.healed.Load(), c.workCrash.Load(), c.workOverrun.Load(),
		c.commitConflict.Load(), c.unplannedDep.Load(), c.hogged.Load(), c.kills.Load(), c.restarts.Load())

	tt.mu.Lock()
	violations := tt.violations
	tt.mu.Unlock()
	for _, v := range violations {
		t.Errorf("torture violation: %s", v)
	}
	if waitErr != nil {
		t.Fatalf("torture: %v\n%s", waitErr, tt.describe(context.Background()))
	}
	tt.verify(ctx, t)
}

// waitDone waits for every job to finish, and gives up if commits stop.
func (tt *torture) waitDone(ctx context.Context, t *testing.T) error {
	const reportEvery = 30 * time.Second
	stallAfter := max(20*tt.cfg.Lease, 30*time.Second)
	// Each round of each job commits a split, its pieces, and a merge.
	want := int64(tt.cfg.Jobs * tt.cfg.Rounds * (tt.cfg.FanOut + 2))
	start := time.Now()
	last, lastAt := int64(-1), start
	reportAt := start.Add(reportEvery)
	for {
		stats, err := tt.eq.QueueStats(ctx, entroq.MatchExact(tt.doneQ))
		if err != nil {
			return fmt.Errorf("done queue stats: %w", err)
		}
		done := 0
		if s := stats[tt.doneQ]; s != nil {
			done = s.Size
		}
		if done >= tt.cfg.Jobs {
			return nil
		}
		if now := time.Now(); now.After(reportAt) {
			reportAt = now.Add(reportEvery)
			n, elapsed := tt.counts.commits.Load(), now.Sub(start)
			eta := "unknown"
			if n > 0 {
				eta = (time.Duration(float64(elapsed) * float64(want-n) / float64(n))).Round(time.Second).String()
			}
			tt.mu.Lock()
			violations := len(tt.violations)
			tt.mu.Unlock()
			t.Logf("torture: progress %v: %d/%d commits (%.1f%%), %d/%d jobs done, %d kills, %d violations, ETA %s",
				elapsed.Round(time.Second), n, want, 100*float64(n)/float64(want), done, tt.cfg.Jobs, tt.counts.kills.Load(), violations, eta)
		}
		if n := tt.counts.commits.Load(); n != last {
			last, lastAt = n, time.Now()
		} else if time.Since(lastAt) > stallAfter {
			return fmt.Errorf("no commit in %v after %d commits", stallAfter, n)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jobs unfinished after %d commits: %w", tt.counts.commits.Load(), ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (tt *torture) killer(ctx context.Context, crew *tortureCrew) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(tt.intN(int(2*tt.cfg.KillEvery)) + 1)):
			crew.killOne(tt.intN(1 << 30))
			tt.counts.kills.Add(1)
		}
	}
}

// verify checks the end state: every job done exactly once, nothing else left
// in any queue or namespace, and the ledger balanced.
func (tt *torture) verify(ctx context.Context, t *testing.T) {
	t.Helper()
	done, err := tt.eq.Tasks(ctx, tt.doneQ)
	if err != nil {
		t.Fatalf("torture: list done: %v", err)
	}
	seen := make(map[int]bool)
	for _, task := range done {
		var v tortureRound
		if err := task.ValueAs(&v); err != nil {
			t.Fatalf("torture: done task %s: %v", task.ID, err)
		}
		if seen[v.Job] || v.Round != tt.cfg.Rounds {
			t.Errorf("torture: done task %s is job %d after round %d (seen before: %v)", task.ID, v.Job, v.Round, seen[v.Job])
		}
		seen[v.Job] = true
	}
	if len(done) != tt.cfg.Jobs || len(seen) != tt.cfg.Jobs {
		t.Errorf("torture: %d done tasks for %d distinct jobs, want %d", len(done), len(seen), tt.cfg.Jobs)
	}

	stats, err := tt.eq.QueueStats(ctx, entroq.MatchPrefix(path.Dir(tt.splitQ)))
	if err != nil {
		t.Fatalf("torture: queue stats: %v", err)
	}
	for name, s := range stats {
		if name != tt.doneQ && name != tt.hogQ && s.Size > 0 {
			t.Errorf("torture: queue %s kept %d tasks", name, s.Size)
		}
	}
	for _, ns := range []string{tt.joinNS, tt.resultNS} {
		docs, err := tt.eq.Docs(ctx, &entroq.DocQuery{Namespace: ns, OmitValues: true})
		if err != nil {
			t.Fatalf("torture: list %s: %v", ns, err)
		}
		if len(docs) > 0 {
			t.Errorf("torture: namespace %s kept %d docs, first %s", ns, len(docs), docs[0])
		}
	}

	accts, err := tt.eq.Docs(ctx, &entroq.DocQuery{Namespace: tt.acctNS})
	if err != nil {
		t.Fatalf("torture: list accounts: %v", err)
	}
	total, want := 0, 0
	for _, d := range accts {
		var balance int
		if err := d.ContentAs(&balance); err != nil {
			t.Fatalf("torture: account %s: %v", d.Key, err)
		}
		total += balance
	}
	for j := range tt.cfg.Jobs {
		for r := range tt.cfg.Rounds {
			want += tt.roundSum(j, r)
		}
	}
	if len(accts) != tt.cfg.Accounts || total != want {
		t.Errorf("torture: %d accounts hold %d, want %d holding %d", len(accts), total, tt.cfg.Accounts, want)
	}
}

// describe reports where tasks sit, for a run that stopped short.
func (tt *torture) describe(ctx context.Context) string {
	stats, err := tt.eq.QueueStats(ctx, entroq.MatchPrefix(path.Dir(tt.splitQ)))
	if err != nil {
		return fmt.Sprintf("queue stats: %v", err)
	}
	var names []string
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		s := stats[name]
		fmt.Fprintf(&b, "  %s: size %d, claimed %d, available %d, future %d, max claims %d\n",
			name, s.Size, s.Claimed, s.Available, s.Future, s.MaxClaims)
	}
	return b.String()
}
