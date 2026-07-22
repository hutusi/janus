package runner

import (
	"context"
	"errors"
	"testing"
)

// enterMember enters a member backed by a real cancellable context, returning
// the member and its context (whose Cause records any supersede).
func enterMember(t *testing.T, g *groupReg, key, id string, cancelInProgress bool) (*groupMember, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return g.enter(key, id, cancel, cancelInProgress), ctx
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
	m, _ := enterMember(t, g, "k", "a", false)
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
	a, _ := enterMember(t, g, "k", "a", false)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, bctx := enterMember(t, g, "k", "b", false)
	if isReady(b) {
		t.Fatal("b should wait behind running a")
	}
	c, cctx := enterMember(t, g, "k", "c", false)
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
	a, actx := enterMember(t, g, "k", "a", true)
	if !g.claim("k", a, context.Background()) {
		t.Fatal("claim a")
	}
	b, _ := enterMember(t, g, "k", "b", true)
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
	m := g.enter("k", "a", cancel, false)
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
	a, actx := enterMember(t, g, "k1", "a", true)
	b, bctx := enterMember(t, g, "k2", "b", true)
	if !isReady(a) || !isReady(b) {
		t.Fatal("members of different groups must not queue behind each other")
	}
	if context.Cause(actx) != nil || context.Cause(bctx) != nil {
		t.Fatal("members of different groups must not supersede each other")
	}
}
