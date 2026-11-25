package cork

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// managerWithWorkers builds a Manager holding the given workers in the order
// listed, which is also the round-robin order. Nothing here touches docker.
func managerWithWorkers(t *testing.T, workers ...*workerConn) *Manager {
	t.Helper()
	m := &Manager{log: newLogger(DISABLED)}
	m.workers = make(map[string]*workerConn, len(workers))
	for _, w := range workers {
		if _, dup := m.workers[w.ip]; dup {
			t.Fatalf("duplicate worker ip in fixture: %s", w.ip)
		}
		m.workers[w.ip] = w
		m.workerOrder = append(m.workerOrder, w.ip)
	}
	return m
}

// workerAt builds a worker fixture on both health axes. The three shorthands
// below cover what most placement tests care about.
func workerAt(ip string, r workerReachable, l workerLoad) *workerConn {
	w := &workerConn{ip: ip}
	w.reachable.Store(int32(r))
	w.load.Store(int32(l))
	return w
}

func okWorker(ip string) *workerConn { return workerAt(ip, workerReachableOk, workerLoadOk) }

// overloadedWorker is reachable -- its daemon answers, its telemetry says the
// box is over its high-water mark.
func overloadedWorker(ip string) *workerConn {
	return workerAt(ip, workerReachableOk, workerOverloaded)
}

// unresponsiveWorker had its daemon stop answering; its last known load is
// irrelevant, since reachability alone keeps it out of placement.
func unresponsiveWorker(ip string) *workerConn {
	return workerAt(ip, workerUnresponsive, workerLoadOk)
}

func downWorker(ip string) *workerConn { return workerAt(ip, workerDown, workerLoadOk) }

// No workers configured is not a failure: it means place the instance on the
// local daemon, which the caller reads as an empty ip with no error.
func TestSelectWorkerWithNoneConfigured(t *testing.T) {
	m := managerWithWorkers(t)

	ip, err := m.selectWorker()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "" {
		t.Fatalf("got worker %q, want \"\" for local placement", ip)
	}
}

// Placement goes round the healthy workers in order and wraps, so a fleet
// shares the load instead of stacking every instance on the first box.
func TestSelectWorkerRoundRobins(t *testing.T) {
	m := managerWithWorkers(t,
		okWorker("10.0.0.1"),
		okWorker("10.0.0.2"),
		okWorker("10.0.0.3"),
	)

	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.1", "10.0.0.2"}
	for i, expect := range want {
		got, err := m.selectWorker()
		if err != nil {
			t.Fatalf("placement %d: unexpected error: %v", i, err)
		}
		if got != expect {
			t.Fatalf("placement %d: got %s, want %s", i, got, expect)
		}
	}
}

// The cursor advances past what it skipped, not just past what it took, so a
// fleet with one healthy worker among unhealthy ones does not rescan the same
// dead prefix on every placement.
func TestSelectWorkerSkipsUnhealthy(t *testing.T) {
	m := managerWithWorkers(t,
		downWorker("10.0.0.1"),
		overloadedWorker("10.0.0.2"),
		unresponsiveWorker("10.0.0.4"),
		okWorker("10.0.0.3"),
	)

	for i := 0; i < 3; i++ {
		got, err := m.selectWorker()
		if err != nil {
			t.Fatalf("placement %d: unexpected error: %v", i, err)
		}
		if got != "10.0.0.3" {
			t.Fatalf("placement %d: got %s, want the one healthy worker 10.0.0.3", i, got)
		}
	}
}

