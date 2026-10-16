package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The supervisor is the app's process manager. Every long-running part (model servers,
// speech server, speech-recognition files, the assistant's agent loop) is a
// managed service with:
//
//	start    blocks until the service is healthy, or returns why it could not be
//	stop     tears it down
//	healthy  a cheap probe, run every tick
//	needs    services that must be running first; if one of them restarts, so does this one
//
// A service the user wants running is kept running: it is started when its needs are up,
// restarted with exponential backoff if it stops answering, and reported as failed (with the
// reason) after repeated quick crashes until the user retries. Services start concurrently
// unless one needs another, so waking the assistant is not slower than it has to be.
type svcState string

const (
	svcStopped  svcState = "stopped"
	svcStarting svcState = "starting"
	svcRunning  svcState = "running"
	svcBackoff  svcState = "restarting"
	svcStopping svcState = "stopping"
	svcFailed   svcState = "failed"
)

const (
	healthMisses      = 2                // consecutive failed probes before a service is declared down
	stableAfter       = 60 * time.Second // running this long clears the crash counter
	maxQuickFailures  = 5                // start failures or quick crashes before giving up
	superviseInterval = 2 * time.Second
)

type managedService struct {
	name    string
	label   string
	needs   []string
	start   func() error
	stop    func()
	healthy func() bool

	// guarded by supervisor.mu
	wanted     bool
	state      svcState
	since      time.Time
	restarts   int
	failures   int
	misses     int
	lastErr    string
	retryAt    time.Time
	generation int            // increments each time the service becomes running
	needGen    map[string]int // generation of each need when this service last started
	busy       bool           // a start/stop/probe for this service is in flight
	restart    bool           // restart it once, now, without waiting for it to fail
}

// serviceStatus is what the dashboard and /api/state show.
type serviceStatus struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	State    svcState `json:"state"`
	Wanted   bool     `json:"wanted"`
	Restarts int      `json:"restarts"`
	Error    string   `json:"error,omitempty"`
	Needs    []string `json:"needs,omitempty"`
}

type supervisor struct {
	mu      sync.Mutex
	list    []*managedService
	byName  map[string]*managedService
	logf    func(string)
	setErr  func(string) // surface a failure to the user; "" clears it
	backoff func(failures int) time.Duration
	tick    time.Duration
	poke    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newSupervisor(logf func(string), setErr func(string)) *supervisor {
	return &supervisor{
		byName: map[string]*managedService{},
		logf:   logf,
		setErr: setErr,
		backoff: func(failures int) time.Duration {
			d := time.Second << uint(min(failures, 6))
			return min(d, time.Minute)
		},
		tick: superviseInterval,
		poke: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// add registers a service. Services must be added in dependency order (needs first).
func (s *supervisor) add(svc *managedService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc.state = svcStopped
	svc.needGen = map[string]int{}
	for _, need := range svc.needs {
		if s.byName[need] == nil {
			panic("supervisor: " + svc.name + " needs unknown service " + need)
		}
	}
	s.list = append(s.list, svc)
	s.byName[svc.name] = svc
}

// Run supervises until Shutdown. Call once, in its own goroutine.
func (s *supervisor) Run() {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		s.reconcile()
		select {
		case <-s.done:
			return
		case <-ticker.C:
		case <-s.poke:
		}
	}
}

func (s *supervisor) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Want says whether a service should be running. Asking for it again after it failed retries it.
// Restart asks a running service to be stopped and started again, once. Nothing happens to a service
// that is not running, and a service nobody wants stays off.
func (s *supervisor) Restart(name string) {
	s.mu.Lock()
	if svc := s.byName[name]; svc != nil && svc.state == svcRunning {
		svc.restart = true
	}
	s.mu.Unlock()
	s.wake()
}

// RestartAndWait restarts a wanted, running service and waits for the replacement generation
// to become healthy. If the service is not running, its next normal start will use new settings.
func (s *supervisor) RestartAndWait(name string, timeout time.Duration) error {
	s.mu.Lock()
	svc := s.byName[name]
	if svc == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown service %q", name)
	}
	if !svc.wanted {
		s.mu.Unlock()
		return nil
	}
	generation := svc.generation
	wasStarting := svc.state == svcStarting
	if svc.state == svcRunning {
		svc.restart = true
	}
	s.mu.Unlock()
	s.wake()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		state, currentGeneration, lastErr := svc.state, svc.generation, svc.lastErr
		wanted := svc.wanted
		s.mu.Unlock()
		if currentGeneration > generation && state == svcRunning {
			if !wasStarting {
				return nil
			}
			// If startup had already begun when a setting changed, restart once more so
			// a model path captured just before the change cannot remain active.
			s.mu.Lock()
			if svc.wanted && svc.state == svcRunning {
				generation = svc.generation
				svc.restart = true
				s.mu.Unlock()
				s.wake()
				wasStarting = false
				continue
			}
			s.mu.Unlock()
			return nil
		}
		if !wanted {
			return fmt.Errorf("%s stopped during restart", svc.label)
		}
		if state == svcFailed {
			return fmt.Errorf("%s could not restart: %s", svc.label, lastErr)
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s to restart", svc.label)
		case <-ticker.C:
		}
	}
}

