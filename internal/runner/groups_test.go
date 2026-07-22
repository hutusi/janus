package runner

import (
	"context"
	"errors"
	"testing"
)

// enterMember enters a member backed by a real cancellable context, returning
// the member and its context (whose Cause records any supersede).
func enterMember(t *testing.T, g *groupReg, key string, seq uint64, id string, cancelInProgress bool) (*groupMember, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return g.enter(key, seq, id, cancel, cancelInProgress), ctx
}

func isReady(m *groupMember) bool {
	select {
	case <-m.ready:
		return true
	default:
		return false
	}
}

func TestGroupFirstMemberRunsImmediately(t *testing.T) {
	g := newGroupReg()
	m, _ := enterMember(t, g, "k", 1, "a", false)
	if !isReady(m) {
		t.Fatal("first member of an empty group should be ready immediately")
	}
	if !g.claim("k", m, context.Background()) {
		t.Fatal("claim should succeed for the sole ready member")
	}
	g.leave("k", m)
	if len(g.groups) != 0 {
		t.Fatalf("registry should be empty after the last leave, has %d entries", len(g.groups))
	}
}

func TestGroupSupersedesWaiter(t *testing.T) {
	g := newGroupReg()
	a, _ := enterMember(t, g, "k", 1, "a", false)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", 2, "b", false)
	if isReady(b) {
		t.Fatal("b should wait behind running a")
	}
	c, cctx := enterMember(t, g, "k", 3, "c", false)
	if cause := context.Cause(bctx); cause == nil || cause.Error() != "superseded by run c" {
		t.Fatalf("b's cancel cause = %v, want 'superseded by run c'", cause)
	}
	if g.claim("k", b, bctx) {
		t.Fatal("a superseded member must not claim")
	}
	g.leave("k", b) // stale leave: must not disturb c

	// Without cancel-in-progress the running member was untouched; when it
	// leaves, the current waiter is released and claims.
	g.leave("k", a)
	if !isReady(c) {
		t.Fatal("c should be ready once a leaves")
	}
	if !g.claim("k", c, cctx) {
		t.Fatal("claim c")
	}
	g.leave("k", c)
	if len(g.groups) != 0 {
		t.Fatalf("registry should be empty, has %d entries", len(g.groups))
	}
}

func TestGroupCancelInProgressCancelsRunning(t *testing.T) {
	g := newGroupReg()
	a, actx := enterMember(t, g, "k", 1, "a", true)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, _ := enterMember(t, g, "k", 2, "b", true)
	if cause := context.Cause(actx); cause == nil || cause.Error() != "superseded by run b" {
		t.Fatalf("a's cancel cause = %v, want 'superseded by run b'", cause)
	}
	// "At most 1 running" holds through the handover: b only becomes ready
	// when the cancelled a has fully unwound (its leave).
	if isReady(b) {
		t.Fatal("b must wait until a leaves")
	}
	g.leave("k", a)
	if !isReady(b) {
		t.Fatal("b should be ready after a leaves")
	}
}

func TestGroupClaimFailsOnCancelledContext(t *testing.T) {
	g := newGroupReg()
	ctx, cancel := context.WithCancelCause(context.Background())
	m := g.enter("k", 1, "a", cancel, false)
	cancel(errors.New("cancelled via API"))
	if g.claim("k", m, ctx) {
		t.Fatal("claim must fail once the member's context is cancelled")
	}
	g.leave("k", m)
	if len(g.groups) != 0 {
		t.Fatalf("registry should be empty, has %d entries", len(g.groups))
	}
}

func TestGroupKeysAreIndependent(t *testing.T) {
	g := newGroupReg()
	a, actx := enterMember(t, g, "k1", 1, "a", true)
	b, bctx := enterMember(t, g, "k2", 2, "b", true)
	if !isReady(a) || !isReady(b) {
		t.Fatal("members of different groups must not queue behind each other")
	}
	if context.Cause(actx) != nil || context.Cause(bctx) != nil {
		t.Fatal("members of different groups must not supersede each other")
	}
}

// --- Trigger-order guarantees -------------------------------------------------
//
// Members enter only after checkout+parse, and checkouts finish in any order,
// so an OLDER trigger can arrive at the registry LAST. These tests pin that
// "newer" means trigger order (seq), never enter-arrival order.

func TestGroupStaleArrivalCannotSupersedeRunning(t *testing.T) {
	for _, cip := range []bool{false, true} {
		name := "queue"
		if cip {
			name = "cancel-in-progress"
		}
		t.Run(name, func(t *testing.T) {
			g := newGroupReg()
			// The newer trigger's checkout finished first: seq 2 is running.
			newer, newerCtx := enterMember(t, g, "k", 2, "newer", cip)
			if !g.claim("k", newer, newerCtx) {
				t.Fatal("claim newer")
			}
			// The older trigger's checkout finishes late: it must be refused,
			// not allowed to supersede (or, with cancel-in-progress, kill) the
			// newer run and then execute a stale commit.
			stale, staleCtx := enterMember(t, g, "k", 1, "stale", cip)
			if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run newer" {
				t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run newer'", cause)
			}
			if cause := context.Cause(newerCtx); cause != nil {
				t.Fatalf("running newer member was cancelled by a stale arrival: %v", cause)
			}
			if isReady(stale) {
				t.Fatal("a refused stale arrival must never become ready")
			}
			g.leave("k", stale) // unregistered: must be a no-op
			g.leave("k", newer)
			if len(g.groups) != 0 {
				t.Fatalf("registry should be empty, has %d entries", len(g.groups))
			}
		})
	}
}

