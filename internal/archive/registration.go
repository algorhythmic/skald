package archive

import (
	"context"
	"errors"

	"github.com/algorhythmic/skald/sessioncapture"
)

// MaxEnrolledSources bounds the selected capture corpus independently of the
// smaller source-health status response. Archived history is never evicted.
const MaxEnrolledSources = 4096

// Registrations recovers persisted logical tokens, including inactive sources,
// for an explicitly configured discovery namespace. Locators are not identities.
func (s *Store) Registrations(ctx context.Context, namespace string) ([]Registration, error) {
	return s.discoveryRegistrations(ctx, namespace, true)
}

// DiscoveryRegistrations excludes opted-out Claude children before enforcing the
// selected-source bound. Their inactive registrations remain archived.
func (s *Store) DiscoveryRegistrations(ctx context.Context, namespace string, includeSubagents bool) ([]Registration, error) {
	return s.discoveryRegistrations(ctx, namespace, includeSubagents)
}

func (s *Store) discoveryRegistrations(ctx context.Context, namespace string, includeSubagents bool) ([]Registration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT src.provider,st.logical_id,l.root,l.relative_path,l.conversation_id
 FROM streams st JOIN sources src USING(namespace) JOIN stream_locators l USING(stream_key)
 WHERE st.namespace=? ORDER BY st.stream_key LIMIT ?`, namespace, 2*MaxEnrolledSources+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registration{}
	scanned := 0
	for rows.Next() {
		var r Registration
		r.Namespace = namespace
		if err := rows.Scan(&r.Provider, &r.StreamID, &r.Root, &r.Path, &r.ConversationID); err != nil {
			return nil, err
		}
		scanned++
		// A namespace can retain a full opted-out child corpus alongside a full
		// primary corpus. Bound recovery work without charging inactive children
		// against the selected capture capacity.
		if scanned > 2*MaxEnrolledSources {
			return nil, errors.New("source_registry_limit")
		}
		if !includeSubagents && r.Provider == sessioncapture.Claude && sessioncapture.IsClaudeSubagentConversation(r.ConversationID) {
			continue
		}
		out = append(out, r)
		if len(out) > MaxEnrolledSources {
			return nil, errors.New("source_registry_limit")
		}
	}
	return out, rows.Err()
}