func (s *supervisor) takeRestart(svc *managedService) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := svc.restart
	svc.restart = false
	return was
}

func (s *supervisor) Want(name string, want bool) {
	s.mu.Lock()
	svc := s.byName[name]
	if svc != nil {
		svc.wanted = want
		if want {
			svc.failures = 0
			svc.retryAt = time.Time{}
			if svc.state == svcFailed {
				svc.state = svcStopped
			}
		}
	}
	s.mu.Unlock()
	s.wake()
}

// Status lists the services in start order.
func (s *supervisor) Status() []serviceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]serviceStatus, 0, len(s.list))
	for _, svc := range s.list {
		out = append(out, serviceStatus{svc.name, svc.label, svc.state, svc.wanted, svc.restarts, svc.lastErr, svc.needs})
	}
	return out
}

// State reports one service's state (stopped if unknown).
func (s *supervisor) State(name string) svcState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.byName[name]; svc != nil {
		return svc.state
	}
	return svcStopped
}

// agentStatus is the one authoritative answer to "what is the assistant doing?". Every tab,
// the voice bridge and the diagnostics read this and nothing else.
type agentStatus struct {
	State  string `json:"state"`            // asleep | waking | awake | recovering | failed
	Label  string `json:"label"`            // short, for the status badge
	Detail string `json:"detail,omitempty"` // what is happening or what went wrong
	Awake  bool   `json:"awake"`            // ready to talk right now
	Wanted bool   `json:"wanted"`           // the user asked for it to be awake
}

// Agent derives the assistant's state from the services' states.
func (s *supervisor) Agent() agentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	core := []string{svcAgent, svcModels}
	agent := s.byName[svcAgent]
	if agent == nil || !agent.wanted {
		if agent != nil && agent.state == svcStopping {
			return agentStatus{State: "going-to-sleep", Label: "Going to sleep"}
		}
		return agentStatus{State: "asleep", Label: "Assistant is asleep"}
	}
	allRunning, recovering := true, false
	for _, name := range core {
		svc := s.byName[name]
		switch svc.state {
		case svcFailed:
			return agentStatus{State: "failed", Label: "Needs attention", Detail: svc.label + ": " + svc.lastErr, Wanted: true}
		case svcRunning:
		case svcBackoff:
			allRunning, recovering = false, true
		default:
			allRunning = false
			if svc.generation > 0 {
				recovering = true
			}
		}
	}
	switch {
	case allRunning:
		return agentStatus{State: "awake", Label: "Assistant is awake", Awake: true, Wanted: true}
	case recovering:
		return agentStatus{State: "recovering", Label: "Recovering", Detail: "Something stopped responding; restarting it", Wanted: true}
	default:
		return agentStatus{State: "waking", Label: "Waking up", Wanted: true}
	}
}

// Shutdown stops every service, last started first, and ends Run.
func (s *supervisor) Shutdown() {
	s.once.Do(func() { close(s.done) })
	s.mu.Lock()
	for _, svc := range s.list {
		svc.wanted = false
	}
	list := append([]*managedService(nil), s.list...)
	s.mu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		list[i].stop()
	}
}

// reconcile moves every service one step toward what is wanted. Services are handled in
// parallel (a slow start must not hold up the others); each is handled by at most one goroutine.
func (s *supervisor) reconcile() {
	s.mu.Lock()
	list := append([]*managedService(nil), s.list...)
	s.mu.Unlock()
	for _, svc := range list {
		s.mu.Lock()
		if svc.busy {
			s.mu.Unlock()
			continue
		}
		svc.busy = true
		s.mu.Unlock()
		go func(svc *managedService) {
			defer func() {
				s.mu.Lock()
				svc.busy = false
				s.mu.Unlock()
			}()
			s.step(svc)
		}(svc)
	}
}

