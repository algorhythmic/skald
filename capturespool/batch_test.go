package capturespool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

func emptyDelivery(t *testing.T, id string, previous sessioncapture.Checkpoint) sessioncapture.Delivery {
	t.Helper()
	source := sessioncapture.Source{Namespace: "synthetic:batch", Provider: sessioncapture.Claude, StreamID: id}
	batch, err := sessioncapture.Read(bytes.NewReader(nil), source, previous, sessioncapture.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := sessioncapture.NewDelivery(source, previous, batch)
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

type groupedSink struct {
	sink
	groups     [][]string
	groupError bool
	wrongGroup bool
	afterGroup func()
}

func (s *groupedSink) AcceptBatch(ctx context.Context, deliveries []sessioncapture.Delivery) ([]sessioncapture.Acknowledgement, error) {
	ids := []string{}
	for _, d := range deliveries {
		ids = append(ids, d.BatchID)
	}
	s.groups = append(s.groups, ids)
	if s.groupError {
		return nil, errors.New("synthetic grouped failure")
	}
	acks := []sessioncapture.Acknowledgement{}
	for _, d := range deliveries {
		ack, err := s.sink.Accept(ctx, d)
		if err != nil {
			return nil, err
		}
		acks = append(acks, ack)
	}
	if s.wrongGroup {
		acks[0].Destination = "wrong"
	}
	if s.afterGroup != nil {
		s.afterGroup()
	}
	return acks, nil
}
func TestEnqueueBatchRollbackCapacityAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "spool")
	s := open(t, dir, []string{"archive"}, Limits{MaxPending: 1, MaxBytes: 1 << 20})
	first, other := emptyDelivery(t, "one", sessioncapture.Checkpoint{}), emptyDelivery(t, "two", sessioncapture.Checkpoint{})
	if err := s.EnqueueBatch(ctx, []sessioncapture.Delivery{first, other}, []string{"archive"}); !errors.Is(err, ErrCapacity) {
		t.Fatal("group should exceed capacity", err)
	}
	if status, _ := s.Status(); status.Pending != 0 {
		t.Fatal("failed atomic group retained envelope")
	}
	if _, ok, err := s.Checkpoint(first.Source.Key()); err != nil || ok {
		t.Fatal("rolled-back producer checkpoint survived", err)
	}
	if err := s.Enqueue(first, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, []string{"archive"}, Limits{MaxPending: 10, MaxBytes: 1 << 20})
	defer s.Close()
	if err := s.EnqueueBatch(ctx, []sessioncapture.Delivery{first, other}, []string{"archive"}); err != nil {
		t.Fatal("idempotent retry with another source", err)
	}
	grouped := &groupedSink{sink: sink{name: "archive"}}
	if n, err := s.Deliver(ctx, grouped, 10); err != nil || n != 2 {
		t.Fatalf("group delivery %d %v", n, err)
	}
	if len(grouped.groups) != 1 || len(grouped.groups[0]) != 2 {
		t.Fatal("eligible sources were not grouped")
	}
	if err := s.EnqueueBatch(ctx, []sessioncapture.Delivery{first, other}, []string{"archive"}); err != nil {
		t.Fatal("pruned identical batches lost retry identity", err)
	}
	if status, _ := s.Status(); status.Pending != 0 {
		t.Fatal("replay re-enqueued completed envelopes")
	}
}
func TestEnqueueBatchRollbackCheckpointAndCancellation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	first := emptyDelivery(t, "one", sessioncapture.Checkpoint{})
	other := emptyDelivery(t, "two", sessioncapture.Checkpoint{})
	bad := emptyDelivery(t, "two", other.Batch.Checkpoint)
	if err := s.EnqueueBatch(context.Background(), []sessioncapture.Delivery{first, bad}, []string{"archive"}); !errors.Is(err, ErrCheckpoint) {
		t.Fatal(err)
	}
	if status, _ := s.Status(); status.Pending != 0 {
		t.Fatal("checkpoint poison partially committed group")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.EnqueueBatch(ctx, []sessioncapture.Delivery{first, other}, []string{"archive"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if status, _ := s.Status(); status.Pending != 0 {
		t.Fatal("cancelled group committed")
	}
}
func TestGroupedFailureFallbackPreservesFIFOAndOtherSources(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	first := emptyDelivery(t, "one", sessioncapture.Checkpoint{})
	second := emptyDelivery(t, "one", first.Batch.Checkpoint)
	other := emptyDelivery(t, "two", sessioncapture.Checkpoint{})
	if err := s.EnqueueBatch(context.Background(), []sessioncapture.Delivery{first, second, other}, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	destination := &groupedSink{sink: sink{name: "archive", failID: first.BatchID}, groupError: true}
	if n, err := s.Deliver(context.Background(), destination, 10); err == nil || n != 1 {
		t.Fatalf("fallback did not isolate poison %d %v", n, err)
	}
	for _, id := range destination.seen {
		if id == second.BatchID {
			t.Fatal("later source envelope overtook failed predecessor")
		}
	}
	if status, _ := s.Status(); status.Pending != 2 {
		t.Fatal("poison source queue lost")
	}
	destination.failID = ""
	destination.groupError = false
	if n, err := s.Deliver(context.Background(), destination, 10); err != nil || n != 2 {
		t.Fatalf("FIFO recovery failed %d %v", n, err)
	}
}
func TestGroupMalformedAckFallbackAndCancelledReceiptRetry(t *testing.T) {
	for _, mode := range []string{"malformed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
			defer s.Close()
			deliveries := []sessioncapture.Delivery{emptyDelivery(t, "one", sessioncapture.Checkpoint{}), emptyDelivery(t, "two", sessioncapture.Checkpoint{})}
			if err := s.EnqueueBatch(context.Background(), deliveries, []string{"archive"}); err != nil {
				t.Fatal(err)
			}
			destination := &groupedSink{sink: sink{name: "archive"}, wrongGroup: mode == "malformed"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				destination.afterGroup = cancel
			}
			n, err := s.Deliver(ctx, destination, 10)
			if mode == "malformed" {
				if err != nil || n != 2 || len(destination.seen) != 4 {
					t.Fatalf("malformed grouped ack was not revalidated individually %d %v", n, err)
				}
				return
			}
			if !errors.Is(err, context.Canceled) || n != 0 {
				t.Fatalf("receipt cancellation %d %v", n, err)
			}
			if status, _ := s.Status(); status.Pending != 2 {
				t.Fatal("cancelled receipt lost durable queue")
			}
			for _, d := range deliveries {
				if _, found, err := s.DestinationCheckpoint(d.Source.Key(), "archive"); err != nil || found {
					t.Fatal("cancelled receipt advanced checkpoint", err)
				}
			}
			destination.afterGroup = nil
			if n, err := s.Deliver(context.Background(), destination, 10); err != nil || n != 2 {
				t.Fatalf("exact receipt retry %d %v", n, err)
			}
		})
	}
}
func TestBatchBoundAndDrainCount(t *testing.T) {
	limits := generous()
	limits.MaxPending = 40
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, limits)
	defer s.Close()
	deliveries := []sessioncapture.Delivery{}
	for i := 0; i < 17; i++ {
		deliveries = append(deliveries, emptyDelivery(t, fmt.Sprint(i), sessioncapture.Checkpoint{}))
	}
	if err := s.EnqueueBatch(context.Background(), deliveries, []string{"archive"}); err == nil {
		t.Fatal("oversized delivery count accepted")
	}
	for _, d := range deliveries {
		if err := s.Enqueue(d, []string{"archive"}); err != nil {
			t.Fatal(err)
		}
	}
	destination := &groupedSink{sink: sink{name: "archive"}}
	if n, err := s.Deliver(context.Background(), destination, 16); err != nil || n != 16 {
		t.Fatalf("bounded drain %d %v", n, err)
	}
	if len(destination.groups) != 1 || len(destination.groups[0]) != 16 {
		t.Fatal("group bound incorrect")
	}
	if status, _ := s.Status(); status.Pending != 1 {
		t.Fatal("drain exceeded maxBatches")
	}
}

