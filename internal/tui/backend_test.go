package tui

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

type pagedArchive struct{ t *testing.T }

func (p pagedArchive) Do(_ context.Context, method, path string, query url.Values, out any) error {
	if method != "GET" || query.Get("namespace") != "fixture:expanded" {
		p.t.Fatal("unexpected method or namespace")
	}
	if path == "/v1/status" {
		*out.(*Status) = Status{Instance: "fixture", Sessions: archive.MaxEnrolledSources}
		return nil
	}
	if path != "/v1/sessions" || query.Get("limit") != "100" {
		p.t.Fatal("unexpected session request")
	}
	after := 0
	if raw := query.Get("cursor"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		after = parsed
	}
	page := archive.Page[archive.Session]{Boundary: 1}
	for len(page.Items) < 100 && after < archive.MaxEnrolledSources {
		page.Items = append(page.Items, archive.Session{Key: fmt.Sprintf("session-%04d", after)})
		after++
	}
	if after < archive.MaxEnrolledSources {
		page.Next = strconv.Itoa(after)
	}
	*out.(*archive.Page[archive.Session]) = page
	return nil
}

func TestRemoteSnapshotIncludesExpandedCaptureCapacity(t *testing.T) {
	snapshot, err := (Remote{Client: pagedArchive{t}, Namespace: "fixture:expanded"}).Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snapshot.Sessions.Items); got != archive.MaxEnrolledSources {
		t.Fatalf("snapshot included %d sessions, want %d", got, archive.MaxEnrolledSources)
	}
	if snapshot.Sessions.Next != "" {
		t.Fatal("snapshot omitted enrolled sessions")
	}
}
