package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService is a service whose health and start result the test controls.
type fakeService struct {
	up       atomic.Bool
	starts   atomic.Int32
	failNext atomic.Int32 // number of upcoming starts that fail
}

func (f *fakeService) start() error {
	f.starts.Add(1)
	if f.failNext.Load() > 0 {
		f.failNext.Add(-1)
		return errors.New("boom")
	}
	f.up.Store(true)
	return nil
}
func (f *fakeService) stop()         { f.up.Store(false) }
func (f *fakeService) healthy() bool { return f.up.Load() }

func testSupervisor(t *testing.T) (*supervisor, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	s := newSupervisor(func(m string) { mu.Lock(); logs = append(logs, m); mu.Unlock() }, func(string) {})
	s.tick = 10 * time.Millisecond
	s.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	go s.Run()
	t.Cleanup(func() { s.once.Do(func() { close(s.done) }) })
	return s, &logs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fakeService) managed(name string, needs ...string) *managedService {
	return &managedService{name: name, label: name, needs: needs, start: f.start, stop: f.stop, healthy: f.healthy}
}

func TestSupervisorStartsInDependencyOrder(t *testing.T) {
	s, _ := testSupervisor(t)
	var a, b fakeService
	s.add(a.managed("a"))
	s.add(b.managed("b", "a"))
	s.Want("b", true)
	s.Want("a", false) // b needs a, but only b is wanted: b must wait and never start
	time.Sleep(100 * time.Millisecond)
	if b.starts.Load() != 0 {
		t.Fatal("b started before its dependency was running")
	}
	s.Want("a", true)
	eventually(t, "both running", func() bool { return s.State("a") == svcRunning && s.State("b") == svcRunning })
}

func TestSupervisorRestartsAServiceThatStopsAnswering(t *testing.T) {
	s, _ := testSupervisor(t)
	var a fakeService
	s.add(a.managed("a"))
	s.Want("a", true)
	eventually(t, "running", func() bool { return s.State("a") == svcRunning })
	a.up.Store(false) // crash
	eventually(t, "restarted", func() bool { return a.starts.Load() >= 2 && s.State("a") == svcRunning })
	if s.Status()[0].Restarts < 1 {
		t.Fatal("restart was not counted")
	}
}

func TestSupervisorRetriesFailedStartsThenGivesUp(t *testing.T) {
	s, _ := testSupervisor(t)
	var a fakeService
	a.failNext.Store(2)
	s.add(a.managed("a"))
	s.Want("a", true)
	eventually(t, "running after two failures", func() bool { return s.State("a") == svcRunning })

	var b fakeService
	b.failNext.Store(100)
	s.add(b.managed("b"))
	s.Want("b", true)
	eventually(t, "given up", func() bool { return s.State("b") == svcFailed })
	attempts := b.starts.Load()
	time.Sleep(80 * time.Millisecond)
	if b.starts.Load() != attempts {
		t.Fatal("kept retrying after giving up")
	}
	b.failNext.Store(0)
	s.Want("b", true) // the user retries
	eventually(t, "recovered by retry", func() bool { return s.State("b") == svcRunning })
}

func TestSupervisorRestartsDependentsWhenANeedRestarts(t *testing.T) {
	s, _ := testSupervisor(t)
	var a, b fakeService
	s.add(a.managed("a"))
	s.add(b.managed("b", "a"))
	s.Want("a", true)
	s.Want("b", true)
	eventually(t, "both running", func() bool { return s.State("b") == svcRunning })
	before := b.starts.Load()
	a.up.Store(false)
	eventually(t, "dependent started again", func() bool { return b.starts.Load() > before && s.State("b") == svcRunning && s.State("a") == svcRunning })
}

func TestSupervisorAdoptsAServiceThatIsAlreadyUp(t *testing.T) {
	s, _ := testSupervisor(t)
	var a fakeService
	a.up.Store(true)
	s.add(a.managed("a"))
	s.Want("a", true)
	eventually(t, "adopted", func() bool { return s.State("a") == svcRunning })
	if a.starts.Load() != 0 {
		t.Fatal("started a service that was already healthy")
	}
}

func TestSupervisorStopsWhenNoLongerWanted(t *testing.T) {
	s, _ := testSupervisor(t)
	var a fakeService
	s.add(a.managed("a"))
	s.Want("a", true)
	eventually(t, "running", func() bool { return s.State("a") == svcRunning })
	s.Want("a", false)
	eventually(t, "stopped", func() bool { return s.State("a") == svcStopped && !a.up.Load() })
}

// The agent status is derived only from the services, so every reader sees the same answer.
func TestAgentStatusIsDerivedFromServices(t *testing.T) {
	s := newSupervisor(func(string) {}, func(string) {})
	for _, name := range []string{svcModels, svcGateway} {
		s.add(&managedService{name: name, label: name, start: func() error { return nil }, stop: func() {}, healthy: func() bool { return true }})
	}
	s.add(&managedService{name: svcAssistant, label: "assistant", needs: []string{svcGateway, svcModels}, start: func() error { return nil }, stop: func() {}, healthy: func() bool { return true }})
	if got := s.Agent().State; got != "asleep" {
		t.Fatalf("nothing wanted: %s", got)
	}
	s.byName[svcGateway].wanted = true
	if got := s.Agent().State; got != "waking" {
		t.Fatalf("wanted but not up: %s", got)
	}
	for _, name := range []string{svcModels, svcGateway, svcAssistant} {
		s.byName[name].state, s.byName[name].generation = svcRunning, 1
	}
	if a := s.Agent(); a.State != "awake" || !a.Awake {
		t.Fatalf("all running: %+v", a)
	}
	s.byName[svcGateway].state = svcBackoff
	if got := s.Agent().State; got != "recovering" {
		t.Fatalf("one restarting: %s", got)
	}
	s.byName[svcGateway].state, s.byName[svcGateway].lastErr = svcFailed, "port in use"
	if a := s.Agent(); a.State != "failed" || a.Detail == "" {
		t.Fatalf("failed: %+v", a)
	}
}
