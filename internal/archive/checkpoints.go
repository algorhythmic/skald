package archive

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/algorhythmic/skald/sessioncapture"
)

// CheckpointState preserves a source-local decode failure without preventing
// unrelated sources from advancing.
type CheckpointState struct {
	Checkpoint sessioncapture.Checkpoint
	Err        error
}

// CheckpointSnapshot reads all durable positions in one statement. Its caller
// owns the returned map and must discard it after the current collection pass.
// Absence has the same meaning as Checkpoint: an uncaptured source.
func (s *Store) CheckpointSnapshot(ctx context.Context) (map[string]CheckpointState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, "SELECT stream_key,checkpoint_json FROM ingest_checkpoints")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]CheckpointState{}
	for rows.Next() {
		var key string
		var wire []byte
		if err := rows.Scan(&key, &wire); err != nil {
			return nil, err
		}
		var state CheckpointState
		if err := json.Unmarshal(wire, &state.Checkpoint); err != nil {
			state.Err = errors.New("corrupt_checkpoint")
		}
		states[key] = state
	}
	return states, rows.Err()
}
