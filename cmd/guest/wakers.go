//go:build linux

package main

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Idle time is skipped only while every thread sleeps, so what a timeline
// takes is the CPU time its processes use and their wake-ups, each of which
// ends a skip. The kernel counts both for every thread anyway: the guest
// reads them at the end of setup (so that every timeline starts with those
// counts, in the setup checkpoint) and as the timeline ends, and does
// nothing in between.

// A procCount is a process's counts: its start (clock ticks since boot: a
// pid used again is another process), its live threads' voluntary context
// switches by thread ID (each a sleep, ended by a wake-up), and the CPU
// time its threads used, exited ones' included, in clock ticks.
type procCount struct {
	start, ticks uint64
	switches     map[int]threadCount
	name         string // its command, or a kernel thread's in brackets
}

// A threadCount is a thread's start (as a process's: a thread ID used
// again is another thread) and its voluntary context switches.
type threadCount struct{ start, n uint64 }

// setupCounts are the processes' counts at the end of setup, and when.
var setupCounts struct {
	procs map[int]procCount
	at    time.Duration
}

const clockTick = 10 * time.Millisecond // (USER_HZ)

// procCounts reads every process's counts.
func procCounts() map[int]procCount {
	m := map[int]procCount{}
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		name, f := stat("/proc/" + d.Name() + "/stat")
		if f == nil {
			continue
		}
		flags, _ := strconv.ParseUint(f[6], 10, 64)
		utime, _ := strconv.ParseUint(f[11], 10, 64)
		stime, _ := strconv.ParseUint(f[12], 10, 64)
		start, _ := strconv.ParseUint(f[19], 10, 64)
		if flags&0x00200000 != 0 { // PF_KTHREAD
			name = "[" + name + "]"
		}
		c := procCount{start: start, ticks: utime + stime, switches: map[int]threadCount{}, name: name}
		tasks, _ := os.ReadDir("/proc/" + d.Name() + "/task")
		for _, t := range tasks {
			tid, _ := strconv.Atoi(t.Name())
			dir := "/proc/" + d.Name() + "/task/" + t.Name()
			_, tf := stat(dir + "/stat")
			b, _ := os.ReadFile(dir + "/status")
			if _, v, ok := strings.Cut(string(b), "\nvoluntary_ctxt_switches:"); ok && tf != nil {
				v, _, _ = strings.Cut(v, "\n")
				var tc threadCount
				tc.start, _ = strconv.ParseUint(tf[19], 10, 64)
				tc.n, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
				c.switches[tid] = tc
			}
		}
		m[pid] = c
	}
	return m
}

// stat is a process's or thread's command and the fields of its stat file
// after it (f[0] is field 3, the state), nil if it's gone.
func stat(path string) (string, []string) {
	b, err := os.ReadFile(path)
	i := strings.LastIndexByte(string(b), ')') // (after the command, which may hold anything)
	if err != nil || i < 0 {
		return "", nil
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", nil
	}
	return string(b[strings.IndexByte(string(b), '(')+1 : i]), f
}

// countSetup takes the processes' counts at the end of setup.
func countSetup() {
	setupCounts.procs, setupCounts.at = procCounts(), uptime()
}

// wakers are the five processes of the containers (cs) and the three of
// the VM's own (Docker's, the guest agent, kernel threads) that woke up
// most since setup (the first_ command included), of those running now,
// with the CPU time they used.
func wakers(cs []container) []protocol.Waker {
	now := uptime()
	var l []protocol.Waker
	for pid, c := range procCounts() {
		was, ok := setupCounts.procs[pid]
		if ok && was.start != c.start {
			was, ok = procCount{}, false
		}
		w := protocol.Waker{Process: c.name, PID: pid, CPUMs: int64(c.ticks-was.ticks) * clockTick.Milliseconds()}
		for tid, t := range c.switches {
			w.Wakeups += int64(t.n)
			if b := was.switches[tid]; b.start == t.start { // (a thread since setup: from 0)
				w.Wakeups -= int64(b.n)
			}
		}
		if w.Wakeups == 0 {
			continue
		}
		since := setupCounts.at
		if !ok {
			since = max(since, time.Duration(c.start)*clockTick)
		}
		w.EveryMs = (now - since).Milliseconds() / w.Wakeups
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup"); err == nil {
			id := filepath.Base(strings.TrimSpace(string(b)))
			if i := slices.IndexFunc(cs, func(c container) bool { return c.ID == id }); i >= 0 {
				w.Container = cs[i].Name
			}
		}
		l = append(l, w)
	}
	most := func(a, b protocol.Waker) int {
		return cmp.Or(cmp.Compare(b.Wakeups, a.Wakeups), cmp.Compare(b.CPUMs, a.CPUMs), cmp.Compare(a.PID, b.PID))
	}
	slices.SortFunc(l, most)
	var ctrs, vm []protocol.Waker
	for _, w := range l {
		if w.Container != "" && len(ctrs) < 5 {
			ctrs = append(ctrs, w)
		} else if w.Container == "" && len(vm) < 3 {
			vm = append(vm, w)
		}
	}
	l = append(ctrs, vm...)
	slices.SortFunc(l, most)
	return l
}
