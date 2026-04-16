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
	id              string
	key             string
	secondaryKey    string
	content         json.RawMessage
	at              time.Time
	skipCollidingID bool
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
// functions are what would trigger a fatal error here.
//
// Use WithRawContent for pre-marshaled data.
func WithContent(v any) DocOpt {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("entroq doc: WithValue: %v", err)
	}
	return WithRawContent(b)
}

// WithDocArrivalTime sets the arrival time on a doc change. When non-zero and in the
// future, the backend will also record the caller as the claimant so the doc
// can be renewed or released.
func WithDocArrivalTime(t time.Time) DocOpt {
	return func(o *docOpts) {
		o.at = t
	}
}

// WithDocArrivalTimeBy sets the doc arrival time to now plus d. Use this to
// claim or renew a doc by pushing its At into the future.
func WithDocArrivalTimeBy(d time.Duration) DocOpt {
	return func(o *docOpts) {
		o.at = time.Now().Add(d)
	}
}

// WithSkipCollidingDoc marks a doc insert as skippable when its explicit ID
// already exists. When the caller specifies an ID and that doc is already
// present, the Modify call removes this insert and retries rather than
// returning an error. Analogous to WithSkipColliding for task inserts.
func WithSkipCollidingDoc(skip bool) DocOpt {
	return func(o *docOpts) {
		o.skipCollidingID = skip
	}
}

// DocID contains the identifying parts of a storage doc.
type DocID struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	Version   int32  `json:"version"`
}

func (r DocID) String() string {
	return fmt.Sprintf("%s/%s:v%d", r.Namespace, r.ID, r.Version)
}

// Delete returns a ModifyArg that deletes the doc identified by this DocID.
func (r DocID) Delete() ModifyArg {
	return DeletingDocID(r.Namespace, r.ID, r.Version)
}

// Depend returns a ModifyArg that adds a version-pinned dependency on this DocID.
func (r DocID) Depend() ModifyArg {
	return DependingOnDocID(r.Namespace, r.ID, r.Version)
}

// DocData contains just the data portion of a storage doc, used for
// insertions and journal replay. Created and Modified are populated when
// journaling to preserve original timestamps on replay.
type DocData struct {
	Namespace    string          `json:"namespace"`
	ID           string          `json:"id"`
	Key          string          `json:"key"`
	SecondaryKey string          `json:"secondary_key"`
	Content      json.RawMessage `json:"content"`
	Created      time.Time       `json:"created"`
	Modified     time.Time       `json:"modified"`

	// skipCollidingID indicates that a collision on insertion is not fatal.
	// When the explicit ID already exists, Modify removes this insert and
	// retries rather than returning an error. Analogous to TaskData's field.
	skipCollidingID bool
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
	cp.Content = make([]byte, len(r.Content))
	copy(cp.Content, r.Content)
	return &cp
}

