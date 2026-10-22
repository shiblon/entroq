package eqsqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/shiblon/entroq"
)

// queryer runs a read, on a transaction or the read pool.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type scanner interface {
	Scan(...any) error
}

func scanTask(s scanner) (*entroq.Task, error) {
	var (
		t        entroq.Task
		value    sql.NullString
		at       int64
		created  int64
		modified int64
	)
	if err := s.Scan(&t.ID, &t.Version, &t.Queue, &at, &t.Claimant, &t.Claims,
		&value, &created, &modified, &t.Attempt, &t.Err); err != nil {
		return nil, err
	}
	t.At = time.UnixMilli(at).UTC()
	t.Created = time.UnixMilli(created).UTC()
	t.Modified = time.UnixMilli(modified).UTC()
	if value.Valid {
		t.Value = json.RawMessage(value.String)
	}
	return &t, nil
}

func scanDoc(s scanner) (*entroq.Doc, error) {
	var (
		d        entroq.Doc
		content  sql.NullString
		at       int64
		created  int64
		modified int64
	)
	if err := s.Scan(&d.Namespace, &d.ID, &d.Version, &d.Claimant, &at,
		&d.Key, &d.SecondaryKey, &content, &created, &modified); err != nil {
		return nil, err
	}
	if at != 0 {
		d.At = time.UnixMilli(at).UTC()
	}
	d.Created = time.UnixMilli(created).UTC()
	d.Modified = time.UnixMilli(modified).UTC()
	if content.Valid {
		d.Content = json.RawMessage(content.String)
	}
	return &d, nil
}

const taskColumns = `id, version, queue, at_ms, claimant, claims, value, created_ms, modified_ms, attempt, err`

// docColumns reads a doc through docsWithLocks: each doc's version, claimant,
// and at come from its group's lock, the only ones a member has.
const docColumns = `d.namespace, d.id, l.version, l.claimant, l.at_ms,
	d.key_primary, d.key_secondary, d.content, d.created_ms, d.modified_ms`

// docsWithLocks joins each doc to its group's lock, as d and l. Every doc has
// one: docs references doc_locks.
const docsWithLocks = `docs d JOIN doc_locks l ON l.namespace = d.namespace AND l.key_primary = d.key_primary`

func jsonValue(v json.RawMessage) any {
	if v == nil {
		return nil
	}
	return string(v)
}

func storedTime(given, fallback time.Time) int64 {
	if given.IsZero() {
		return fallback.UnixMilli()
	}
	return given.UnixMilli()
}
