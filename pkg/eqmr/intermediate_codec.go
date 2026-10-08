package eqmr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cespare/xxhash/v2"
)

// intermediateRunMagic opens every run: "EQMRRUN" and the format version.
var intermediateRunMagic = [8]byte{'E', 'Q', 'M', 'R', 'R', 'U', 'N', 2}

// errCorruptRun marks a run whose bytes are wrong: a checksum or count that
// does not match, a format or frame no writer produces, data after the end, or
// a run that is short although its source ended cleanly. Reading it again
// reads the same wrong bytes, so it is permanent. A run that is missing, or
// whose source broke off partway, is not marked: missing is how a losing
// duplicate finds inputs already collected, and a broken source may read whole
// next time.
var errCorruptRun = errors.New("corrupt intermediate run")

// corruptf reports a corrupt run.
func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errCorruptRun, fmt.Sprintf(format, args...))
}

const (
	intermediateBlockTag  = byte(1)
	intermediateFooterTag = byte(0)

	// maxIntermediateRecordLen bounds the three fields of one record.
	maxIntermediateRecordLen = 64 << 20

	// intermediateBlockBytes is the size at which a writer closes a block. A
	// reader holds one verified block at a time, so it is also the reader's
	// memory per open run, except that a single record larger than this makes
	// a block of its own.
	intermediateBlockBytes = 64 << 10

	// maxIntermediateBlockLen bounds a block's payload as a reader sees it: a
	// full block plus one maximal record and its field lengths.
	maxIntermediateBlockLen = intermediateBlockBytes + maxIntermediateRecordLen + 3*binary.MaxVarintLen64
)

// encodeIntermediateRun writes one canonical immutable run.
//
// Records are grouped into blocks, each carrying its record count and a
// checksum, so a reader can verify a whole block before handing any of its
// records on: damaged data is caught before anything uses it. Each checksum
// covers the previous one as well as the block, chaining the blocks together,
// so a block that is missing, repeated, reordered, or taken from another run
// breaks the chain. The footer closes the chain with the run's record count,
// which distinguishes a complete run from a truncated one.
func encodeIntermediateRun(w io.Writer, records []intermediateRecord) error {
	if _, err := w.Write(intermediateRunMagic[:]); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	prev := chainStart()
	var (
		payload []byte
		count   int
	)
	flush := func() error {
		if count == 0 {
			return nil
		}
		head := binary.AppendUvarint([]byte{intermediateBlockTag}, uint64(count))
		head = binary.AppendUvarint(head, uint64(len(payload)))
		prev = chainSum(prev, head, payload)
		for _, b := range [][]byte{head, payload, prev[:]} {
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("write block: %w", err)
			}
		}
		payload, count = payload[:0], 0
		return nil
	}
	for i, record := range records {
		total := uint64(len(record.Primary)) + uint64(len(record.Secondary)) + uint64(len(record.Value))
		if total > maxIntermediateRecordLen {
			return fmt.Errorf("record %d is %d bytes, limit is %d", i, total, maxIntermediateRecordLen)
		}
		payload = appendIntermediateRecord(payload, record)
		count++
		if len(payload) >= intermediateBlockBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	footer := binary.AppendUvarint([]byte{intermediateFooterTag}, uint64(len(records)))
	sum := chainSum(prev, footer)
	for _, b := range [][]byte{footer, sum[:]} {
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("write footer: %w", err)
		}
	}
	return nil
}

// appendIntermediateRecord appends one record's field lengths and then its
// fields.
func appendIntermediateRecord(b []byte, record intermediateRecord) []byte {
	for _, field := range []string{record.Primary, record.Secondary, record.Value} {
		b = binary.AppendUvarint(b, uint64(len(field)))
	}
	for _, field := range []string{record.Primary, record.Secondary, record.Value} {
		b = append(b, field...)
	}
	return b
}

// chainStart is the checksum the chain begins from, so a run's first block is
// bound to the format it was written in.
func chainStart() [8]byte {
	var sum [8]byte
	binary.BigEndian.PutUint64(sum[:], xxhash.Sum64(intermediateRunMagic[:]))
	return sum
}

// chainSum is the checksum of parts, chained to the previous checksum prev.
func chainSum(prev [8]byte, parts ...[]byte) [8]byte {
	h := xxhash.New()
	// Writes to an xxhash digest cannot fail.
	_, _ = h.Write(prev[:])
	for _, p := range parts {
		_, _ = h.Write(p)
	}
	var sum [8]byte
	binary.BigEndian.PutUint64(sum[:], h.Sum64())
	return sum
}

// encodedIntermediateReader streams the records of one encoded run, verifying
// each block before yielding any of its records.
type encodedIntermediateReader struct {
	body          io.ReadCloser
	src           *sourceReader
	raw           *bufio.Reader
	prev          [8]byte
	expectedCount int
	readCount     int

	// block is the unread remainder of the current verified block, and
	// blockLeft how many of its records are still to come.
	block     []byte
	blockLeft int

	done   bool
	closed bool
}

// sourceReader records whether its source ended cleanly. A source that ends
// cleanly says io.EOF: a file at its end, or a response body that arrived
// whole. One that breaks off says something else, such as io.ErrUnexpectedEOF
// for a connection dropped mid-body. A run that is short although its source
// ended cleanly is short in storage, which is corruption; one whose source
// broke off may read whole next time.
type sourceReader struct {
	r     io.Reader
	ended bool
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		s.ended = true
	}
	return n, err
}

