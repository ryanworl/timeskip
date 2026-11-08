//go:build linux

package main

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Faults (GO's faults=S), as docs/reference.md's Faults describes them: a
// baseline latency, thread pausing, network faults (partitions and clogs)
// and, with node_faults=1 and on their own schedule, node faults (pauses, stops and
// kills of nodes without test commands, and throttles) and CPU modulation
// (see cpu.go); with disk=KINDS, disk faults (see disk.go). Generators of their own, from the
// timeline's seed, plan each fault from the nodes as they were when faults
// began; between changes the injector sleeps on timers. ANTITHESIS_STOP_FAULTS reaches it through a
// socket (see serve).

// netem is a netem qdisc's parameters: one-way delay, its variation, and a
// loss rate (in percent).
type netem struct {
	delay, jitter time.Duration
	loss          float64
}

func (n netem) String() string {
	return fmt.Sprintf("delay %dus %dus distribution paretonormal loss %.4f%%", n.delay.Microseconds(), n.jitter.Microseconds(), n.loss)
}

// A link is traffic from one container to another (by ID), disrupted.
type link struct {
	from, to string
}

// The directory, shared with every container, of ANTITHESIS_STOP_FAULTS:
// the guest agent (which acts as it when run under that name) and the
// injector's socket.
const (
	faultsDir   = "/run/timeskip-faults"
	faultsInCtr = "/timeskip-faults"
)

// A node is a container with a network namespace of its own, and the
// containers in that namespace (network mode container:ID, as for a
// service:NAME, directly or through another), which go down and come back
// with it.
type node struct {
	levels   [][]container   // the owner, the containers on its network, those on theirs, ...
	excluded map[string]bool // the kinds of fault (network, pause, stop, kill, throttle, clock, disk) it's never a target of
	zone     string          // its zone (com.timeskip.zone), if any
	region   string          // its region (com.timeskip.region), if any
}

func (n *node) owner() container { return n.levels[0][0] }

type injector struct {
	r         *runner
	level     string   // the fault tolerance level (see protocol.Go)
	power     bool     // whether some kills are power losses (disk=power, see power.go)
	diskKinds []string // the kinds of disk fault (disk=)
	nodes     []*node  // as when faults began
	base      netem
	cross     netem // the baseline between zones, if the nodes are in two or more
	far       netem // ... between regions, likewise
	stop      chan struct{}
	sock      net.Listener

	mu         sync.Mutex
	changed    chan struct{} // closed (and replaced) when what follows changes
	quietUntil time.Time     // no faults until then (ANTITHESIS_STOP_FAULTS)
	stopping   int           // ... nor while requests wait for the faults to be healed
	active     int           // faults in effect

	net     sync.Mutex      // the network's faults, and applying them
	links   []link          // disrupted
	how     string          // ... stopped (dropped), slowed or jammed (held until healed)
	slow    netem           // ... the delay, if slowed
	set     map[string]bool // network namespaces (by netKey) with timeskip's qdiscs and chain
	dropped map[string]bool // ... whose chain drops something
	slowed  map[string]bool // ... whose filters slow something
	jammed  map[string]bool // ... whose filters jam something
	crossed map[string]bool // ... whose filters delay traffic to other zones and regions
	gap     time.Duration   // the longest between faults
	skip    bool            // whether idle time is skipped
}

// inject starts injecting g's faults; the function it returns stops and
// heals every fault.
func (r *runner) inject(g protocol.Go) func() {
	in := &injector{r: r, level: g.Level, skip: g.Skip, gap: time.Duration(g.Faults * float64(time.Second)), stop: make(chan struct{}), changed: make(chan struct{}),
		set: map[string]bool{}, dropped: map[string]bool{}, slowed: map[string]bool{}, jammed: map[string]bool{}, crossed: map[string]bool{}}
	if err := in.inventory(); err != nil {
		in.errorf("%v", err)
	}
	nrng, prng := random(g.Seed, 3), random(g.Seed, 4)
	// The mixture, so that timelines differ in their kinds of fault and
	// how many: with a level that makes node faults, half the timelines
	// (a coin) make none, as node faults find some bugs but restart or
	// freeze what others need up, and their other faults come three times
	// as close; and each timeline's gap is drawn from a third of the
	// level's to three times it (log-uniform).
	nodes, mrng := in.level != "legacy", random(g.Seed, 12)
	r.mu.Lock()
	if nodes {
		r.faultMix = "node faults"
		if mrng.IntN(2) == 0 {
			nodes, in.gap, r.faultMix = false, in.gap/3, "no node faults"
		}
	}
	in.gap = time.Duration(float64(in.gap) * logUniformF(mrng, 1.0/3, 3))
	r.faultGapMs = in.gap.Milliseconds()
	say("fault: %s, at most %.1f s apart", cmp.Or(r.faultMix, "network faults"), in.gap.Seconds())
	r.mu.Unlock()
	d := logUniform(nrng, 100*time.Microsecond, time.Millisecond)
	in.base = netem{d, time.Duration(float64(d) * (0.2 + 0.4*nrng.Float64())), logUniformF(nrng, 0.001, 0.05)}
	base := protocol.Fault{Kind: "latency", Detail: in.base.String(), AtMs: (uptime() - r.t0).Milliseconds()}
	if len(in.groups(zoneOf)) > 1 {
		in.cross = in.base
		in.cross.delay += logUniform(random(g.Seed, 9), 250*time.Microsecond, time.Millisecond)
		base.Detail += "; between zones: " + in.cross.String()
	}
	if len(in.groups(regionOf)) > 1 {
		in.far = in.base
		in.far.delay += logUniform(random(g.Seed, 10), 5*time.Millisecond, 50*time.Millisecond)
		base.Detail += "; between regions: " + in.far.String()
	}
	in.listen()
	if err := in.disrupt(nil, "", netem{}); err != nil {
		in.errorf("baseline latency: %v", err)
	}
	say("fault: latency, %s", base.Detail)
	var wg sync.WaitGroup
	wg.Go(func() { in.schedule(nrng, in.network) })
	if in.level != "legacy" {
		if nodes {
			wg.Go(func() { in.schedule(prng, in.node) })
		}
		wg.Go(func() { in.schedule(random(g.Seed, 7), in.modulate) })
		// (The VM has one wall clock: a node excluded from clock skips
		// spares every node.)
		if len(in.targets("clock")) == len(in.nodes) {
			wg.Go(func() { in.clock(random(g.Seed, 8)) })
		}
	}
	// (A power loss is a kind of kill, not a disk fault of its own.)
	in.diskKinds, in.power = g.Disk, slices.Contains(g.Disk, "power")
	if in.power {
		if err := cache(); err != nil {
			in.errorf("disk caches: %v", err)
		}
	}
	if slices.ContainsFunc(g.Disk, func(k string) bool { return k != "power" }) {
		wg.Go(func() { in.schedule(random(g.Seed, 11), in.disk) })
	}
	pauses := pauseCount()
	say("fault: thread pausing")
	wg.Go(func() { in.pauseThreads(g.Seed) })
	return func() {
		close(in.stop)
		if in.sock != nil {
			in.sock.Close()
		}
		wg.Wait()
		if err := in.clear(); err != nil {
			in.errorf("removing the latency: %v", err)
		}
		// (Thread pauses aren't listed, as in Antithesis: too many.)
		say("fault: %d thread pauses", pauseCount()-pauses)
		base.Ms = (uptime() - r.t0).Milliseconds() - base.AtMs
		r.mu.Lock()
		r.faults = append(r.faults, base)
		r.mu.Unlock()
	}
}

// pauseThreads has the kernel pause the containers' threads now and then,
// with a generator seeded with seed (see timeskip_pause in
// guest/kernel/timeskip.patch), until the injector stops; but not while
// ANTITHESIS_STOP_FAULTS has stopped faults. As in Antithesis, every level
// pauses threads, and in every container: exclusions don't apply to it.
func (in *injector) pauseThreads(seed uint64) {
	const parm = "/sys/module/kernel/parameters/timeskip_pause"
	if err := os.WriteFile(pauseSeed, strconv.AppendUint(nil, seed, 10), 0); err != nil {
		in.errorf("thread pausing: %v", err)
		return
	}
	for {
		if ok, _ := in.begin(); !ok {
			return
		}
		err := os.WriteFile(parm, []byte("Y"), 0)
		running := err == nil && in.sleep(math.MaxInt64, true, nil)
		if err == nil {
			err = os.WriteFile(parm, []byte("N"), 0)
		}
		in.end()
		if err != nil {
			in.errorf("thread pausing: %v", err)
		}
		if !running {
			return
		}
	}
}

const pauseSeed = "/sys/module/kernel/parameters/timeskip_pause_seed"

// pauseCount is how many thread pauses the kernel has made.
func pauseCount() int {
	b, _ := os.ReadFile("/sys/module/kernel/parameters/timeskip_pauses")
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// schedule makes faults with next, one at a time: each gap/100 to gap after
// the last, for 0.1 to 30 s (each about a fifth of the time); next returns the
// fault and how to heal it, or none. A fault counts from when next returns
// it, or from when next calls start, if making it takes time: a stop counts
// from its first SIGTERM, which may take 10 s to stop a container, and its
// line, once it's made, says when that was. Each start and end is due at a
// time planned from the last one's, not from when the last change was done,
// so that the time making and healing faults takes (stopping and starting
// containers, say) doesn't shift the faults after it.
// A fault stopped early by ANTITHESIS_STOP_FAULTS is healed then; one due
// while faults are stopped starts when they resume, and the plan with it.
func (in *injector) schedule(rng *rand.Rand, next func(*rand.Rand, func()) (*protocol.Fault, func() error, error)) {
	due := time.Now()
	for {
		due = due.Add(logUniform(rng, in.gap/100, in.gap))
		if !in.sleep(time.Until(due), false, func() time.Duration {
			due = time.Now().Add(logUniform(rng, in.gap/100, in.gap))
			return time.Until(due)
		}) {
			return
		}
		ok, waited := in.begin()
		if !ok {
			return
		} else if waited {
			due = time.Now()
		}
		var t time.Duration
		var once sync.Once
		f, heal, err := next(rng, func() { once.Do(func() { t = uptime() }) })
		// (Drawn even if no fault was made, so that what follows doesn't
		// depend on it.)
		due = due.Add(logUniform(rng, 100*time.Millisecond, 30*time.Second))
		if err != nil || f == nil {
			in.end()
			if err != nil {
				in.errorf("%v", err)
				return
			}
			continue
		}
		since := ""
		if t > 0 {
			since = fmt.Sprintf(" (since %.2fs)", t.Seconds())
		} else {
			t = uptime()
		}
		f.AtMs = (t - in.r.t0).Milliseconds()
		say("fault: %s (%s) %s%s", f.Kind, f.Detail, strings.Trim(fmt.Sprint(f.Targets), "[]"), since)
		stopped := !in.sleep(time.Until(due), true, nil)
		if err := heal(); err != nil {
			in.errorf("healing a %s: %v", f.Kind, err)
		}
		f.Ms = (uptime() - t).Milliseconds()
		say("fault: healed the %s after %v", f.Kind, time.Duration(f.Ms)*time.Millisecond)
		in.r.mu.Lock()
		in.r.faults = append(in.r.faults, *f)
		in.r.mu.Unlock()
		in.end()
		if stopped {
			return
		}
	}
}

// sleep sleeps for d, or until faults are stopped if quiet; false if the
// injector stopped. After a branch meanwhile (see branch), it
// sleeps as long as redraw, if any, says then.
func (in *injector) sleep(d time.Duration, quiet bool, redraw func() time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	re := replans()
	for {
		in.mu.Lock()
		quieted, changed := quiet && (in.stopping > 0 || time.Now().Before(in.quietUntil)), in.changed
		in.mu.Unlock()
		if quieted {
			return true
		}
		select {
		case <-in.stop:
			return false
		case <-t.C:
			return true
		case <-changed:
		case <-re:
			re = replans()
			if redraw != nil {
				t.Reset(redraw())
			}
		}
	}
}

// begin waits until faults aren't stopped (nor being stopped), then counts
// one more fault in effect; not ok if the injector stopped. waited says
// whether it had to.
func (in *injector) begin() (ok, waited bool) {
	for {
		in.mu.Lock()
		left, stopping, changed := time.Until(in.quietUntil), in.stopping, in.changed
		if left <= 0 && stopping == 0 {
			in.active++
			in.mu.Unlock()
			return true, waited
		}
		in.mu.Unlock()
		t := time.NewTimer(left)
		if left <= 0 { // (but requests wait for the faults to be healed)
			t.Stop()
		}
		select {
		case <-in.stop:
			t.Stop()
			return false, waited
		case <-t.C:
		case <-changed:
		}
		t.Stop()
		waited = true
	}
}

// end counts a fault healed.
func (in *injector) end() {
	in.mu.Lock()
	in.active--
	in.notify()
	in.mu.Unlock()
}

// notify wakes whoever waits on a change. (With in.mu held.)
func (in *injector) notify() {
	close(in.changed)
	in.changed = make(chan struct{})
}

// listen serves ANTITHESIS_STOP_FAULTS on the socket in faultsDir: a
// request is a number of seconds; the answer comes once no fault is in
// effect.
func (in *injector) listen() {
	path := faultsDir + "/faults.sock"
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		in.errorf("ANTITHESIS_STOP_FAULTS: %v", err)
		return
	}
	os.Chmod(path, 0o777) // (for containers' other users)
	in.sock = l
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go in.serve(c)
		}
	}()
}

