package entroq

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// DocOpt is an option for doc creation or modification. Options that
// only apply to creation (WithKeys, WithIDKeys) are documented as such;
// passing them to Change has no effect.
type DocOpt func(*docOpts)

type docOpts struct {
	id           string
	key          string
	secondaryKey string
	content      json.RawMessage
	at           time.Time
}

// WithKeys sets the primary and secondary keys for doc creation. The ID is
// auto-assigned. This option has no effect when passed to Change (keys are
// immutable after creation). Prefer this over WithIDKeys for normal use.
func WithKeys(key, secondary string) DocOpt {
	return func(o *docOpts) {
		o.key = key
		o.secondaryKey = secondary
	}
}

// WithIDKeys sets the ID and keys for doc creation. The ID is normally
// auto-assigned; only use this when you need explicit ID control, such as
// when replaying a journal, migrating data, or proxying through a gRPC
// service. This option has no effect when passed to Change (keys are
// immutable after creation).
func WithIDKeys(id, key, secondary string) DocOpt {
	return func(o *docOpts) {
		o.id = id
		o.key = key
		o.secondaryKey = secondary
	}
}

// WithRawContent sets the content payload of a doc.
func WithRawContent(val json.RawMessage) DocOpt {
	return func(o *docOpts) {
		o.content = val
	}
}

// WithContent sets the content payload of a doc, marshaling it to JSON first.
// This is a "must" function in the sense that the value must be marshalable.
// If this is a data value, it will always work. Things like channels and
// functions are what would trigger a panic here.
//
// Use WithRawContent for pre-marshaled data.
func WithContent(v any) DocOpt {
	b, err := json.Marshal(v)
	if err != nil {
		log.Panicf("entroq doc: WithValue: %v", err)
	}
	return WithRawContent(b)
}

// WithDocArrivalTime sets the arrival time on a doc insertion or change. When
// non-zero and in the future, the backend also records the caller as the
// claimant so the doc can be renewed or released.
//
// The arrival time belongs to the doc's set, not the doc: setting it claims
// or renews the whole set. When one modification sets different future
// arrival times on docs of one set, the latest wins; if none is in the
// future, the modification releases the set.
func WithDocArrivalTime(t time.Time) DocOpt {
	return func(o *docOpts) {
		o.at = t
	}
}

// WithDocArrivalTimeBy sets the doc arrival time to now plus d. Use this to
// insert a claimed doc, or to claim or renew an existing doc, by pushing its At
// into the future.
func WithDocArrivalTimeBy(d time.Duration) DocOpt {
	return func(o *docOpts) {
		o.at = time.Now().Add(d)
	}
}

// DocID contains the identifying parts of a storage doc. It names a doc by
// ID, or, with no ID, a whole doc set by its key: a set is a doc with only a
// namespace and a primary key. Either way, Version is the set's.
type DocID struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id,omitempty"`
	Key       string `json:"key,omitempty"`
	Version   int32  `json:"version"`
}

// NewDocID creates a DocID for the given namespace, id, and version. The
// namespace is part of the doc's modify key, the way a queue is for a task.
func NewDocID(ns string, id string, version int32) *DocID {
	return &DocID{Namespace: ns, ID: id, Version: version}
}

// NewDocSetRef creates a DocID naming the doc set with the given namespace
// and key, at version.
func NewDocSetRef(ns, key string, version int32) *DocID {
	return &DocID{Namespace: ns, Key: key, Version: version}
}

// IsSetRef reports whether r names a whole doc set rather than one doc.
func (r DocID) IsSetRef() bool {
	return r.ID == "" && r.Key != ""
}

func (r DocID) String() string {
	if r.IsSetRef() {
		return fmt.Sprintf("%s/[%s]:v%d", r.Namespace, r.Key, r.Version)
	}
	return fmt.Sprintf("%s/%s:v%d", r.Namespace, r.ID, r.Version)
}

// Delete returns a ModifyArg that deletes the doc identified by this DocID.
func (r DocID) Delete() ModifyArg {
	return func(m *Modification) {
		m.DocDeletes = append(m.DocDeletes, &r)
	}
}

// Depend returns a ModifyArg that adds a version-pinned dependency on this DocID.
func (r DocID) Depend() ModifyArg {
	return func(m *Modification) {
		m.DocDepends = append(m.DocDepends, &r)
	}
}

