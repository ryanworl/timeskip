//go:build linux

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The Docker Engine API, over dockerd's socket: cheaper than a docker CLI
// process per call on one vCPU.
var engine = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
}}}

// call makes an API request with a JSON body (if any) and decodes a JSON
// answer into out (if any).
func call(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	resp, err := do(method, path, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func do(method, path, ctype string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://docker"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := engine.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified { // (304: already so)
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("docker %s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return resp, nil
}

// exits are the project's containers' exits, from Docker's die events, and
// the signals Docker sent them, from its kill events.
var exits struct {
	sync.Mutex
	once  sync.Once
	l     []exit
	marks map[string]int // where each mark came, in l (see mark)
}

var markSeq atomic.Uint64

// exit is a container's exit, fault if a fault caused it (see claimExit
// and signaledExits); or, if kill, a signal Docker sent it.
type exit struct {
	id, name, code string
	fault, kill    bool
	taken          bool // matched to a crash (see takeExit and takeSignaledExit)
	boot           int  // a disk fault's reboot's kill: the kernel log's sequence number before it, plus 1 (see markBoot)
}

// unexpected is whether e counts as Antithesis's "No unexpected container
// exits" counts them: with an exit code other than 0, 137 (SIGKILL) or 143
// (SIGTERM), and not caused by a fault, since a process may exit with
// another code on SIGTERM (as Antithesis filters the exits of containers it
// stopped). So the stops and kills of node faults and restarts don't count,
// and a crash does, restarted or not.
func (e exit) unexpected() bool {
	return !e.fault && !e.kill && e.code != "0" && e.code != "137" && e.code != "143"
}

func (e exit) String() string { return e.name + " exited with code " + e.code }

// watchExits records the exits from now on, from Docker's events.
func watchExits() {
	exits.once.Do(func() {
		// (The project's containers' events, and marks: volumes' (see
		// markEvents), which have no labels, so the project's label is
		// matched here.)
		f, _ := json.Marshal(map[string][]string{"type": {"container", "volume"}, "event": {"die", "kill", "exec_create", "create"}})
		resp, err := do("GET", "/events?filters="+url.QueryEscape(string(f)), "", nil)
		check(err)
		go func() {
			for d := json.NewDecoder(resp.Body); ; {
				var e struct {
					Type   string
					Action string
					Actor  struct {
						ID         string
						Attributes map[string]string
					}
				}
				if d.Decode(&e) != nil {
					return
				}
				a := e.Actor.Attributes
				if e.Type == "container" && a["com.docker.compose.project"] != project {
					continue
				}
				exits.Lock()
				switch {
				case e.Type == "volume":
					if tok, ok := strings.CutPrefix(e.Actor.ID, "timeskip-mark-"); ok && e.Action == "create" {
						if exits.marks == nil {
							exits.marks = map[string]int{}
						}
						exits.marks[tok] = len(exits.l)
					}
				case e.Action == "die" || e.Action == "kill":
					exits.l = append(exits.l, exit{id: e.Actor.ID, name: a["name"], code: a["exitCode"], kill: e.Action == "kill"})
				default: // ("exec_create: CMD ARGS", as a test command's; "create")
					if tok, ok := strings.CutPrefix(e.Action, "exec_create: true timeskip-mark "); ok {
						if exits.marks == nil {
							exits.marks = map[string]int{}
						}
						exits.marks[tok] = len(exits.l)
					}
				}
				exits.Unlock()
			}
		}()
	})
}

// exitsSince are the unexpected exits after the first n exits.
func exitsSince(n int) []string {
	exits.Lock()
	defer exits.Unlock()
	var l []string
	for _, e := range exits.l[n:] {
		if e.unexpected() {
			l = append(l, e.String())
		}
	}
	return l
}

// exitCount is how many exits there were so far.
func exitCount() int {
	exits.Lock()
	defer exits.Unlock()
	return len(exits.l)
}

// claimExits marks the unexpected exits of container id after the first
// n, but those with exit codes keep lists, as a fault's doing.
func claimExits(id string, n int, keep ...string) {
	exits.Lock()
	defer exits.Unlock()
	for i := n; i < len(exits.l); i++ {
		if e := &exits.l[i]; e.id == id && e.unexpected() && !slices.Contains(keep, e.code) {
			e.fault = true
		}
	}
}

// claimDeath marks the exit of container c that its state shows, an
// error's (see errExit), and its other exits after the first n, as a
// fault's doing (see claimExits), once they're recorded: Docker logs the
// die event before its state shows the exit (both under the container's
// lock), so a mark made since (see markEvents) comes after the events of
// each of c's runs so far that Docker delivered. Without a mark, it claims
// none, and says so: the exit stays an unexpected one.
func claimDeath(c container, n int) {
	if _, ok := markEvents(); !ok {
		say("fault: no mark in Docker's events for %s's exit: it isn't claimed (an unexpected exit)", c.Name)
		return
	}
	claimExits(c.ID, n, "132", "139")
}

// takeExit marks the first exit of container id after the first n that no
// crash took yet and that match accepts as taken by a crash, and is whether
// there was one. (An exit that matches no crash stays for another fault's.)
func takeExit(id string, n int, match func(exit) bool) bool {
	exits.Lock()
	defer exits.Unlock()
	for i := n; i < len(exits.l); i++ {
		if e := &exits.l[i]; e.id == id && !e.kill && !e.taken && match(*e) {
			e.taken = true
			return true
		}
	}
	return false
}

// mark is how many exits had been recorded once every event Docker logged
// for container c before now was (an index in exits.l), and whether there
// was a mark: Docker logs its events in order, and mark has it log one of
// its own, creating an exec in c that never starts. (Docker logs an exit
// before it can start the container again, so that, with c frozen, the
// exits after the mark are of c's current run.) Docker drops an event for
// a subscriber that takes none for 100 ms, though, with 1024 waiting: the
// mark orders the events that were delivered.
func mark(c container) (int, bool) {
	tok := strconv.FormatUint(markSeq.Add(1), 10)
	if call("POST", "/containers/"+c.ID+"/exec", map[string][]string{"Cmd": {"true", "timeskip-mark", tok}}, nil) != nil {
		return 0, false
	}
	return awaitMark(tok)
}

// markEvents is as mark, but for every event Docker logged before now, of
// any container, running or not: Docker logs a volume's create event
// before it returns, and markEvents creates one, timeskip-mark-HEX, a
// random name nothing else has, and removes it again (its destroy event
// isn't watched) once its event has shown that this create made it.
// Docker creates nothing for a name a volume has, and logs no event: then
// it's left alone. (A workload's own volume listing, or a prune, may see
// it meanwhile.)
func markEvents() (int, bool) {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	if call("POST", "/volumes/create", map[string]string{"Name": "timeskip-mark-" + tok}, nil) != nil {
		return 0, false
	}
	i, ok := awaitMark(tok)
	if ok {
		if err := call("DELETE", "/volumes/timeskip-mark-"+tok, nil, nil); err != nil {
			say("fault: removing volume timeskip-mark-%s, a mark: %v", tok, err)
		}
	}
	return i, ok
}

// awaitMark waits up to 5 s for mark tok's event, and is how many exits
// had been recorded by then, and whether it came.
func awaitMark(tok string) (int, bool) {
	for t := time.Now(); time.Since(t) < 5*time.Second; time.Sleep(time.Millisecond) {
		exits.Lock()
		i, ok := exits.marks[tok]
		delete(exits.marks, tok)
		exits.Unlock()
		if ok {
			return i, true
		}
	}
	return 0, false
}

// takeSignaledExit marks the exit of container id that follows the first
// signal Docker sent it after the first n exits (a stop's: see
// signaledExits) as taken by a crash, if its code is code and no crash took
// it yet, and is whether it did.
func takeSignaledExit(id string, n int, code string) bool {
	exits.Lock()
	defer exits.Unlock()
	l := exits.l[n:]
	k := slices.IndexFunc(l, func(e exit) bool { return e.id == id && e.kill })
	if k < 0 {
		return false
	}
	i := slices.IndexFunc(l[k:], func(e exit) bool { return e.id == id && !e.kill })
	if i < 0 || l[k+i].taken || l[k+i].code != code {
		return false
	}
	l[k+i].taken = true
	return true
}

// markBoot marks container id's first exit after the first m, waiting up to
// 5 s for it, as a disk fault's reboot's kill, sent once the kernel's log
// had reached record bound: a crash logged by then may be one it cut short.
// Only if it's a SIGKILL's, 137: Docker takes a kill of a process that has
// exited already, and then the exit is the process's own.
func markBoot(id string, m, bound int) {
	for t := time.Now(); time.Since(t) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		exits.Lock()
		i := slices.IndexFunc(exits.l[m:], func(e exit) bool { return e.id == id && !e.kill })
		if i >= 0 && exits.l[m+i].code == "137" {
			exits.l[m+i].boot = bound + 1
		}
		exits.Unlock()
		if i >= 0 {
			return
		}
	}
}

// signaledExits marks the exit of each of the containers that follows the
// first signal Docker sent it after the first n exits (a stop fault's
// signal: Docker logs the signal before the exit it causes), as the
// fault's doing, if its code is the container's in codes ("": any code),
// waiting up to 5 s for Docker's events, or after its mark in marks (see
// mark), if it has one. An exit before the signal, a crash's that the stop
// came after, stays unexpected, as does one with another code.
func signaledExits(cs []container, codes map[string]string, n int, marks map[string]int) {
	for t := time.Now(); len(cs) > 0 && time.Since(t) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		exits.Lock()
		cs = slices.DeleteFunc(cs, func(c container) bool {
			l := exits.l[max(n, marks[c.ID]):]
			k := slices.IndexFunc(l, func(e exit) bool { return e.id == c.ID && e.kill })
			if k < 0 {
				return false
			}
			i := slices.IndexFunc(l[k:], func(e exit) bool { return e.id == c.ID && !e.fault && !e.kill })
			if i >= 0 && (codes[c.ID] == "" || l[k+i].code == codes[c.ID]) {
				l[k+i].fault = true
			}
			return i >= 0
		})
		exits.Unlock()
	}
}

// frozen freezes container c's cgroup until the returned func is called,
// and is whether it froze (cgroup.events says frozen 1) within 1 s; if
// not, it thaws it again at once. Frozen, its processes can't exec, and
// their signal dispositions stay as they are, while a stop reads them and
// Docker signals them (a signal to a frozen process is taken or dropped
// as it's sent, by them). none is whether it has no process (ok too): it
// exited, maybe as Docker hasn't noticed yet, so no signal is taken.
func frozen(c container) (thaw func(), ok, none bool) {
	thaw = func() {}
	var d container
	if call("GET", "/containers/"+c.ID+"/json", nil, &d) != nil {
		return thaw, false, false
	}
	if d.State.Pid == 0 {
		return thaw, true, true
	}
	dir, err := cgroupDir(d)
	if err == nil {
		err = os.WriteFile(dir+"/cgroup.freeze", []byte("1"), 0)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return thaw, true, true
	}
	if err != nil {
		return thaw, false, false
	}
	thaw = func() { os.WriteFile(dir+"/cgroup.freeze", []byte("0"), 0) }
	for t := time.Now(); time.Since(t) < time.Second; time.Sleep(time.Millisecond) {
		b, err := os.ReadFile(dir + "/cgroup.events")
		if errors.Is(err, fs.ErrNotExist) { // (it exited as it froze)
			return func() {}, true, true
		}
		if strings.Contains(string(b), "frozen 1") {
			return thaw, true, false
		}
	}
	thaw() // (a task that doesn't freeze, as in an uninterruptible sleep)
	return func() {}, false, false
}

// awaitKill is whether Docker reports a signal it sent container id after
// the first n exits within 5 s, waiting for it, but no longer than until
// ended is closed and there's none.
func awaitKill(id string, n int, ended <-chan struct{}) bool {
	for t := time.Now(); time.Since(t) < 5*time.Second; time.Sleep(time.Millisecond) {
		exits.Lock()
		k := slices.ContainsFunc(exits.l[n:], func(e exit) bool { return e.id == id && e.kill })
		exits.Unlock()
		select {
		case <-ended:
			return k
		default:
		}
		if k {
			return true
		}
	}
	return false
}

// stopExits is which exit codes of container c a stop may cause beyond 143
// and 137 (ok false: none), as Docker's stop sends its stop signal (SIGTERM,
// unless the image's STOPSIGNAL or the service's stop_signal names
// another, such as the postgres image's SIGINT): any code ("") if its
// process is running (not exited, nor a zombie) and takes the signal
// itself (see takes); else, for a signal other than SIGTERM, the code of a
// death by it, 128+N.
// With init: true, in a PID namespace of the container's own (Docker
// inserts no init with pid: host or another container's), any code:
// Docker signals its init (tini), which forwards the signal to its child
// only once it has taken it itself, after the stop's freeze ends, so what
// the child does with it can't be known beforehand; and init exits with
// the child's code. That is, while its child runs: once that has exited,
// the code is the child's own.
func stopExits(c container) (code string, ok bool) {
	var d container
	if call("GET", "/containers/"+c.ID+"/json", nil, &d) != nil || d.State.Pid == 0 {
		return "", false
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", d.State.Pid)); err != nil || zombie(b) {
		return "", false // (exited, as Docker may not have noticed yet)
	}
	if m := d.HostConfig.PidMode; d.HostConfig.Init && m != "host" && !strings.HasPrefix(m, "container:") {
		return "", mainChildRuns(d.State.Pid)
	}
	pid := d.State.Pid
	sig := stopSignal(d.Config.StopSignal)
	if sig != 0 && takes(pid, uint64(1)<<(sig-1)) {
		return "", true
	}
	if sig != 0 && sig != syscall.SIGTERM {
		return strconv.Itoa(128 + int(sig)), true
	}
	return "", false
}

// mainChildRuns is whether init process pid's main child, the one whose
// code it exits with, hasn't exited (as tini's, whatever children it
// adopted). That is its earliest child (by start time, then PID), which
// started before any process it left for init to adopt; unless init has
// just reaped it, and is about to exit with its code.
// (The kernel has no /proc/PID/task/TID/children: CONFIG_PROC_CHILDREN.)
func mainChildRuns(pid int) bool {
	ds, _ := os.ReadDir("/proc")
	var first []byte
	var start, firstPID uint64
	for _, e := range ds {
		n, err := strconv.ParseUint(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		b, _ := os.ReadFile("/proc/" + e.Name() + "/stat")
		i := bytes.LastIndex(b, []byte(") "))
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+2:]))
		if len(f) < 20 || f[1] != strconv.Itoa(pid) {
			continue
		}
		t, _ := strconv.ParseUint(f[19], 10, 64) // starttime
		if first == nil || t < start || t == start && n < firstPID {
			first, start, firstPID = b, t, n
		}
	}
	return first != nil && !zombie(first)
}

