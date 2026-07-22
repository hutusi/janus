package runner

import (
	"context"
	"errors"
	"testing"
)

// enterMember enters a member backed by a real cancellable context, returning
// the member and its context (whose Cause records any supersede). Seqs must
// come from beginTrigger — enter resolves the trigger inline, and a literal
// seq absent from inflight would make every sweep run against the wrong set.
func enterMember(t *testing.T, g *groupReg, key string, seq uint64, id string, cancelInProgress bool) (*groupMember, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return g.enter(ctx, key, seq, id, cancel, cancelInProgress), ctx
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
	t1 := g.beginTrigger()
	m, _ := enterMember(t, g, "k", t1, "a", false)
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
	t1, t2, t3 := g.beginTrigger(), g.beginTrigger(), g.beginTrigger()
	a, _ := enterMember(t, g, "k", t1, "a", false)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", t2, "b", false)
	if isReady(b) {
		t.Fatal("b should wait behind running a")
	}
	c, cctx := enterMember(t, g, "k", t3, "c", false)
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
	if len(g.groups) != 0 || len(g.seen) != 0 || len(g.inflight) != 0 {
		t.Fatalf("registry should be empty: groups=%d seen=%d inflight=%d", len(g.groups), len(g.seen), len(g.inflight))
	}
}

