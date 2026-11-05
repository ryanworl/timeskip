//go:build linux

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// timeline runs the timeline g.
func timeline(g protocol.Go) {
	tmpl := g.Template
	r := newRunner(tmpl)
	r.begin(g)
	mountInputs(g.Inputs)
	edits := readEdits()
	var pre []edit
	for _, e := range edits {
		if slices.Contains(g.Pre, e.name) {
			pre = append(pre, e)
		}
	}
	apply(r.ctx, pre)
	cat, _, err := discover()
	check(err)
	// (A template the edits add may not be there yet.)
	if !slices.Equal(cat.firsts(tmpl), g.Firsts) {
		msg("EXIT", protocol.Exit{Kind: "replan", Catalog: cat.wire()})
		return
	}
	if f := g.First; f != "" {
		// What the first_ command leaves is checkpointed and shared, so it
		// runs alike in every timeline (with seed 0's random numbers); after
		// it every timeline starts again with its own seed, as one restored
		// from the checkpoint does, so that its replays match it.
		reseed(0)
		i := slices.IndexFunc(cat[tmpl], func(c command) bool { return c.Name == f })
		if rec := r.run(cat[tmpl][i]); rec.Exit != 0 || rec.Killed {
			r.finish()
			return
		}
		if g.Checkpoint {
			if g.Skip {
				idle()
			}
			syscall.Unmount("/inputs", 0)
			freeze(false)
			msg("CHECKPOINT", struct{}{})
			if verb, body := await("CONTINUE", "GO"); verb == "GO" {
				g = protocol.ParseGo(body)
			}
			mountInputs(g.Inputs)
			edits = readEdits()
		}
		r.begin(g)
	}
	apply(r.ctx, edits)
	cat, invalid, err := discover()
	check(err)
	for _, i := range invalid { // (as Antithesis's "Invalid commands found")
		r.errors = append(r.errors, "invalid test command: "+i)
	}
	if tmpl != "" && len(cat[tmpl]) == 0 {
		fail("no test template %s: no running container has commands in %s/%s/", tmpl, templates, tmpl)
	}
	syscall.Unmount("/inputs", 0) // (applied; and the VM can be saved now)
	if g.Explore {
		watch(r, g)
	}
	r.drive(cat, g)
	r.finish()
}

// random is a generator of the timeline's, from its seed and a stream of
// its own.
func random(seed, stream uint64) *rand.Rand {
	s := &source{stream: stream}
	s.pcg.Seed(seed, stream)
	sources.Lock()
	sources.l = append(sources.l, s)
	sources.Unlock()
	return rand.New(s)
}

// A source is random's generator, which branch can reseed meanwhile.
type source struct {
	mu     sync.Mutex
	pcg    rand.PCG
	stream uint64
}

func (s *source) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pcg.Uint64()
}

var sources struct {
	sync.Mutex
	l []*source
}

// branch makes a timeline restored from a moment of another (see
// README.md, "Exploring") go on with seed's random numbers: its
// generators', the kernel's and its thread pauses'. What's drawn as it
// comes (the next command, the next fault) is then drawn anew, and so are
// the waits drawn before (see sleepDrawn), for the next launch, anytime_
// command and fault, and the eventually_ ending's moment.
func branch(seed uint64) {
	sources.Lock()
	for _, s := range sources.l {
		s.mu.Lock()
		s.pcg.Seed(seed, s.stream)
		s.mu.Unlock()
	}
	sources.Unlock()
	reseed(seed)
	os.WriteFile(pauseSeed, strconv.AppendUint(nil, seed, 10), 0)
	replanned.Lock()
	close(replanned.c)
	replanned.c = make(chan struct{})
	replanned.Unlock()
}

// replanned.c is closed, and replaced, when the timeline branches.
var replanned = struct {
	sync.Mutex
	c chan struct{}
}{c: make(chan struct{})}

func replans() <-chan struct{} {
	replanned.Lock()
	defer replanned.Unlock()
	return replanned.c
}

// sleepDrawn waits d, or until stop (false); after a branch meanwhile, as
// long as redraw says then.
func sleepDrawn(d time.Duration, redraw func() time.Duration, stop <-chan struct{}) bool {
	re := replans()
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return true
		case <-stop:
			return false
		case <-re:
			re = replans()
			t.Reset(redraw())
		}
	}
}