// zombie is whether a process's /proc/PID/stat says it exited (Z, X) or
// is exiting (PF_EXITING in its flags: still R or D in do_exit, which the
// cgroup freezer counts as frozen without stopping it).
// (The fields follow the command, in parentheses, which may hold ") ".)
func zombie(stat []byte) bool {
	i := bytes.LastIndex(stat, []byte(") "))
	if i < 0 {
		return true
	}
	f := strings.Fields(string(stat[i+2:]))
	if len(f) < 7 || f[0] == "Z" || f[0] == "X" {
		return true
	}
	flags, err := strconv.ParseUint(f[6], 10, 64)
	return err != nil || flags&0x4 != 0 // PF_EXITING
}

// takes is whether process pid is running (not exited, nor a zombie) and
// takes the signal of mask bit itself: with a handler (SigCgt), or by
// sigwait or a signalfd, as mongod does, even if it's ignored (SigIgn: the
// kernel delivers a blocked signal to sigwait whatever its disposition, as
// kernel/signal.c's sig_ignored checks blocked and real_blocked first). Both
// need it blocked: it's in some thread's SigBlk (each thread has its own mask;
// a signalfd reader's stays blocked), or a thread waits for it in
// rt_sigtimedwait, which unblocks the signals it waits for meanwhile, so
// that thread's set is read from the process's memory at the call's
// first argument. A thread that's running shows "running" in its syscall
// file, not a call, and a set that can't be read counts as not taking it.
// (A process that blocks it and never takes it is killed after the stop's
// timeout, 137, which counts anyway.) It reads each thread's files once,
// as a stop starts.
func takes(pid int, bit uint64) bool {
	field := func(b []byte, k string) string {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, k+":"); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	hex := func(b []byte, k string) uint64 { m, _ := strconv.ParseUint(field(b, k), 16, 64); return m }
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if state := field(b, "State"); err != nil || state == "" || state[0] == 'Z' || state[0] == 'X' {
		return false
	}
	if hex(b, "SigCgt")&bit != 0 {
		return true
	}
	nr := map[string]string{"arm64": "137", "amd64": "128"}[runtime.GOARCH] // (rt_sigtimedwait)
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	for _, t := range tasks {
		dir := fmt.Sprintf("/proc/%d/task/%s/", pid, t.Name())
		if b, _ := os.ReadFile(dir + "status"); hex(b, "SigBlk")&bit != 0 {
			return true
		}
		b, _ := os.ReadFile(dir + "syscall")
		if f := strings.Fields(string(b)); len(f) > 1 && f[0] == nr {
			if at, err := strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64); err == nil {
				var set [8]byte
				if f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid)); err == nil {
					n, _ := f.ReadAt(set[:], int64(at))
					f.Close()
					if n == 8 && binary.LittleEndian.Uint64(set[:])&bit != 0 {
						return true
					}
				}
			}
		}
	}
	return false
}

