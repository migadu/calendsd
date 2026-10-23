package push

// Postgres-backed tests for the audit 3.3 hardening: the per-principal
// subscription cap and the bounded, per-collection-independent dispatch.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"calendsd/internal/storage"
)

func TestUpsertEnforcesPerPrincipalCap(t *testing.T) {
	db := openScratchDB(t, "calserver_push_cap")
	store := &Store{DB: db, MaxPerPrincipal: 2}
	expires := time.Now().Add(time.Hour)
	sub := func(n int) Subscription {
		return Subscription{
			Token: fmt.Sprintf("tok%d", n), Kind: "calendar", CollectionID: "1",
			CollectionPath: "calendars/alice/home", PrincipalURI: "principals/alice",
			PushResource: fmt.Sprintf("https://push.example/%d", n), P256dh: "PUB", AuthSecret: "AUTH",
		}
	}
	for n := 1; n <= 2; n++ {
		if _, err := store.Upsert(sub(n), 0, expires); err != nil {
			t.Fatalf("subscription %d: %v", n, err)
		}
	}
	if _, err := store.Upsert(sub(3), 0, expires); !errors.Is(err, ErrTooManySubscriptions) {
		t.Fatalf("third subscription: err = %v, want ErrTooManySubscriptions", err)
	}
	// Re-registering an existing endpoint is not a new subscription.
	if _, err := store.Upsert(sub(2), 0, expires); err != nil {
		t.Fatalf("re-register at the cap: %v", err)
	}
	// Another principal has its own budget.
	other := sub(4)
	other.PrincipalURI = "principals/bob"
	if _, err := store.Upsert(other, 0, expires); err != nil {
		t.Fatalf("other principal: %v", err)
	}
	// An expired row no longer counts.
	if _, err := db.Exec(`UPDATE push_subscriptions SET expires = now() - interval '1 day' WHERE token = 'tok1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(sub(3), 0, expires); err != nil {
		t.Fatalf("after one expired: %v", err)
	}
}

// stallingSender blocks on one endpoint until its context ends and answers
// every other endpoint immediately.
type stallingSender struct {
	stall string
	mu    sync.Mutex
	done  map[string]time.Time
}

func (s *stallingSender) Send(ctx context.Context, sub Subscription, _ string, _ []byte) (int, error) {
	if sub.PushResource == s.stall {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	s.mu.Lock()
	s.done[sub.PushResource] = time.Now()
	s.mu.Unlock()
	return 201, nil
}

// One subscriber that never answers must not delay another collection's
// notification, and must not hold its own collection past the send timeout.
func TestSlowEndpointDoesNotStallOthers(t *testing.T) {
	db := openScratchDB(t, "calserver_push_stall")
	backend := &storage.CalDAV{DB: db}
	store := &Store{DB: db}
	var keys []string
	for _, name := range []string{"slow", "fast"} {
		id, err := backend.CreateCalendar(t.Context(), "principals/alice", name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.CreateCalendarObject(t.Context(), id, "e.ics", []byte(sampleICS)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Upsert(Subscription{
			Token: "tok-" + name, Kind: "calendar", CollectionID: id.Calendar,
			CollectionPath: "calendars/alice/" + name, PrincipalURI: "principals/alice",
			PushResource: "https://push.example/" + name, P256dh: "PUB", AuthSecret: "AUTH",
		}, 0, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, "calendar:"+id.Calendar)
	}

	sender := &stallingSender{stall: "https://push.example/slow", done: map[string]time.Time{}}
	// The stalled send lives for sendTimeout; the fast one must land well
	// inside that, which proves it did not queue behind the stall.
	const stallFor = 2 * time.Second
	w := &Worker{cfg: Config{TopicSecret: "s"}, store: store, sender: sender,
		checker: okChecker{}, log: slog.Default(), sendTimeout: stallFor}
	ctx := context.Background()

	started := time.Now()
	w.dispatchAsync(ctx, keys[0]) // slow collection first
	w.dispatchAsync(ctx, keys[1])

	deadline := started.Add(stallFor)
	for {
		sender.mu.Lock()
		_, fastDone := sender.done["https://push.example/fast"]
		sender.mu.Unlock()
		if fastDone || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sender.mu.Lock()
	at, fastDone := sender.done["https://push.example/fast"]
	sender.mu.Unlock()
	if !fastDone {
		t.Fatal("fast collection was never delivered while the slow one stalled")
	}
	if took := at.Sub(started); took > stallFor/2 {
		t.Errorf("fast collection waited %v behind the stalled one; want independent delivery", took)
	}

	// The stalled send is cut off by the per-send timeout and its collection's
	// dispatch finishes, freeing the key for the next change.
	deadline = time.Now().Add(stallFor + 2*time.Second)
	for {
		w.mu.Lock()
		inflight := len(w.inflight)
		w.mu.Unlock()
		if inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d dispatches still in flight after the send timeout; want 0", inflight)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The fast subscription recorded its delivery; the slow one did not.
	subs, _ := store.AllLive()
	for _, s := range subs {
		switch {
		case strings.HasSuffix(s.PushResource, "/fast") && s.LastSyncToken == 0:
			t.Error("fast subscription's last token not recorded")
		case strings.HasSuffix(s.PushResource, "/slow") && s.LastSyncToken != 0:
			t.Error("stalled subscription must not be marked delivered")
		}
	}
}

// A second change to a collection that is mid-dispatch is delivered after the
// first run finishes, not lost and not run concurrently.
func TestDispatchAsyncRedispatchesPendingKey(t *testing.T) {
	db := openScratchDB(t, "calserver_push_redo")
	backend := &storage.CalDAV{DB: db}
	store := &Store{DB: db}
	id, err := backend.CreateCalendar(t.Context(), "principals/alice", "home", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateCalendarObject(t.Context(), id, "e.ics", []byte(sampleICS)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(Subscription{
		Token: "t", Kind: "calendar", CollectionID: id.Calendar,
		CollectionPath: "calendars/alice/home", PrincipalURI: "principals/alice",
		PushResource: "https://push.example/a", P256dh: "PUB", AuthSecret: "AUTH",
	}, 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	sender := &gatedSender{gate: gate, entered: make(chan struct{})}
	w := &Worker{cfg: Config{TopicSecret: "s"}, store: store, sender: sender,
		checker: okChecker{}, log: slog.Default()}
	key := "calendar:" + id.Calendar
	ctx := context.Background()

	w.dispatchAsync(ctx, key)
	select {
	case <-sender.entered: // first run has read the token and is blocked in Send
	case <-time.After(5 * time.Second):
		t.Fatal("first dispatch never reached Send")
	}
	if _, err := backend.CreateCalendarObject(t.Context(), id, "f.ics", []byte(sampleICS)); err != nil {
		t.Fatal(err)
	}
	w.dispatchAsync(ctx, key) // must be queued, not run concurrently
	close(gate)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.inflight)
		w.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sender.calls(); got != 2 {
		t.Fatalf("sends = %d, want 2 (one per change, run back to back)", got)
	}
	cur, _, _ := store.CurrentToken("calendar", id.Calendar)
	subs, _ := store.ByCollection("calendar", id.Calendar)
	if len(subs) != 1 || subs[0].LastSyncToken != cur {
		t.Errorf("last token = %d, want current %d", subs[0].LastSyncToken, cur)
	}
}

// gatedSender blocks the first Send until gate closes (signalling entered
// when it gets there); later sends pass.
type gatedSender struct {
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	n       int
}

func (g *gatedSender) Send(ctx context.Context, _ Subscription, _ string, _ []byte) (int, error) {
	g.mu.Lock()
	g.n++
	first := g.n == 1
	g.mu.Unlock()
	if first {
		close(g.entered)
		select {
		case <-g.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 201, nil
}

func (g *gatedSender) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}
