package eqsqlite

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/shiblon/entroq"
)

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
const docColumns = `namespace, id, version, claimant, at_ms, key_primary, key_secondary, content, created_ms, modified_ms`

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
