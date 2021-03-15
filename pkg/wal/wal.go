// Package wal implements a straightforward write-ahead log using
// consistent-overhead byte stuffing as described in
// https://www.pvk.ca/Blog/2021/01/11/stuff-your-logs/.
package wal

import (
	"bytes"
	"fmt"
	"io"
)

const (
	reserved   = [2]byte{0xfe, 0xfd}
	radix      = 0xfd
	smallLimit = 0xfc
	largeLimit = 0xfcfc
)

var (
	ShortRead = fmt.Errorf("short read")
)

// RecWriter wraps an underlying writer and appends records to the stream when
// requested, encoding them using constant-overhead word stuffing.
type RecWriter struct {
	dest io.Writer
}

func isDelimiter(b []byte, pos int) bool {
	if pos > len(b)-len(reserved) {
		return false
	}
	return bytes.Equal(b[pos:pos+len(reserved)], reserved[:])
}

func findReserved(p []byte, end int) (end int, found bool) {
	if end > len(p) {
		end = len(p)
	}
	if end < len(reserved) {
		return end, false
	}
	// Check up to the penultimate position (two-byte check).
	for i := start; i < end-len(reserved)+1; i++ {
		if isDelimiter(p, i) {
			return i, true
		}
	}
	return end, false
}

// Append adds a record to the end of the underlying writer. It encodes it
// using word stuffing.
func (w *RecWriter) Append(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	// Always start with the delimiter.
	buf := bytes.NewBuffer(reserved[:])

	// First block is small, try to find the reserved sequence in the first smallLimit bytes.
	// Format that block as |reserved 0|reserved 1|length|actual bytes...|.
	// Note that nowhere in these bytes can the reserved sequence fully appear (by construction).
	end, foundReserved := findReserved(p, smallLimit)

	// Add the size and data.
	if _, err := buf.Write(append(make([]byte, 0, 1+end), byte(end), p[:end]...)); err != nil {
		return fmt.Errorf("write rec: %w", err)
	}

	// Set the starting point for the next rounds. If we found a delimiter, we
	// need to advance past it first.
	if foundReserved {
		end += len(reserved)
	}

	// The next blocks are larger, up to largeLimit bytes each. Find the
	// reserved sequence if it's in there.
	// Format each block as |len1|len2|actual bytes...|.

	p = p[end:]
	for len(p) != 0 {
		end, foundReserved = findReserved(p, largeLimit)
		// Little-Endian length.
		len1 := (end - start) % radix
		len2 := (end - start) / radix
		if _, err := buf.Write(append(make([]byte, 0, 2+end), byte(len1), byte(len2), p[:end]...)); err != nil {
			return fmt.Errorf("write rec: %w", err)
		}

		if foundReserved {
			end += 2
		}
		p = p[end:]
	}
	if _, err := io.Copy(w.dest, buf); err != nil {
		return fmt.Errorf("write rec: %w", err)
	}
	return nil
}

// RecReader wraps an io.Reader and allows full records to be pulled at once.
type RecReader struct {
	src     io.Reader
	buf     []byte
	pos     int  // position in the unused read buffer.
	end     int  // one past the end of unused data.
	ended   bool // EOF reached, don't read again.
	started bool // First read has occurred.
}

// NewRecReader creates a RecReader from the given src, which is assumed to be
// a word-stuffed log.
func NewRecReader(src io.Reader) *RecReader {
	return &RecReader{
		src: src,
		buf: make([]byte, 1<<17),
	}
}

// fillBuf ensures that the internal buffer is at least half full, which is
// enough space for one short read and one long read.
func (r *RecReader) fillBuf() error {
	if r.ended {
		return nil // just use pos/end, no more reading.
	}
	if r.end-r.pos >= len(buf)/2 {
		// Don't bother filling if it's at least half full.
		// The buffer is designed to
		return nil
	}
	// Room to move, shift left.
	if r.pos != 0 {
		copy(r.buf[:], r.buf[r.pos:r.end])
	}
	r.end -= r.pos
	r.pos = 0

	// Read as much as possible.
	n, err := r.src.Read(r.buf[r.end:])
	if err != nil {
		if err != io.EOF {
			return fmt.Errorf("fill buffer: %w", err)
		}
		r.ended = true
	}
	r.end += n
	return nil
}

// discardLeader advances the position of the buffer, only if it contains a leading delimiter.
func (r *RecReader) discardLeader() bool {
	if r.end-r.pos < len(reserved) {
		return false
	}
	if bytes.Equal(r.buf[r.pos:r.pos+len(reserved)], reserved[:]) {
		r.pos += 2
		return true
	}
	return false
}

// pullN gets the next n bytes from the buffer, consuing it. The buffer must
// already have at least that many bytes, or it will return an error. It does
// not trigger a read.
func (r *RecReader) pullN(n int) ([]byte, error) {
	// Get the next n values from the buffer, consuming it.
	if want, have := n, r.end-r.pos; want > have {
		return nil, fmt.Errorf("pull n: want %d bytes, only have %d", want, have)
	}
	start := r.pos
	r.pos += n
	return r.buf[start:r.pos], nil
}

