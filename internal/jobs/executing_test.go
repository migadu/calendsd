package jobs

import (
	"context"
	"errors"
	"testing"
)

// A job a worker is running must not be re-queued from calctl: a retry
// would send it twice (audit 6.6).
func TestMutationsRefuseExecutingJob(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_executing")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}
	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if _, err := q.store.claim(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(context.Context, int64) error{
		"Retry": ops.Retry, "RelayOverride": ops.RelayOverride,
	} {
		if err := fn(t.Context(), id); !errors.Is(err, ErrJobExecuting) {
			t.Errorf("%s on an executing job: err = %v, want ErrJobExecuting", name, err)
		}
	}
	if r := getJob(t, db, id); r.state != "executing" || !r.credSet {
		t.Errorf("executing job was touched: state=%q cred=%v", r.state, r.credSet)
	}
	// Cancel is the exception: it may stop a running job, and the worker's
	// settle cannot resurrect it (stale-claim check in store).
	if err := ops.Cancel(t.Context(), id); err != nil {
		t.Errorf("Cancel on an executing job: %v", err)
	}
	if r := getJob(t, db, id); r.state != "discarded" || r.credSet {
		t.Errorf("after Cancel: state=%q cred=%v, want discarded without credential", r.state, r.credSet)
	}
}