// stopSignal is the number of a container's stop signal as Docker names it
// ("SIGINT", "INT", "2" or "SIGRTMIN+3"; "": SIGTERM), or 0 for a name it
// doesn't know.
func stopSignal(name string) syscall.Signal {
	if name == "" {
		return syscall.SIGTERM
	}
	if n, err := strconv.Atoi(name); err == nil {
		if n > 0 && n <= 64 {
			return syscall.Signal(n)
		}
		return 0
	}
	name = strings.TrimPrefix(strings.ToUpper(name), "SIG")
	// (Docker's real-time signals: RTMIN is 34, as glibc keeps 32 and 33.)
	if v, ok := strings.CutPrefix(name, "RTMIN+"); ok {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 && k <= 30 {
			return syscall.Signal(34 + k)
		}
		return 0
	}
	if v, ok := strings.CutPrefix(name, "RTMAX-"); ok {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 && k <= 30 {
			return syscall.Signal(64 - k)
		}
		return 0
	}
	return map[string]syscall.Signal{
		"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "ILL": syscall.SIGILL,
		"TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT, "IOT": syscall.SIGIOT, "BUS": syscall.SIGBUS,
		"FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "SEGV": syscall.SIGSEGV,
		"USR2": syscall.SIGUSR2, "PIPE": syscall.SIGPIPE, "ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM,
		"STKFLT": syscall.SIGSTKFLT, "CHLD": syscall.SIGCHLD, "CLD": syscall.SIGCLD, "CONT": syscall.SIGCONT,
		"STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP, "TTIN": syscall.SIGTTIN, "TTOU": syscall.SIGTTOU,
		"URG": syscall.SIGURG, "XCPU": syscall.SIGXCPU, "XFSZ": syscall.SIGXFSZ, "VTALRM": syscall.SIGVTALRM,
		"PROF": syscall.SIGPROF, "WINCH": syscall.SIGWINCH, "IO": syscall.SIGIO, "POLL": syscall.SIGPOLL,
		"PWR": syscall.SIGPWR, "SYS": syscall.SIGSYS, "RTMIN": 34, "RTMAX": 64,
	}[name]
}