// The three exhaustion errors mean different things to the caller. Overloaded
// and unresponsive are both states a worker leaves on its own, so they are
// retryable (503); every worker having been taken down by an operator is not
// going to resolve itself (500). See errorResponse in cmd/corkd, which maps
// exactly this distinction.
func TestSelectWorkerExhaustionErrors(t *testing.T) {
	tests := []struct {
		name    string
		workers []*workerConn
		want    error
	}{
		{
			name:    "every worker overloaded",
			workers: []*workerConn{overloadedWorker("10.0.0.1"), overloadedWorker("10.0.0.2")},
			want:    ErrAllWorkersOverloaded,
		},
		{
			name:    "every worker unresponsive",
			workers: []*workerConn{unresponsiveWorker("10.0.0.1"), unresponsiveWorker("10.0.0.2")},
			want:    ErrAllWorkersUnresponsive,
		},
		{
			name:    "every worker taken down",
			workers: []*workerConn{downWorker("10.0.0.1"), downWorker("10.0.0.2")},
			want:    ErrAllWorkersDown,
		},
		{
			// Unresponsive is a state a worker leaves on its own, so one
			// among the taken-down keeps the placement retryable.
			name:    "one unresponsive among the taken-down",
			workers: []*workerConn{downWorker("10.0.0.1"), unresponsiveWorker("10.0.0.2")},
			want:    ErrAllWorkersUnresponsive,
		},
		{
			// One overloaded among the dead is enough to make the whole
			// placement retryable: that box may come back on its own, where a
			// fleet that is entirely down needs someone to intervene.
			name:    "one overloaded among the down",
			workers: []*workerConn{downWorker("10.0.0.1"), overloadedWorker("10.0.0.2")},
			want:    ErrAllWorkersOverloaded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := managerWithWorkers(t, tc.workers...)

			ip, err := m.selectWorker()
			if ip != "" {
				t.Fatalf("placed on %s despite no eligible worker", ip)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// The scan starts at the cursor and wraps, so one healthy worker anywhere in
// the order is always found however far round it sits. A fleet only refuses a
// placement when nothing is eligible.
func TestSelectWorkerWrapsToReachAHealthyWorker(t *testing.T) {
	first := okWorker("10.0.0.1")
	m := managerWithWorkers(t, first, overloadedWorker("10.0.0.2"))

	// Leaves the cursor on the overloaded worker.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("first placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}

	// The scan passes the overloaded worker, wraps, and comes back to the
	// healthy one rather than reporting the fleet exhausted.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("second placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}
}

// A refused placement must not move the cursor, or a fleet riding out a spell
// where everything is overloaded would walk the order while nothing is being
// placed, and resume somewhere arbitrary once it recovers.
func TestSelectWorkerCursorSurvivesExhaustion(t *testing.T) {
	one, two := okWorker("10.0.0.1"), okWorker("10.0.0.2")
	m := managerWithWorkers(t, one, two)

	// Takes the first worker and leaves the cursor on the second.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("first placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}

	one.load.Store(int32(workerOverloaded))
	two.load.Store(int32(workerOverloaded))
	for i := 0; i < 3; i++ {
		if _, err := m.selectWorker(); !errors.Is(err, ErrAllWorkersOverloaded) {
			t.Fatalf("refused placement %d: got %v, want ErrAllWorkersOverloaded", i, err)
		}
	}

	// Recovered: the cursor is where the last successful placement left it, so
	// the second worker is next rather than the first one over again.
	one.load.Store(int32(workerLoadOk))
	two.load.Store(int32(workerLoadOk))
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.2" {
		t.Fatalf("after recovery: got (%q, %v), want (10.0.0.2, nil)", got, err)
	}
}

// The address handed to a player: the configured public one, else the private
// ip, and "" for a worker that has been removed since the instance was placed.
func TestWorkerPublicAddr(t *testing.T) {
	withPublic := okWorker("10.0.0.1")
	withPublic.public = "ctf.example.org"
	m := managerWithWorkers(t, withPublic, okWorker("10.0.0.2"))

	tests := []struct{ ip, want string }{
		{"10.0.0.1", "ctf.example.org"},
		{"10.0.0.2", "10.0.0.2"},
		{"10.0.0.9", ""},
	}
	for _, tc := range tests {
		if got := m.workerPublicAddr(tc.ip); got != tc.want {
			t.Fatalf("workerPublicAddr(%s): got %q, want %q", tc.ip, got, tc.want)
		}
	}
}

// Every read that hands out instances reports the player-facing address under
// "hostname" and "worker_public": the platform resolves {{server}} from
// whichever read it stored, the launch response or the schema state.
func TestInstanceReadsReportThePublicAddress(t *testing.T) {
	m := setupTestManager(t)
	t.Cleanup(func() { m.db.Close() })
	named, bare := okWorker("10.0.0.1"), okWorker("10.0.0.2")
	named.public = "ctf.example.org"
	m.workers = map[string]*workerConn{named.ip: named, bare.ip: bare}
	m.workerOrder = []string{named.ip, bare.ip}

	challenge := testChallenge("test/public-address", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	build := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)

	// The address each instance's player is handed: the worker's public name,
	// else its ip, and none at all on the local daemon.
	want := map[InstanceId]string{}
	for worker, addr := range map[string]string{named.ip: named.public, bare.ip: bare.ip, "": ""} {
		res, err := m.db.Exec("INSERT INTO instances(build, lastsolved, is_finalized, worker) VALUES (?, 0, 1, ?);", build, worker)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		want[InstanceId(id)] = addr
	}

	inState := func(state []*ChallengeMetadata) []*InstanceMetadata {
		var instances []*InstanceMetadata
		for _, c := range state {
			for _, b := range c.Builds {
				instances = append(instances, b.Instances...)
			}
		}
		return instances
	}
	reads := []struct {
		name string
		read func() ([]*InstanceMetadata, error)
	}{
		// GET /instances/<id>, and the launch response
		{"instance", func() ([]*InstanceMetadata, error) {
			var instances []*InstanceMetadata
			for id := range want {
				meta, err := m.GetInstanceMetadata(id)
				if err != nil {
					return nil, err
				}
				instances = append(instances, meta)
			}
			return instances, nil
		}},
		// GET /schemas/<name>
		{"schema-state", func() ([]*InstanceMetadata, error) {
			state, err := m.GetSchemaState("event")
			return inState(state), err
		}},
		// GET /state, and through dumpChallenge the hand-over response
		{"state-dump", func() ([]*InstanceMetadata, error) {
			state, err := m.DumpState(nil)
			return inState(state), err
		}},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			instances, err := r.read()
			if err != nil {
				t.Fatal(err)
			}
			if len(instances) != len(want) {
				t.Fatalf("read %d instances, want %d", len(instances), len(want))
			}
			for _, meta := range instances {
				body, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				var out map[string]any
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatal(err)
				}
				addr := want[meta.Id]
				// "worker" stays the private address that placement and teardown dial.
				if worker, _ := out["worker"].(string); worker != meta.Worker {
					t.Errorf("instance %d: worker is %q, want the private address %q", meta.Id, worker, meta.Worker)
				}
				// Under the exact names each consumer reads.
				for _, key := range []string{"hostname", "worker_public"} {
					got, present := out[key]
					if addr == "" && present {
						t.Errorf("instance %d on the local daemon reported %s %v", meta.Id, key, got)
					}
					if addr != "" && got != addr {
						t.Errorf("instance %d on worker %q: %s is %v, want %q", meta.Id, meta.Worker, key, got, addr)
					}
				}
			}
		})
	}
}

// A local (single-host) instance has no worker, so neither address appears --
// omitempty keeps both keys out rather than reporting an empty hostname a
// consumer might template into a description.
func TestHostnameAbsentWithoutAWorker(t *testing.T) {
	body, err := json.Marshal(&InstanceMetadata{Id: 1})
	if err != nil {
		t.Fatalf("marshalling the instance: %s", err)
	}
	for _, key := range []string{"hostname", "worker_public", "worker"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("a workerless instance reported %q: %s", key, body)
		}
	}
}