// DocData contains just the data portion of a storage doc, used for
// insertions and journal replay. Created and Modified are populated when
// journaling to preserve original timestamps on replay.
type DocData struct {
	Namespace    string          `json:"namespace"`
	ID           string          `json:"id"`
	At           time.Time       `json:"at"`
	Key          string          `json:"key"`
	SecondaryKey string          `json:"secondary_key"`
	Content      json.RawMessage `json:"content"`
	Created      time.Time       `json:"created"`
	Modified     time.Time       `json:"modified"`
}

// Doc represents a durable state record in EntroQ.
type Doc struct {
	Namespace    string          `json:"namespace"`
	ID           string          `json:"id"`
	Version      int32           `json:"version"`
	Claimant     string          `json:"claimant"`
	At           time.Time       `json:"at"`
	Key          string          `json:"key"`
	SecondaryKey string          `json:"secondary_key"`
	Content      json.RawMessage `json:"content"`
	Created      time.Time       `json:"created"`
	Modified     time.Time       `json:"modified"`
}

// Data returns a DocData from this Doc, preserving timestamps for journaling.
func (r *Doc) Data() *DocData {
	rd := &DocData{
		Namespace:    r.Namespace,
		ID:           r.ID,
		At:           r.At,
		Key:          r.Key,
		SecondaryKey: r.SecondaryKey,
		Created:      r.Created,
		Modified:     r.Modified,
	}
	if len(r.Content) > 0 {
		rd.Content = make(json.RawMessage, len(r.Content))
		copy(rd.Content, r.Content)
	}
	return rd
}

// String returns a human-readable representation of this doc.
func (r *Doc) String() string {
	return fmt.Sprintf("Doc [%s/%s:v%d key=%q/%q claimant=%s]",
		r.Namespace, r.ID, r.Version, r.Key, r.SecondaryKey, r.Claimant)
}

// ContentAs unmarshals the doc content into v. Same semantics as json.Unmarshal.
// For one-shot unmarshaling into a new value of a known type, prefer the
// package-level ContentAs[T] generic function.
func (r *Doc) ContentAs(v any) error {
	return json.Unmarshal(r.Content, v)
}

// ContentAs unmarshals raw into a new value of type T and returns it.
//
//	count, err := entroq.ContentAs[int](doc.Content)
func ContentAs[T any](raw json.RawMessage) (T, error) {
	return ValueAs[T](raw)
}

// GetContent unmarshals the doc's content into a new value of type T and returns it.
// It is a convenience wrapper around ContentAs[T](doc.Content).
func GetContent[T any](r *Doc) (T, error) {
	if r == nil {
		var v T
		return v, fmt.Errorf("GetContent on nil doc")
	}
	return ContentAs[T](r.Content)
}

// Copy returns a deep copy of the doc.
func (r *Doc) Copy() *Doc {
	cp := *r
	if len(r.Content) > 0 {
		cp.Content = make([]byte, len(r.Content))
		copy(cp.Content, r.Content)
	}
	return &cp
}

// Change returns a ModifyArg that changes this doc. Accepts WithContent,
// WithDocArrivalTime, and WithDocArrivalTimeBy. WithIDKeys is ignored (keys are immutable).
//
// Much like Task.Change, a change releases by default: the arrival time is
// reset (so the backend substitutes now(), see Backend.Modify) unless an
// explicit WithDocArrivalTime / WithDocArrivalTimeBy pushes it into the future
// to keep the doc claimed. Callers therefore do not silently keep a claim just
// by omitting the arrival time.
func (r *Doc) Change(opts ...DocOpt) ModifyArg {
	return func(m *Modification) {
		o := &docOpts{}
		for _, opt := range opts {
			opt(o)
		}
		nr := r.Copy()
		nr.At = time.Time{}
		if len(o.content) > 0 {
			nr.Content = o.content
		}
		if !o.at.IsZero() {
			nr.At = o.at
		}
		m.DocChanges = append(m.DocChanges, nr)
	}
}

// IDVersion returns a DocID identifying this doc's current version.
func (r *Doc) IDVersion() *DocID {
	return &DocID{
		Namespace: r.Namespace,
		ID:        r.ID,
		Version:   r.Version,
	}
}

