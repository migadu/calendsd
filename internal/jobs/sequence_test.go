package jobs

import (
	"context"
	"encoding/json"
	"testing"
)

// The coalesce predicate used to cast args->>'sequence' to int, so a
// SEQUENCE past 2^31-1 errored the enqueue; the caller then fell back to a
// synchronous send and the stale pending job delivered later (audit 5.6).
func TestCoalesceAcceptsLargeSequence(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_bigseq")
	q := newTestQueue(t, db)
	ctx := context.Background()
	for _, seq := range []string{"2147483648", "2147483649"} {
		if err := q.Enqueue(ctx, "imip_send",
			imipArgs("u-big", "mailto:big@x.test", "REQUEST", seq, "20260101T000000Z")); err != nil {
			t.Fatalf("enqueue SEQUENCE %s: %v", seq, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM jobs WHERE args->>'uid' = 'u-big'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows for the UID, want 1 (second enqueue supersedes)", n)
	}
	r := getJob(t, db, lastJobID(t, db))
	var args map[string]any
	if err := json.Unmarshal(r.args, &args); err != nil {
		t.Fatal(err)
	}
	if args["sequence"] != "2147483649" {
		t.Errorf("stored sequence = %v, want the newer 2147483649", args["sequence"])
	}
}