func TestGroupCancelInProgressCancelsRunning(t *testing.T) {
	g := newGroupReg()
	t1, t2 := g.beginTrigger(), g.beginTrigger()
	a, actx := enterMember(t, g, "k", t1, "a", true)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, _ := enterMember(t, g, "k", t2, "b", true)
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
	t1 := g.beginTrigger()
	ctx, cancel := context.WithCancelCause(context.Background())
	m := g.enter(ctx, "k", t1, "a", cancel, false)
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
	t1, t2 := g.beginTrigger(), g.beginTrigger()
	a, actx := enterMember(t, g, "k1", t1, "a", true)
	b, bctx := enterMember(t, g, "k2", t2, "b", true)
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

// TestGroupStaleArrivalCannotSupersedeRunning is ALSO the regression guard for
// enter's gate-before-retire ordering: the stale trigger here is the only
// remaining unresolved seq, so its own enrollment is all that keeps the mark
// alive — if enter retired the seq before the gate check, the sweep would drop
// the mark inside that very call and admit the stale run.
func TestGroupStaleArrivalCannotSupersedeRunning(t *testing.T) {
	for _, cip := range []bool{false, true} {
		name := "queue"
		if cip {
			name = "cancel-in-progress"
		}
		t.Run(name, func(t *testing.T) {
			g := newGroupReg()
			tStale, tNewer := g.beginTrigger(), g.beginTrigger()
			// The newer trigger's checkout finished first: it is running.
			newer, newerCtx := enterMember(t, g, "k", tNewer, "newer", cip)
			if !g.claim("k", newer, newerCtx) {
				t.Fatal("claim newer")
			}
			// The older trigger's checkout finishes late: it must be refused,
			// not allowed to supersede (or, with cancel-in-progress, kill) the
			// newer run and then execute a stale commit.
			stale, staleCtx := enterMember(t, g, "k", tStale, "stale", cip)
			if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run newer" {
				t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run newer'", cause)
			}
			if cause := context.Cause(newerCtx); cause != nil {
				t.Fatalf("running newer member was cancelled by a stale arrival: %v", cause)
			}
			if isReady(stale) {
				t.Fatal("a refused stale arrival must never become ready")
			}
			// The refusal resolved the last unresolved trigger, so the whole
			// accounting drains — while the newer member still runs.
			if len(g.inflight) != 0 || len(g.seen) != 0 {
				t.Fatalf("inflight=%d seen=%d after all triggers resolved, want 0/0", len(g.inflight), len(g.seen))
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
	tStale, tA, tB := g.beginTrigger(), g.beginTrigger(), g.beginTrigger()
	a, _ := enterMember(t, g, "k", tA, "a", false)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", tB, "b", false)
	// The oldest trigger arrives after both: the waiter must be untouched.
	_, staleCtx := enterMember(t, g, "k", tStale, "stale", false)
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
	tStale, tNewer := g.beginTrigger(), g.beginTrigger()
	// The newer run enters, runs, and finishes entirely before the older
	// trigger's checkout completes; its group entry is deleted.
	newer, _ := enterMember(t, g, "k", tNewer, "newer", false)
	if !g.claim("k", newer, context.Background()) {
		t.Fatal("claim newer")
	}
	g.leave("k", newer)
	if len(g.groups) != 0 {
		t.Fatal("group entry should be gone")
	}
	// The high-water mark outlives the entry exactly as long as the older
	// trigger stays unresolved: the stale arrival is still refused rather
	// than executing an older commit after a newer one.
	if len(g.seen) != 1 {
		t.Fatalf("seen has %d entries, want the mark retained while the stale trigger is unresolved", len(g.seen))
	}
	_, staleCtx := enterMember(t, g, "k", tStale, "stale", false)
	if cause := context.Cause(staleCtx); cause == nil || cause.Error() != "superseded by run newer" {
		t.Fatalf("stale arrival's cancel cause = %v, want 'superseded by run newer'", cause)
	}
	// A genuinely newer trigger is admitted normally.
	tNext := g.beginTrigger()
	next, nextCtx := enterMember(t, g, "k", tNext, "next", false)
	if context.Cause(nextCtx) != nil {
		t.Fatal("a newer arrival must not be refused")
	}
	if !isReady(next) {
		t.Fatal("next should be ready in the empty group")
	}
}

// --- Trigger resolution & mark retirement -------------------------------------
//
// A trigger can be a stale arrival only until it RESOLVES — enters its one
// group, turns out ungrouped, or fails pre-enter. Resolution retires its seq
// and sweeps the marks; only unresolved (pre-enter, deadline-bounded) triggers
// keep marks alive, so a hung run can never pin the registry's memory.

func TestGroupEnterResolvesTrigger(t *testing.T) {
	g := newGroupReg()
	t1 := g.beginTrigger()
	m, _ := enterMember(t, g, "k", t1, "a", false)
	// Admission itself resolved the trigger: no explicit endTrigger, and with
	// no older unresolved trigger left, even its own fresh mark is dropped —
	// while the member still occupies the group entry.
	if len(g.inflight) != 0 || len(g.seen) != 0 {
		t.Fatalf("inflight=%d seen=%d right after enter, want 0/0", len(g.inflight), len(g.seen))
	}
	if !g.claim("k", m, context.Background()) {
		t.Fatal("claim")
	}
	g.leave("k", m)
	// The run-scope safety net double-retires: must be a harmless no-op.
	g.endTrigger(t1)
	if len(g.groups) != 0 || len(g.seen) != 0 || len(g.inflight) != 0 {
		t.Fatalf("registry not empty: groups=%d seen=%d inflight=%d", len(g.groups), len(g.seen), len(g.inflight))
	}
	// A later trigger on the same key is admitted normally.
	t2 := g.beginTrigger()
	next, nextCtx := enterMember(t, g, "k", t2, "b", false)
	if context.Cause(nextCtx) != nil || !isReady(next) {
		t.Fatal("a fresh trigger after full drain should be admitted")
	}
}

func TestGroupUngroupedResolutionReleasesMarks(t *testing.T) {
	g := newGroupReg()
	tOld, tNew := g.beginTrigger(), g.beginTrigger()
	newer, _ := enterMember(t, g, "k", tNew, "newer", false)
	if !g.claim("k", newer, context.Background()) {
		t.Fatal("claim newer")
	}
	if len(g.seen) != 1 {
		t.Fatal("mark should be pinned by the older unresolved trigger")
	}
	// The older trigger turns out ungrouped (or fails pre-enter): it can
	// never be a stale arrival, so its resolution releases the mark — even
	// though the newer run is still executing.
	g.endTrigger(tOld)
	if len(g.seen) != 0 || len(g.inflight) != 0 {
		t.Fatalf("seen=%d inflight=%d after the older trigger resolved, want 0/0", len(g.seen), len(g.inflight))
	}
}

func TestGroupSweepSparesNewerMarks(t *testing.T) {
	g := newGroupReg()
	t1, t2, t3 := g.beginTrigger(), g.beginTrigger(), g.beginTrigger()

	// t1's own admission resolves it, leaving {t2, t3} unresolved — both
	// newer than t1 — so k1's mark (seq t1) is dropped by the sweep inside
	// t1's own enter: no unresolved trigger can be stale against it.
	a, _ := enterMember(t, g, "k1", t1, "a", false)
	g.claim("k1", a, context.Background())
	if _, ok := g.seen["k1"]; ok {
		t.Error("k1's mark should be swept inside its own enter: every unresolved trigger is newer")
	}
	g.leave("k1", a)

	// t3's mark survives its own enter: t2 is unresolved and older.
	c, _ := enterMember(t, g, "k3", t3, "c", false)
	g.claim("k3", c, context.Background())
	if _, ok := g.seen["k3"]; !ok {
		t.Error("k3's mark must survive: t2 is unresolved and older than it")
	}
	g.leave("k3", c)

	// t2 resolves → nothing unresolved remains → all marks drain.
	g.endTrigger(t2)
	if len(g.seen) != 0 || len(g.inflight) != 0 {
		t.Fatalf("seen=%d inflight=%d after all triggers resolved, want 0/0", len(g.seen), len(g.inflight))
	}
}

func TestGroupEnterWithCancelledContextTouchesNobody(t *testing.T) {
	g := newGroupReg()
	tA, tB, tC := g.beginTrigger(), g.beginTrigger(), g.beginTrigger()
	a, actx := enterMember(t, g, "k", tA, "a", false)
	if !g.claim("k", a, actx) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", tB, "b", false)

	// c is cancelled (e.g. via the API) after checkout but before entering:
	// a doomed arrival must not supersede the waiter or kill the running
	// member on its way out — even with cancel-in-progress.
	cctx, ccancel := context.WithCancelCause(context.Background())
	ccancel(errors.New("cancelled via API"))
	c := g.enter(cctx, "k", tC, "c", ccancel, true)
	if cause := context.Cause(actx); cause != nil {
		t.Fatalf("running a was disturbed by a doomed arrival: %v", cause)
	}
	// b's ctx is only cancelled by its test cleanup later, so any cause here
	// means the doomed arrival superseded it.
	if cause := context.Cause(bctx); cause != nil {
		t.Fatalf("waiter b was disturbed by a doomed arrival: %v", cause)
	}
	if isReady(c) {
		t.Fatal("a doomed arrival must never become ready")
	}
	if len(g.inflight) != 0 {
		t.Fatalf("inflight=%d, the doomed arrival should have resolved", len(g.inflight))
	}
	g.leave("k", c) // unregistered: no-op
	g.leave("k", a)
	if !isReady(b) {
		t.Fatal("b should be released when a leaves")
	}
}