// container is what the agent needs of a container's inspect document.
type container struct {
	ID    string
	Name  string
	State struct {
		Status     string
		Pid        int
		ExitCode   int
		FinishedAt string // (RFC 3339)
		Health     *struct{ Status string }
	}
	Config struct {
		Labels     map[string]string
		StopSignal string
	}
	HostConfig struct {
		NetworkMode string
		Init        bool
		PidMode     string
	}
	Mounts          []mount
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

// A mount is a container's volume, bind mount or tmpfs.
type mount struct{ Type, Source, Name, Driver string }

// ips are the container's addresses, on each of its networks.
func (c container) ips() []string {
	var l []string
	for _, n := range c.NetworkSettings.Networks {
		if n.IPAddress != "" {
			l = append(l, n.IPAddress)
		}
	}
	slices.Sort(l)
	return l
}

// root is the container's filesystem as its processes see it, volumes
// included (none, if it doesn't run).
func (c container) root() string { return fmt.Sprintf("/proc/%d/root", c.State.Pid) }

// containers inspects the project's containers, all of them or service's.
func containers(service string) ([]container, error) {
	f := map[string][]string{"label": {"com.docker.compose.project=" + project}}
	if service != "" {
		f["label"] = append(f["label"], "com.docker.compose.service="+service)
	}
	fj, _ := json.Marshal(f)
	var list []struct{ ID string }
	if err := call("GET", "/containers/json?all=1&filters="+url.QueryEscape(string(fj)), nil, &list); err != nil {
		return nil, err
	}
	var cs []container
	for _, l := range list {
		var c container
		if err := call("GET", "/containers/"+l.ID+"/json", nil, &c); err != nil {
			return nil, err
		}
		c.Name = strings.TrimPrefix(c.Name, "/")
		cs = append(cs, c)
	}
	return cs, nil
}

// execIn runs path in a container as docker exec would, writing its output
// to out, and returns its exit code and the CPU time it used (and what it
// waited for). started gets the process's pid (in the
// VM) once it runs. It doesn't go through Docker: a docker exec costs about
// 57 ms of the VM's one CPU (dockerd, containerd, a shim and runc), most of
// a short test command's cost. nsenter enters the container's namespaces
// (but its cgroup namespace) and root, in its cgroup, as its user, in its
// working directory, with its environment. What only isolates is left out:
// its capability bounds, seccomp and AppArmor profiles.
func execIn(id, path string, out io.Writer, started func(pid int)) (int, time.Duration, error) {
	t, err := target(id)
	if err != nil {
		return 0, 0, err
	}
	// (nsenter opens -w's directory before it enters the mount namespace.
	// It starts as root with the gid and groups, and -S switches to the
	// uid: its -G would drop the groups.)
	cmd := exec.Command("nsenter", "-t", strconv.Itoa(t.pid), "-m", "-u", "-i", "-n", "-p", "-r",
		"-w"+t.dir, "-S", t.uid, "--", path)
	cmd.Env = t.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Gid: t.gid, Groups: t.groups}}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", t.pid)); err == nil {
		if cg, err := os.Open("/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(string(b), "0::"))); err == nil {
			defer cg.Close()
			cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, int(cg.Fd())
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return 0, 0, err
	}
	defer r.Close()
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		return 0, 0, err
	}
	if started != nil {
		started(cmd.Process.Pid)
	}
	exited, copied := make(chan int, 1), make(chan struct{})
	go func() {
		copyOutput(r, out, exited)
		close(copied)
	}()
	err = cmd.Wait()
	exited <- queued(r)
	r.SetReadDeadline(time.Now()) // (see copyOutput)
	<-copied
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), cpu, nil // (as runc reports it)
	}
	var x *exec.ExitError
	if err != nil && !errors.As(err, &x) {
		return 0, cpu, err
	}
	return cmd.ProcessState.ExitCode(), cpu, nil
}