// Delete returns a ModifyArg that deletes this doc.
func (r *Doc) Delete() ModifyArg {
	return r.IDVersion().Delete()
}

// Depend returns a ModifyArg that adds a version-pinned dependency on this doc.
func (r *Doc) Depend() ModifyArg {
	return r.IDVersion().Depend()
}

// PuttingDoc returns a ModifyArg that inserts the given doc data directly.
// Prefer PuttingDocInto for most use cases.
func PuttingDoc(rd *DocData) ModifyArg {
	return func(m *Modification) {
		m.DocInserts = append(m.DocInserts, rd)
	}
}

// PuttingDocInto returns a ModifyArg that creates a doc in the given namespace.
// Use WithKeys to set the primary and secondary keys, WithContent/WithRawContent
// to set the payload, and WithDocArrivalTime/WithDocArrivalTimeBy to insert the
// doc with a claim. Use WithIDKeys only when explicit ID control is required.
func PuttingDocInto(ns string, opts ...DocOpt) ModifyArg {
	return func(m *Modification) {
		o := &docOpts{}
		for _, opt := range opts {
			opt(o)
		}
		rd := &DocData{
			Namespace:    ns,
			ID:           o.id,
			At:           o.at,
			Key:          o.key,
			SecondaryKey: o.secondaryKey,
			Content:      o.content,
		}
		m.DocInserts = append(m.DocInserts, rd)
	}
}

// DocQuery is used to list docs from a namespace with one of three mutually
// exclusive filter modes: IDs, KeyExact, or KeyStart/KeyEnd range.
//
//	eq.Docs(ctx, &entroq.DocQuery{Namespace: "config"})
//	eq.Docs(ctx, &entroq.DocQuery{Namespace: "metrics", KeyStart: "2024-01-01", KeyEnd: "2025-01-01"})
//	eq.Docs(ctx, &entroq.DocQuery{Namespace: "metrics", KeyExact: "2024-06-01"})
//	eq.Docs(ctx, &entroq.DocQuery{Namespace: "items", IDs: []string{"id-a", "id-b"}})
type DocQuery struct {
	Namespace  string   `json:"namespace"`
	IDs        []string `json:"ids"`
	KeyExact   string   `json:"key_exact"`
	KeyStart   string   `json:"key_start"`
	KeyEnd     string   `json:"key_end"`
	Limit      int      `json:"limit"`
	OmitValues bool     `json:"omit_values"`
}

// Validate checks that the query names a namespace. Doc IDs are unique only
// within a namespace, unlike task IDs, so even a lookup by ID needs one.
func (q *DocQuery) Validate() error {
	if q.Namespace == "" {
		return InvalidArgumentf("docs query must name a namespace")
	}
	return nil
}

// DocSet is a doc set: the docs sharing a primary key in a namespace,
// which have one version, claimant, and arrival time between them, as a task
// does. ClaimDocs returns the set it claimed, which may have no docs yet;
// each member carries the set's version and claim.
type DocSet struct {
	Namespace string
	Key       string
	Version   int32
	Claimant  string
	At        time.Time
	NumDocs   int    // how many docs the set has
	Docs      []*Doc // its docs
}

// DocsIn returns the docs of sets, in order: every member of the first
// set, then of the next.
func DocsIn(sets []*DocSet) []*Doc {
	var docs []*Doc
	for _, g := range sets {
		docs = append(docs, g.Docs...)
	}
	return docs
}

// DocSetClaim names one doc set of a claim: the docs sharing a primary key in a
// namespace, which may be none. Build one with ClaimKey.
type DocSetClaim struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	// OmitMembers returns the set alone, with its lock and count, and none
	// of its docs. Its holder can read them later, or insert one it knows is
	// new, without racing anyone.
	OmitMembers bool `json:"omit_members,omitempty"`
}

// ClaimKey names the doc set with the given namespace and primary key, for
// ClaimDocs.
func ClaimKey(ns, key string) *DocSetClaim {
	return &DocSetClaim{Namespace: ns, Key: key}
}

// WithoutMembers makes the claim of this set return the set alone, without
// its docs (see DocSetClaim.OmitMembers).
func (s *DocSetClaim) WithoutMembers() *DocSetClaim {
	s.OmitMembers = true
	return s
}

