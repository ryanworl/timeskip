//go:build linux

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// CPU faults limit cgroups' CPU bandwidth (cpu.max): a quota of CPU time
// per period, the kernel's and Docker's default 100 ms. A throttle (a node
// fault) limits each of its targets' containers; modulation varies the
// speed of every container together, through the cgroup they're all in
// (Docker's). A cgroup that used its quota waits for its next period, whose
// timer the kernel skips to when nothing else runs: throttled work takes
// guest time as it would on a slower CPU, but no wall time.
const cpuPeriod = 100 * time.Millisecond

// A cgroupUse is the CPU time (µs) a cgroup used, and which cgroup (its
// inode: a container started again gets a new one).
type cgroupUse struct {
	us, ino uint64
}

// cgroupCPU is what each running container's cgroup used, by container
// ID: the usage_usec of the cgroups Docker makes, docker/ID.
func cgroupCPU() map[string]cgroupUse {
	m := map[string]cgroupUse{}
	stats, _ := filepath.Glob("/sys/fs/cgroup/docker/*/cpu.stat")
	for _, f := range stats {
		var st syscall.Stat_t
		b, _ := os.ReadFile(f)
		line, _, _ := strings.Cut(string(b), "\n")
		v, ok := strings.CutPrefix(line, "usage_usec ")
		if us, err := strconv.ParseUint(v, 10, 64); ok && err == nil && syscall.Stat(filepath.Dir(f), &st) == nil {
			m[filepath.Base(filepath.Dir(f))] = cgroupUse{us, st.Ino}
		}
	}
	return m
}

// cgroupDir is the directory of the cgroup (v2) of container c's main
// process.
func cgroupDir(c container) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", c.State.Pid))
	if err != nil {
		return "", err
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if !ok {
		return "", fmt.Errorf("%s: no cgroup v2 in /proc/%d/cgroup", c.Name, c.State.Pid)
	}
	return filepath.Join("/sys/fs/cgroup", p), nil
}

// cpuLimit is the cpu.max that limits a cgroup whose cpu.max was was to
// share of a CPU, or was if that's less.
func cpuLimit(was string, share float64) string {
	q, p, _ := strings.Cut(was, " ")
	quota, err1 := strconv.ParseFloat(q, 64)
	period, err2 := strconv.ParseFloat(p, 64)
	if err1 == nil && err2 == nil && quota/period <= share {
		return was
	}
	return fmt.Sprintf("%d %d", int64(share*float64(cpuPeriod.Microseconds())), cpuPeriod.Microseconds())
}

// A cpuCap limits cgroups' CPU bandwidth (cpu.max: "QUOTA PERIOD" in µs,
// or "max PERIOD"), and lifts the limits.
type cpuCap struct{ dirs, was []string }

// add takes cgroup dir in, and says whether it could (not if it's gone).
func (cc *cpuCap) add(dir string) bool {
	b, err := os.ReadFile(dir + "/cpu.max")
	if err == nil {
		cc.dirs, cc.was = append(cc.dirs, dir), append(cc.was, strings.TrimSpace(string(b)))
	}
	return err == nil
}

// set limits each cgroup to share of a CPU (see cpuLimit).
func (cc *cpuCap) set(share float64) error {
	var errs []error
	for i, d := range cc.dirs {
		errs = append(errs, writeCPUMax(d, cpuLimit(cc.was[i], share)))
	}
	return errors.Join(errs...)
}

// restore lifts the limits.
func (cc *cpuCap) restore() error {
	var errs []error
	for i, d := range cc.dirs {
		errs = append(errs, writeCPUMax(d, cc.was[i]))
	}
	return errors.Join(errs...)
}

// writeCPUMax writes v to cgroup dir's cpu.max, unless the cgroup is gone (its
// container stopped).
func writeCPUMax(dir, v string) error {
	if err := os.WriteFile(dir+"/cpu.max", []byte(v), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// throttle limits each of the containers (a node fault's targets') to
// the same share of a CPU, 5% to 50%.
func (in *injector) throttle(rng *rand.Rand, cs []container) (*protocol.Fault, func() error, error) {
	share := logUniformF(rng, 0.05, 0.5)
	var cc cpuCap
	var ok []container
	for _, c := range cs {
		if dir, err := cgroupDir(c); err == nil && cc.add(dir) { // (else it exited since current)
			ok = append(ok, c)
		}
	}
	if err := cc.set(share); err != nil {
		return nil, nil, errors.Join(err, cc.restore())
	}
	if len(ok) == 0 {
		return nil, nil, nil
	}
	f := &protocol.Fault{Kind: "throttle", Detail: fmt.Sprintf("%.0f%% of a CPU each", share*100), Targets: [][]string{names(ok)}}
	return f, cc.restore, nil
}

// modulate varies the CPU speed of every container, all together: every 0.1
// to 1 s, a new speed, 10% to 100% of the CPU, for the cgroups the nodes'
// containers are in (Docker's), until healed.
func (in *injector) modulate(rng *rand.Rand, _ func()) (*protocol.Fault, func() error, error) {
	// (A generator of its own for the changes, so that the schedule's draws
	// don't depend on how many there were.)
	mrng := rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64()))
	now, err := current()
	if err != nil {
		return nil, nil, err
	}
	var cc cpuCap
	for _, n := range in.nodes {
		for _, c := range slices.Concat(n.levels...) {
			if c = now[c.ID]; c.State.Status != "running" {
				continue
			}
			if dir, err := cgroupDir(c); err == nil && filepath.Dir(dir) != "/sys/fs/cgroup" && !slices.Contains(cc.dirs, filepath.Dir(dir)) {
				cc.add(filepath.Dir(dir))
			}
		}
	}
	if len(cc.dirs) == 0 {
		return nil, nil, nil
	}
	if err := cc.set(logUniformF(mrng, 0.1, 1)); err != nil {
		return nil, nil, errors.Join(err, cc.restore())
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			t := time.NewTimer(logUniform(mrng, 100*time.Millisecond, time.Second))
			select {
			case <-stop:
				t.Stop()
				return
			case <-t.C:
			}
			if err := cc.set(logUniformF(mrng, 0.1, 1)); err != nil {
				in.errorf("CPU modulation: %v", err)
				return
			}
		}
	}()
	heal := func() error {
		close(stop)
		<-done
		return cc.restore()
	}
	f := &protocol.Fault{Kind: "modulation", Detail: "the CPU's speed for every container, 10% to 100%, changing every 0.1 to 1 s"}
	return f, heal, nil
}