func TestGroupStaleArrivalCannotSupersedeWaiter(t *testing.T) {
	g := newGroupReg()
	a, _ := enterMember(t, g, "k", 3, "a", false)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", 4, "b", false)
	// An older trigger arrives after both: the waiter must be untouched.
	_, staleCtx := enterMember(t, g, "k", 2, "stale", false)
	if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run b" {
		t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run b'", cause)
	}
	if cause := context.Cause(bctx); cause != nil {
		t.Fatalf("waiter b was disturbed by a stale arrival: %v", cause)
	}
	g.leave("k", a)
	if !isReady(b) {
		t.Fatal("b should be released when a leaves")
	}
}

func TestGroupStaleArrivalAfterGroupEmptied(t *testing.T) {
	g := newGroupReg()
	// The newer run enters, runs, and finishes entirely before the older
	// trigger's checkout completes; its group entry is deleted.
	newer, _ := enterMember(t, g, "k", 2, "newer", false)
	if !g.claim("k", newer, context.Background()) {
		t.Fatal("claim newer")
	}
	g.leave("k", newer)
	if len(g.groups) != 0 {
		t.Fatal("group entry should be gone")
	}
	// The high-water mark survives the entry: the stale arrival is still
	// refused rather than executing an older commit after a newer one.
	_, staleCtx := enterMember(t, g, "k", 1, "stale", false)
	if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run newer" {
		t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run newer'", cause)
	}
	// A genuinely newer trigger is admitted normally.
	next, nextCtx := enterMember(t, g, "k", 3, "next", false)
	if context.Cause(nextCtx) != nil {
		t.Fatal("a newer arrival must not be refused")
	}
	if !isReady(next) {
		t.Fatal("next should be ready in the empty group")
	}
}

// --- High-water mark retirement -----------------------------------------------
//
// Marks exist to refuse stale arrivals, and a stale arrival is by definition
// an admitted older trigger that has not yet entered. Once no such trigger is
// in flight, a mark is garbage — endTrigger must reclaim it, or the registry
// grows with every branch the daemon ever sees.

func TestGroupMarkRetainedWhileOlderTriggerInflight(t *testing.T) {
	g := newGroupReg()
	t1 := g.beginTrigger() // older trigger, checkout still in flight
	t2 := g.beginTrigger()

	newer, _ := enterMember(t, g, "k", t2, "newer", false)
	if !g.claim("k", newer, context.Background()) {
		t.Fatal("claim newer")
	}
	g.leave("k", newer)
	g.endTrigger(t2)

	// t1 could still arrive stale, so the sweep must keep the mark.
	if len(g.seen) != 1 {
		t.Fatalf("seen has %d entries, want the mark retained while t1 is in flight", len(g.seen))
	}
	_, staleCtx := enterMember(t, g, "k", t1, "stale", false)
	if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run newer" {
		t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run newer'", cause)
	}
	g.endTrigger(t1)
	if len(g.seen) != 0 {
		t.Fatalf("seen has %d entries after all triggers ended, want 0", len(g.seen))
	}
}

func TestGroupMarksClearedWhenNoTriggersInflight(t *testing.T) {
	g := newGroupReg()
	t1 := g.beginTrigger()
	m, _ := enterMember(t, g, "k", t1, "a", false)
	if !g.claim("k", m, context.Background()) {
		t.Fatal("claim")
	}
	g.leave("k", m)
	if len(g.seen) != 1 {
		t.Fatal("mark should exist while its own trigger is in flight")
	}
	g.endTrigger(t1)
	if len(g.seen) != 0 || len(g.groups) != 0 || len(g.inflight) != 0 {
		t.Fatalf("registry not empty after the only trigger ended: seen=%d groups=%d inflight=%d",
			len(g.seen), len(g.groups), len(g.inflight))
	}
	// A later trigger on the same key is admitted normally.
	t2 := g.beginTrigger()
	next, nextCtx := enterMember(t, g, "k", t2, "b", false)
	if context.Cause(nextCtx) != nil || !isReady(next) {
		t.Fatal("a fresh trigger on a swept key should be admitted")
	}
}

func TestGroupSweepSparesNewerMarks(t *testing.T) {
	g := newGroupReg()
	t1 := g.beginTrigger()
	t2 := g.beginTrigger()
	t3 := g.beginTrigger()

	a, _ := enterMember(t, g, "k1", t1, "a", false)
	g.claim("k1", a, context.Background())
	g.leave("k1", a)
	c, _ := enterMember(t, g, "k3", t3, "c", false)
	g.claim("k3", c, context.Background())
	g.leave("k3", c)

	// t1 ends; the oldest in-flight trigger is now t2, so k1's mark (below
	// t2) is swept while k3's (above t2) survives — selective, not clear-all.
	g.endTrigger(t1)
	if _, ok := g.seen["k1"]; ok {
		t.Error("k1's mark should be swept: no in-flight trigger can be stale against it")
	}
	if _, ok := g.seen["k3"]; !ok {
		t.Error("k3's mark must survive: t2 is in flight and older than it")
	}
	g.endTrigger(t2)
	g.endTrigger(t3)
	if len(g.seen) != 0 {
		t.Fatalf("seen has %d entries after all triggers ended, want 0", len(g.seen))
	}
}
