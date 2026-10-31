package downloads

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestHubSnapshotAndUpdates(t *testing.T) {
	hub := NewEventHub()
	userID := "user-1"
	hub.Publish(userID, JobEvent{ID: "j1", State: "queued", Progress: 0})
	ch, snapshot, cancel, ok := hub.Subscribe(userID)
	if !ok {
		t.Fatal("Subscribe refused a user under no cap")
	}
	defer cancel()
	if len(snapshot) != 1 || snapshot[0].ID != "j1" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	hub.Publish(userID, JobEvent{ID: "j1", State: "downloading", Progress: 40})
	select {
	case ev := <-ch:
		if ev.State != "downloading" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestHubCoalescesPerJob(t *testing.T) {
	hub := NewEventHub()
	userID := "user-2"
	ch, _, cancel, ok := hub.Subscribe(userID)
	if !ok {
		t.Fatal("Subscribe refused a user under no cap")
	}
	defer cancel()
	const lastProgress = 98.0
	for i := 0; i < 50; i++ {
		hub.Publish(userID, JobEvent{ID: "j1", State: "downloading", Progress: float64(i) * 2})
	}
	// The first publish delivers at once, the rest must coalesce: only a
	// handful of events may arrive and the final published state must be the
	// last one delivered.
	var got []JobEvent
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
			if ev.Progress == lastProgress {
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if len(got) == 0 {
		t.Fatal("no events delivered")
	}
	if len(got) > 3 {
		t.Fatalf("coalescing failed: %d events for 50 publishes", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Progress < got[i-1].Progress {
			t.Fatalf("stale event after newer one: %+v then %+v", got[i-1], got[i])
		}
	}
	if final := got[len(got)-1]; final.Progress != lastProgress {
		t.Fatalf("final state lost to coalescing: %+v", final)
	}
}

func TestHubCleansUpOnCancel(t *testing.T) {
	hub := NewEventHub()
	userID := "user-3"
	before := runtime.NumGoroutine()

	ch1, _, cancel1, ok1 := hub.Subscribe(userID)
	if !ok1 {
		t.Fatal("first Subscribe refused")
	}
	ch2, _, cancel2, ok2 := hub.Subscribe(userID)
	if !ok2 {
		t.Fatal("second Subscribe refused")
	}
	if hub.Len() != 2 {
		t.Fatalf("Len = %d, want 2", hub.Len())
	}
	// Two publishes inside the window leave a pending flush behind.
	hub.Publish(userID, JobEvent{ID: "j1", State: "downloading", Progress: 10})
	hub.Publish(userID, JobEvent{ID: "j1", State: "downloading", Progress: 20})

	cancel1()
	cancel1() // idempotent
	if hub.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after first cancel", hub.Len())
	}
	drainUntilClosed(t, ch1)

	cancel2()
	if hub.Len() != 0 {
		t.Fatalf("Len = %d, want 0", hub.Len())
	}
	drainUntilClosed(t, ch2)

	done := make(chan struct{})
	go func() {
		hub.Publish(userID, JobEvent{ID: "j1", State: "ready"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked with no subscribers")
	}

	ch3, snapshot, cancel3, ok3 := hub.Subscribe(userID)
	if !ok3 {
		t.Fatal("resubscribe refused")
	}
	defer cancel3() // idempotent; the explicit call below frees the slot first
	if len(snapshot) != 1 || snapshot[0].State != "ready" {
		t.Fatalf("snapshot after resubscribe: %+v", snapshot)
	}
	// The pending flush was torn down with the last stream: no stale event may
	// surface on the new stream once the old window would have expired.
	select {
	case ev := <-ch3:
		t.Fatalf("stale flush after cancel delivered %+v", ev)
	case <-time.After(1200 * time.Millisecond):
	}
	cancel3()
	if hub.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after resubscribe cancel", hub.Len())
	}
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before %d, after %d", before, runtime.NumGoroutine())
}

func TestHubCapsSubscribers(t *testing.T) {
	hub := NewEventHub()
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	sub := func(userID string) bool {
		_, _, cancel, ok := hub.Subscribe(userID)
		if ok {
			cancels = append(cancels, cancel)
		}
		return ok
	}
	if !sub("u-a") || !sub("u-a") {
		t.Fatal("per-user streams below cap were refused")
	}
	if sub("u-a") {
		t.Fatal("third concurrent stream for one user was accepted")
	}
	for i := 0; i < 98; i++ {
		if !sub(fmt.Sprintf("u-%02d", i)) {
			t.Fatalf("stream %d refused before the total cap", len(cancels))
		}
	}
	if hub.Len() != 100 {
		t.Fatalf("Len = %d, want 100", hub.Len())
	}
	if sub("u-over") {
		t.Fatal("101st stream was accepted")
	}
	if hub.Len() != 100 {
		t.Fatalf("refused subscribe changed Len to %d", hub.Len())
	}
	cancels[0]()
	cancels = cancels[1:]
	if !sub("u-over") {
		t.Fatal("subscribe refused after a slot freed")
	}
}

func drainUntilClosed(t *testing.T, ch <-chan JobEvent) {
	t.Helper()
	for i := 0; i < streamBuffer*2; i++ {
		if _, open := <-ch; !open {
			return
		}
	}
	t.Fatal("channel never closed")
}