// begin starts (or, after the first_ command, restarts) the timeline's
// clock: the kernel reseeded with its seed, skipping as asked and its
// timeout set.
func (r *runner) begin(g protocol.Go) {
	reseed(g.Seed)
	setSkip(g.Skip)
	timeout := time.Duration(g.Timeout) * time.Second
	r.stopWatchdog()
	r.t0, r.s0, r.cpu0 = uptime(), skips(), cgroupCPU()
	logsT0 = r.t0
	if r.prof != nil { // (it profiled the first_ command, before a checkpoint)
		r.prof.stop()
		r.prof = nil
	}
	r.symbols = g.Symbols
	if g.Profile {
		var err error
		if r.prof, err = profile(); err != nil {
			r.mu.Lock()
			r.errors = append(r.errors, "profiling: "+err.Error())
			r.mu.Unlock()
		}
	}
	fired := make(chan struct{})
	r.fired = fired
	r.watchdog = time.AfterFunc(timeout, func() {
		defer close(fired)
		if r.tmpl == "" { // (the end of a timeline without a template: see drive)
			return
		}
		r.mu.Lock()
		r.dead, r.timedOut = true, true
		r.mu.Unlock()
		r.expire(fmt.Errorf("the timeline ran out of time (%v of guest time)", timeout))
		r.kill()
	})
}

// stopWatchdog stops the watchdog, or waits until it's done.
func (r *runner) stopWatchdog() {
	if r.watchdog != nil && !r.watchdog.Stop() {
		<-r.fired
	}
}

// reseed makes the kernel's random numbers those of seed: different between
// timelines restored from the same checkpoint (which would otherwise share
// them), and the same in a replay. The kernel's timeskip_seed (see
// guest/kernel/timeskip.patch) keys its generator with the seed alone, and
// from then on mixes in nothing else: not interrupt timings, nor the host's
// randomness.
func reseed(seed uint64) {
	check(os.WriteFile("/sys/module/kernel/parameters/timeskip_seed", strconv.AppendUint(nil, seed, 10), 0))
}

// ---- the runner ---------------------------------------------------------------

type proc struct {
	pid    int
	killed bool // by kill
	rec    protocol.Record
	done   chan struct{}
}

type runner struct {
	t0         time.Duration        // guest uptime at the start
	s0         int                  // skips by then
	cpu0       map[string]cgroupUse // the containers' CPU time by then
	prof       *profiler            // since then, if profiling
	symbols    bool                 // send the symbol tables with the coverage
	tmpl       string
	mu         sync.Mutex
	running    map[*proc]string // to their kind
	killing    bool
	dead       bool // out of time: nothing more starts
	timedOut   bool // ... since the watchdog fired
	records    []protocol.Record
	started    int // commands started
	errors     []string
	watchdog   *time.Timer
	fired      chan struct{}   // closed once the watchdog that fired is done
	ctx        context.Context // done once out of time: the edits' Compose commands are stopped
	expire     context.CancelCauseFunc
	exits      int             // container exits before GO (see exits)
	down       map[string]bool // containers a node fault took down (see downed)
	downs      map[string]int  // ... how many times
	downKill   bool            // whether a node fault killed a command
	paused     map[string]bool // containers a node fault paused (see enter)
	starting   map[string]int  // ... and processes starting in them
	kmsg       int             // the kernel's log from GO on (see crashes)
	faults     []protocol.Fault
	faultMix   string // the half of the mixture drawn, if any (see inject)
	faultGapMs int64  // the most guest time between faults drawn, if any
	failMs     int64  // when it failed (guest time since it started), or 0: a command failed, an assertion (see watchSDK), or a container exited unexpectedly (see over)
	exitMs     int64  // when over first saw an exit no fault had claimed, or 0
	ending     bool   // its ending came, or with none its drivers ended
}

// failed notes that the timeline failed, now, if it hadn't.
func (r *runner) failed() {
	r.mu.Lock()
	r.fail()
	r.mu.Unlock()
}

// fail is failed, with r.mu held.
func (r *runner) fail() {
	if r.failMs == 0 {
		r.failMs = max(1, (uptime() - r.t0).Milliseconds())
	}
}

