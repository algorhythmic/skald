package tui

import (
	"context"
	"net/url"
	"strconv"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/client"
)

type RootStatus struct {
	ModifiedSince string            `json:"modified_since"`
	Namespace     string            `json:"namespace"`
	Root          string            `json:"root"`
	State         string            `json:"state"`
	Enrolled      int               `json:"enrolled"`
	Candidates    int               `json:"candidates"`
	MaxSessions   int               `json:"max_sessions"`
	Rejected      int               `json:"rejected"`
	Issues        map[string]string `json:"issues"`
}
type Status struct {
	Discovery []RootStatus           `json:"discovery"`
	Instance  string                 `json:"archive_instance"`
	Boundary  int64                  `json:"boundary"`
	Sessions  int64                  `json:"sessions"`
	Sources   []archive.SourceHealth `json:"sources"`
	Issues    map[string]string      `json:"capture_issues"`
}
type Snapshot struct {
	Status   Status
	Sessions archive.Page[archive.Session]
}
type Backend interface {
	Snapshot(context.Context, string) (Snapshot, error)
	Transcript(context.Context, string, string, bool) (archive.Page[archive.TranscriptEntry], error)
}
type Remote struct {
	Client    *client.Client
	Namespace string
}

func (r Remote) query() url.Values {
	q := url.Values{}
	if r.Namespace != "" {
		q.Set("namespace", r.Namespace)
	}
	return q
}
func (r Remote) Snapshot(ctx context.Context, cursor string) (Snapshot, error) {
	var out Snapshot
	q := r.query()
	if err := r.Client.Do(ctx, "GET", "/v1/status", q, &out.Status); err != nil {
		return out, err
	}
	// Walk every session page so grouping sees the whole archive, not just the
	// first hundred conversation keys. The page size stays the API bound.
	out.Sessions.Items = []archive.Session{}
	for pages := 0; pages < 20; pages++ {
		var page archive.Page[archive.Session]
		pq := r.query()
		pq.Set("limit", "100")
		pq.Set("cursor", cursor)
		if err := r.Client.Do(ctx, "GET", "/v1/sessions", pq, &page); err != nil {
			return out, err
		}
		out.Sessions.Items = append(out.Sessions.Items, page.Items...)
		out.Sessions.Boundary = page.Boundary
		out.Sessions.Next = page.Next
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	return out, nil
}
func (r Remote) Transcript(ctx context.Context, key, cursor string, history bool) (archive.Page[archive.TranscriptEntry], error) {
	var out archive.Page[archive.TranscriptEntry]
	q := r.query()
	q.Set("conversation", key)
	q.Set("cursor", cursor)
	q.Set("limit", "25")
	q.Set("history", strconv.FormatBool(history))
	err := r.Client.Do(ctx, "GET", "/v1/transcript", q, &out)
	return out, err
}
