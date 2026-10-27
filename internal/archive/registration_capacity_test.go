package archive

import (
	"context"
	"fmt"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func TestSelectedRegistrationCapacityRetainsInactiveChildren(t *testing.T) {
	ctx := context.Background()
	s, base, _ := testStore(t)
	// Replace the fixture registration so this corpus hits the exact bound.
	children := make([]Registration, MaxEnrolledSources)
	for i := range children {
		children[i] = base
		children[i].StreamID = fmt.Sprintf("child-%04d", i)
		children[i].ConversationID = sessionrecord.Key("claude-subagent", "parent", fmt.Sprintf("agent-%d", i))
	}
	if base.Provider != sessioncapture.Claude {
		t.Fatal("test needs a Claude namespace")
	}
	if err := s.Register(ctx, children); err != nil {
		t.Fatal("selected capacity boundary rejected", err)
	}
	primary := base
	primary.StreamID, primary.ConversationID = "new-primary", "native-primary"
	if err := s.Enroll(ctx, []Registration{primary}); err == nil || err.Error() != "source_registry_limit" {
		t.Fatal("active registration bound not enforced", err)
	}
	var afterOverflow int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM stream_locators WHERE active=1").Scan(&afterOverflow); err != nil || afterOverflow != MaxEnrolledSources {
		t.Fatal("failed enrollment changed selected registrations", afterOverflow, err)
	}
	// Opting out deactivates children; it does not remove their saved identities.
	if err := s.Register(ctx, []Registration{primary}); err != nil {
		t.Fatal(err)
	}
	selected, err := s.DiscoveryRegistrations(ctx, base.Namespace, false)
	if err != nil || len(selected) != 2 { // initial fixture and new primary
		t.Fatal("inactive children consumed selected recovery capacity", len(selected), err)
	}
	if _, err := s.DiscoveryRegistrations(ctx, base.Namespace, true); err == nil || err.Error() != "source_registry_limit" {
		t.Fatal("oversized child opt-in silently truncated", err)
	}
	var archived, active int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),SUM(active) FROM stream_locators").Scan(&archived, &active); err != nil {
		t.Fatal(err)
	}
	if archived != MaxEnrolledSources+2 || active != 1 {
		t.Fatal("archive registrations lost or active corpus wrong", archived, active)
	}
}