func (in *injector) serve(c net.Conn) {
	defer c.Close()
	var s float64
	if _, err := fmt.Fscan(c, &s); err != nil || s < 0 {
		return
	}
	say("fault: stopped for %gs (ANTITHESIS_STOP_FAULTS)", s)
	in.mu.Lock()
	in.stopping++
	in.notify()
	for in.active > 0 {
		changed := in.changed
		in.mu.Unlock()
		<-changed
		in.mu.Lock()
	}
	// (The quiet time counts from when the faults are healed: healing a
	// stop can take seconds.)
	if t := time.Now().Add(time.Duration(s * float64(time.Second))); t.After(in.quietUntil) {
		in.quietUntil = t
	}
	in.stopping--
	in.notify()
	in.mu.Unlock()
	fmt.Fprintln(c, "ok")
}

func (in *injector) errorf(format string, a ...any) {
	in.r.mu.Lock()
	in.r.errors = append(in.r.errors, "faults: "+fmt.Sprintf(format, a...))
	in.r.mu.Unlock()
}

// inventory finds the nodes: the running (or paused: still on the network)
// containers with a network namespace of their own, and the containers in
// it. A container labeled com.antithesis.exclude_from_faults, as some
// Antithesis projects are, excludes its node from the kinds of fault the
// label lists (network, pause, stop, kill, throttle, clock, disk). A container labeled
// com.timeskip.zone or com.timeskip.region puts its node in that zone or
// region.
func (in *injector) inventory() error {
	cs, err := containers("")
	if err != nil {
		return err
	}
	slices.SortFunc(cs, func(a, b container) int { return strings.Compare(a.Name, b.Name) })
	byID, owned := map[string]container{}, map[string]*node{}
	for _, c := range cs {
		byID[c.ID] = c
		if m := c.HostConfig.NetworkMode; (c.State.Status == "running" || c.State.Status == "paused") && m != "host" && !strings.HasPrefix(m, "container:") && len(c.ips()) > 0 {
			owned[c.ID] = &node{levels: [][]container{{c}}, excluded: map[string]bool{}}
			in.nodes = append(in.nodes, owned[c.ID])
		}
	}
	for _, c := range cs {
		o, hops := netOwner(c, byID)
		n := owned[o.ID]
		if n == nil {
			continue
		}
		if hops > 0 {
			for len(n.levels) <= hops {
				n.levels = append(n.levels, nil)
			}
			n.levels[hops] = append(n.levels[hops], c)
		}
		if z := c.Config.Labels["com.timeskip.zone"]; z != "" && n.zone == "" {
			n.zone = z
		}
		if z := c.Config.Labels["com.timeskip.region"]; z != "" && n.region == "" {
			n.region = z
		}
		for _, k := range strings.Split(c.Config.Labels["com.antithesis.exclude_from_faults"], ",") {
			n.excluded[strings.TrimSpace(k)] = true
		}
	}
	return nil
}

