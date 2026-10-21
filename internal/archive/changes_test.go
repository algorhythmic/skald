package archive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/archivefeed"
	"github.com/algorhythmic/skald/sessioncapture"
)

func TestChangesFixedSnapshotThenTail(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw[:strings.IndexByte(string(raw), '\n')+1], r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	page, err := s.Changes(ctx, []string{r.Namespace}, 0, "", 1)
	if err != nil || page.Version != archivefeed.Version || page.Phase != "snapshot" || len(page.Events) != 1 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	if !page.More {
		t.Fatal("bounded snapshot did not report more")
	}
	high := page.HighWater
	more := testBatch(t, raw, r, first.Checkpoint)
	mustIngest(t, s, r, first.Checkpoint, more)
	var snapshot []archivefeed.Event
	for {
		wire, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		var next feedCursor
		if err := json.Unmarshal(wire, &next); err != nil {
			t.Fatal(err)
		}
		if next.Phase == "tail" {
			break
		}
		page, err = s.Changes(ctx, []string{r.Namespace}, 0, page.NextCursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			if e.Sequence > high {
				t.Fatal("post-boundary write entered snapshot", e)
			}
			snapshot = append(snapshot, e)
		}
	}
	if page.HighWater != high || page.Phase != "snapshot" || len(snapshot) == 0 {
		t.Fatal("snapshot did not reach fixed high water", page)
	}
	if page.More {
		t.Fatal("snapshot falsely reports more after final page")
	}
	tail, err := s.Changes(ctx, []string{r.Namespace}, 0, page.NextCursor, 100)
	if err != nil || tail.Phase != "tail" || len(tail.Events) == 0 {
		t.Fatalf("tail: %+v %v", tail, err)
	}
	for _, e := range tail.Events {
		if e.Sequence <= high {
			t.Fatal("snapshot event repeated in tail", e)
		}
	}
	empty, err := s.Changes(ctx, []string{r.Namespace}, 0, tail.NextCursor, 100)
	if err != nil || len(empty.Events) != 0 || empty.NextCursor == "" {
		t.Fatalf("empty tail: %+v %v", empty, err)
	}
}

