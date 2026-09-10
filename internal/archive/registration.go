package archive

import (
	"context"
	"errors"
)

// Registrations recovers persisted logical tokens, including inactive sources,
// for an explicitly configured discovery namespace. Locators are not identities.
func (s *Store) Registrations(ctx context.Context, namespace string) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT src.provider,st.logical_id,l.root,l.relative_path,l.conversation_id
 FROM streams st JOIN sources src USING(namespace) JOIN stream_locators l USING(stream_key)
 WHERE st.namespace=? ORDER BY st.stream_key LIMIT 257`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registration{}
	for rows.Next() {
		var r Registration
		r.Namespace = namespace
		if err := rows.Scan(&r.Provider, &r.StreamID, &r.Root, &r.Path, &r.ConversationID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) > 256 {
		return nil, errors.New("source_registry_limit")
	}
	return out, rows.Err()
}
