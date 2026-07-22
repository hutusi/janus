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
	// seen records the newest member ever admitted per key, refusing stale
	// arrivals even after the group has emptied. A mark only matters while an
	// UNRESOLVED older trigger could still reach enter, so retirement sweeps
	// drop marks below the oldest unresolved seq — memory is bounded by the
	// admission window and, in time, by the pre-enter phase (a checkout
	// window at most), never by run duration or by how many branches (groups)
	// the daemon has ever seen.
	seen map[string]groupNewest

	// Trigger-order accounting. inflight holds UNRESOLVED triggers: admitted
	// (beginTrigger) but not yet arrived at their fate — entering a group,
	// turning out ungrouped, or failing pre-enter. Only unresolved triggers
	// can still call enter, so this set is exactly the set of possible future
	// arrivals. nextSeq is handed out and enrolled in inflight under the same
	// mu acquisition (beginTrigger): if numbering and enrollment were two
	// steps, a sweep between them could observe an empty inflight set and
	// drop a mark the not-yet-enrolled older trigger still needs. Bounded by
	// the admission cap, and each entry lives only through the deadline-
	// bounded pre-enter phase.
	nextSeq  uint64
	inflight map[uint64]struct{}
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
		groups:   make(map[string]*groupEntry),
		seen:     make(map[string]groupNewest),
		inflight: make(map[uint64]struct{}),
	}
}

// beginTrigger stamps a new trigger with the next sequence number and enrolls
// it as unresolved. Called from the synchronous part of Trigger, so the
// numbering reflects when triggers arrived — not when their checkouts finish.
// Every trigger must be enrolled (its group is unknown until parse) and is
// resolved exactly once: enter resolves grouped triggers inline; ungrouped and
// pre-enter-failure paths resolve via endTrigger. Retirement is idempotent, so
// a run-scope deferred endTrigger doubles as the safety net for panics and
// early exits.
func (g *groupReg) beginTrigger() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextSeq++
	g.inflight[g.nextSeq] = struct{}{}
	return g.nextSeq
}

// endTrigger resolves a trigger that never entered a group (ungrouped,
// pre-enter failure, or the run-scope safety net). Idempotent.
func (g *groupReg) endTrigger(seq uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retireLocked(seq)
}

// retireLocked (mu held) retires seq and drops every high-water mark no
// unresolved trigger can be stale against (mark seq below the oldest
// unresolved seq; all marks when none remains). A mark may be dropped while
// its group entry still has live members — harmless: the sweep condition
// means every possible future enter carries a larger seq than anything live
// in that entry, so the entry-level supersede logic alone orders them
// correctly.
func (g *groupReg) retireLocked(seq uint64) {
	delete(g.inflight, seq)
	var oldest uint64
	for s := range g.inflight {
		if oldest == 0 || s < oldest {
			oldest = s
		}
	}
	for key, newest := range g.seen {
		if oldest == 0 || newest.seq < oldest {
			delete(g.seen, key)
		}
	}
}

// enter registers runID as the group's sole waiting member, and RESOLVES the
// trigger: whichever branch is taken, this trigger will never call enter
// again, so its seq retires here rather than when the (unboundedly long) run
// finishes — otherwise one hung run would pin every newer mark in memory.
//
// A run already cancelled on arrival (ctx done — e.g. an API cancel landed
// between checkout and here) is retired without touching anyone: a doomed
// arrival must not supersede, or with cancelInProgress kill, innocent
// members on its way out.
//
// An arrival older (by seq) than the newest ever admitted is stale — its
// checkout simply finished late — and is cancelled on the spot with cause
// "superseded by run <newest>", never registered (leave on it no-ops). ORDER
// IS CORRECTNESS-CRITICAL: this gate must run BEFORE retireLocked. The stale
// arrival's own unresolved seq is exactly what has kept the refusing mark
// alive (min(inflight) ≤ its seq < the mark's seq); retiring first would let
// the sweep drop the mark inside this very call and admit the stale run —
// the trigger-order bug the mark exists to prevent.
//
// Past those gates the arrival is strictly the newest, so any current waiter
// is superseded — cancelled with cause "superseded by run <runID>" — and
// with cancelInProgress the running member is cancelled the same way. When
// nothing is running, the returned member's ready channel is already closed.
func (g *groupReg) enter(ctx context.Context, key string, seq uint64, runID string, cancel context.CancelCauseFunc, cancelInProgress bool) *groupMember {
	m := &groupMember{seq: seq, runID: runID, cancel: cancel, ready: make(chan struct{})}
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx.Err() != nil {
		// No seen update either: this run never acted, so an older trigger
		// still unresolved must not later be refused in its name.
		g.retireLocked(seq)
		return m
	}
	if newest := g.seen[key]; seq < newest.seq {
		m.cancel(fmt.Errorf("superseded by run %s", newest.runID))
		g.retireLocked(seq)
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
	g.retireLocked(seq)
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