// ended notes that the timeline's ending came (see drive).
func (r *runner) ended() {
	r.mu.Lock()
	r.ending = true
	r.mu.Unlock()
}

// over says whether the timeline failed (a command or an assertion did,
// or a container exited unexpectedly) or its ending came: then it saves
// no branch points (see watch). An exit no fault claimed yet may still be
// a fault's (see claimExit): it fails the timeline only if it's still
// unexpected at the end (see finish), from when over first saw it.
func (r *runner) over() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(exitsSince(r.exits)) > 0 {
		if r.exitMs == 0 {
			r.exitMs = max(1, (uptime() - r.t0).Milliseconds())
		}
		return true
	}
	return r.failMs > 0 || r.ending
}

func newRunner(tmpl string) *runner {
	r := &runner{tmpl: tmpl, running: map[*proc]string{}, exits: exitCount(), kmsg: -1, down: map[string]bool{}, downs: map[string]int{}, paused: map[string]bool{}, starting: map[string]int{}}
	if fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
		syscall.Seek(fd, 0, io.SeekEnd)
		r.kmsg = fd
	}
	r.ctx, r.expire = context.WithCancelCause(context.Background())
	return r
}

// start runs a command in the background.
func (r *runner) start(c command) *proc {
	p := &proc{done: make(chan struct{})}
	r.mu.Lock()
	r.running[p] = c.kind
	r.started++
	killing := r.killing || r.dead
	downs, down := r.downs[c.container], r.down[c.container]
	r.mu.Unlock()
	go func() {
		defer close(p.done)
		t := uptime()
		rec := protocol.Record{Name: c.Name, Exit: -1}
		ran := !killing && !down && r.enter(c.container) // (enter: false if killing began meanwhile)
		if ran {
			w := &prefixed{prefix: "[" + r.tmpl + "/" + c.Name + "] "}
			entered := true
			code, cpu, err := execIn(c.container, c.path, w, func(pid int) {
				r.leave(c.container)
				r.mu.Lock()
				p.pid, entered = pid, false
				r.mu.Unlock()
			})
			if entered { // (it didn't start)
				r.leave(c.container)
			}
			w.flush()
			if err != nil {
				say("%s: %v", c.Name, err)
				code = -1
			}
			rec.Exit, rec.CPUMs = code, cpu.Milliseconds()
		}
		r.mu.Lock()
		delete(r.running, p)
		// (Never started, if killing or its container is down; and a
		// command that ended by itself as kill came wasn't killed.)
		byFault := down || r.downs[c.container] != downs && rec.Exit != 0
		r.downKill = r.downKill || byFault
		rec.Killed = !ran || byFault || p.killed && rec.Exit == 128+int(syscall.SIGKILL)
		rec.Ms = (uptime() - t).Milliseconds()
		if rec.Exit != 0 && !rec.Killed {
			r.fail()
		}
		r.records = append(r.records, rec)
		p.rec = rec
		r.mu.Unlock()
	}()
	return p
}

// downed notes that a node fault is taking container c down (stopping or
// killing it, and with it the commands running there), or that it's up
// again. A command it kills, or that would start while it's down, counts
// as killed by the fault, as in Antithesis.
func (r *runner) downed(c container, down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if down && !r.down[c.ID] {
		r.downs[c.ID]++
	}
	r.down[c.ID] = down
}

// isDown is whether a node fault took container id down.
func (r *runner) isDown(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.down[id]
}