// Change returns a ModifyArg that changes this doc. Accepts WithContent,
// WithDocArrivalTime, and WithDocArrivalTimeBy. WithIDKeys is ignored (keys are immutable).
func (r *Doc) Change(opts ...DocOpt) ModifyArg {
	return func(m *Modification) {
		o := &docOpts{}
		for _, opt := range opts {
			opt(o)
		}
		nr := r.Copy()
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
	return DeletingDoc(r)
}

// Depend returns a ModifyArg that adds a version-pinned dependency on this doc.
func (r *Doc) Depend() ModifyArg {
	return DependingOnDoc(r)
}

// DeletingDoc returns a ModifyArg that deletes the given doc.
func DeletingDoc(r *Doc) ModifyArg {
	return func(m *Modification) {
		m.DocDeletes = append(m.DocDeletes, &DocID{
			Namespace: r.Namespace,
			ID:        r.ID,
			Version:   r.Version,
		})
	}
}

// DeletingDocID returns a ModifyArg that deletes the doc by namespace, ID and version.
func DeletingDocID(ns, id string, version int32) ModifyArg {
	return func(m *Modification) {
		m.DocDeletes = append(m.DocDeletes, &DocID{
			Namespace: ns,
			ID:        id,
			Version:   version,
		})
	}
}

// DependingOnDoc returns a ModifyArg that adds a dependency on the given doc.
func DependingOnDoc(r *Doc) ModifyArg {
	return func(m *Modification) {
		m.DocDepends = append(m.DocDepends, &DocID{
			Namespace: r.Namespace,
			ID:        r.ID,
			Version:   r.Version,
		})
	}
}

// DependingOnDocID returns a ModifyArg that adds a dependency on a doc by namespace, ID and version.
func DependingOnDocID(ns, id string, version int32) ModifyArg {
	return func(m *Modification) {
		m.DocDepends = append(m.DocDepends, &DocID{
			Namespace: ns,
			ID:        id,
			Version:   version,
		})
	}
}

// InsertingDoc returns a ModifyArg that inserts the given doc data directly.
// Prefer CreatingIn for new code.
func InsertingDoc(rd *DocData) ModifyArg {
	return func(m *Modification) {
		m.DocInserts = append(m.DocInserts, rd)
	}
}

// CreatingIn returns a ModifyArg that creates a doc in the given namespace.
// Use WithKeys to set the primary and secondary keys, WithContent/WithRawContent
// to set the payload. Use WithIDKeys only when explicit ID control is required.
// Use WithSkipCollidingDoc to allow the insert to be silently dropped on ID collision.
func CreatingIn(ns string, opts ...DocOpt) ModifyArg {
	return func(m *Modification) {
		o := &docOpts{}
		for _, opt := range opts {
			opt(o)
		}
		rd := &DocData{
			Namespace:       ns,
			ID:              o.id,
			Key:             o.key,
			SecondaryKey:    o.secondaryKey,
			Content:         o.content,
			skipCollidingID: o.skipCollidingID,
		}
		m.DocInserts = append(m.DocInserts, rd)
	}
}

// DocQuery is used to list docs from a namespace with optional range filtering.
// If IDs are present, key range is ignored. Only one will be used. Limit does
// not apply to ID lists.
//
// Construct with DocsIn for a fluent interface:
//
//	eq.Docs(ctx, entroq.DocsIn("config"))
//	eq.Docs(ctx, entroq.DocsIn("metrics").WithKeyRange("2024-01-01", "2025-01-01"))
//	eq.Docs(ctx, entroq.DocsIn("items").WithIDs("id-a", "id-b"))
type DocQuery struct {
	Namespace  string   `json:"namespace"`
	IDs        []string `json:"ids"`
	KeyStart   string   `json:"key_start"`
	KeyEnd     string   `json:"key_end"`
	Limit      int      `json:"limit"`
	OmitValues bool     `json:"omit_values"`
}

// DocsIn returns a DocQuery scoped to the given namespace. Chain methods to
// refine the query.
func DocsIn(ns string) *DocQuery {
	return &DocQuery{Namespace: ns}
}

// WithKeyRange filters docs to the half-open primary-key range [start, end).
// An empty end means no upper bound.
func (q *DocQuery) WithKeyRange(start, end string) *DocQuery {
	q.KeyStart = start
	q.KeyEnd = end
	return q
}

// WithIDs restricts the query to specific doc IDs. Key range and Limit are
// ignored when IDs are set. Docs are returned in the order IDs are listed.
func (q *DocQuery) WithIDs(ids ...string) *DocQuery {
	q.IDs = ids
	return q
}

// WithLimit caps the number of docs returned. Has no effect when WithIDs is set.
func (q *DocQuery) WithLimit(n int) *DocQuery {
	q.Limit = n
	return q
}

// WithOmitValues strips content payloads from returned docs, useful when only
// keys and metadata are needed.
func (q *DocQuery) WithOmitValues() *DocQuery {
	q.OmitValues = true
	return q
}

// DocClaim is used to claim all docs that share a primary key in a namespace.
//
// Construct with ClaimKey for a fluent interface:
//
//	eq.ClaimDocs(ctx, entroq.ClaimKey("state", "counter"))
//	eq.ClaimDocs(ctx, entroq.ClaimKey("state", "counter").For(5*time.Second))
type DocClaim struct {
	Namespace string        `json:"namespace"`
	Claimant  string        `json:"claimant"`
	Key       string        `json:"key"`
	Duration  time.Duration `json:"duration"`
}

// ClaimKey returns a DocClaim for the given namespace and primary key, using
// DefaultClaimDuration. Call For to override the duration.
func ClaimKey(ns, key string) *DocClaim {
	return &DocClaim{
		Namespace: ns,
		Key:       key,
		Duration:  DefaultClaimDuration,
	}
}

// For sets the claim duration, overriding the DefaultClaimDuration set by ClaimKey.
func (c *DocClaim) For(d time.Duration) *DocClaim {
	c.Duration = d
	return c
}

// Validate checks that the claim query specifies exactly one valid strategy.
func (q *DocClaim) Validate() error {
	if q.Key == "" {
		return fmt.Errorf("can't claim on a blank key")
	}
	return nil
}

// DocClaimOpt specifies document claim functionality.
type DocClaimOpt func(*DocClaim)

// LockingFor specifies the duration of a successful claim before it becomes
// available for other claimants..
func LockingFor(dt time.Duration) DocClaimOpt {
	return func(dc *DocClaim) {
		dc.Duration = dt
	}
}

// WithDocClaimant specifies the claimant ID for this document. Only needed
// in very specific circmunstances, such as developing a server that proxies
// through to an EntroQ backend. Otherwise don't use it - it's set for you.
func WithDocClaimant(id string) DocClaimOpt {
	return func(dc *DocClaim) {
		dc.Claimant = id
	}
}
