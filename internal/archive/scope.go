package archive

import (
	"context"
	"errors"
	"slices"
)

// SetReadScope records the daemon's current namespace allowlist. Identical
// restarts preserve cursors; narrowing and later regranting never revive them.
func (s *Store) SetReadScope(ctx context.Context, namespaces []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns := append([]string{}, namespaces...)
	slices.Sort(ns)
	ns = slices.Compact(ns)
	for _, n := range ns {
		if n == "" {
			return errors.New("invalid_read_scope")
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE archive_scope SET generation=generation+1,namespaces_json=? WHERE singleton=1 AND namespaces_json!=?`, encode(ns), encode(ns))
	return err
}

func (s *Store) ReadScopeGeneration(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var generation int64
	err := s.db.QueryRowContext(ctx, "SELECT generation FROM archive_scope WHERE singleton=1").Scan(&generation)
	return generation, err
}
