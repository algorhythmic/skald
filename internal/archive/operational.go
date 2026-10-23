package archive

import "context"

// OperationalHealth counts enrollment diagnostics without reading payloads or
// counting record versions. Names are fixed aggregate states, never identities.
func (s *Store) OperationalHealth(ctx context.Context) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, "SELECT health,count(*) FROM stream_locators WHERE active=1 GROUP BY health")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var health string
		var n int64
		if err := rows.Scan(&health, &n); err != nil {
			return nil, err
		}
		counts[health] = n
	}
	return counts, rows.Err()
}
