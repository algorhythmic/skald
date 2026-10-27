package sessioncapture_test

import (
	"bytes"
	"github.com/algorhythmic/skald/sessioncapture"
	"reflect"
	"testing"
)

func TestSnapshotContinuationMatchesMutableReader(t *testing.T) {
	data := []byte("{\"kind\":\"meta\",\"payload\":{}}\n")
	s := source(sessioncapture.Devin)
	s.ConversationID = "synthetic-session"
	first, err := sessioncapture.ReadSnapshot(data, s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"unchanged": data,
		"append":    append(append([]byte{}, data...), data...),
		"rewrite":   bytes.ReplaceAll(data, []byte("meta"), []byte("note")),
		"truncate":  data[:len(data)/2],
		"partial":   append(append([]byte{}, data...), '{'),
	} {
		t.Run(name, func(t *testing.T) {
			want, e := sessioncapture.Read(bytes.NewReader(candidate), s, first.Checkpoint, sessioncapture.DefaultLimits(), clock)
			got, err := sessioncapture.ReadSnapshot(candidate, s, first.Checkpoint, sessioncapture.DefaultLimits(), clock)
			if e != nil || err != nil {
				t.Fatalf("read errors: %v %v", e, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("snapshot changed capture semantics")
			}
			if (name == "rewrite" || name == "truncate") && len(got.Gaps) == 0 {
				t.Fatal("missing continuity gap")
			}
		})
	}
}

func BenchmarkUnchangedSnapshotPrefix(b *testing.B) {
	// Synthetic already-consumed data isolates the hot prefix verification path.
	data := bytes.Repeat([]byte("{\"kind\":\"meta\",\"payload\":{}}\n"), 1<<18)
	s := source(sessioncapture.Devin)
	s.ConversationID = "synthetic-session"
	cp := sessioncapture.Checkpoint{}
	for {
		batch, err := sessioncapture.ReadSnapshot(data, s, cp, sessioncapture.Limits{Records: 10000, RecordBytes: 4 << 20, BatchBytes: 16 << 20}, clock)
		if err != nil {
			b.Fatal(err)
		}
		cp = batch.Checkpoint
		if !batch.More {
			break
		}
	}
	for _, immutable := range []bool{false, true} {
		name := "mutable"
		if immutable {
			name = "snapshot"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				if immutable {
					_, err = sessioncapture.ReadSnapshot(data, s, cp, sessioncapture.DefaultLimits(), clock)
				} else {
					_, err = sessioncapture.Read(bytes.NewReader(data), s, cp, sessioncapture.DefaultLimits(), clock)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
