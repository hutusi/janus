package runner

import (
	"context"
	"fmt"
	"sync"
)

// groupReg tracks, per concurrency group, the single member allowed to run and
// the single member allowed to wait — the invariant behind the `concurrency:`
// key's "at most 1 running + 1 pending per group". Keys are repo-scoped by the
// caller (repo URL + NUL + expanded group), so identical group strings in
// different repos never meet. Entries exist only while a member is active;
// leave deletes empty ones, so the map never grows without bound.
//
// A member moves enter (waiting) → ready closed → claim (running) → leave. It
// holds no run-slot (Runner.sem) while waiting — the group gate is ordered
// before the global semaphore, so a queued group member cannot starve
// unrelated repos. All cancellation goes through the members' per-run cancel
// funcs, which the waits and the execution already select on.
//
// "Newer" means TRIGGER order (the seq assigned synchronously in Trigger),
// not arrival order at enter: members enter only after their checkout+parse,
// and checkout durations vary, so an older trigger can reach the registry
// last. The seen high-water mark refuses such stale arrivals — even when the
// newer run has already finished and left — so an out-of-order checkout can
// never make a stale commit supersede or outlive a newer one.
type groupReg struct {
	mu     sync.Mutex
	groups map[string]*groupEntry
	// seen records the newest member ever admitted per key. It deliberately
	// never shrinks (precedent: Runner.locks) — a few dozen bytes per distinct
	// (repo, group) pair is the price of refusing stale arrivals after the
	// group has emptied.
	seen map[string]groupNewest
}

// groupNewest identifies the newest member ever admitted to a group.
type groupNewest struct {
	seq   uint64
	runID string
}

type groupEntry struct {
	running *groupMember
	pending *groupMember
}

type groupMember struct {
	seq    uint64 // trigger order, assigned synchronously in Trigger
	runID  string
	cancel context.CancelCauseFunc
	ready  chan struct{} // closed when the running slot frees up for this member
}

func newGroupReg() *groupReg {
	return &groupReg{
		groups: make(map[string]*groupEntry),
		seen:   make(map[string]groupNewest),
	}
}

// enter registers runID as the group's sole waiting member. An arrival older
// (by seq) than the newest ever admitted is stale — its checkout simply
// finished late — and is cancelled on the spot with cause "superseded by run
// <newest>", never registered (leave on it no-ops). Past that gate the
// arrival is strictly the newest, so any current waiter is superseded —
// cancelled with cause "superseded by run <runID>" — and with
// cancelInProgress the running member is cancelled the same way. When nothing
// is running, the returned member's ready channel is already closed.
func (g *groupReg) enter(key string, seq uint64, runID string, cancel context.CancelCauseFunc, cancelInProgress bool) *groupMember {
	m := &groupMember{seq: seq, runID: runID, cancel: cancel, ready: make(chan struct{})}
	g.mu.Lock()
	defer g.mu.Unlock()
	if newest := g.seen[key]; seq < newest.seq {
		m.cancel(fmt.Errorf("superseded by run %s", newest.runID))
		return m
	}
	g.seen[key] = groupNewest{seq: seq, runID: runID}
	e := g.groups[key]
	if e == nil {
		e = &groupEntry{}
		g.groups[key] = e
	}
	if e.pending != nil {
		e.pending.cancel(fmt.Errorf("superseded by run %s", runID))
	}
	e.pending = m
	if e.running == nil {
		close(m.ready)
	} else if cancelInProgress {
		e.running.cancel(fmt.Errorf("superseded by run %s", runID))
	}
	return m
}

// claim promotes m from waiting to running; the caller must hold a run slot
// and have seen m.ready close. It returns false when m is no longer the
// group's waiter (superseded between the slot acquire and here) or ctx is
// already cancelled — the caller must then unwind as cancelled.
func (g *groupReg) claim(key string, m *groupMember, ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.groups[key]
	if e == nil || e.pending != m || ctx.Err() != nil {
		return false
	}
	// ready is only ever closed with the running slot free, and only claim
	// fills it, so m can take it unconditionally here.
	e.pending = nil
	e.running = m
	return true
}

// leave removes m from whichever slot it occupies (a no-op for a superseded
// member, which was already displaced). Releasing the running slot wakes the
// current waiter; ready closes only here or at enter, exactly once per member.
func (g *groupReg) leave(key string, m *groupMember) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.groups[key]
	if e == nil {
		return
	}
	switch m {
	case e.running:
		e.running = nil
		if e.pending != nil {
			close(e.pending.ready)
		}
	case e.pending:
		e.pending = nil
	}
	if e.running == nil && e.pending == nil {
		delete(g.groups, key)
	}
}
