package capturespool

import (
	"context"
	"encoding/json"

	"github.com/algorhythmic/skald/sessioncapture"
)

// CheckpointState combines the producer position, this destination's durable
// acknowledgement, and the source's total outstanding envelope count.
type CheckpointState struct {
	Checkpoint   sessioncapture.Checkpoint
	Accepted     sessioncapture.Checkpoint
	Acknowledged bool
	Pending      int
	Err          error
}

// CheckpointSnapshot reads a consistent spool view in one statement. Pending
// counts include every destination, matching StatusForSource's Pending field.
// Decode errors remain local to a source, as with the point checkpoint reads.
func (s *Spool) CheckpointSnapshot(ctx context.Context, destination string) (map[string]CheckpointState, error) {
	if !s.hasDestination(destination) {
		return nil, ErrRegistry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.source_key,p.checkpoint,a.checkpoint,coalesce(q.pending,0)
FROM source_progress p
LEFT JOIN destination_progress a ON a.source_key=p.source_key AND a.destination=?
LEFT JOIN (SELECT source_key,count(*) AS pending FROM deliveries GROUP BY source_key) q ON q.source_key=p.source_key`, destination)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]CheckpointState{}
	for rows.Next() {
		var key string
		var producer, accepted []byte
		var state CheckpointState
		if err := rows.Scan(&key, &producer, &accepted, &state.Pending); err != nil {
			return nil, err
		}
		if json.Unmarshal(producer, &state.Checkpoint) != nil || state.Checkpoint.Version != 1 || state.Checkpoint.SourceKey != key {
			state.Err = ErrCorrupt
		}
		if accepted != nil {
			state.Acknowledged = true
			if json.Unmarshal(accepted, &state.Accepted) != nil || state.Accepted.Version != 1 || state.Accepted.SourceKey != key {
				state.Err = ErrCorrupt
			}
		}
		states[key] = state
	}
	return states, rows.Err()
}