// copyOutput copies a command's output from r to out until r ends, or once
// the command exited (r's read deadline, set to then, says so, and exited
// how much it left in the pipe), until that's taken too, and then 1 s more:
// output after it exited, from what it left running, isn't waited for, but
// what it wrote before is copied whole, however slowly out takes it.
func copyOutput(r *os.File, out io.Writer, exited <-chan int) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		} else if err != nil {
			return
		}
	}
	// (Some of it may be read already: then reads stop as the pipe empties.)
	r.SetReadDeadline(time.Time{})
	rc, err := r.SyscallConn()
	if err != nil {
		return
	}
	for left := <-exited; left > 0; {
		n := 0
		rc.Read(func(fd uintptr) bool {
			n, _ = syscall.Read(int(fd), buf[:min(left, len(buf))])
			return true // (r is non-blocking)
		})
		if n <= 0 {
			break
		}
		out.Write(buf[:n])
		left -= n
	}
	r.SetReadDeadline(time.Now().Add(time.Second))
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			return
		}
	}
}

// queued is how many bytes are in pipe r.
func queued(r *os.File) int {
	var n int32
	if rc, err := r.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n)))
		})
	}
	return int(n)
}

// An execTarget is what execIn needs of a running container: its main
// process (and when that started), and an exec's working directory (as the
// VM sees it), user, group, groups and environment.
type execTarget struct {
	pid             int
	start, dir, uid string
	gid             uint32
	groups          []uint32
	env             []string
}

