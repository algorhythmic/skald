package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/internal/testsupport"
	"github.com/algorhythmic/skald/sessioncapture"
	"strings"
	"testing"
)

func TestEveryPatternCheckpointMetadataAndResume(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			for _, configured := range []bool{false, true} {
				s := source(sessioncapture.Claude)
				if configured {
					s.ConversationID = fixture.Input
					s.ProviderVersion = fixture.Input
				}
				line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": fixture.Input, "uuid": "metadata-record", "version": fixture.Input, "message": map[string]string{"role": "user", "content": "ordinary synthetic body"}})
				line = append(line, '\n')
				first, e := sessioncapture.Read(bytes.NewReader(line), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
				if e != nil || len(first.Records) != 1 {
					t.Fatal("metadata capture failed")
				}
				header, _ := json.Marshal(first.Checkpoint)
				if bytes.Contains(header, []byte(fixture.Value)) || !strings.Contains(first.Checkpoint.ConversationID, "[REDACTED:checkpoint_identity]") {
					t.Fatal("checkpoint identity or version leaked")
				}
				d, e := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, first)
				if e != nil || d.Validate() != nil {
					t.Fatal("private metadata replay failed")
				}
				wire, _ := json.Marshal(d)
				if secretredact.CheckWire(wire) != nil {
					t.Fatal("delivery metadata leaked")
				}
				// Recreate the source from public registration metadata, modeling restart.
				public := sessioncapture.PublicSource(s)
				tail, _ := json.Marshal(map[string]any{"type": "user", "sessionId": fixture.Input, "uuid": "metadata-tail", "message": map[string]string{"role": "user", "content": "ordinary tail"}})
				tail = append(tail, '\n')
				input := append(append([]byte{}, line...), tail...)
				next, e := sessioncapture.Read(bytes.NewReader(input), public, first.Checkpoint, sessioncapture.DefaultLimits(), clock)
				if e != nil || len(next.Records) != 1 || next.Checkpoint.Offset != int64(len(input)) {
					t.Fatal("redacted checkpoint did not resume")
				}
				if next.Records[0].Record.ConversationKey == nil || *next.Records[0].Record.ConversationKey != *first.Records[0].Record.ConversationKey {
					t.Fatal("anonymous identity changed")
				}
				if _, e = sessioncapture.NewDelivery(public, first.Checkpoint, next); e != nil {
					t.Fatal("metadata continuation replay failed")
				}
			}
		})
	}
}
