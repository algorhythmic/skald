package archive

import (
	"context"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

type QuarantineStatus struct {
	Total    int64  `json:"total"`
	LastHour int64  `json:"last_hour"`
	Health   string `json:"health"`
	Reason   string `json:"reason,omitempty"`
}

// Existing capture_gaps provides transactional, replay-deduplicated receipts.
// No payload scans, schema change, startup rewrite or new secret-bearing store.
func (s *Store) quarantineStatus(ctx context.Context, namespaces []string, now time.Time) (QuarantineStatus, error) {
	out := QuarantineStatus{Health: "healthy"}
	in, args := scopeSQL(namespaces)
	params := []any{stamp(now.Add(-time.Hour)), stamp(now), sessioncapture.QuarantineReason}
	params = append(params, args...)
	err := s.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(julianday(g.observed_at)>=julianday(?) AND julianday(g.observed_at)<=julianday(?)),0)
 FROM capture_gaps g JOIN streams st USING(stream_key) WHERE g.code=? AND st.namespace IN (`+in+`)`, params...).Scan(&out.Total, &out.LastHour)
	if out.LastHour > 0 {
		out.Health = "degraded"
		out.Reason = sessioncapture.QuarantineReason
	}
	return out, err
}
