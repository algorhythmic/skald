package archive

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"sync"

	"github.com/algorhythmic/skald/sessionrecord"
	"modernc.org/sqlite"
)

const maxPayloadBytes = 64 << 20

func init() {
	if err := sqlite.RegisterDeterministicScalarFunction("skald_inflate", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		b, ok := args[0].([]byte)
		if !ok {
			return nil, errors.New("invalid_compressed_payload")
		}
		n, ok := args[1].(int64)
		if !ok {
			return nil, errors.New("invalid_payload_length")
		}
		raw, err := inflatePayload(b, n)
		digest, ok := args[2].(string)
		if err != nil {
			return nil, err
		}
		if !ok || sessionrecord.Digest(raw) != digest {
			return nil, errors.New("payload_digest_mismatch")
		}
		return raw, nil
	}); err != nil {
		panic(err)
	}
}
func inflatePayload(b []byte, n int64) ([]byte, error) {
	if n < 0 || n > maxPayloadBytes || len(b) > maxPayloadBytes {
		return nil, errors.New("payload_size_limit")
	}
	input := bytes.NewReader(b)
	r, err := zlib.NewReader(input)
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(r, n+1))
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(out)) != n || input.Len() != 0 {
		return nil, errors.New("payload_length_mismatch")
	}
	return out, nil
}

// Each pooled encoder points at a stable forwarding writer. Detaching its
// destination before pooling avoids retaining a large previous output buffer.
type payloadEncoder struct {
	writer      *zlib.Writer
	destination io.Writer
}

func (p *payloadEncoder) Write(b []byte) (int, error) { return p.destination.Write(b) }

var payloadEncoders = sync.Pool{New: func() any {
	p := &payloadEncoder{destination: io.Discard}
	var err error
	p.writer, err = zlib.NewWriterLevel(p, zlib.BestSpeed)
	if err != nil {
		panic(err)
	}
	return p
}}

func deflatePayload(raw []byte) ([]byte, error) {
	var output bytes.Buffer
	encoder := payloadEncoders.Get().(*payloadEncoder)
	encoder.destination = &output
	encoder.writer.Reset(encoder)
	if _, err := encoder.writer.Write(raw); err != nil {
		encoder.destination = io.Discard
		return nil, err
	}
	if err := encoder.writer.Close(); err != nil {
		encoder.destination = io.Discard
		return nil, err
	}
	encoder.destination = io.Discard
	payloadEncoders.Put(encoder)
	return output.Bytes(), nil
}

// CompressionResult reports logical savings; freed pages remain reusable by SQLite.
type CompressionResult struct {
	Examined    int   `json:"examined"`
	Compressed  int   `json:"compressed"`
	InputBytes  int64 `json:"input_bytes"`
	StoredBytes int64 `json:"stored_bytes"`
}

// CompressPayloads converts at most maxRows and maxInputBytes in one transaction.
// It never vacuums or changes source revisions, normalizations, or feed positions.
// Call repeatedly for explicit maintenance. Limits bound memory, WAL and lock time.
func (s *Store) CompressPayloads(ctx context.Context, maxRows int, maxInputBytes int64) (result CompressionResult, err error) {
	if maxRows < 1 || maxRows > 1000 || maxInputBytes < 1 || maxInputBytes > maxPayloadBytes {
		return result, errors.New("invalid_compression_limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.checkSpace(uint64(maxInputBytes*2 + 1<<20)); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	type candidate struct {
		table  string
		id     int64
		raw    []byte
		digest string
	}
	var items []candidate
	// rowid order plus a negative decoded_length sentinel prevents repeated scans
	// of tiny or incompressible payloads. Views continue exposing original bytes.
	for _, table := range []string{"content_storage", "normalization_storage"} {
		query := "SELECT rowid,original_bytes,digest FROM content_storage INDEXED BY content_pending_compression WHERE decoded_length IS NULL AND byte_length<=? LIMIT ?"
		if table == "normalization_storage" {
			query = "SELECT rowid,CAST(envelope_json AS BLOB),'' FROM normalization_storage INDEXED BY normalization_pending_compression WHERE decoded_length IS NULL AND length(CAST(envelope_json AS BLOB))<=? LIMIT ?"
		}
		rows, e := tx.QueryContext(ctx, query, maxInputBytes-result.InputBytes, maxRows-result.Examined)
		if e != nil {
			return result, e
		}
		for rows.Next() {
			var c candidate
			c.table = table
			if e = rows.Scan(&c.id, &c.raw, &c.digest); e != nil {
				rows.Close()
				return result, e
			}
			if int64(len(c.raw))+result.InputBytes > maxInputBytes {
				break
			}
			items = append(items, c)
			result.Examined++
			result.InputBytes += int64(len(c.raw))
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return result, e
		}
	}
	for _, c := range items {
		if e := ctx.Err(); e != nil {
			return result, e
		}
		if c.digest != "" && sessionrecord.Digest(c.raw) != c.digest {
			return result, errors.New("payload_digest_mismatch")
		}
		packed, e := deflatePayload(c.raw)
		if e != nil {
			return result, e
		}
		decoded, e := inflatePayload(packed, int64(len(c.raw)))
		if e != nil || !bytes.Equal(decoded, c.raw) {
			return result, errors.New("payload_compression_verification_failed")
		}
		if len(packed)+32 >= len(c.raw) {
			// Mark examined values without rewriting their bytes.
			update := "UPDATE " + c.table + " SET decoded_length=-1 WHERE rowid=?"
			if c.table == "normalization_storage" {
				update = "UPDATE normalization_storage SET decoded_length=-1,display_role=json_extract(envelope_json,'$.role'),display_usable=(trim(coalesce(json_extract(envelope_json,'$.body.text'),''))<>'' AND coalesce(json_extract(envelope_json,'$.availability.state'),'') IN ('available','partial') AND coalesce(json_extract(envelope_json,'$.body.state'),'') NOT IN ('unavailable','opaque','unsupported')) WHERE rowid=?"
			}
			if _, e = tx.ExecContext(ctx, update, c.id); e != nil {
				return result, e
			}
			result.StoredBytes += int64(len(c.raw))
			continue
		}
		if c.table == "content_storage" {
			_, e = tx.ExecContext(ctx, "UPDATE content_storage SET original_bytes=?,byte_length=?,decoded_length=? WHERE rowid=?", packed, len(packed), len(c.raw), c.id)
		} else {
			_, e = tx.ExecContext(ctx, "UPDATE normalization_storage SET display_role=json_extract(envelope_json,'$.role'),display_usable=(trim(coalesce(json_extract(envelope_json,'$.body.text'),''))<>'' AND coalesce(json_extract(envelope_json,'$.availability.state'),'') IN ('available','partial') AND coalesce(json_extract(envelope_json,'$.body.state'),'') NOT IN ('unavailable','opaque','unsupported')),compressed_envelope=?,envelope_json='{}',decoded_length=?,envelope_digest=? WHERE rowid=?", packed, len(c.raw), sessionrecord.Digest(c.raw), c.id)
		}
		if e != nil {
			return result, capacityError(e)
		}
		result.Compressed++
		result.StoredBytes += int64(len(packed))
	}
	if err = tx.Commit(); err != nil {
		return result, capacityError(err)
	}
	return result, nil
}
