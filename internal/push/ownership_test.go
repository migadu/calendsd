package push

import (
	"errors"
	"testing"
	"time"
)

// A re-register of an endpoint by a different principal must not take the
// subscription (and its keys) over (audit 6.13).
func TestUpsertRefusesAnotherPrincipalsEndpoint(t *testing.T) {
	db := openScratchDB(t, "calserver_push_owner")
	store := &Store{DB: db}
	expires := time.Now().Add(time.Hour)
	mine := Subscription{Token: "t1", Kind: "calendar", CollectionID: "1", CollectionPath: "calendars/alice/home",
		PrincipalURI: "principals/alice", PushResource: "https://push.example/ep", P256dh: "A", AuthSecret: "A"}
	if _, err := store.Upsert(mine, 0, expires); err != nil {
		t.Fatal(err)
	}
	theirs := mine
	theirs.Token, theirs.PrincipalURI, theirs.P256dh, theirs.AuthSecret = "t2", "principals/bob", "B", "B"
	if _, err := store.Upsert(theirs, 0, expires); !errors.Is(err, ErrEndpointOwnedByOther) {
		t.Fatalf("hijack attempt: err = %v, want ErrEndpointOwnedByOther", err)
	}
	subs, _ := store.ByCollection("calendar", "1")
	if len(subs) != 1 || subs[0].PrincipalURI != "principals/alice" || subs[0].P256dh != "A" {
		t.Errorf("subscription changed by the refused upsert: %+v", subs)
	}
	// The owner's own re-register still refreshes it.
	mine.P256dh = "A2"
	if tok, err := store.Upsert(mine, 0, expires); err != nil || tok != "t1" {
		t.Errorf("owner re-register: tok=%q err=%v", tok, err)
	}
}
