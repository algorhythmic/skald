package archive

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	"modernc.org/sqlite"
)

type checkpointCountingDriver struct{ queries *int }
type checkpointCountingConn struct {
	driver.Conn
	queries *int
}

func (d checkpointCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return checkpointCountingConn{Conn: conn, queries: d.queries}, nil
}
func (c checkpointCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	*c.queries++
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestCheckpointSnapshotUsesOneQueryFor615Sources(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	registrations := []Registration{r}
	for i := 0; i < 614; i++ {
		other := r
		other.StreamID = fmt.Sprintf("source-%d", i)
		other.Path = fmt.Sprintf("source-%d.jsonl", i)
		registrations = append(registrations, other)
	}
	if err := s.Register(context.Background(), registrations); err != nil {
		t.Fatal(err)
	}
	// This exercises real checkpoint rows without copying transcript payloads.
	for _, other := range registrations[1:] {
		cp := batch.Checkpoint
		cp.SourceKey = other.Key()
		wire, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO ingest_checkpoints SELECT ?,generation,captured_offset,parsed_offset,ordinal,?,has_gaps FROM ingest_checkpoints WHERE stream_key=?", cp.SourceKey, string(wire), r.Key()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	queries := 0
	driverName := "checkpoint-count-" + NewID()
	sql.Register(driverName, checkpointCountingDriver{queries: &queries})
	var err error
	s.db, err = sql.Open(driverName, DSN(filepath.Join(s.dir, "archive.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.CheckpointSnapshot(context.Background())
	if err != nil || len(states) != 615 || queries != 1 {
		t.Fatalf("bulk read: sources=%d queries=%d err=%v", len(states), queries, err)
	}
	queries = 0
	for key, state := range states {
		cp, err := s.Checkpoint(context.Background(), key)
		if err != nil || cp != state.Checkpoint || state.Err != nil {
			t.Fatalf("snapshot differs for %s: %v %v", key, state, err)
		}
	}
	if queries != 615 {
		t.Fatalf("baseline point read query count=%d", queries)
	}
}

func TestCheckpointSnapshotPreservesSourceLocalCorruptionAndCancellation(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	other := r
	other.StreamID, other.Path = "corrupt", "corrupt.jsonl"
	if err := s.Register(context.Background(), []Registration{r, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO ingest_checkpoints SELECT ?,generation,captured_offset,parsed_offset,ordinal,'[]',has_gaps FROM ingest_checkpoints WHERE stream_key=?", other.Key(), r.Key()); err != nil {
		t.Fatal(err)
	}
	states, err := s.CheckpointSnapshot(context.Background())
	if err != nil || states[r.Key()].Checkpoint != batch.Checkpoint || states[r.Key()].Err != nil || states[other.Key()].Err == nil {
		t.Fatalf("source-local corruption lost: %+v %v", states, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CheckpointSnapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot: %v", err)
	}
}
