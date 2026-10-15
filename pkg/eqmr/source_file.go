package eqmr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiblon/entroq/pkg/worker"
)

// fileSourceDriver is NewFileSource's driver name and compatibility version.
const fileSourceDriver = "entroq.eqmr.filesource/1"

// maxSourceLineBytes bounds one line of a file source.
const maxSourceLineBytes = maxIntermediateRecordLen

// FileFormat is how a file source turns lines into records: Lines or TSV.
type FileFormat struct {
	name  string
	parse func(path string, line int, text string) (*KV, error)
}

var (
	// Lines reads each line as a record keyed "<path>:<line>", numbered from 1.
	Lines = FileFormat{name: "lines", parse: func(path string, line int, text string) (*KV, error) {
		return NewKV(fmt.Sprintf("%s:%d", path, line), text), nil
	}}

	// TSV reads each line as a key, a tab, and a value.
	TSV = FileFormat{name: "tsv", parse: func(path string, line int, text string) (*KV, error) {
		key, value, ok := strings.Cut(text, "\t")
		if !ok {
			return nil, worker.MoveErrorf("%s:%d has no tab", path, line)
		}
		return NewKV(key, value), nil
	}}
)

// FileSourceOption configures NewFileSource.
type FileSourceOption func(*fileSource)

// WithFiles sets the absolute paths a file source reads, one split each, in
// order. Only the process that calls SetupFrom needs them.
func WithFiles(files ...string) FileSourceOption {
	return func(s *fileSource) { s.files = files }
}

// WithFormat sets how a file source reads lines; the default is Lines. It is
// part of the source's identity, so every process must set the same one.
func WithFormat(format FileFormat) FileSourceOption {
	return func(s *fileSource) { s.format = format }
}

// NewFileSource returns a source that reads files, one split per file. Lines
// end in "\n" or "\r\n". name is the logical name the run records.
//
// Every process that maps the run must see the files at the same paths. The
// source is identified by its format.
func NewFileSource(name string, opts ...FileSourceOption) (Source, error) {
	s := &fileSource{format: Lines}
	for _, opt := range opts {
		opt(s)
	}
	if s.format.parse == nil {
		return nil, fmt.Errorf("eqmr file source: format is required")
	}
	seen := make(map[string]bool, len(s.files))
	for _, f := range s.files {
		if !filepath.IsAbs(f) {
			return nil, fmt.Errorf("eqmr file source: %q is not an absolute path", f)
		}
		if seen[f] {
			return nil, fmt.Errorf("eqmr file source: %q is listed twice", f)
		}
		seen[f] = true
	}
	s.desc = Descriptor{
		Name:     name,
		Driver:   fileSourceDriver,
		Identity: "format=" + s.format.name,
	}
	return s, nil
}

type fileSource struct {
	desc   Descriptor
	files  []string
	format FileFormat
}

// fileSplit is a file source's split: one file.
type fileSplit struct {
	Path string `json:"path"`
}

func (s *fileSource) Descriptor() Descriptor { return s.desc }

// Splits lists one split per file, each of which must be a regular file.
func (s *fileSource) Splits(context.Context) ([]json.RawMessage, error) {
	splits := make([]json.RawMessage, 0, len(s.files))
	for _, f := range s.files {
		info, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("eqmr file source: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("eqmr file source: %q is not a regular file", f)
		}
		raw, err := json.Marshal(fileSplit{Path: f})
		if err != nil {
			return nil, fmt.Errorf("eqmr file source: encode split: %w", err)
		}
		splits = append(splits, raw)
	}
	return splits, nil
}

// Open reads one file. A file that is gone fails the run.
func (s *fileSource) Open(_ context.Context, raw json.RawMessage) (Records, error) {
	var split fileSplit
	if err := json.Unmarshal(raw, &split); err != nil {
		return nil, worker.MoveErrorf("parse file split: %v", err)
	}
	if !filepath.IsAbs(split.Path) {
		return nil, worker.MoveErrorf("file split %q is not an absolute path", split.Path)
	}
	f, err := os.Open(split.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, worker.MoveErrorf("open input: %v", err)
	}
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	return &fileRecords{f: f, r: bufio.NewReader(f), path: split.Path, format: s.format}, nil
}

// fileRecords reads one file's lines as records.
type fileRecords struct {
	f      *os.File
	r      *bufio.Reader
	path   string
	format FileFormat
	line   int
}

func (r *fileRecords) Next(context.Context) (*KV, error) {
	var text []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		text = append(text, chunk...)
		if len(text) > maxSourceLineBytes {
			return nil, worker.MoveErrorf("%s:%d is longer than %d bytes", r.path, r.line+1, maxSourceLineBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == io.EOF && len(text) == 0 {
			return nil, io.EOF
		}
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read %s: %w", r.path, err)
		}
		break
	}
	r.line++
	s := string(text)
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = strings.TrimSuffix(t, "\r")
	}
	return r.format.parse(r.path, r.line, s)
}

func (r *fileRecords) Close() error { return r.f.Close() }
