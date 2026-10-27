package cork

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// restarted is another corkd started on m's database, as a restart would
// leave it.
func restarted(t *testing.T, m *Manager) *Manager {
	t.Helper()
	r := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: m.workerTiming, db: m.db}
	if err := r.initWorkers(); err != nil {
		t.Fatalf("initWorkers after the restart: %s", err)
	}
	t.Cleanup(func() {
		r.workersMu.Lock()
		defer r.workersMu.Unlock()
		for _, w := range r.workers {
			close(w.done)
		}
	})
	return r
}

func storedDownSince(t *testing.T, m *Manager, ip string) sql.NullInt64 {
	t.Helper()
	var downSince sql.NullInt64
	if err := m.db.Get(&downSince, "SELECT downsince FROM workers WHERE ip = ?;", ip); err != nil {
		t.Fatalf("reading back downsince of %s: %s", ip, err)
	}
	return downSince
}

// Down is the one state a restart cannot re-derive from a probe, so it is the
// one that survives it: the worker comes back down, as of when it was taken
// down, and out of placement. An ejection is an observation and does not.
func TestWorkerDownSurvivesARestart(t *testing.T) {
	const downIP, ejectedIP = "127.0.0.1", "127.0.0.2"
	m := addWorkerTestManager(t)
	for _, ip := range []string{downIP, ejectedIP} {
		if err := m.AddWorker(ip, ""); err != nil {
			t.Fatalf("worker-add %s: %s", ip, err)
		}
	}
	if err := m.SetWorkerDown(downIP); err != nil {
		t.Fatalf("worker-down: %s", err)
	}
	m.workersMu.RLock()
	since := m.workers[downIP].since.Load()
	m.eject(m.workers[ejectedIP], "its daemon did not answer")
	m.workersMu.RUnlock()

	r := restarted(t, m)
	down := r.workers[downIP]
	if got := reachableOf(down); got != workerDown {
		t.Fatalf("reachable %s after a restart, want down: the restart returned a downed worker to placement", got)
	}
	if got := down.since.Load(); got != since {
		t.Errorf("since %d after a restart, want %d, when the operator took it down", got, since)
	}
	if reason := down.reason.Load(); reason == nil || *reason != downReason {
		t.Errorf("reason %v after a restart, want %q", reason, downReason)
	}
	if got := reachableOf(r.workers[ejectedIP]); got == workerDown {
		t.Fatal("an ejected worker came back down: an observation was persisted as an assertion, and no probe will ever lift it")
	}
}

// Marked before the write: a worker-down that cannot persist still takes the
// worker out of placement for the life of this corkd, and its error says so.
func TestWorkerDownThatCannotPersistStillTakesTheWorkerDown(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background()}
	w := testWorkerConn(workerReachableOk)
	m.workers = map[string]*workerConn{w.ip: w}
	m.db = workerDB(t, w.ip)
	m.db.Close()

	err := m.SetWorkerDown(w.ip)
	if err == nil {
		t.Fatal("worker-down reported success on a database it could not write")
	}
	if got := reachableOf(w); got != workerDown {
		t.Fatalf("reachable %s after a worker-down whose write failed, want down", got)
	}
	if !strings.Contains(err.Error(), "is down") {
		t.Errorf("error %q does not say the worker is down, so worker-down reads as having done nothing", err)
	}
}

// worker-add lifts the persisted assertion along with the one in memory, or
// the next restart takes the worker back out of placement.
func TestWorkerAddLiftsAPersistedDown(t *testing.T) {
	const ip = "127.0.0.1"
	m := addWorkerTestManager(t)
	if err := m.AddWorker(ip, ""); err != nil {
		t.Fatalf("worker-add: %s", err)
	}
	if err := m.SetWorkerDown(ip); err != nil {
		t.Fatalf("worker-down: %s", err)
	}
	if !storedDownSince(t, m, ip).Valid {
		t.Fatal("worker-down was not persisted")
	}

	if err := m.AddWorker(ip, ""); err != nil {
		t.Fatalf("worker-add after worker-down: %s", err)
	}
	if storedDownSince(t, m, ip).Valid {
		t.Fatal("worker-add left worker-down persisted, so the next restart takes the worker out of placement again")
	}
	if got := reachableOf(restarted(t, m).workers[ip]); got == workerDown {
		t.Fatal("a worker brought back with worker-add is down again after a restart")
	}
}