// short reports a run that stopped before it should have. It is corrupt if
// the source ended cleanly, and otherwise an ordinary, retryable read error.
func (r *encodedIntermediateReader) short(err error, format string, args ...any) error {
	what := fmt.Sprintf(format, args...)
	if r.src.ended && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return corruptf("%s: run ends early", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func newEncodedIntermediateReader(body io.ReadCloser, expectedCount int) (*encodedIntermediateReader, error) {
	if body == nil {
		return nil, fmt.Errorf("nil run body")
	}
	src := &sourceReader{r: body}
	r := &encodedIntermediateReader{
		body:          body,
		src:           src,
		raw:           bufio.NewReader(src),
		prev:          chainStart(),
		expectedCount: expectedCount,
	}
	var magic [len(intermediateRunMagic)]byte
	if _, err := io.ReadFull(r.raw, magic[:]); err != nil {
		return nil, r.short(err, "read header")
	}
	if magic != intermediateRunMagic {
		return nil, corruptf("unknown run format %q", magic)
	}
	return r, nil
}

func (r *encodedIntermediateReader) Next(ctx context.Context) (intermediateRecord, error) {
	for r.blockLeft == 0 {
		if r.done {
			return intermediateRecord{}, io.EOF
		}
		select {
		case <-ctx.Done():
			return intermediateRecord{}, ctx.Err()
		default:
		}
		if err := r.readFrame(); err != nil {
			return intermediateRecord{}, err
		}
	}
	record, err := r.decodeRecord()
	if err != nil {
		return intermediateRecord{}, err
	}
	r.readCount++
	return record, nil
}

// readFrame reads and verifies the next block, or the footer.
func (r *encodedIntermediateReader) readFrame() error {
	tag, err := r.raw.ReadByte()
	if err != nil {
		return r.short(err, "read frame tag after %d records", r.readCount)
	}
	switch tag {
	case intermediateBlockTag:
		return r.readBlock()
	case intermediateFooterTag:
		return r.readFooter()
	default:
		return corruptf("unknown frame tag %d after %d records", tag, r.readCount)
	}
}

func (r *encodedIntermediateReader) readBlock() error {
	head := []byte{intermediateBlockTag}
	count, head, err := r.readUvarint(head, "block record count")
	if err != nil {
		return err
	}
	length, head, err := r.readUvarint(head, "block length")
	if err != nil {
		return err
	}
	// Every record has at least three bytes of field lengths, so a count that
	// would not fit is as wrong as a length beyond the limit.
	if count == 0 || length > maxIntermediateBlockLen || count > length/3 {
		return corruptf("block of %d records in %d bytes", count, length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r.raw, payload); err != nil {
		return r.short(err, "read block")
	}
	if err := r.verify(head, payload); err != nil {
		return err
	}
	r.block, r.blockLeft = payload, int(count)
	return nil
}

func (r *encodedIntermediateReader) readFooter() error {
	count, footer, err := r.readUvarint([]byte{intermediateFooterTag}, "footer count")
	if err != nil {
		return err
	}
	if err := r.verify(footer); err != nil {
		return err
	}
	if count != uint64(r.readCount) {
		return corruptf("run footer count is %d, read %d", count, r.readCount)
	}
	if r.expectedCount >= 0 && count != uint64(r.expectedCount) {
		return corruptf("run footer count is %d, pointer requires %d", count, r.expectedCount)
	}
	if _, err := r.raw.ReadByte(); err == nil {
		return corruptf("run has data after footer")
	} else if err != io.EOF {
		return fmt.Errorf("check run end: %w", err)
	}
	r.done = true
	return nil
}

// verify reads the checksum that follows parts and checks it against the
// chain, advancing the chain when it matches.
func (r *encodedIntermediateReader) verify(parts ...[]byte) error {
	var got [8]byte
	if _, err := io.ReadFull(r.raw, got[:]); err != nil {
		return r.short(err, "read checksum")
	}
	want := chainSum(r.prev, parts...)
	if got != want {
		return corruptf("checksum is %x, want %x, after %d records", got, want, r.readCount)
	}
	r.prev = got
	return nil
}

// readUvarint reads one uvarint from the stream, returning it along with buf
// extended by its encoded bytes, which the frame's checksum covers.
func (r *encodedIntermediateReader) readUvarint(buf []byte, what string) (uint64, []byte, error) {
	var n uint64
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.raw.ReadByte()
		if err != nil {
			return 0, buf, r.short(err, "read %s", what)
		}
		buf = append(buf, b)
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, buf, corruptf("%s overflows 64 bits", what)
			}
			return n | uint64(b)<<uint(7*i), buf, nil
		}
		n |= uint64(b&0x7f) << uint(7*i)
	}
	return 0, buf, corruptf("%s overflows 64 bits", what)
}

// decodeRecord takes the next record from the current verified block. A block
// whose checksum matched but which does not decode was written wrong, which is
// as permanent as damage.
func (r *encodedIntermediateReader) decodeRecord() (intermediateRecord, error) {
	var lengths [3]uint64
	for i := range lengths {
		n, size := binary.Uvarint(r.block)
		if size <= 0 {
			return intermediateRecord{}, corruptf("record %d: bad field %d length", r.readCount, i)
		}
		lengths[i], r.block = n, r.block[size:]
	}
	var fields [3]string
	for i, n := range lengths {
		if n > uint64(len(r.block)) {
			return intermediateRecord{}, corruptf("record %d: field %d overruns its block", r.readCount, i)
		}
		fields[i], r.block = string(r.block[:n]), r.block[n:]
	}
	r.blockLeft--
	if r.blockLeft == 0 && len(r.block) != 0 {
		return intermediateRecord{}, corruptf("block ending at record %d has %d bytes left over", r.readCount, len(r.block))
	}
	return intermediateRecord{Primary: fields[0], Secondary: fields[1], Value: fields[2]}, nil
}

func (r *encodedIntermediateReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.done = true
	r.block, r.blockLeft = nil, 0
	return r.body.Close()
}
