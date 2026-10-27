package capturespool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
)

type snapshotCountingDriver struct{ queries *int }
type snapshotCountingConn struct {
	driver.Conn
	queries *int
}

func (d snapshotCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return snapshotCountingConn{Conn: conn, queries: d.queries}, nil
}
func (c snapshotCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	*c.queries++
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestCheckpointSnapshotUsesOneQueryFor615Sources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s := open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	d := fixture(t)
	for i := 0; i < 615; i++ {
		source := d.Source
		source.StreamID = fmt.Sprintf("snapshot-%d", i)
		cp := d.Batch.Checkpoint
		cp.SourceKey = source.Key()
		if err := s.SeedCheckpoint(source, cp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	queries := 0
	driverName := "snapshot-count-" + t.TempDir()
	sql.Register(driverName, snapshotCountingDriver{queries: &queries})
	var err error
	s.db, err = sql.Open(driverName, filepath.Join(dir, "capture-spool.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.CheckpointSnapshot(context.Background(), "archive")
	if err != nil || len(states) != 615 || queries != 1 {
		t.Fatalf("bulk read: sources=%d queries=%d err=%v", len(states), queries, err)
	}
	queries = 0
	for key, state := range states {
		producer, found, err := s.Checkpoint(key)
		if err != nil || !found || producer != state.Checkpoint || state.Err != nil {
			t.Fatalf("producer differs for %s: %+v %v", key, state, err)
		}
		accepted, found, err := s.DestinationCheckpoint(key, "archive")
		if err != nil || !found || !state.Acknowledged || accepted != state.Accepted {
			t.Fatalf("receipt differs for %s: %+v %v", key, state, err)
		}
		status, err := s.StatusForSource(key)
		if err != nil || state.Pending != status.Pending {
			t.Fatalf("pending differs for %s: %+v %v", key, state, err)
		}
	}
	if queries != 615*4 {
		t.Fatalf("baseline spool query count=%d", queries)
	}
}

func TestCheckpointSnapshotTracksIndependentAcksAndCorruption(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive", "peer"}, generous())
	defer s.Close()
	d := fixture(t)
	if err := s.Enqueue(d, []string{"archive", "peer"}); err != nil {
		t.Fatal(err)
	}
	states, err := s.CheckpointSnapshot(context.Background(), "archive")
	state := states[d.Source.Key()]
	if err != nil || state.Err != nil || state.Pending != 1 || state.Acknowledged || state.Checkpoint != d.Batch.Checkpoint {
		t.Fatalf("unacknowledged view: %+v %v", state, err)
	}
	if _, err := s.Deliver(context.Background(), &sink{name: "archive"}, 1); err != nil {
		t.Fatal(err)
	}
	states, err = s.CheckpointSnapshot(context.Background(), "archive")
	state = states[d.Source.Key()]
	if err != nil || state.Err != nil || state.Pending != 1 || !state.Acknowledged || state.Accepted != d.Batch.Checkpoint {
		t.Fatalf("independent acknowledgement lost: %+v %v", state, err)
	}
	if _, err := s.Deliver(context.Background(), &sink{name: "peer"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO source_progress VALUES ('corrupt',?)", []byte("{")); err != nil {
		t.Fatal(err)
	}
	states, err = s.CheckpointSnapshot(context.Background(), "archive")
	if err != nil || states[d.Source.Key()].Pending != 0 || states[d.Source.Key()].Err != nil || !errors.Is(states["corrupt"].Err, ErrCorrupt) {
		t.Fatalf("prune/corruption view: %+v %v", states, err)
	}
	if _, err := s.db.Exec("UPDATE destination_progress SET checkpoint=? WHERE source_key=? AND destination='archive'", []byte("[]"), d.Source.Key()); err != nil {
		t.Fatal(err)
	}
	states, err = s.CheckpointSnapshot(context.Background(), "archive")
	if err != nil || !errors.Is(states[d.Source.Key()].Err, ErrCorrupt) {
		t.Fatalf("corrupt destination accepted: %+v %v", states, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CheckpointSnapshot(ctx, "archive"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot: %v", err)
	}
	if _, err := s.CheckpointSnapshot(context.Background(), "unknown"); !errors.Is(err, ErrRegistry) {
		t.Fatalf("unknown destination: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckpointSnapshot(context.Background(), "archive"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed spool: %v", err)
	}
}