// DocClaim claims doc sets, all of them or none, for one claimant until one
// time. Build one with ClaimDocs's arguments:
//
//	eq.ClaimDocs(ctx,
//		entroq.ClaimKey("orders", "cust-17").WithoutMembers(),
//		entroq.ClaimKey("stock", "sku-9"),
//		entroq.ClaimingSetsFor(time.Minute),
//	)
type DocClaim struct {
	Sets     []*DocSetClaim `json:"sets"`
	Claimant string         `json:"claimant"`
	Duration time.Duration  `json:"duration"`
	// At, if set, holds the sets until then instead of for Duration.
	At time.Time `json:"at"`

	leases int // lease arguments given, of which there may be one
}

// DocClaimArg is an argument to ClaimDocs: a set to claim (ClaimKey), or an
// option of the whole claim.
type DocClaimArg interface {
	applyDocClaim(*DocClaim)
}

func (s *DocSetClaim) applyDocClaim(c *DocClaim) {
	c.Sets = append(c.Sets, s)
}

type docClaimOption func(*DocClaim)

func (o docClaimOption) applyDocClaim(c *DocClaim) {
	o(c)
}

// ClaimingSetsFor holds a claim's sets for d from when it is made. Without a
// lease argument a claim lasts DefaultClaimDuration.
func ClaimingSetsFor(d time.Duration) DocClaimArg {
	return docClaimOption(func(c *DocClaim) {
		c.Duration, c.At = d, time.Time{}
		c.leases++
	})
}

// ClaimingSetsUntil holds a claim's sets until t, which must be in the future:
// a worker claims its task's sets until its task's arrival time, so that they
// expire together.
func ClaimingSetsUntil(t time.Time) DocClaimArg {
	return docClaimOption(func(c *DocClaim) {
		c.Duration, c.At = 0, t
		c.leases++
	})
}

// ClaimingSetsAs makes the claim for claimant instead of the client's own ID,
// as ModifyAs does for a modification. A service claiming on a caller's
// behalf needs it; a client otherwise does not.
func ClaimingSetsAs(claimant string) DocClaimArg {
	return docClaimOption(func(c *DocClaim) {
		c.Claimant = claimant
	})
}

// NewDocClaim builds the claim that args describe, filling in nothing.
func NewDocClaim(args ...DocClaimArg) *DocClaim {
	c := new(DocClaim)
	for _, a := range args {
		a.applyDocClaim(c)
	}
	return c
}

// Validate checks that the claim names at least one set, each once by
// namespace and key, a claimant, and at most one lease, and that its duration
// is neither negative nor longer than MaxClaimDuration. A zero duration and no
// time means the default, which ClaimDocs fills in. Whether At is in the
// future, or too far into it, is the backend's to check, by its own clock.
func (q *DocClaim) Validate() error {
	if len(q.Sets) == 0 {
		return InvalidArgumentf("doc claim must name a doc set")
	}
	seen := make(map[[2]string]bool, len(q.Sets))
	for _, s := range q.Sets {
		if s == nil || s.Namespace == "" {
			return InvalidArgumentf("doc claim must name a namespace for each set")
		}
		if s.Key == "" {
			return InvalidArgumentf("doc claim must name a key for each set")
		}
		k := [2]string{s.Namespace, s.Key}
		if seen[k] {
			return InvalidArgumentf("doc claim names set %q in %q more than once", s.Key, s.Namespace)
		}
		seen[k] = true
	}
	if q.Claimant == "" {
		return InvalidArgumentf("doc claim must name a claimant")
	}
	if q.leases > 1 {
		return InvalidArgumentf("doc claim gives more than one lease")
	}
	if q.Duration < 0 {
		return InvalidArgumentf("doc claim duration must not be negative, got %v", q.Duration)
	}
	if q.Duration > MaxClaimDuration {
		return InvalidArgumentf("doc claim duration is %v, limit is %v (a unit error?)", q.Duration, MaxClaimDuration)
	}
	if q.Duration > 0 && !q.At.IsZero() {
		return InvalidArgumentf("doc claim gives both a duration and a time")
	}
	return nil
}

// Until returns when the claim's sets are held until, for a claim made at
// now.
func (q *DocClaim) Until(now time.Time) time.Time {
	if !q.At.IsZero() {
		return q.At
	}
	return now.Add(q.Duration)
}
