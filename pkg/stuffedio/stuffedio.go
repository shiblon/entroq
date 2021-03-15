// Package stuffedio implements a straightforward self-synchronizing log using
// consistent-overhead word stuffing (yes, COWS) as described in Paul Khuong's
// https://www.pvk.ca/Blog/2021/01/11/stuff-your-logs/.
package stuffedio

import (
	"bytes"
	"fmt"
	"io"
)

var (
	reserved = []byte{0xfe, 0xfd}
)

const (
	radix      int = 0xfd
	smallLimit int = 0xfc
	largeLimit int = 0xfcfc
)

var (
	// CorruptRecord errors are returned (wrapped, use errors.Is to detect)
	// when a situation is encountered that can't happen in a clean record.
	// It is usually safe to skip after receiving this error, provided that a
	// missing entry doesn't cause consistency issues for the reader.
	CorruptRecord = fmt.Errorf("corrupt record")
)

// Writer wraps an underlying writer and appends records to the stream when
// requested, encoding them using constant-overhead word stuffing.
type Writer struct {
	dest io.Writer
}

// NewWriter creates a new Writer with the underlying output writer.
func NewWriter(dest io.Writer) *Writer {
	return &Writer{
		dest: dest,
	}
}

func isDelimiter(b []byte, pos int) bool {
	if pos > len(b)-len(reserved) {
		return false
	}
	return bytes.Equal(b[pos:pos+len(reserved)], reserved[:])
}

func findReserved(p []byte, end int) (int, bool) {
	if end > len(p) {
		end = len(p)
	}
	if end < len(reserved) {
		return end, false
	}
	// Check up to the penultimate position (two-byte check).
	for i := 0; i < end-len(reserved)+1; i++ {
		if isDelimiter(p, i) {
			return i, true
		}
	}
	return end, false
}

// Append adds a record to the end of the underlying writer. It encodes it
// using word stuffing.
func (w *Writer) Append(p []byte) error {
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
	rec := append(make([]byte, 0, 1+end), byte(end))
	rec = append(rec, p[:end]...)
	if _, err := buf.Write(rec); err != nil {
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
		len1 := end % radix
		len2 := end / radix
		rec := append(make([]byte, 0, 2+end), byte(len1), byte(len2))
		rec = append(rec, p[:end]...)
		if _, err := buf.Write(rec); err != nil {
			return fmt.Errorf("write rec: %w", err)
		}

		if foundReserved {
			end += len(reserved)
		}
		p = p[end:]
	}
	// If the last pass through found a reserved sequence, then that means
	// it _ended_ with a reserved sequence. That means we need an empty record to terminate.
	// Empty records indicate "the whole thing was a delimiter" (zero
	// non-delimiter bytes, which is less than the record max).
	if foundReserved {
		if _, err := buf.Write([]byte{0, 0}); err != nil {
			return fmt.Errorf("write rec: %w", err)
		}
	}
	if _, err := io.Copy(w.dest, buf); err != nil {
		return fmt.Errorf("write rec: %w", err)
	}
	return nil
}

// Reader wraps an io.Reader and allows full records to be pulled at once.
type Reader struct {
	src   io.Reader
	buf   []byte
	pos   int  // position in the unused read buffer.
	end   int  // one past the end of unused data.
	ended bool // EOF reached, don't read again.
}

// NewReader creates a Reader from the given src, which is assumed to be
// a word-stuffed log.
func NewReader(src io.Reader) *Reader {
	return &Reader{
		src: src,
		buf: make([]byte, 1<<17),
	}
}