func TestGroupedReceiptFailureRollsBackEveryAcknowledgement(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	deliveries := []sessioncapture.Delivery{emptyDelivery(t, "one", sessioncapture.Checkpoint{}), emptyDelivery(t, "two", sessioncapture.Checkpoint{})}
	if err := s.EnqueueBatch(context.Background(), deliveries, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_second_receipt BEFORE INSERT ON receipts WHEN NEW.seq=2 BEGIN SELECT RAISE(ABORT,'synthetic receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	destination := &groupedSink{sink: sink{name: "archive"}}
	if n, err := s.Deliver(context.Background(), destination, 10); err == nil || n != 0 {
		t.Fatalf("partial receipt success reported %d %v", n, err)
	}
	var receipts int
	if err := s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("earlier receipt leaked out of failed group", err)
	}
	if status, _ := s.Status(); status.Pending != 2 {
		t.Fatal("receipt failure pruned durable envelopes")
	}
	for _, d := range deliveries {
		if _, found, err := s.DestinationCheckpoint(d.Source.Key(), "archive"); err != nil || found {
			t.Fatal("receipt failure advanced destination checkpoint", err)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_second_receipt"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Deliver(context.Background(), destination, 10); err != nil || n != 2 {
		t.Fatalf("receipt retry %d %v", n, err)
	}
}

func TestBatchByteBoundAndLargeSingleCompatibility(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	first := emptyDelivery(t, strings.Repeat("a", MaxBatchBytes/2), sessioncapture.Checkpoint{})
	second := emptyDelivery(t, strings.Repeat("b", MaxBatchBytes/2), sessioncapture.Checkpoint{})
	if err := s.EnqueueBatch(context.Background(), []sessioncapture.Delivery{first, second}, []string{"archive"}); err == nil {
		t.Fatal("serialized group byte cap ignored")
	}
	if status, _ := s.Status(); status.Pending != 0 {
		t.Fatal("over-byte group partially committed")
	}
	for _, d := range []sessioncapture.Delivery{first, second} {
		if err := s.Enqueue(d, []string{"archive"}); err != nil {
			t.Fatal("existing scalar path rejected supported envelope", err)
		}
	}
	destination := &groupedSink{sink: sink{name: "archive"}}
	if n, err := s.Deliver(context.Background(), destination, 10); err != nil || n != 2 {
		t.Fatalf("bounded byte drain %d %v", n, err)
	}
	if len(destination.groups) != 0 {
		t.Fatal("optional group sink received oversized aggregate")
	}
}
