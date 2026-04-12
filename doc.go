package entroq

import (
	"encoding/json"
	"fmt"
	"time"
)

// DocOpt is an option for doc creation or modification. Options that
// only apply to creation (WithIDKeys) are documented as such; passing them to
// Change has no effect.
type DocOpt func(*docOpts)

type docOpts struct {
	id           string
	keyPrimary   string
	keySecondary string
	content      json.RawMessage
	expiresAt    time.Time
}

// WithIDKeys sets the ID and keys for doc creation. The primary and
// secondary keys may be empty. This option has no effect when passed to
// Change (keys are immutable after creation).
func WithIDKeys(id, primary, secondary string) DocOpt {
	return func(o *docOpts) {
		o.id = id
		o.keyPrimary = primary
		o.keySecondary = secondary
	}
}

// WithContent sets the content payload of a doc.
func WithContent(val json.RawMessage) DocOpt {
	return func(o *docOpts) {
		o.content = val
	}
}

// WithExpiry sets the expiration time of a doc.
func WithExpiry(at time.Time) DocOpt {
	return func(o *docOpts) {
		o.expiresAt = at
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

// DocData contains just the data portion of a storage doc, used for
// insertions and journal replay. Created and Modified are populated when
// journaling to preserve original timestamps on replay.
type DocData struct {
	Namespace    string          `json:"namespace"`
	ID           string          `json:"id"`
	KeyPrimary   string          `json:"key_primary"`
	KeySecondary string          `json:"key_secondary"`
	Content      json.RawMessage `json:"content"`
	ExpiresAt    time.Time       `json:"expires_at"`
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
	ExpiresAt    time.Time       `json:"expires_at"`
	KeyPrimary   string          `json:"key_primary"`
	KeySecondary string          `json:"key_secondary"`
	Content      json.RawMessage `json:"content"`
	Created      time.Time       `json:"created"`
	Modified     time.Time       `json:"modified"`
}

// Data returns a DocData from this Doc, preserving timestamps for journaling.
func (r *Doc) Data() *DocData {
	rd := &DocData{
		Namespace:    r.Namespace,
		ID:           r.ID,
		KeyPrimary:   r.KeyPrimary,
		KeySecondary: r.KeySecondary,
		ExpiresAt:    r.ExpiresAt,
		Created:      r.Created,
		Modified:     r.Modified,
	}
	if len(r.Content) > 0 {
		rd.Content = make(json.RawMessage, len(r.Content))
		copy(rd.Content, r.Content)
	}
	return rd
}

// Copy returns a deep copy of the doc.
func (r *Doc) Copy() *Doc {
	cp := *r
	cp.Content = make([]byte, len(r.Content))
	copy(cp.Content, r.Content)
	return &cp
}

// Change returns a ModifyArg that changes this doc. Accepts WithContent
// and WithExpiry options. WithIDKeys is ignored (keys are immutable).
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
		if !o.expiresAt.IsZero() {
			nr.ExpiresAt = o.expiresAt
		}
		m.DocChanges = append(m.DocChanges, nr)
	}
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
// Use WithIDKeys to set the ID and keys, WithContent to set the payload, and
// WithExpiry to set an expiration time.
func CreatingIn(ns string, opts ...DocOpt) ModifyArg {
	return func(m *Modification) {
		o := &docOpts{}
		for _, opt := range opts {
			opt(o)
		}
		rd := &DocData{
			Namespace:    ns,
			ID:           o.id,
			KeyPrimary:   o.keyPrimary,
			KeySecondary: o.keySecondary,
			Content:      o.content,
			ExpiresAt:    o.expiresAt,
		}
		m.DocInserts = append(m.DocInserts, rd)
	}
}

// DocQuery is used to list docs from a namespace with optional range filtering.
type DocQuery struct {
	Namespace  string `json:"namespace"`
	KeyStart   string `json:"key_start"`
	KeyEnd     string `json:"key_end"`
	Limit      int    `json:"limit"`
	OmitValues bool   `json:"omit_values"`
}

// DocClaimQuery is used to claim specific docs or a range of docs.
//
// Exactly one claim strategy must be specified:
//   - IDs: claim these exact docs by ID; key fields are ignored.
//   - KeyStart only (KeyEnd == ""): exact match on primary key.
//   - KeyStart + KeyEnd: half-open range [KeyStart, KeyEnd) on primary key;
//     KeyEnd must be lexicographically after KeyStart.
//
// At least one of IDs or KeyStart must be set.
type DocClaimQuery struct {
	Namespace string        `json:"namespace"`
	Claimant  string        `json:"claimant"`
	IDs       []string      `json:"ids"`
	KeyStart  string        `json:"key_start"`
	KeyEnd    string        `json:"key_end"`
	Duration  time.Duration `json:"duration"`
}

// Validate checks that the claim query specifies exactly one valid strategy.
func (q *DocClaimQuery) Validate() error {
	if len(q.IDs) > 0 {
		return nil
	}
	if q.KeyStart == "" {
		return fmt.Errorf("DocClaimQuery: must specify IDs or KeyStart")
	}
	if q.KeyEnd != "" && q.KeyEnd <= q.KeyStart {
		return fmt.Errorf("DocClaimQuery: KeyEnd %q must be lexicographically after KeyStart %q", q.KeyEnd, q.KeyStart)
	}
	return nil
}

// DocRequest describes the docs a worker needs before doing work.
// Required docs must all be claimable or the task is considered poison.
// Optional docs are claimed if available, absent if not.
// ReadOnly docs are fetched at acquisition time and depended on (version-pinned) in the final Modify.
type DocRequest struct {
	Required []DocClaimQuery
	Optional []DocClaimQuery
	ReadOnly []DocID
}

// AcquiredDocs holds the docs obtained during the acquisition phase.
// Required and Optional hold claimed docs; ReadOnly holds fetched docs.
// Optional may be shorter than the number of optional queries if some were absent.
type AcquiredDocs struct {
	Required []*Doc
	Optional []*Doc
	ReadOnly []*Doc
}

// ModifyResponse contains the results of a successful atomic modification.
type ModifyResponse struct {
	InsertedTasks []*Task
	ChangedTasks  []*Task
	InsertedDocs  []*Doc
	ChangedDocs   []*Doc
}
