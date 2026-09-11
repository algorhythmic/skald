package tui

import (
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestClusterFoldPrefixAndExpand(t *testing.T) {
	m := newModel()
	m.snapshot = fixtureSnapshot()
	m.snapshot.Sessions.Items = []archive.Session{
		{Key: "a", Title: "Authorized disposable evaluation. Use only wcu tools. Both isolated applications start closed. Launch the browser at http://127.0.0.1:33301/", TitleKind: "derived", Namespace: "n", Provider: "codex"},
		{Key: "b", Title: "Authorized disposable evaluation. Use only wcu tools. Both isolated applications start closed. Launch the browser at http://127.0.0.1:42237/", TitleKind: "derived", Namespace: "n", Provider: "codex"},
		{Key: "c", Title: "Distinct conversation", TitleKind: "native", Namespace: "n", Provider: "codex"},
		{Key: "d", Title: "aa55bb33-uuid", Namespace: "n", Provider: "devin"},
		{Key: "e", Title: "cc77dd99-uuid", Namespace: "n", Provider: "devin"},
	}
	m.grouping = len(groupNames) - 1 // all
	m.rebuildRows()
	clusters, members := 0, 0
	for _, r := range m.rows {
		if r.cluster != nil {
			clusters++
		} else {
			members++
		}
	}
	if clusters != 2 || members != 1 {
		t.Fatalf("expected two clusters (shared prefix + untitled) and one plain row, got %d/%d", clusters, members)
	}
	if m.expandedClusters == nil {
		m.expandedClusters = map[string]bool{}
	}
	for _, r := range m.rows {
		if r.cluster != nil {
			m.expandedClusters[clusterKey(r.group, r.cluster)] = true
		}
	}
	m.rebuildRows()
	if len(m.rows) != 2+5 { // two headers plus all members
		t.Fatalf("expanded clusters should list all sessions, got %d rows", len(m.rows))
	}
	seen := map[string]bool{}
	for _, r := range m.rows {
		if r.cluster == nil {
			seen[r.session.Key] = true
		}
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if !seen[k] {
			t.Fatalf("expanded cluster missing member %s", k)
		}
	}
}