func (s *supervisor) needsRunning(svc *managedService) (ok bool, changed bool) {
	ok = true
	for _, need := range svc.needs {
		n := s.byName[need]
		if n.state != svcRunning {
			ok = false
		} else if svc.state == svcRunning && n.generation != svc.needGen[need] {
			changed = true
		}
	}
	return ok, changed
}

func (s *supervisor) step(svc *managedService) {
	s.mu.Lock()
	wanted, state := svc.wanted, svc.state
	needsOK, needsChanged := s.needsRunning(svc)
	retryAt := svc.retryAt
	s.mu.Unlock()

	switch {
	case !wanted && (state == svcRunning || state == svcStarting || state == svcBackoff):
		s.stopService(svc, "")
	case !wanted:
		// nothing to do
	case state == svcFailed:
		// stays failed until Want(true) is asked again
	case state == svcRunning && (!needsOK || needsChanged):
		s.stopService(svc, svc.label+" is restarting because something it depends on restarted")
	case state == svcRunning && s.takeRestart(svc):
		s.stopService(svc, "") // it is started again on the next pass, because it is still wanted
	case state == svcRunning:
		s.probe(svc)
	case !needsOK:
		// waiting for what this service depends on
	case time.Now().Before(retryAt):
		// backing off
	default:
		if svc.healthy() { // already up (started by setup, or left by a previous run): adopt it
			s.markRunning(svc, false)
			return
		}
		s.startService(svc)
	}
}

func (s *supervisor) markRunning(svc *managedService, restarted bool) {
	s.mu.Lock()
	svc.state = svcRunning
	svc.since = time.Now()
	svc.misses = 0
	svc.lastErr = ""
	svc.generation++
	for _, need := range svc.needs {
		svc.needGen[need] = s.byName[need].generation
	}
	if restarted {
		svc.restarts++
	}
	s.mu.Unlock()
	s.setErr("")
	s.wake() // dependents may be waiting on this
}

func (s *supervisor) startService(svc *managedService) {
	s.mu.Lock()
	again := svc.generation > 0
	svc.state = svcStarting
	s.mu.Unlock()
	if again {
		s.logf("Restarting " + svc.label)
	}
	err := svc.start()
	if err == nil {
		s.markRunning(svc, again)
		return
	}
	s.fail(svc, "could not start", err)
}

// fail records a start failure or a crash and schedules the next attempt, or gives up.
func (s *supervisor) fail(svc *managedService, what string, err error) {
	s.mu.Lock()
	svc.failures++
	svc.lastErr = err.Error()
	failures := svc.failures
	if failures >= maxQuickFailures {
		svc.state = svcFailed
	} else {
		svc.state = svcBackoff
		svc.retryAt = time.Now().Add(s.backoff(failures))
	}
	state := svc.state
	s.mu.Unlock()
	if state == svcFailed {
		message := fmt.Sprintf("%s %s after %d attempts: %v", svc.label, what, failures, err)
		s.logf(message)
		s.setErr(message)
		return
	}
	s.logf(fmt.Sprintf("%s %s (%v); trying again", svc.label, what, err))
}

func (s *supervisor) probe(svc *managedService) {
	ok := svc.healthy()
	s.mu.Lock()
	if ok {
		svc.misses = 0
		if svc.failures > 0 && time.Since(svc.since) > stableAfter {
			svc.failures = 0
		}
		s.mu.Unlock()
		return
	}
	svc.misses++
	down := svc.misses >= healthMisses
	quick := time.Since(svc.since) < stableAfter
	s.mu.Unlock()
	if !down {
		return
	}
	svc.stop() // clear whatever is left of it before starting afresh
	s.mu.Lock()
	if !quick {
		svc.failures = 0
	}
	s.mu.Unlock()
	s.fail(svc, "stopped responding", fmt.Errorf("it stopped answering"))
}

func (s *supervisor) stopService(svc *managedService, reason string) {
	s.mu.Lock()
	svc.state = svcStopping
	s.mu.Unlock()
	if reason != "" {
		s.logf(reason)
	}
	svc.stop()
	s.mu.Lock()
	svc.state = svcStopped
	svc.retryAt = time.Time{}
	s.mu.Unlock()
	s.wake()
}