// targets are the containers' exec targets, by container ID: a test command
// doesn't cost an inspect (dockerd's CPU and the agent's) until the
// container's main process changes.
var targets sync.Map

func target(id string) (*execTarget, error) {
	if t, ok := targets.Load(id); ok && procStart(t.(*execTarget).pid) == t.(*execTarget).start {
		return t.(*execTarget), nil
	}
	var c struct {
		State  struct{ Pid int }
		Config struct {
			Env                        []string
			User, WorkingDir, Hostname string
		}
		HostConfig struct{ GroupAdd []string }
	}
	if err := call("GET", "/containers/"+id+"/json", nil, &c); err != nil {
		return nil, err
	}
	if c.State.Pid == 0 {
		return nil, fmt.Errorf("container %s is not running", id)
	}
	root := fmt.Sprintf("/proc/%d/root", c.State.Pid)
	t := &execTarget{pid: c.State.Pid, start: procStart(c.State.Pid),
		dir: root + "/" + strings.TrimPrefix(cmp.Or(c.Config.WorkingDir, "/"), "/")}
	uid, gid, home, groups := lookupUser(root, c.Config.User, c.HostConfig.GroupAdd)
	t.uid = uid
	for _, g := range append([]string{gid}, groups...) { // (Docker's groups include the gid)
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("container %s: no group %s", id, g)
		}
		t.groups = append(t.groups, uint32(n))
	}
	t.gid = t.groups[0]
	// (As dockerd and runc make an exec's environment.)
	t.env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOSTNAME=" + c.Config.Hostname}
	for _, e := range c.Config.Env {
		k, _, _ := strings.Cut(e, "=")
		t.env = slices.DeleteFunc(t.env, func(x string) bool { return strings.HasPrefix(x, k+"=") })
		t.env = append(t.env, e)
	}
	if !slices.ContainsFunc(t.env, func(x string) bool { return strings.HasPrefix(x, "HOME=") }) {
		t.env = append(t.env, "HOME="+home)
	}
	targets.Store(id, t)
	return t, nil
}

