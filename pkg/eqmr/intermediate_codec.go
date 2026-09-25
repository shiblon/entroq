package eqmr

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"io"

	"github.com/cespare/xxhash/v2"
)

var intermediateRunMagic = [8]byte{'E', 'Q', 'M', 'R', 'R', 'U', 'N', 1}

const (
	intermediateRecordTag    = byte(1)
	intermediateFooterTag    = byte(0)
	maxIntermediateRecordLen = 64 << 20
)

// encodeIntermediateRun writes one canonical immutable run. The footer count
// and checksum make a durable commit distinguishable from a truncated stream.
func encodeIntermediateRun(w io.Writer, records []intermediateRecord) error {
	checksum := xxhash.New()
	hashed := io.MultiWriter(w, checksum)
	if _, err := hashed.Write(intermediateRunMagic[:]); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for i, record := range records {
		if err := encodeIntermediateRecord(hashed, record); err != nil {
			return fmt.Errorf("write record %d: %w", i, err)
		}
	}
	if _, err := hashed.Write([]byte{intermediateFooterTag}); err != nil {
		return fmt.Errorf("write footer: %w", err)
	}
	if err := writeUvarint(hashed, uint64(len(records))); err != nil {
		return fmt.Errorf("write footer count: %w", err)
	}
	var sum [8]byte
	binary.BigEndian.PutUint64(sum[:], checksum.Sum64())
	if _, err := w.Write(sum[:]); err != nil {
		return fmt.Errorf("write footer checksum: %w", err)
	}
	return nil
}

func encodeIntermediateRecord(w io.Writer, record intermediateRecord) error {
	total := uint64(len(record.Primary)) + uint64(len(record.Secondary)) + uint64(len(record.Value))
	if total > maxIntermediateRecordLen {
		return fmt.Errorf("record is %d bytes, limit is %d", total, maxIntermediateRecordLen)
	}
	if _, err := w.Write([]byte{intermediateRecordTag}); err != nil {
		return err
	}
	for _, field := range []string{record.Primary, record.Secondary, record.Value} {
		if err := writeUvarint(w, uint64(len(field))); err != nil {
			return err
		}
	}
	for _, field := range []string{record.Primary, record.Secondary, record.Value} {
		if _, err := io.WriteString(w, field); err != nil {
			return err
		}
	}
	return nil
}

func writeUvarint(w io.Writer, n uint64) error {
	var b [binary.MaxVarintLen64]byte
	size := binary.PutUvarint(b[:], n)
	_, err := w.Write(b[:size])
	return err
}

type encodedIntermediateReader struct {
	body          io.ReadCloser
	raw           *bufio.Reader
	hashed        *hashingReader
	expectedCount int
	readCount     int
	done          bool
	closed        bool
}

func newEncodedIntermediateReader(body io.ReadCloser, expectedCount int) (*encodedIntermediateReader, error) {
	if body == nil {
		return nil, fmt.Errorf("nil run body")
	}
	raw := bufio.NewReader(body)
	r := &encodedIntermediateReader{
		body:          body,
		raw:           raw,
		hashed:        &hashingReader{r: raw, h: xxhash.New()},
		expectedCount: expectedCount,
	}
	var magic [len(intermediateRunMagic)]byte
	if _, err := io.ReadFull(r.hashed, magic[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if magic != intermediateRunMagic {
		return nil, fmt.Errorf("unknown run format %q", magic)
	}
	return r, nil
}

func (r *encodedIntermediateReader) Next(ctx context.Context) (intermediateRecord, error) {
	if r.done {
		return intermediateRecord{}, io.EOF
	}
	select {
	case <-ctx.Done():
		return intermediateRecord{}, ctx.Err()
	default:
	}
	tag, err := r.hashed.ReadByte()
	if err != nil {
		return intermediateRecord{}, fmt.Errorf("read frame tag: %w", err)
	}
	switch tag {
	case intermediateRecordTag:
		record, err := r.readRecord()
		if err != nil {
			return intermediateRecord{}, fmt.Errorf("read record %d: %w", r.readCount, err)
		}
		r.readCount++
		return record, nil

	case intermediateFooterTag:
		if err := r.readFooter(); err != nil {
			return intermediateRecord{}, err
		}
		r.done = true
		return intermediateRecord{}, io.EOF

	default:
		return intermediateRecord{}, fmt.Errorf("read frame %d: unknown tag %d", r.readCount, tag)
	}
}

func (r *encodedIntermediateReader) readRecord() (intermediateRecord, error) {
	lengths := [3]int{}
	total := 0
	for i := range lengths {
		n, err := readUvarint(r.hashed)
		if err != nil {
			return intermediateRecord{}, fmt.Errorf("read field %d length: %w", i, err)
		}
		if n > maxIntermediateRecordLen || total > maxIntermediateRecordLen-int(n) {
			return intermediateRecord{}, fmt.Errorf("record exceeds %d bytes", maxIntermediateRecordLen)
		}
		lengths[i] = int(n)
		total += int(n)
	}
	fields := [3]string{}
	for i, size := range lengths {
		b := make([]byte, size)
		if _, err := io.ReadFull(r.hashed, b); err != nil {
			return intermediateRecord{}, fmt.Errorf("read field %d: %w", i, err)
		}
		fields[i] = string(b)
	}
	return intermediateRecord{Primary: fields[0], Secondary: fields[1], Value: fields[2]}, nil
}

func (r *encodedIntermediateReader) readFooter() error {
	count, err := readUvarint(r.hashed)
	if err != nil {
		return fmt.Errorf("read footer count: %w", err)
	}
	var wantBytes [8]byte
	if _, err := io.ReadFull(r.raw, wantBytes[:]); err != nil {
		return fmt.Errorf("read footer checksum: %w", err)
	}
	want := binary.BigEndian.Uint64(wantBytes[:])
	if got := r.hashed.h.Sum64(); got != want {
		return fmt.Errorf("run checksum is %016x, want %016x", got, want)
	}
	if count != uint64(r.readCount) {
		return fmt.Errorf("run footer count is %d, read %d", count, r.readCount)
	}
	if r.expectedCount >= 0 && count != uint64(r.expectedCount) {
		return fmt.Errorf("run footer count is %d, pointer requires %d", count, r.expectedCount)
	}
	if _, err := r.raw.ReadByte(); err == nil {
		return fmt.Errorf("run has data after footer")
	} else if err != io.EOF {
		return fmt.Errorf("check run end: %w", err)
	}
	return nil
}

func (r *encodedIntermediateReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.done = true
	return r.body.Close()
}

type hashingReader struct {
	r io.Reader
	h hash.Hash64
}

func (r *hashingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		_, _ = r.h.Write(p[:n])
	}
	return n, err
}

func (r *hashingReader) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func readUvarint(r interface{ ReadByte() (byte, error) }) (uint64, error) {
	var n uint64
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, fmt.Errorf("uvarint overflows 64 bits")
			}
			return n | uint64(b)<<uint(7*i), nil
		}
		n |= uint64(b&0x7f) << uint(7*i)
	}
	return 0, fmt.Errorf("uvarint overflows 64 bits")
}