func TestChangesExactLocatorAndCursorBinding(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	page, err := s.Changes(ctx, []string{r.Namespace}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range page.Events {
		if e.Kind != "record" {
			continue
		}
		if e.Locator == nil || e.RecordKey == "" || e.SourceRevision == "" || e.RecordKey != e.Locator.RecordKey || e.SourceRevision != e.Locator.SourceRevision {
			t.Fatal("record identity missing", e)
		}
		if e.Locator == nil || e.Locator.RecordKey == "" || e.Locator.SourceRevision == "" || e.Locator.AdapterVersion == "" || e.Ordinal == nil || e.StreamKey != r.Key() {
			t.Fatal("incomplete record locator", e)
		}
		if _, err := s.Get(ctx, []string{r.Namespace}, e.Locator.RecordKey, e.Locator.SourceRevision, e.Locator.AdapterVersion, false); err != nil {
			t.Fatal("locator did not resolve", err)
		}
		found = true
	}
	if !found {
		t.Fatal("record event missing")
	}
	if _, err := s.Changes(ctx, []string{"other"}, 0, page.NextCursor, 100); !errors.Is(err, ErrFeedCursorExpired) {
		t.Fatal("scope change accepted", err)
	}
	if _, err := s.Changes(ctx, []string{r.Namespace}, 1, page.NextCursor, 100); !errors.Is(err, ErrFeedCursorExpired) {
		t.Fatal("read scope generation change accepted", err)
	}
	oldInstance := s.instance
	s.instance = NewID()
	if _, err := s.Changes(ctx, []string{r.Namespace}, 0, page.NextCursor, 100); !errors.Is(err, ErrFeedCursorExpired) {
		t.Fatal("restored archive instance accepted old cursor", err)
	}
	s.instance = oldInstance
	wire, _ := base64.RawURLEncoding.DecodeString(page.NextCursor)
	var c map[string]any
	if err := json.Unmarshal(wire, &c); err != nil {
		t.Fatal(err)
	}
	c["version"] = float64(2)
	wire, _ = json.Marshal(c)
	if _, err := s.Changes(ctx, []string{r.Namespace}, 0, base64.RawURLEncoding.EncodeToString(wire), 100); !errors.Is(err, archivefeed.ErrUnsupportedVersion) {
		t.Fatal("new version accepted", err)
	}
	if _, err := s.Changes(ctx, []string{r.Namespace}, 0, "%%", 100); !errors.Is(err, ErrFeedCursor) {
		t.Fatal("malformed cursor accepted", err)
	}
}

func TestChangesRemovalAndRedactionAreDistinct(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	rec := b.Records[0].Record
	if err := s.Register(ctx, nil); err != nil {
		t.Fatal(err)
	}
	beforeRemovalReplay, err := s.Stats(ctx, []string{r.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, nil); err != nil {
		t.Fatal(err)
	}
	afterRemovalReplay, err := s.Stats(ctx, []string{r.Namespace})
	if err != nil || beforeRemovalReplay["boundary"] != afterRemovalReplay["boundary"] {
		t.Fatal("repeated unconfiguration duplicated event", err)
	}
	page, err := s.Changes(ctx, []string{r.Namespace}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var removed, available bool
	removals := 0
	for _, e := range page.Events {
		if e.Kind == "source_removed" {
			removed = e.Unavailable == "native_source_removed"
			removals++
		}
		if e.Locator != nil && e.Locator.RecordKey == rec.RecordKey {
			available = true
		}
	}
	if !removed || !available || removals != 1 {
		t.Fatal("native source removal withdrew retained evidence", page.Events)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO purge_tombstones VALUES(?, 'record', ?, 1, ?)`, r.Namespace, rec.RecordKey, stamp(testNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO changes(change_kind,namespace,record_key,payload_json) VALUES('withdrawal',?,?,?)`, r.Namespace, rec.RecordKey, encode(map[string]string{"reason": "redacted", "scope_kind": "record", "scope_key": rec.RecordKey})); err != nil {
		t.Fatal(err)
	}
	page, err = s.Changes(ctx, []string{r.Namespace}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var redacted, unavailable bool
	for _, e := range page.Events {
		if e.Kind == "redaction" && e.ScopeKey == rec.RecordKey {
			redacted = true
		}
		if e.Kind == "record" && e.Ordinal != nil && e.Locator == nil && e.Unavailable == "redacted" {
			if e.RecordKey != rec.RecordKey || e.SourceRevision == "" {
				t.Fatal("redacted revision lost identity", e)
			}
			unavailable = true
		}
	}
	if !redacted || !unavailable {
		t.Fatal("redaction did not invalidate prior locator", page.Events)
	}
}

func TestChangesCorrectionRetainsExactOldRevision(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	before, err := s.Changes(ctx, []string{r.Namespace}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "I will check the archive boundaries.", "Revised answer.", 1)
	if changed == string(raw) {
		t.Fatal("fixture did not change")
	}
	second := testBatch(t, []byte(changed), r, first.Checkpoint)
	mustIngest(t, s, r, first.Checkpoint, second)
	tail, err := s.Changes(ctx, []string{r.Namespace}, 0, before.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	var corrected *archivefeed.Event
	for i := range tail.Events {
		if tail.Events[i].Kind == "correction" {
			corrected = &tail.Events[i]
			break
		}
	}
	if corrected == nil || corrected.Locator == nil {
		t.Fatal("rewrite did not publish correction", tail.Events)
	}
	var old *archivefeed.Locator
	for _, e := range before.Events {
		if e.Locator != nil && e.Locator.RecordKey == corrected.Locator.RecordKey && e.Locator.SourceRevision != corrected.Locator.SourceRevision {
			old = e.Locator
			break
		}
	}
	if old == nil {
		t.Fatal("old exact revision missing")
	}
	if _, err := s.Get(ctx, []string{r.Namespace}, old.RecordKey, old.SourceRevision, old.AdapterVersion, false); err != nil {
		t.Fatal("old revision no longer resolves", err)
	}
	if _, err := s.Get(ctx, []string{r.Namespace}, corrected.Locator.RecordKey, corrected.Locator.SourceRevision, corrected.Locator.AdapterVersion, false); err != nil {
		t.Fatal("corrected revision does not resolve", err)
	}
}