func (r *RecReader) isDelimiter(pos int) bool {
	return isDelimiter(r.buf[:r.end], pos)
}

// Done indicates whether the underlying stream is exhausted and all records are returned.
func (r *RecReader) Done() bool {
	return r.end == r.pos && r.ended
}

// scanN returns at most the next n bytes, fewer if it hits the end or a delimiter.
// It conumes them from the buffer. It does not read from the source: ensure
// that the buffer is full enough to proceed before calling. It can only go up
// to the penultimate byte, to ensure that it doesn't read half a delimiter.
func (r *RecReader) scanN(n int) ([]byte, error) {
	start := r.pos
	maxEnd := r.end - len(reserved) + 1
	// Ensure that we don't go beyond the end of the buffer (minus 1). The
	// caller should never ask for more than this. But it can happen if, for
	// example, the underlying stream is exhausted on a final block, with only
	// the implicit delimiter.
	if have := maxEnd - r.pos; n > have {
		if !r.ended {
			return nil, fmt.Errorf("scan: asked for %d, only had %d to read on in-progress buffer", n, have)
		}
		n = have
	}
	if maxEnd > start+n {
		maxEnd = start + n
	}
	for r.pos < maxEnd {
		if r.isDelimiter(r.pos) {
			break
		}
		r.pos++
	}
	return r.buf[start:r.pos], nil
}

// discardToDelimiter attempts to read until it finds a delimiter. Assumes that
// the buffer begins full. It may be filled again, in here.
func (r *RecReader) discardToDelimiter() error {
	for !r.isDelimiter(r.pos) {
		if _, err := r.scanN(r.end - r.pos); err != nil {
			return fmt.Errorf("discard: %w", err)
		}
		if err := f.fillBuf(); err != nil {
			return fmt.Errorf("discard: %w", err)
		}
	}
	return nil
}

// Next returns the next record in the underying stream, or an error. It begins
// by consuming the stream until it finds a delimiter (requiring each record to
// start with one), so even if there was an error in a previous record, this
// can skip bytes until it finds a new one. It does not require the first
// record to begin with a delimiter.
func (r *RecReader) Next() ([]byte, error) {
	if r.Done() {
		return nil, io.EOF
	}
	buf := new(bytes.Buffer)
	if err := r.fillBuf(); err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}

	// If we have "started" (read another record previously), fast forward to
	// the next delimiter in case the previous read failed due to corruption.
	if r.started {
		if err := r.discardToDelimiter(); err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
	}
	r.started = true

	// Every record (except perhaps the first) begins with a delimiter. Discard
	// it if present.
	r.discardLeader()

	// Read the first (small) section.
	b, err := r.pullN(1)
	if err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}
	smallSize := int(b[0])
	if smallSize > smallLimit {
		return nil, fmt.Errorf("next: short size header %d is too large", smallSize)
	}
	// The size portion can't be part of a delimiter, because it would have
	// triggered a "too big" error. Now we scan for delimiters while reading
	// from the buffer. Technically, when everything goes well, we should
	// always read exactly the right number of bytes. But the point of this is
	// that sometimes a record will be corrupted, so we might encounter a
	// delimiter in an unexpected place, so we scan and then check the size of
	// the return value. It can be wrong. In that case, return a meaingful
	// error so the caller can decide whether to keep going with the next
	// record.
	b, err := r.scanN(smallSize)
	if err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}
	if len(b) != smallSize {
		return nil, fmt.Errorf("next wanted %d, got %d: %w", smallSize, len(b), ShortRead)
	}

	if _, err := buf.Write(b); err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}
	if smallSize != smallLimit {
		// Implied delimiter in the data itself. Write the reserved word.
		if _, err := buf.Write(reserved[:]); err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
	}

	// Now we read zero or more large sections, stopping when we hit a delimiter or the end of the input stream.
	for !r.isDelimiter(r.pos) && !r.Done() {
		if err := r.fillBuf(); err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		// Extract 2 size bytes, convert using the radix.
		b, err := r.pullN(2)
		if err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		if b[0] >= radix || b[1] >= radix {
			return nil, fmt.Errorf("next: one of the two size bytes has an invalid value: %x", b[0])
		}
		size := int(b[0]) + radix*int(b[1]) // little endian size, in radix base.
		if size > largeLimit {
			return nil, fmt.Errorf("next: large interior size %d", size)
		}
		b, err := r.scanN(size)
		if err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		if len(b) != size {
			return nil, fmt.Errorf("next wanted %d, got %d: %w", size, len(b), ShortRead)
		}
		if _, err := buf.Write(b); err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		if size != largeLimit {
			// Implied delimiter in the data itself, append.
			if _, err := buf.Write(reserved[:]); err != nil {
				return nil, fmt.Errorf("next: %w", err)
			}
		}
	}

	// Note: the last block will always have an extra delimiter appended to the
	// end of it, so we have to not return that part.
	return buf.Bytes()[:buf.Len()-len(reserved)]
}