// current inspects the project's containers now, by ID.
func current() (map[string]container, error) {
	cs, err := containers("")
	m := map[string]container{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m, err
}

// network makes a partition or a clog, stopped, slowed or jammed.
func (in *injector) network(rng *rand.Rand, _ func()) (*protocol.Fault, func() error, error) {
	targets := in.targets("network")
	if len(in.nodes) < 2 || len(targets) == 0 {
		return nil, nil, nil
	}
	rng.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	// (resilient: no partitions, nor stopped clogs.)
	partition := len(targets) > 1 && in.level != "resilient" && rng.IntN(2) == 0
	var a, b []*node
	scope, inside := in.scope(rng, 2, targets, partition)
	if scope != "" {
		// The zone's or region's targets, and the other targets (a
		// partition) or nodes.
		a = slices.DeleteFunc(slices.Clone(targets), func(n *node) bool { return !inside(n) })
		b = slices.DeleteFunc(slices.Clone(in.nodes), inside)
		if partition {
			b = slices.DeleteFunc(slices.Clone(targets), inside)
		}
	} else if partition {
		k := 1 + rng.IntN(len(targets)-1)
		a, b = targets[:k], targets[k:]
	} else {
		a = targets[:1+rng.IntN(min(len(targets), len(in.nodes)-1))]
		b = slices.DeleteFunc(slices.Clone(in.nodes), func(n *node) bool { return slices.Contains(a, n) })
	}
	f := &protocol.Fault{}
	var links []link
	add := func(from, to []*node) {
		for _, x := range from {
			for _, y := range to {
				links = append(links, link{x.owner().ID, y.owner().ID})
			}
		}
	}
	if partition {
		// A partition: a's packets to b and, unless one way, b's to a.
		f.Kind, f.Targets = "partition", [][]string{owners(a), owners(b)}
		add(a, b)
		f.Detail = "one way"
		if rng.IntN(3) > 0 {
			add(b, a)
			f.Detail = "both ways"
		}
	} else {
		// A clog: a's inbound, outbound or both.
		f.Kind, f.Targets = "clog", [][]string{owners(a)}
		switch rng.IntN(3) {
		case 0:
			add(b, a)
			f.Detail = "inbound"
		case 1:
			add(a, b)
			f.Detail = "outbound"
		default:
			add(a, b)
			add(b, a)
			f.Detail = "both ways"
		}
	}
	if scope != "" {
		f.Detail = scope + ", " + f.Detail
	}
	// Its packets stopped, slowed or jammed (resilient: not stopped).
	ways := []string{"stopped", "slowed", "jammed"}
	if in.level == "resilient" {
		ways = ways[1:]
	}
	how, slow := ways[rng.IntN(len(ways))], netem{}
	if how == "slowed" {
		d := logUniform(rng, 20*time.Millisecond, time.Second)
		slow = netem{d, d / 4, in.base.loss}
		f.Detail += ": " + slow.String()
	}
	f.Detail = how + ", " + f.Detail
	if err := in.disrupt(links, how, slow); err != nil {
		in.disrupt(nil, "", netem{})
		return nil, nil, err
	}
	return f, func() error { return in.disrupt(nil, "", netem{}) }, nil
}

// disrupt makes links the disrupted ones, how (with slow, if slowed), and
// applies that to the running nodes.
func (in *injector) disrupt(links []link, how string, slow netem) error {
	in.net.Lock()
	defer in.net.Unlock()
	in.links, in.how, in.slow = links, how, slow
	return in.apply()
}

// reapply applies the network's faults to the running nodes again: to a
// container that was started again, with a new network namespace.
func (in *injector) reapply() error {
	in.net.Lock()
	defer in.net.Unlock()
	return in.apply()
}

// apply disrupts in.links (and only them) among the running nodes:
// dropped on arrival, delayed by in.slow on sending, or held on sending in
// a plug qdisc, which releases what it holds once the links are no longer
// jammed (up to its limit: more is dropped). Each node gets the baseline
// latency first, and in.cross on what it sends to other zones of its
// region, in.far to other regions. (With in.net held.)
func (in *injector) apply() error {
	now, err := current()
	if err != nil {
		return err
	}
	links, how, slow := in.links, in.how, in.slow
	var errs []error
	for _, nd := range in.nodes {
		n := now[nd.owner().ID]
		if s := n.State.Status; s != "running" && s != "paused" || len(n.ips()) == 0 {
			continue
		}
		pid := n.State.Pid
		key := netKey(pid)
		ifs := interfaces(pid)
		var tc, ipt strings.Builder
		if !in.set[key] {
			for _, i := range ifs {
				fmt.Fprintf(&tc, "qdisc replace dev %s root handle 1: prio bands 5 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n", i)
				for _, h := range []string{"1:1 handle 10:", "1:2 handle 20:"} {
					fmt.Fprintf(&tc, "qdisc replace dev %s parent %s netem limit 10000 %s rate 10gbit\n", i, h, in.base)
				}
				// (Plugged from the start. 64 MiB is more than a socket's
				// buffer holds, so only many sockets' traffic reaches it.)
				fmt.Fprintf(&tc, "qdisc replace dev %s parent 1:3 handle 30: plug limit %d\n", i, 64<<20)
				for j, n := range []netem{in.cross, in.far} {
					if n.delay > 0 {
						fmt.Fprintf(&tc, "qdisc replace dev %s parent 1:%d handle %d0: netem limit 10000 %s rate 10gbit\n", i, 4+j, 4+j, n)
					}
				}
			}
			ipt.WriteString(":TIMESKIP - [0:0]\n-I INPUT -j TIMESKIP\n")
		}
		var drop, slowTo, jamTo, crossTo, farTo []string
		for _, o := range in.nodes {
			switch {
			case in.far.delay > 0 && nd.region != "" && o.region != "" && o.region != nd.region:
				farTo = append(farTo, now[o.owner().ID].ips()...)
			case in.cross.delay > 0 && nd.zone != "" && o.zone != "" && o.zone != nd.zone:
				crossTo = append(crossTo, now[o.owner().ID].ips()...)
			}
		}
		for _, l := range links {
			switch {
			case how == "stopped" && l.to == n.ID:
				drop = append(drop, now[l.from].ips()...)
			case how == "slowed" && l.from == n.ID:
				slowTo = append(slowTo, now[l.to].ips()...)
			case how == "jammed" && l.from == n.ID:
				jamTo = append(jamTo, now[l.to].ips()...)
			}
		}
		if len(drop) > 0 || in.dropped[key] {
			if in.set[key] {
				ipt.WriteString(":TIMESKIP - [0:0]\n") // (declaring it empties it)
			}
			for _, ip := range drop {
				fmt.Fprintf(&ipt, "-A TIMESKIP -s %s -j DROP\n", ip)
			}
		}
		for _, i := range ifs {
			if in.slowed[key] {
				fmt.Fprintf(&tc, "filter del dev %s parent 1: protocol ip prio 1\n", i)
			}
			if len(slowTo) > 0 {
				fmt.Fprintf(&tc, "qdisc change dev %s parent 1:2 handle 20: netem limit 10000 %s rate 10gbit\n", i, slow)
				for _, ip := range slowTo {
					fmt.Fprintf(&tc, "filter add dev %s parent 1: protocol ip prio 1 u32 match ip dst %s/32 flowid 1:2\n", i, ip)
				}
			}
			// A jam plugs the queue as it starts and releases it as it
			// ends (not as it's applied again, to a restarted node).
			if in.jammed[key] {
				fmt.Fprintf(&tc, "filter del dev %s parent 1: protocol ip prio 2\n", i)
			}
			if len(jamTo) > 0 {
				if !in.jammed[key] {
					fmt.Fprintf(&tc, "qdisc change dev %s parent 1:3 handle 30: plug block\n", i)
				}
				for _, ip := range jamTo {
					fmt.Fprintf(&tc, "filter add dev %s parent 1: protocol ip prio 2 u32 match ip dst %s/32 flowid 1:3\n", i, ip)
				}
			} else if in.jammed[key] {
				fmt.Fprintf(&tc, "qdisc change dev %s parent 1:3 handle 30: plug release_indefinite\n", i)
			}
			// (Other zones' and regions' addresses, again: a restarted
			// node's may change.)
			if in.crossed[key] {
				fmt.Fprintf(&tc, "filter del dev %s parent 1: protocol ip prio 3\n", i)
			}
			for _, ip := range crossTo {
				fmt.Fprintf(&tc, "filter add dev %s parent 1: protocol ip prio 3 u32 match ip dst %s/32 flowid 1:4\n", i, ip)
			}
			for _, ip := range farTo {
				fmt.Fprintf(&tc, "filter add dev %s parent 1: protocol ip prio 3 u32 match ip dst %s/32 flowid 1:5\n", i, ip)
			}
		}
		var nerrs []error
		if ipt.Len() > 0 {
			nerrs = append(nerrs, inNetns(n, "/sbin/iptables-restore --noflush", "*filter\n"+ipt.String()+"COMMIT\n"))
		}
		if tc.Len() > 0 {
			nerrs = append(nerrs, inNetns(n, "/sbin/tc -batch -", tc.String()))
		}
		if hasNetns(pid) { // (else a node fault stopped it meanwhile)
			errs = append(errs, nerrs...)
		}
		in.set[key], in.dropped[key], in.slowed[key], in.jammed[key], in.crossed[key] = true, len(drop) > 0, len(slowTo) > 0, len(jamTo) > 0, len(crossTo)+len(farTo) > 0
	}
	return errors.Join(errs...)
}

// clear removes the baseline latency and what's left of the injector's
// netfilter rules from the network namespaces that still exist.
func (in *injector) clear() error {
	in.net.Lock()
	defer in.net.Unlock()
	now, err := current()
	if err != nil {
		return err
	}
	var errs []error
	for _, nd := range in.nodes {
		n := now[nd.owner().ID]
		pid := n.State.Pid
		if s := n.State.Status; s != "running" && s != "paused" || !in.set[netKey(pid)] || !hasNetns(pid) {
			continue
		}
		var tc strings.Builder
		for _, i := range interfaces(pid) {
			fmt.Fprintf(&tc, "qdisc del dev %s root\n", i)
		}
		nerrs := []error{inNetns(n, "/sbin/tc -batch -", tc.String()),
			inNetns(n, "/sbin/iptables-restore --noflush", "*filter\n:TIMESKIP - [0:0]\nCOMMIT\n")}
		if hasNetns(pid) {
			errs = append(errs, nerrs...)
		}
	}
	return errors.Join(errs...)
}

// interfaces are the network interfaces, less lo, in the network namespace
// of process pid.
func interfaces(pid int) []string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	var l []string
	lines := strings.Split(string(b), "\n") // (none if the process is gone)
	for _, line := range lines[min(2, len(lines)):] {
		if name, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && name != "lo" {
			l = append(l, name)
		}
	}
	slices.Sort(l)
	return l
}