// procStart is when a process started (in clock ticks since boot), or "" if
// there's no such process.
func procStart(pid int) string {
	if f := procStat(strconv.Itoa(pid)); f != nil {
		return f[19] // (field 22)
	}
	return ""
}

// procStat is /proc/PID/stat's fields from the third (after the command),
// or nil if there's no such process.
func procStat(pid string) []string {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
}

// lookupUser resolves a container's user (name or uid, and maybe :group
// or :gid) in its /etc/passwd and /etc/group, under root: its uid, gid,
// home directory and supplementary groups, as Docker sets them: the groups
// that list the user (unless a group is given) and groupAdd's.
// Like runc, an unknown numeric uid is fine; its gid is 0 and its home /.
func lookupUser(root, user string, groupAdd []string) (uid, gid, home string, groups []string) {
	u, g, _ := strings.Cut(user, ":")
	uid, gid, home = cmp.Or(u, "0"), "0", "/"
	name := ""
	if b, err := os.ReadFile(root + "/etc/passwd"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) >= 6 && (f[0] == uid || f[2] == uid) {
				name, uid, gid, home = f[0], f[2], f[3], f[5]
				break
			}
		}
	}
	b, _ := os.ReadFile(root + "/etc/group")
	var entries [][]string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Split(l, ":"); len(f) >= 4 {
			entries = append(entries, f)
		}
	}
	gidOf := func(g string) string {
		for _, f := range entries {
			if f[0] == g {
				return f[2]
			}
		}
		return g
	}
	if g != "" {
		gid = gidOf(g)
	} else if name != "" {
		for _, f := range entries {
			if slices.Contains(strings.Split(f[3], ","), name) {
				groups = append(groups, f[2])
			}
		}
	}
	for _, a := range groupAdd {
		groups = append(groups, gidOf(a))
	}
	return uid, gid, home, groups
}

// untar extracts a tar archive at dir in a container, with the archive's
// owners, as docker cp -a and docker compose watch do.
func untar(id, dir string, tarball io.Reader) error {
	resp, err := do("PUT", "/containers/"+id+"/archive?copyUIDGID=true&path="+url.QueryEscape(dir), "application/x-tar", tarball)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// load loads the images in a docker save archive.
func load(archive io.Reader) error {
	resp, err := do("POST", "/images/load?quiet=1", "application/x-tar", archive)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for d := json.NewDecoder(resp.Body); ; {
		var m struct{ Error string }
		if err := d.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

// compose runs Docker Compose (standalone: no docker CLI process in
// between), its output going to the console.
func compose(args ...string) error { return composeCtx(context.Background(), args...) }

// composeCtx runs Docker Compose until ctx is done, then kills it.
func composeCtx(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/local/libexec/docker/cli-plugins/docker-compose", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return fmt.Errorf("docker compose %s: %v", strings.Join(args, " "), err)
	}
	return nil
}