// fillBuf ensures that the internal buffer is at least half full, which is
// enough space for one short read and one long read.
func (r *Reader) fillBuf() error {
	if r.ended {
		return nil // just use pos/end, no more reading.
	}
	if r.end-r.pos >= len(r.buf)/2 {
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
	if r.end < len(r.buf) {
		// Assume a short read means there's no more data.
		r.ended = true
	}
	return nil
}

// discardLeader advances the position of the buffer, only if it contains a leading delimiter.
func (r *Reader) discardLeader() bool {
	if r.end-r.pos < len(reserved) {
		return false
	}
	if bytes.Equal(r.buf[r.pos:r.pos+len(reserved)], reserved[:]) {
		r.pos += len(reserved)
		return true
	}
	return false
}

func (r *Reader) atDelimiter() bool {
	return isDelimiter(r.buf[r.pos:r.end], 0)
}

// Done indicates whether the underlying stream is exhausted and all records are returned.
func (r *Reader) Done() bool {
	return r.end == r.pos && r.ended
}

// scanN returns at most the next n bytes, fewer if it hits the end or a delimiter.
// It conumes them from the buffer. It does not read from the source: ensure
// that the buffer is full enough to proceed before calling. It can only go up
// to the penultimate byte, to ensure that it doesn't read half a delimiter.
func (r *Reader) scanN(n int) []byte {
	start := r.pos
	maxEnd := r.end
	// Ensure that we don't go beyond the end of the buffer (minus 1). The
	// caller should never ask for more than this. But it can happen if, for
	// example, the underlying stream is exhausted on a final block, with only
	// the implicit delimiter.
	if have := maxEnd - r.pos; n > have {
		n = have
	}
	if maxEnd > start+n {
		maxEnd = start + n
	}
	for r.pos < maxEnd {
		if r.atDelimiter() {
			break
		}
		r.pos++
	}
	return r.buf[start:r.pos]
}

// discardToDelimiter attempts to read until it finds a delimiter. Assumes that
// the buffer begins full. It may be filled again, in here.
func (r *Reader) discardToDelimiter() error {
	for !r.atDelimiter() {
		r.scanN(r.end - r.pos)
		if err := r.fillBuf(); err != nil {
			return fmt.Errorf("discard: %w", err)
		}
	}
	return nil
}

// Next returns the next record in the underying stream, or an error. It begins
// by consuming the stream until it finds a delimiter (requiring each record to
// start with one), so even if there was an error in a previous record, this
// can skip bytes until it finds a new one. It does not require the first
// record to begin with a delimiter. Returns a wrapped io.EOF when complete.
// More idiomatically, check Done after every iteration.
func (r *Reader) Next() ([]byte, error) {
	if r.Done() {
		return nil, io.EOF
	}
	buf := new(bytes.Buffer)
	if err := r.fillBuf(); err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}

	// Find the first real delimiter. This can help get things on track after a
	// corrupt record, or at the start of a shard that comes in the middle of a
	// record. We strictly require every record to be prefixed with the
	// delimiter, including the first, allowing this logic to work properly.
	if err := r.discardToDelimiter(); err != nil {
		return nil, fmt.Errorf("next: %w", err)
	}

	if !r.discardLeader() {
		return nil, fmt.Errorf("next: no leading delimiter in record: %w", CorruptRecord)
	}

	// Read the first (small) section.
	b := r.scanN(1)
	if len(b) != 1 {
		return nil, fmt.Errorf("next: short read on size byte: %w", CorruptRecord)
	}
	smallSize := int(b[0])
	if smallSize > smallLimit {
		return nil, fmt.Errorf("next: short size header %d is too large: %w", smallSize, CorruptRecord)
	}
	// The size portion can't be part of a delimiter, because it would have
	// triggered a "too big" error. Now we scan for delimiters while reading
	// from the buffer. Technically, when everything goes well, we should
	// always read exactly the right number of bytes. But the point of this is
	// that sometimes a record will be corrupted, so we might encounter a
	// delimiter in an unexpected place, so we scan and then check the size of
	// the return value. It can be wrong. In that case, return a meaningful
	// error so the caller can decide whether to keep going with the next
	// record.
	b = r.scanN(smallSize)
	if len(b) != smallSize {
		return nil, fmt.Errorf("next: wanted short %d, got %d: %w", smallSize, len(b), CorruptRecord)
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
	for !r.atDelimiter() && !r.Done() {
		if err := r.fillBuf(); err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		// Extract 2 size bytes, convert using the radix.
		b := r.scanN(2)
		if len(b) != 2 {
			return nil, fmt.Errorf("next: short read on size bytes: %w", CorruptRecord)
		}
		if int(b[0]) >= radix || int(b[1]) >= radix {
			return nil, fmt.Errorf("next: one of the two size bytes has an invalid value: %x: %w", b[0], CorruptRecord)
		}
		size := int(b[0]) + radix*int(b[1]) // little endian size, in radix base.
		if size > largeLimit {
			return nil, fmt.Errorf("next: large interior size %d: %w", size, CorruptRecord)
		}
		b = r.scanN(size)
		if len(b) != size {
			return nil, fmt.Errorf("next: wanted long %d, got %d: %w", size, len(b), CorruptRecord)
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
	return buf.Bytes()[:buf.Len()-len(reserved)], nil
}
