package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/gdamore/tcell/v2"
)

func timestamp(value string) *string { return &value }

func TestRecencyOrdersInstantsAndFoldedMembers(t *testing.T) {
	m := newModel()
	m.grouping = len(groupNames) - 1
	m.snapshot.Sessions.Items = []archive.Session{
		{Key: "whole", Title: "Shared title", TitleKind: "native", LastRecordTime: timestamp("2026-10-23T10:00:00Z")},
		{Key: "fraction", Title: "Shared title", TitleKind: "native", LastRecordTime: timestamp("2026-10-23T10:00:00.1Z")},
		{Key: "offset", Title: "Newest offset", TitleKind: "native", LastRecordTime: timestamp("2026-10-23T06:00:01-04:00")},
		{Key: "older", Title: "Older", TitleKind: "native", LastRecordTime: timestamp("2026-10-23T11:59:59+02:00")},
		{Key: "invalid", Title: "Invalid", TitleKind: "native", LastRecordTime: timestamp("invalid")},
		{Key: "missing", Title: "Missing", TitleKind: "native"},
	}
	m.expandedClusters = map[string]bool{clusterKey("all conversations", m.snapshot.Sessions.Items[:2]): true}
	m.rebuildRows()
	got := []string{}
	for _, row := range m.rows {
		if row.cluster == nil {
			got = append(got, row.session.Key)
		}
	}
	if strings.Join(got, ",") != "offset,fraction,whole,older,invalid,missing" {
		t.Fatal("recency ordering:", got)
	}
	if m.rows[1].cluster == nil {
		t.Fatal("folded unit was not ordered by its newest member")
	}
	if compareObservedTime(timestamp("2026-10-23T10:00:00Z"), timestamp("2026-10-23T06:00:00-04:00")) != 0 {
		t.Fatal("equal instants differ")
	}
}

type recencyBackend struct{ snapshot Snapshot }

func (b recencyBackend) Snapshot(context.Context, string) (Snapshot, error) { return b.snapshot, nil }
func (b recencyBackend) Transcript(context.Context, string, string, bool) (archive.Page[archive.TranscriptEntry], error) {
	return archive.Page[archive.TranscriptEntry]{Boundary: 1}, nil
}

func TestRecencyHeaderCycleAndGlobalShortcut(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	sim.SetSize(120, 40)
	screen := &observedScreen{sim, make(chan string, 100)}
	snapshot := fixtureSnapshot()
	snapshot.Sessions.Items = []archive.Session{
		{Key: "old", Title: "Older session", TitleKind: "native", Projects: []archive.Project{{Path: "/project/a"}}, LastRecordTime: timestamp("2026-10-22T10:00:00Z")},
		{Key: "new", Title: "Newest session", TitleKind: "native", Projects: []archive.Project{{Path: "/project/z"}}, LastRecordTime: timestamp("2026-10-23T10:00:00Z")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runScreen(ctx, screen, recencyBackend{snapshot}, ThemeDesktop) }()
	waitView(t, screen, "order recent")
	for _, order := range []string{"title", "status", "recent"} {
		key(screen, 'O')
		waitView(t, screen, "order "+order)
	}
	key(screen, 'R')
	view := waitView(t, screen, "sessions / by all")
	newest, older := strings.Index(view, "Newest session"), strings.Index(view, "Older session")
	if !strings.Contains(view, "order recent") || newest < 0 || older < 0 || newest > older {
		t.Fatal("global recency shortcut did not show newest first:\n" + view)
	}
	key(screen, 'q')
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRecencyAcrossEveryLoadedPage(t *testing.T) {
	snapshot, err := (Remote{Client: pagedArchive{t}, Namespace: "fixture:expanded"}).Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// The newest item deliberately comes from the last API page.
	for i := range snapshot.Sessions.Items {
		item := &snapshot.Sessions.Items[i]
		item.Title, item.TitleKind = item.Key, "native"
		item.LastRecordTime = timestamp("2026-10-22T10:00:00Z")
	}
	snapshot.Sessions.Items[len(snapshot.Sessions.Items)-1].LastRecordTime = timestamp("2026-10-23T10:00:00Z")
	m := newModel()
	m.grouping = len(groupNames) - 1
	m.snapshot = snapshot
	m.rebuildRows()
	if len(m.rows) != archive.MaxEnrolledSources || m.rows[0].session.Key != fmt.Sprintf("session-%04d", archive.MaxEnrolledSources-1) {
		t.Fatal("recency did not cover the full loaded archive")
	}
}