// evictOrphans stops leftover model servers from an earlier run of this install (a crash or forced
// close leaves child processes behind, and a leftover server would hold the port so a new one refuses to
// start). It only ever stops a process whose program lives inside root, this install's own runtime
// folder. Anything else holding a port (another copy of the app with its own data folder, or an unrelated
// program) is left alone and reported. Call before anything is started.
func evictOrphans(ports []int, root string, logf func(string)) {
	if runtime.GOOS != "windows" {
		return
	}
	output, err := exec.Command("netstat.exe", "-ano", "-p", "tcp").Output()
	if err != nil {
		return
	}
	killed := map[int]bool{}
	for _, owner := range listeningOwners(string(output), ports) {
		if owner.pid == os.Getpid() || killed[owner.pid] {
			continue
		}
		image := processImagePath(owner.pid)
		if !pathInside(root, image) {
			what := "a program we could not identify"
			if image != "" {
				what = image
			}
			logf(fmt.Sprintf("Port %d is in use by %s, which is not part of this install, so it was left running", owner.port, what))
			continue
		}
		killed[owner.pid] = true
		_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(owner.pid), "/T", "/F").Run()
		logf(fmt.Sprintf("Stopped a leftover process from an earlier run (port %d)", owner.port))
	}
}

type portOwner struct{ port, pid int }

// listeningOwners reads `netstat -ano -p tcp` output and returns who listens on the wanted loopback ports.
func listeningOwners(netstat string, ports []int) []portOwner {
	wanted := map[int]bool{}
	for _, p := range ports {
		wanted[p] = true
	}
	var owners []portOwner
	for _, line := range strings.Split(netstat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[3] != "LISTENING" || !strings.HasPrefix(fields[1], "127.0.0.1:") {
			continue
		}
		port, portErr := strconv.Atoi(fields[1][strings.LastIndex(fields[1], ":")+1:])
		pid, pidErr := strconv.Atoi(fields[4])
		if portErr == nil && pidErr == nil && wanted[port] {
			owners = append(owners, portOwner{port: port, pid: pid})
		}
	}
	return owners
}

// pathInside reports whether path is root or lies under it. An empty path is never inside.
func pathInside(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ---- the app's services ----------------------------------------------------

const (
	svcModels  = "models"
	svcSpeech  = "speech"
	svcWhistle = "whistle"
	svcAgent   = "agent"
	svcWidget  = "widget"
)

func (a *app) newAppSupervisor() *supervisor {
	s := newSupervisor(a.recordCommandOutput, a.setServiceError)
	s.add(&managedService{
		name: svcModels, label: "Language model",
		start: a.startLlama, stop: a.stopLlama,
		healthy: func() bool { return a.llamaHealthy(llamaChatPort) && a.llamaHealthy(llamaEmbedPort) },
	})
	s.add(&managedService{
		name: svcSpeech, label: "Voice",
		start: a.startAudio, stop: a.stopAudio, healthy: a.audioHealthy,
	})
	s.add(&managedService{
		name: svcWhistle, label: "Speech recognition",
		start: a.ensureWhistle, stop: func() {}, healthy: a.whistleReady,
	})
	// The agent loop lives in this program, so "awake" only needs the language model. If the
	// model restarts, the agent restarts with it and warms the new server up again.
	s.add(&managedService{
		name: svcAgent, label: "Assistant", needs: []string{svcModels},
		start: a.startAgent, stop: func() { a.agentWarm.Store(false) }, healthy: func() bool { return a.llamaHealthy(llamaChatPort) },
	})
	// The desktop widget is optional: it is only wanted while the user has it switched on.
	s.add(&managedService{
		name: svcWidget, label: "Desktop widget",
		start: a.startWidget, stop: a.stopWidget, healthy: a.widgetHealthy,
	})
	return s
}

// setServiceError shows (or clears) a supervisor failure in the dashboard's error box.
func (a *app) setServiceError(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if message == "" && !a.serviceError {
		return
	}
	a.serviceError = message != ""
	a.state.Error = message
	_ = a.persistLocked()
}

// superviseInstalled keeps the always-on parts running once an install exists.
func (a *app) superviseInstalled() {
	for _, name := range []string{svcModels, svcSpeech, svcWhistle} {
		a.sup.Want(name, true)
	}
	a.mu.Lock()
	widget := a.state.WidgetEnabled
	a.mu.Unlock()
	a.sup.Want(svcWidget, widget)
}

// wakeAssistant and sleepAssistant are the user-facing on/off for the agent.
func (a *app) wakeAssistant() {
	a.superviseInstalled()
	a.sup.Want(svcAgent, true)
}

func (a *app) sleepAssistant() {
	a.sup.Want(svcAgent, false)
}