// netKey names process pid's network namespace for as long as the process
// lives: a container started again may get a pid an earlier one had.
func netKey(pid int) string { return strconv.Itoa(pid) + "@" + procStart(pid) }

// hasNetns is whether process pid still has its network namespace: not if
// it exited, even while it's a zombie, which signals still reach.
func hasNetns(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
	return err == nil
}

// inNetns runs command, with input, in container c's network namespace.
func inNetns(c container, command, input string) error {
	cmd := exec.Command("/usr/bin/nsenter", append([]string{"-t", strconv.Itoa(c.State.Pid), "-n"}, strings.Fields(command)...)...)
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s: %v: %s", c.Name, strings.Fields(command)[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// node pauses, stops, kills or throttles nodes (resilient: only pauses
// and throttles), as pick picks them. Test commands running in them
// pause or are killed with them (see downed).
func (in *injector) node(rng *rand.Rand, start func()) (*protocol.Fault, func() error, error) {
	kinds := []int{0, 1, 2, 3}
	if in.level == "resilient" {
		kinds = []int{0, 3}
	}
	kind := kinds[rng.IntN(len(kinds))]
	scope, levels, err := in.pick(rng, []string{"pause", "stop", "kill", "throttle"}[kind])
	if err != nil {
		return nil, nil, err
	}
	all := slices.Concat(levels...)
	if len(all) == 0 {
		return nil, nil, nil
	} else if kind == 3 {
		f, heal, err := in.throttle(rng, all)
		if f != nil && scope != "" {
			f.Detail = scope + ", " + f.Detail
		}
		return f, heal, err
	}
	f := &protocol.Fault{}
	var do, undo func(c container) error
	power := false // (a kill that's a power loss)
	switch kind {
	case 0:
		f.Kind, f.Detail = "pause", "docker pause"
		do = func(c container) error {
			in.r.pausing(c, true)
			err := call("POST", "/containers/"+c.ID+"/pause", nil, nil)
			if err != nil {
				in.r.pausing(c, false)
			}
			return err
		}
		undo = func(c container) error {
			err := call("POST", "/containers/"+c.ID+"/unpause", nil, nil)
			in.r.pausing(c, false)
			if err != nil && (strings.Contains(err.Error(), "is not paused") || exited(err)) {
				return nil // (it exited as it was paused, and maybe a restart policy started it again)
			}
			return err
		}
	case 1:
		f.Kind, f.Detail = "stop", "SIGTERM, then SIGKILL after 10 s"
		do = func(c container) error { return call("POST", "/containers/"+c.ID+"/stop?t=10", nil, nil) }
	default:
		f.Kind, f.Detail = "kill", "SIGKILL"
		do = func(c container) error { return call("POST", "/containers/"+c.ID+"/kill", nil, nil) }
		// (A draw only with power losses, so that other seeds' faults
		// stay as they were. A power loss freezes the containers first,
		// then cuts their power at once: see powerOff.)
		if power = in.power && rng.IntN(2) == 0; power {
			f.Detail = "power loss"
			do = func(c container) error {
				in.r.pausing(c, true) // (see runner.enter)
				err := call("POST", "/containers/"+c.ID+"/pause", nil, nil)
				if err != nil {
					in.r.pausing(c, false)
				}
				return err
			}
		}
	}
	if scope != "" {
		f.Detail = scope + ", " + f.Detail
	}
	if undo == nil { // (a stop or kill)
		undo = func(c container) error {
			err := in.restart(c)
			if err == errLater { // (still down)
				return nil
			}
			in.r.downed(c, false)
			return err
		}
	}
	var gone, signaled sync.Map
	n := exitCount()
	ok, err := each(all, func(c container) error {
		in.r.downed(c, f.Kind != "pause")
		// A stop's exit is its doing, whatever its code, if the process
		// takes the stop signal itself (else it's 143, 128+N for another
		// signal, or 137 after the timeout); a kill's is 137. Neither
		// counts as unexpected; a crash's exit before the stop's signal
		// does. The container is frozen from before that's read until
		// Docker has sent the signal (it reports the kill once it has),
		// so that an exec in between (an entrypoint exec'ing its server
		// as the stop comes) can't change what takes it. If it doesn't
		// freeze, or Docker reports no kill while it's frozen, what was
		// read may have changed by the signal: then any code after the
		// signal is the stop's, as in Antithesis.
		var sent chan bool
		ended := make(chan struct{})
		if f.Kind == "stop" {
			in.r.pausing(c, true) // (see runner.enter)
			thaw, ok := frozen(c)
			if code, takes := stopExits(c); takes || !ok {
				signaled.Store(c.ID, map[bool]string{true: code}[ok])
			}
			sent = make(chan bool, 1)
			go func() {
				k := ok && awaitKill(c.ID, n, ended)
				thaw()
				in.r.pausing(c, false)
				sent <- k
			}()
			start() // (it counts from the first SIGTERM, which Docker's stop sends at once)
		}
		err := do(c)
		if sent != nil {
			if err != nil { // (no signal coming)
				close(ended)
			}
			if !<-sent {
				signaled.Store(c.ID, "")
			}
		}
		if exited(err) { // (it exited since current)
			gone.Store(c.ID, true)
			err = nil
		}
		if _, g := gone.Load(c.ID); err != nil || g {
			in.r.downed(c, false)
		}
		return err
	})
	ok = slices.DeleteFunc(ok, func(c container) bool { _, g := gone.Load(c.ID); return g })
	if len(ok) == 0 && err == nil {
		return nil, nil, nil
	}
	f.Targets = [][]string{names(ok)}
	if power {
		err = errors.Join(err, in.powerOff(ok))
		for _, c := range ok {
			in.r.pausing(c, false)
		}
	} else if f.Kind == "kill" {
		_, werr := each(ok, func(c container) error {
			return call("POST", "/containers/"+c.ID+"/wait?condition=not-running", nil, nil)
		})
		err = errors.Join(err, werr)
	}
	if f.Kind == "stop" {
		codes := map[string]string{}
		signaled.Range(func(id, code any) bool { codes[id.(string)] = code.(string); return true })
		signaledExits(slices.DeleteFunc(slices.Clone(ok), func(c container) bool { _, s := codes[c.ID]; return !s }), codes, n)
	}
	done := map[string]bool{}
	for _, c := range ok {
		done[c.ID] = true
	}
	// Healing undoes what was done, and only that, level by level (a
	// container that shares a network starts once it's there again).
	heal := func() error {
		var errs []error
		for _, l := range levels {
			_, err := each(slices.DeleteFunc(slices.Clone(l), func(c container) bool { return !done[c.ID] }), undo)
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	if err != nil {
		return nil, nil, errors.Join(err, heal())
	}
	return f, heal, nil
}

// pick picks a node fault's targets among the nodes not excluded from
// faults of any of kinds: one three times in four, else two or more together
// (followers enough to lose a quorum, say); with zones or regions, a
// zone's or region's nodes one time in four. It returns the fault's scope
// ("zone NAME", "region NAME" or "") and the targets' containers that run
// (and whose network does), by hops from their namespace's owner.
func (in *injector) pick(rng *rand.Rand, kinds ...string) (string, [][]container, error) {
	targets := in.targets(kinds...)
	if len(targets) == 0 {
		return "", nil, nil
	}
	scope, inside := in.scope(rng, 4, targets, false)
	i, n := rng.IntN(len(targets)), len(targets)
	switch {
	case scope != "":
		targets = slices.DeleteFunc(targets, func(n *node) bool { return !inside(n) })
	case n > 1 && rng.IntN(4) == 0:
		others := slices.Delete(slices.Clone(targets), i, i+1)
		rng.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
		targets = append(others[:1+rng.IntN(n-1)], targets[i])
		slices.SortFunc(targets, func(a, b *node) int { return strings.Compare(a.owner().Name, b.owner().Name) })
	default:
		targets = targets[i : i+1]
	}
	now, err := current()
	if err != nil {
		return "", nil, err
	}
	var levels [][]container
	up := map[string]bool{}
	for _, n := range targets {
		for i, l := range n.levels {
			if len(levels) <= i {
				levels = append(levels, nil)
			}
			for _, c := range l {
				on, _ := strings.CutPrefix(c.HostConfig.NetworkMode, "container:")
				if now[c.ID].State.Status == "running" && (i == 0 || up[on]) {
					up[c.ID] = true
					levels[i] = append(levels[i], c)
				}
			}
		}
	}
	return scope, levels, nil
}

func zoneOf(n *node) string   { return n.zone }
func regionOf(n *node) string { return n.region }

// groups are the nodes' zones or regions (by of), sorted.
func (in *injector) groups(of func(*node) string) (l []string) {
	for _, n := range in.nodes {
		if g := of(n); g != "" && !slices.Contains(l, g) {
			l = append(l, g)
		}
	}
	slices.Sort(l)
	return l
}

// scope, when the nodes are in two zones or more, or two regions, one time
// in odds picks a zone or region (each half the time, if both) with some of
// targets (and, if apart, some outside it): a fault's scope, "zone NAME" or
// "region NAME", and whether a node is in it; else "".
func (in *injector) scope(rng *rand.Rand, odds int, targets []*node, apart bool) (string, func(*node) bool) {
	what, of, gs := "zone", zoneOf, in.groups(zoneOf)
	rs := in.groups(regionOf)
	if len(gs) < 2 && len(rs) < 2 || rng.IntN(odds) > 0 {
		return "", nil
	}
	if len(rs) > 1 && (len(gs) < 2 || rng.IntN(2) == 0) {
		what, of, gs = "region", regionOf, rs
	}
	gs = slices.DeleteFunc(gs, func(g string) bool {
		inside := slices.ContainsFunc(targets, func(n *node) bool { return of(n) == g })
		outside := slices.ContainsFunc(targets, func(n *node) bool { return of(n) != g })
		return !inside || apart && !outside
	})
	if len(gs) == 0 {
		return "", nil
	}
	g := gs[rng.IntN(len(gs))]
	return what + " " + g, func(n *node) bool { return of(n) == g }
}

// targets are the nodes that aren't excluded from faults of any of kinds.
func (in *injector) targets(kinds ...string) []*node {
	return slices.DeleteFunc(slices.Clone(in.nodes), func(n *node) bool {
		return slices.ContainsFunc(kinds, func(k string) bool { return n.excluded[k] })
	})
}

// netOwner is the container whose network namespace c is in (network mode
// container:ID, as for a service:NAME), and how many such hops away; c
// itself, 0 if it has its own.
func netOwner(c container, byID map[string]container) (container, int) {
	for hops := 0; ; hops++ {
		id, ok := strings.CutPrefix(c.HostConfig.NetworkMode, "container:")
		if !ok || hops > len(byID) {
			return c, hops
		}
		c = byID[id]
	}
}

// names are the containers' names.
func names(cs []container) (l []string) {
	for _, c := range cs {
		l = append(l, c.Name)
	}
	return l
}

// owners are the names of the nodes' owners.
func owners(ns []*node) (l []string) {
	for _, n := range ns {
		l = append(l, n.owner().Name)
	}
	return l
}

// each runs do on the containers at once, and returns those it succeeded on
// and the errors.
func each(cs []container, do func(container) error) (ok []container, err error) {
	errs := make([]error, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Go(func() { errs[i] = do(c) })
	}
	wg.Wait()
	for i, c := range cs {
		if errs[i] == nil {
			ok = append(ok, c)
		}
	}
	return ok, errors.Join(errs...)
}

// exited is whether err is Docker refusing to pause, unpause, stop or kill
// a container because it exited (or a restart policy is restarting it)
// since the fault picked it: the daemon says it's not running, or the
// runtime that it's stopped, when the daemon hasn't noticed yet.
// killWait kills container c and waits until it's no longer running.
func killWait(c container) error {
	err := call("POST", "/containers/"+c.ID+"/kill", nil, nil)
	if err == nil || exited(err) { // (it exited meanwhile)
		err = call("POST", "/containers/"+c.ID+"/wait?condition=not-running", nil, nil)
	}
	return err
}

func exited(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "not running") || strings.Contains(m, "stopped container") ||
		strings.Contains(m, "is restarting") || strings.Contains(m, "already finished")
}

// restart starts a container that a fault stopped (or killed) again, with
// the network's faults. (Docker refuses to start a paused container: it
// runs, as another fault's heal started it again, and a pause holds it.)
func (in *injector) restart(c container) error {
	d := disks[c.ID]
	if d != nil { // (not while its disk is down)
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	if err := call("POST", "/containers/"+c.ID+"/start", nil, nil); err != nil && !strings.Contains(err.Error(), "cannot start a paused container") {
		// (On a failing or full disk, Docker may fail to start it: it's
		// started as the disk heals.)
		if d != nil && d.faulty > 0 {
			say("fault: %s starts once its disk heals (%v)", c.Name, err)
			d.later = append(d.later, c)
			return errLater
		}
		return err
	}
	return in.reapply()
}

// errLater is restart's when a container is to start as its disk heals.
var errLater = errors.New("later")

// logUniform is a random duration from lo to hi, as likely in each power of
// ten: short ones as often as long ones.
func logUniform(rng *rand.Rand, lo, hi time.Duration) time.Duration {
	return time.Duration(logUniformF(rng, float64(lo), float64(hi)))
}

func logUniformF(rng *rand.Rand, lo, hi float64) float64 {
	return math.Exp(math.Log(lo) + rng.Float64()*(math.Log(hi)-math.Log(lo)))
}

// stopFaults is ANTITHESIS_STOP_FAULTS, the guest agent run in a container
// as faultsInCtr/stop-faults SECONDS: it asks the injector, if faults are
// being injected, to stop them for that long, and returns once they're
// healed.
func stopFaults() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: $ANTITHESIS_STOP_FAULTS SECONDS")
		os.Exit(2)
	}
	if s, err := strconv.ParseFloat(os.Args[1], 64); err != nil || s < 0 {
		fmt.Fprintf(os.Stderr, "ANTITHESIS_STOP_FAULTS: %q isn't a number of seconds\n", os.Args[1])
		os.Exit(2)
	}
	c, err := net.Dial("unix", faultsInCtr+"/faults.sock")
	if err != nil {
		return // no faults
	}
	fmt.Fprintln(c, os.Args[1])
	c.Read(make([]byte, 16))
}