// enter waits while a node fault pauses container id, then counts a
// process starting there until leave; false if commands are being killed
// meanwhile. (A process started in a frozen cgroup freezes before it
// executes, and the agent, its vfork parent, with it, and then every
// goroutine at the next garbage collection.)
func (r *runner) enter(id string) bool {
	for {
		r.mu.Lock()
		if r.killing || r.dead {
			r.mu.Unlock()
			return false
		}
		if !r.paused[id] {
			r.starting[id]++
			r.mu.Unlock()
			return true
		}
		r.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *runner) leave(id string) {
	r.mu.Lock()
	r.starting[id]--
	r.mu.Unlock()
}

// pausing notes that a node fault is about to pause container c, once no
// process is starting there (see enter), or that it unpaused it.
func (r *runner) pausing(c container, paused bool) {
	for {
		r.mu.Lock()
		if !paused || r.starting[c.ID] == 0 {
			r.paused[c.ID] = paused
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func (r *runner) run(c command) protocol.Record {
	p := r.start(c)
	<-p.done
	return p.rec
}

// wait waits until no command of kind (any, if "") runs, and reports true;
// or until end is closed (never, if nil), and reports false.
func (r *runner) wait(kind string, end <-chan struct{}) bool {
	for {
		var p *proc
		r.mu.Lock()
		for q, k := range r.running {
			if kind == "" || k == kind {
				p = q
				break
			}
		}
		r.mu.Unlock()
		if p == nil {
			return true
		}
		select {
		case <-p.done:
		case <-end:
			return false
		}
	}
}

// kill kills the running commands and what they started, until none is
// left.
func (r *runner) kill() {
	r.mu.Lock()
	r.killing = true
	r.mu.Unlock()
	for {
		var pids []int
		left := false
		r.mu.Lock()
		for p := range r.running {
			left = true
			if p.pid > 0 {
				p.killed = true
				pids = append(pids, p.pid)
			}
		}
		r.mu.Unlock()
		if !left {
			break
		}
		for _, pid := range pids {
			killTree(pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	r.killing = false
	r.mu.Unlock()
}

// killTree kills pid and its descendants.
func killTree(pid int) {
	children := map[int][]int{}
	ds, _ := os.ReadDir("/proc")
	for _, d := range ds {
		f := procStat(d.Name())
		if f == nil {
			continue
		}
		c, _ := strconv.Atoi(d.Name())
		pp, _ := strconv.Atoi(f[1])
		children[pp] = append(children[pp], c)
	}
	var walk func(int)
	walk = func(p int) {
		syscall.Kill(p, syscall.SIGKILL)
		for _, c := range children[p] {
			walk(c)
		}
	}
	walk(pid)
}

// drive schedules the template's commands after its first_ from the seed,
// as the Test Composer does (see README.md, "Timelines"); with faults,
// faults are injected until the ending (see inject). Without a template
// (the project has none: its workload runs by itself), the timeline is its
// faults until the timeout.
func (r *runner) drive(cat catalog, g protocol.Go) {
	seed, drivers := g.Seed, g.Drivers
	rng := random(seed, 1)
	t := r.tmpl
	// (Faults first, so that an anytime_ command's ANTITHESIS_STOP_FAULTS
	// finds them.)
	healed := func() {}
	if g.Faults > 0 {
		healed = r.inject(g)
	}
	if t == "" {
		<-r.fired
		healed()
		return
	}
	singles, mix := cat.of(t, "singleton_driver"), append(cat.of(t, "parallel_driver"), cat.of(t, "serial_driver")...)
	anytimeOnly := len(singles)+len(mix) == 0
	pick := func(l []command) command { return l[rng.IntN(len(l))] }
	gap := func(max time.Duration) time.Duration { return time.Duration(rng.Int64N(int64(max))) }
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	var endings []string
	if len(cat.of(t, "finally")) > 0 {
		endings = append(endings, "finally")
	}
	if len(cat.of(t, "eventually")) > 0 {
		endings = append(endings, "eventually")
	}
	ending := ""
	if len(endings) > 0 && !anytimeOnly { // (an ending follows drivers)
		ending = endings[rng.IntN(len(endings))]
	}

	stop := make(chan struct{})
	ticked := make(chan struct{})
	go func() { // anytime_ commands, with a generator of their own
		defer close(ticked)
		anys, rng := cat.of(t, "anytime"), random(seed, 2)
		for n := 0; len(anys) > 0 && !(anytimeOnly && n == drivers); n++ {
			wait := func() time.Duration { return time.Duration(rng.Int64N(int64(2 * time.Second))) }
			if !sleepDrawn(wait(), wait, stop) {
				return
			}
			r.start(anys[rng.IntN(len(anys))])
		}
	}()

	single := len(singles) > 0 && (len(mix) == 0 || rng.IntN(len(singles)+1) < len(singles))
	end := make(chan struct{}) // (closed at the eventually_ ending's moment)
	switch {
	case single:
		p := r.start(pick(singles))
		if ending == "eventually" {
			sleepDrawn(gap(5*time.Second), func() time.Duration { return gap(5 * time.Second) }, p.done)
		}
	case len(mix) > 0:
		// (The eventually_ ending comes at any moment after the first
		// launch, up to 5 s after the launches would be done, on average,
		// if nothing held them up: a serial_driver_ that doesn't end, or
		// one waiting for a parallel_driver_ that doesn't, can't hold it
		// off. Each launch is drawn as it comes, so that a branch draws
		// the rest anew.)
		if ending == "eventually" {
			// (After a branch, drawn anew over what's left of
			// that time, with a generator of its own: the launches draw
			// meanwhile.)
			window, t0, erng := time.Duration(drivers)*250*time.Millisecond+5*time.Second, time.Now(), random(seed, 13)
			d := gap(window)
			go func() {
				sleepDrawn(d, func() time.Duration {
					return time.Duration(erng.Int64N(int64(max(time.Millisecond, window-time.Since(t0)))))
				}, nil)
				close(end)
			}()
		}
	launch:
		for range drivers {
			c := pick(mix)
			if c.kind == "serial_driver" {
				if !r.wait("parallel_driver", end) {
					break launch
				}
				select {
				case <-r.start(c).done:
				case <-end:
					break launch
				}
			} else {
				r.start(c)
			}
			if !sleepDrawn(gap(500*time.Millisecond), func() time.Duration { return gap(500 * time.Millisecond) }, end) {
				break launch
			}
		}
	default: // (anytime_ commands only: drivers launches of them)
		<-ticked
	}
	if ending == "eventually" {
		if !single {
			<-end
		}
		r.ended()
		close(stop)
		<-ticked
		r.kill()
		healed()
		r.run(pick(cat.of(t, "eventually")))
		return
	}
	r.wait("singleton_driver", nil)
	r.wait("parallel_driver", nil)
	r.wait("serial_driver", nil)
	close(stop)
	<-ticked
	healed()
	r.wait("", nil)
	r.ended()
	r.mu.Lock()
	if r.started == started {
		r.errors = append(r.errors, "no test command ran: a template needs parallel_driver_, serial_driver_, singleton_driver_ or anytime_ commands, and drivers more than 0")
	}
	r.mu.Unlock()
	// (A finally_ command follows drivers that completed: none that a fault
	// killed.)
	r.mu.Lock()
	killed := r.downKill
	r.mu.Unlock()
	if ending == "finally" && !killed {
		r.run(pick(cat.of(t, "finally")))
	}
}

// finish reports the timeline's result: its commands, the assertions in
// sdk.jsonl and what went wrong besides.
func (r *runner) finish() {
	r.stopWatchdog()
	r.wait("", nil)
	ms := (uptime() - r.t0).Milliseconds()
	r.mu.Lock()
	for _, e := range exitsSince(r.exits) {
		r.errors = append(r.errors, "containers: "+e)
		if r.failMs == 0 {
			r.failMs = r.exitMs
		}
	}
	for _, c := range crashes(r.kmsg) {
		r.errors = append(r.errors, "crash: "+c)
	}
	commands, errs, faults, faultMix, faultGapMs, timedOut := r.records, r.errors, r.faults, r.faultMix, r.faultGapMs, r.timedOut
	r.mu.Unlock()
	if len(errs) > 0 {
		say("the services' last logs:")
		compose("logs", "--no-color", "--tail", "50")
	}
	cpu := map[string]int64{}
	cs, _ := containers("")
	for id, u := range cgroupCPU() {
		if i := slices.IndexFunc(cs, func(c container) bool { return c.ID == id }); i >= 0 {
			if base := r.cpu0[id]; base.ino == u.ino { // (else it started again since, in a new cgroup)
				u.us -= base.us
			}
			cpu[cs[i].Name] = int64(u.us / 1000)
		}
	}
	var stacks map[string]int64
	if r.prof != nil {
		var lost uint64
		if stacks, lost = r.prof.stop(); lost > 0 {
			say("profile: %d samples lost", lost)
		}
	}
	cov, syms := coverage(r.symbols)
	noting.Lock() // (for good: see watch)
	sdkScan()     // (the assertions' last first falses)
	msg("EXIT", protocol.Exit{Kind: "timeline", Result: &protocol.Result{GuestMs: ms, Skips: skips() - r.s0,
		Commands: commands, Assertions: assertions(), Errors: errs, Faults: faults, FaultMix: faultMix, FaultGapMs: faultGapMs, TimedOut: timedOut, ContainerCPUMs: cpu, Profile: stacks,
		Coverage: cov, Symbols: syms, FailedMs: r.failMs}})
	// (If it failed, the CLI asks for the containers' logs; if not, it
	// stops the VM.)
	if watching {
		<-logsAsked
	} else {
		await("LOGS")
	}
	if err := dumpLogs(); err != nil {
		say("writing the containers' logs: %v", err)
	}
	msg("LOGGED", struct{}{})
}

// assertions sum up the Antithesis SDK assertions' evaluations in sdk.jsonl.
func assertions() []protocol.Assertion {
	var l []protocol.Assertion
	by := map[string]int{} // index in l
	for _, line := range sdkLines() {
		s, key := parseAssert(line)
		if s == nil {
			continue
		}
		i, ok := by[key]
		if !ok {
			i, by[key] = len(l), len(l)
			l = append(l, protocol.Assertion{ID: s.ID, Message: s.Message, Type: s.Type, Display: s.Display, MustHit: s.MustHit,
				FirstFalseMs: firstFalse[key]})
		}
		a := &l[i]
		if s.Hit {
			a.Hits++
			if s.Condition {
				a.True++
			} else if a.Details == nil && !bytes.Equal(s.Details, []byte("null")) {
				a.Details = s.Details
			}
		}
	}
	return l
}

// An sdkAssert is an SDK's assertion line, {"antithesis_assert": ...}.
type sdkAssert struct {
	ID        string          `json:"id"`
	Message   string          `json:"message"`
	Condition bool            `json:"condition"`
	Display   string          `json:"display_type"`
	Hit       bool            `json:"hit"`
	MustHit   bool            `json:"must_hit"`
	Type      string          `json:"assert_type"`
	Details   json.RawMessage `json:"details"`
}

// parseAssert is line's assertion, if it's one, and its key: its ID, or
// else its message.
func parseAssert(line []byte) (*sdkAssert, string) {
	var m struct {
		A *sdkAssert `json:"antithesis_assert"`
	}
	if json.Unmarshal(line, &m) != nil || m.A == nil {
		return nil, ""
	}
	return m.A, cmp.Or(m.A.ID, m.A.Message)
}

// crashes are the processes that died of a program error signal (SIGSEGV,
// SIGBUS, SIGABRT and the like), as Antithesis's "No unexpected crashes"
// counts them, and those the kernel killed when the VM ran out of memory:
// what the kernel logged (print-fatal-signals, which setup turns on) since
// kmsg, a descriptor of /dev/kmsg, was opened.
func crashes(kmsg int) []string {
	var l []string
	buf := make([]byte, 8192)
	for kmsg >= 0 {
		n, err := syscall.Read(kmsg, buf)
		if err == syscall.EPIPE { // (records were overwritten)
			continue
		} else if err != nil || n <= 0 {
			break
		}
		// "LEVEL,SEQ,TIME,FLAGS;[EXE: ]COMM: potentially unexpected fatal signal N."
		_, m, _ := strings.Cut(strings.SplitN(string(buf[:n]), "\n", 2)[0], ";")
		if who, sig, ok := strings.Cut(m, "potentially unexpected fatal signal "); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(sig, "."))
			l = append(l, fmt.Sprintf("%s died of signal %d (%v)", strings.TrimSuffix(who, ": "), n, syscall.Signal(n)))
		} else if p, ok := strings.CutPrefix(m, "Out of memory: Killed process "); ok {
			// (The VM ran out of memory, past Antithesis's "Peak memory
			// usage" bound of 95%; not a container's own memory limit.)
			p, _, _ = strings.Cut(p, " total-vm")
			l = append(l, "the VM ran out of memory and killed process "+p)
		}
	}
	return l
}

// prefixed writes a command's output to the console a line at a time,
// each line prefixed.
type prefixed struct {
	prefix string
	buf    []byte
}

func (w *prefixed) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		consoleMu.Lock()
		os.Stdout.Write(append([]byte(w.prefix), w.buf[:i+1]...))
		consoleMu.Unlock()
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (w *prefixed) flush() {
	if len(w.buf) > 0 {
		w.Write([]byte("\n"))
	}
}
