//go:build linux

package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Clock skips (resilient and high_availability, as in Antithesis): the
// VM's wall clock, CLOCK_REALTIME, jumps forward or back while the
// monotonic clocks (CLOCK_MONOTONIC, CLOCK_BOOTTIME) run on. The VM has one
// wall clock, every container's, as Antithesis's virtual time is every
// node's: a skip targets no node, and excluding any node from clock skips
// ("clock" in com.antithesis.exclude_from_faults)
// turns them off for the whole VM. timeskip's own
// timekeeping is monotonic, so skips don't move it: uptime, Go's timers and
// the monotonic reading of time.Now that comparisons and time.Until use.

// skip steps CLOCK_REALTIME by d, at once and exactly (adjtimex's
// ADJ_SETOFFSET, as clock_settime would, without the time between reading
// the clock and setting it).
func skip(d time.Duration) error {
	const adjSetOffset, adjNano = 0x0100, 0x2000
	sec, nsec := int64(d/time.Second), int64(d%time.Second)
	if nsec < 0 { // (the nanoseconds are positive, the seconds signed)
		sec, nsec = sec-1, nsec+int64(time.Second)
	}
	tx := syscall.Timex{Modes: adjSetOffset | adjNano, Time: syscall.Timeval{Sec: sec, Usec: nsec}}
	_, err := syscall.Adjtimex(&tx)
	return err
}

// clock makes clock skips until the injector stops, on a schedule of their
// own: each gap/100 to gap after the last, the wall clock jumps forward or
// back by 10 ms to 30 s, half the time for 0.1 to 30 s, then back, and
// else for good. Skips stack. Unlike the other faults they go on while
// ANTITHESIS_STOP_FAULTS holds faults off, as Antithesis's do; when the
// injector stops, the temporary skips in effect are reverted.
func (in *injector) clock(rng *rand.Rand) {
	type temporary struct {
		due   time.Time // to revert it
		d, at time.Duration
		f     protocol.Fault
	}
	var temps []temporary // by due
	record := func(f protocol.Fault) {
		in.r.mu.Lock()
		in.r.faults = append(in.r.faults, f)
		in.r.mu.Unlock()
	}
	revert := func(t temporary) {
		if err := skip(-t.d); err != nil {
			in.errorf("reverting a clock skip: %v", err)
		}
		t.f.Ms = (uptime() - t.at).Milliseconds()
		say("fault: reverted the skip (%s) after %v", t.f.Detail, time.Duration(t.f.Ms)*time.Millisecond)
		record(t.f)
	}
	defer func() {
		for _, t := range temps {
			revert(t)
		}
	}()
	next := time.Now().Add(logUniform(rng, in.gap/100, in.gap))
	for {
		due, reverting := next, len(temps) > 0 && temps[0].due.Before(next)
		if reverting {
			due = temps[0].due
		}
		redraw := func() time.Duration { // (the next skip's wait; not a revert's, a fault's end)
			next = time.Now().Add(logUniform(rng, in.gap/100, in.gap))
			if reverting {
				return time.Until(due)
			}
			return time.Until(next)
		}
		if !in.sleep(time.Until(due), false, redraw) {
			return
		}
		if reverting {
			revert(temps[0])
			temps = temps[1:]
			continue
		}
		d := logUniform(rng, 10*time.Millisecond, 30*time.Second).Round(time.Millisecond)
		dir := "forward"
		if rng.IntN(2) == 0 {
			d, dir = -d, "back"
		}
		lasts, temp := logUniform(rng, 100*time.Millisecond, 30*time.Second).Round(time.Millisecond), rng.IntN(2) == 0
		next = next.Add(logUniform(rng, in.gap/100, in.gap))
		at := uptime()
		f := protocol.Fault{Kind: "skip", Detail: fmt.Sprintf("%s %v, permanent", dir, d.Abs()), AtMs: (at - in.r.t0).Milliseconds()}
		if temp {
			f.Detail = fmt.Sprintf("%s %v, for %v", dir, d.Abs(), lasts)
		}
		if err := skip(d); err != nil {
			in.errorf("clock skip: %v", err)
			return
		}
		say("fault: skip (%s)", f.Detail)
		if !temp {
			record(f)
			continue
		}
		t := temporary{due.Add(lasts), d, at, f}
		i, _ := slices.BinarySearchFunc(temps, t, func(a, b temporary) int { return a.due.Compare(b.due) })
		temps = slices.Insert(temps, i, t)
	}
}
