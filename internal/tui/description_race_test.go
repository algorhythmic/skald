package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/gdamore/tcell/v2"
)

type changingDescriptionBackend struct{ *fakeBackend }

func (b changingDescriptionBackend) Descriptions(context.Context, string) (archive.DescriptionSelection, error) {
	return archive.DescriptionSelection{Boundary: 999, Kind: "assistant_excerpt", Selected: &archive.TranscriptEntry{Text: "MISMATCHED NEW DESCRIPTION"}}, nil
}

func TestDescriptionChangeCannotDiscardSuccessfulTranscript(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if e := sim.Init(); e != nil {
		t.Fatal(e)
	}
	defer sim.Fini()
	sim.SetSize(120, 40)
	screen := &observedScreen{sim, make(chan string, 100)}
	fake := &fakeBackend{make(chan snapshotRequest, 10), make(chan pageRequest, 10)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runScreen(ctx, screen, changingDescriptionBackend{fake}, ThemeDesktop) }()
	snapshot := waitFor(t, fake.snap)
	snapshot.reply <- snapshotReply{value: fixtureSnapshot()}
	page := waitFor(t, fake.page)
	page.reply <- pageReply{value: fixturePage("Valid concurrent transcript")}
	view := waitView(t, screen, "Valid concurrent transcript")
	if strings.Contains(view, "MISMATCHED NEW DESCRIPTION") {
		t.Fatal("mixed archive boundaries", view)
	}
	cancel()
	if e := waitFor(t, done); e != nil {
		t.Fatal(e)
	}
}
