package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

type options struct {
	templates                []string
	timelines, jobs, drivers int
	timeout                  int // guest seconds per timeline
	seed                     uint64
	skip                     bool
	faults                   string   // the fault tolerance level, if any
	faultGap                 float64  // guest seconds: the longest gap between faults
	diskFaults               []string // the kinds of disk fault
	sets                     []string // --set's, for replays
	replay                   bool     // a replay: one planned timeline of a given seed (--seed) or from --branch
	out                      string   // where to write results.json, coverage/ and profiles/, if anywhere
	profile                  bool
	explore                  bool   // branch timelines from moments that did something new (see explore.go)
	branch                   string // a branch point's checkpoint to run the timelines from (a replay)
	maxFailures              int    // stop the run once this many timelines failed (0: never)
	firstFailures            bool   // ... counting only those that failed in their first_ command or before it
}

// A plan is a timeline to run.
type plan struct {
	tmpl   string
	i      int // its number in its template
	seed   uint64
	first  string   // its first_ command, if any
	firsts []string // its template's
	from   *branchPoint
}

// A run is a timeline's result.
type run struct {
	Template string     `json:"template"`
	Seed     uint64     `json:"seed"`
	First    string     `json:"first,omitempty"`
	Pass     bool       `json:"pass"`
	NotRun   bool       `json:"not_run,omitempty"` // the run stopped (--max-failures) before it started or ended: it has no result
	Wall     float64    `json:"wall_s"`
	Branch   *branchRef `json:"branch,omitempty"`
	Saves    int        `json:"saves,omitempty"`  // branch points saved from it (exploring)
	SaveS    float64    `json:"save_s,omitempty"` // ... and the wall time it was paused for them
	protocol.Result
	Log string `json:"log,omitempty"`
	// ContainerLogs are its containers' logs, if it failed.
	ContainerLogs string `json:"container_logs,omitempty"`
	i             int
	setup         bool // its edits or new images couldn't be applied
	from          *branchPoint
}

// exitOf is l's EXIT body, if it's an EXIT line.
func exitOf(l ctlLine) protocol.Exit {
	var e protocol.Exit
	if l.verb == "EXIT" {
		json.Unmarshal([]byte(l.body), &e)
	}
	return e
}

func firstsOf(cat protocol.Catalog, tmpl string) []string {
	var f []string
	for _, c := range cat[tmpl] {
		if strings.HasPrefix(c.Name, "first_") {
			f = append(f, c.Name)
		}
	}
	sort.Strings(f)
	return f
}

func (p *plan) choose(cat protocol.Catalog) {
	p.firsts, p.first = firstsOf(cat, p.tmpl), ""
	if len(p.firsts) > 0 {
		p.first = p.firsts[p.seed%uint64(len(p.firsts))]
	}
}

// cmdTest runs the timelines from setup with edits.
func cmdTest(setup *checkpoint, edits []edit, o options) {
	runs := tempDir()
	in := stage(runs, edits)
	// (A template the edits add has, as far as the host knows, no first_
	// commands; the guest replans the timeline if it has.)
	cat := maps.Clone(setup.meta.Catalog)
	for _, t := range in.templates() {
		if _, ok := cat[t]; !ok {
			cat[t] = nil
		}
	}
	tmpls := o.templates
	var replay *branchPoint
	if o.branch != "" {
		replay = loadBranch(o.branch, setup)
		tmpls = []string{replay.Template}
	}
	if len(tmpls) == 0 {
		tmpls = slices.Sorted(maps.Keys(cat))
	}
	if len(cat) == 0 && len(tmpls) == 0 {
		// No test templates: the workload runs by itself (as its
		// containers' entrypoints), as Antithesis allows. A timeline is
		// timeout of it, with faults if asked.
		cat, tmpls = protocol.Catalog{"": nil}, []string{""}
	}
	var plans []*plan
	for _, t := range tmpls {
		if _, ok := cat[t]; !ok {
			die("no test template %s (the project has %s)", t, strings.Join(slices.Sorted(maps.Keys(cat)), ", "))
		}
		for i := range o.timelines {
			p := &plan{tmpl: t, i: i + 1, seed: o.seed + uint64(len(plans)), from: replay}
			p.choose(cat)
			plans = append(plans, p)
		}
	}
	o.replay = o.replay && len(plans) == 1 // (a search of given seeds that stops early is still judged: see properties)
	// The timelines, up to -j at a time.
	results := make([]run, len(plans))
	var mu sync.Mutex
	ended, failed := 0, 0 // (timelines that ended, and failed)
	var wg sync.WaitGroup
	jobs := make(chan struct{}, o.jobs)
	// (--max-failures: once enough timelines failed, the others don't
	// start, and those running stop: see vm.next.)
	enough, failures := make(chan struct{}), 0
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(enough) }) }
	interrupt.Lock()
	interrupt.stop = stop
	interrupt.Unlock()
	var x *explorer
	if o.explore && replay == nil {
		x = newExplorer(o)
	}
	for i, p := range plans {
		wg.Go(func() {
			jobs <- struct{}{}
			defer func() { <-jobs }()
			notRun := func() { results[i] = run{Template: p.tmpl, Seed: p.seed, First: p.first, NotRun: true, i: p.i} }
			select {
			case <-enough:
				notRun()
				return
			default:
			}
			if x != nil {
				p.from = x.start(p.tmpl)
			}
			for {
				r, newCat := timeline(setup, p, in, o, x, runs, len(plans) == 1, enough)
				if r.NotRun {
					if x != nil && p.from != nil {
						x.done(p.from, false)
					}
					os.RemoveAll(filepath.Dir(r.Log))
					notRun()
					return
				}
				if newCat == nil {
					if x != nil && p.from != nil {
						x.done(p.from, !r.Pass)
					}
					if r.Pass { // (a log that isn't kept: see below)
						os.RemoveAll(filepath.Dir(r.Log))
					}
					if r.Pass || len(plans) == 1 {
						r.Log = ""
					}
					mu.Lock()
					if o.maxFailures > 0 && countsAsFailure(r, o) {
						if failures++; failures == o.maxFailures {
							stop()
						}
					}
					mu.Unlock()
					results[i] = r
					if len(plans) > 1 { // (each timeline's line as it ends, and how many have)
						mu.Lock()
						ended++
						status := "ok"
						if !r.Pass {
							status = "FAIL"
							failed++
						}
						printTimeline(r, status, o, fmt.Sprintf("; %d of %d done, %d failed", ended, len(plans), failed))
						mu.Unlock()
					}
					return
				}
				mu.Lock()
				cat = newCat
				p.choose(cat)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	interrupt.Lock()
	interrupt.stop = nil // (from here on, a signal exits at once)
	interrupted := interrupt.signaled
	interrupt.Unlock()
	if x != nil {
		x.keepFailed(results)
	}
	// Keep the logs of the timelines that failed (with one timeline, the
	// console streamed) and their containers' logs: with --out, with the
	// results, where the cache's trim doesn't reach them.
	// With --out, in a directory of this run's own under DIR/logs, so that
	// runs into the same DIR don't overwrite each other's.
	stamp := time.Now().Format("20060102-150405.000")
	keep := filepath.Join(cache, "logs", stamp)
	if o.out != "" {
		keep = ""
	}
	for i := range results {
		for f, ext := range map[*string]string{&results[i].Log: ".log", &results[i].ContainerLogs: ".containers.log"} {
			if *f != "" {
				if keep == "" {
					keep = newDir(filepath.Join(o.out, "logs", stamp))
				}
				check(os.MkdirAll(keep, 0o755))
				to := filepath.Join(keep, filepath.Base(filepath.Dir(*f))+ext)
				check(move(*f, to))
				*f = to
			}
		}
	}
	ok := report(results, o)
	if exists(keep) {
		fmt.Fprintf(os.Stderr, "timeskip: the failed timelines' logs are in %s\n", keep)
	}
	trim()
	if interrupted {
		exit(130)
	}
	if !ok {
		if !slices.ContainsFunc(results, func(r run) bool { return !r.setup && !r.NotRun }) {
			exit(125)
		}
		exit(1)
	}
}

// countsAsFailure is whether r counts toward --max-failures: it failed, and
// with first:N, in its first_ command or before it (its edits or new images
// couldn't be applied).
func countsAsFailure(r run, o options) bool {
	return !r.Pass && (!o.firstFailures || r.setup || slices.ContainsFunc(r.Commands, func(c protocol.Record) bool {
		return c.Name == r.First && c.Exit != 0 && !c.Killed
	}))
}

// timeline runs one timeline from setup, or from p's branch point.
// Exploring, x saves branch points from it. A new catalog comes back if the
// edits changed the template's first_ commands. Once enough is closed, it
// stops the VM, with a result that's NotRun.
func timeline(setup *checkpoint, p *plan, in *staged, o options, x *explorer, runs string, echo bool, enough chan struct{}) (run, protocol.Catalog) {
	dir := filepath.Join(runs, fmt.Sprintf("%s-%d", p.tmpl, p.i))
	os.RemoveAll(dir)
	from := setup
	if p.from != nil {
		from, p.first, p.firsts = p.from.checkpoint, p.from.First, p.from.Firsts
	}
	check(os.MkdirAll(dir, 0o755))
	data := filepath.Join(dir, "disk")
	clone(from.file("disk"), data)
	defer os.Remove(data) // (once the VM is gone: a run may have thousands)
	v := start(dir, setup.meta.Nonce, nil, in.dir, from, data, in.img, x != nil)
	defer v.kill()
	v.cut = enough
	if echo {
		v.echo = os.Stdout
	}
	r := run{Template: p.tmpl, Seed: p.seed, First: p.first, Log: v.file("console.log"), i: p.i}
	t0, paused := time.Now(), time.Duration(0)
	if p.from != nil {
		ref := *p.from.Ref
		r.Branch, r.from = &ref, p.from
		v.send("BRANCH", fmt.Sprintf("seed=%d", p.seed))
	} else {
		v.send("GO", protocol.Go{Skip: o.skip, Seed: p.seed, Template: p.tmpl, First: p.first, Firsts: p.firsts, Drivers: o.drivers,
			Timeout: o.timeout, Inputs: in.spec,
			Faults: map[bool]float64{true: o.faultGap}[o.faults != ""], Level: o.faults, Disk: o.diskFaults, Profile: o.profile,
			Symbols: wantSymbols.Load(), Explore: x != nil}.String())
	}
	// (Exploring: a branch point at most every second of guest time, of
	// what was new to the run since the last.)
	const spacing int64 = 1000
	news, lastSave, saved := 0.0, -spacing, []*branchPoint(nil)
	base := &memBase{file: from.file("mem"), made: make(chan struct{})}
	close(base.made)
	defer func() { <-base.made; x.release(base.point) }()
	for {
		l := v.nextLine()
		switch l.verb {
		case "CUT":
			r.NotRun = true
			return r, nil
		case "EDGES": // (a replay's too, from a branch point saved exploring: x is nil then)
			var e protocol.Edges
			json.Unmarshal([]byte(l.body), &e)
			if x != nil {
				if news += x.news(e); news > 0 && e.AtMs-lastSave >= spacing {
					if b := x.reserve(p, setup, e, news); b != nil {
						t := time.Now()
						x.save(v, b, data, in.img, base) // (and sends NOTED)
						paused += time.Since(t)
						r.Saves, r.SaveS = r.Saves+1, r.SaveS+time.Since(t).Seconds()
						news, lastSave, saved = 0, e.AtMs, append(saved, b)
						continue
					}
				}
			}
			v.send("NOTED", "")
			continue
		case "EXIT":
			e := exitOf(l)
			switch e.Kind {
			case "replan":
				return r, e.Catalog
			case "timeline":
				r.Result = *e.Result
				if len(r.Symbols) > 0 && len(r.Symbols) == len(r.Coverage) {
					wantSymbols.Store(false)
				}
				r.Wall = (time.Since(t0) - paused).Seconds()
				r.Pass = len(r.Errors) == 0 && len(broken(r)) == 0
				if !r.Pass {
					v.send("LOGS", "")
					switch l := v.nextLine(); {
					case l.verb == "LOGGED" && containerLogs(data, filepath.Join(dir, "containers.log")):
						r.ContainerLogs = filepath.Join(dir, "containers.log")
					case l.verb == "EXITED" && l.body != "":
						r.Errors = append(r.Errors, l.body)
					}
				}
				if x != nil && !r.Pass {
					x.withdraw(saved, cmp.Or(r.FailedMs, r.GuestMs))
				}
				return r, nil
			case "setup":
				r.Errors, r.setup = append(r.Errors, "setup failed: "+e.Error), true
				if containerLogs(data, filepath.Join(dir, "containers.log")) {
					r.ContainerLogs = filepath.Join(dir, "containers.log")
				}
			default:
				die("the guest's EXIT makes no sense: %s", l.body)
			}
		default: // EXITED (the VMM's failure fails only this timeline)
			r.Errors = append(r.Errors, cmp.Or(l.body, "the VM stopped without a result"))
		}
		return r, nil
	}
}

// containerLogs copies the containers' logs that the guest wrote to the
// data disk, data, to to, if it wrote any (see protocol.ContainerLogsAt).
func containerLogs(data, to string) bool {
	f, err := os.Open(data)
	if err != nil {
		return false
	}
	defer f.Close()
	r, buf := io.NewSectionReader(f, protocol.ContainerLogsAt, protocol.ContainerLogsSize), make([]byte, 1<<20)
	var out *os.File
	for {
		n, err := r.Read(buf)
		b, end := buf[:n], err != nil
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b, end = b[:i], true
		}
		if len(b) > 0 && out == nil {
			out, _ = os.Create(to)
			if out == nil {
				return false
			}
			defer out.Close()
		}
		if out != nil {
			out.Write(b)
		}
		if end {
			return out != nil
		}
	}
}

// newDir creates and returns directory base, or base-2, base-3 and so on if
// it exists.
func newDir(base string) string {
	check(os.MkdirAll(filepath.Dir(base), 0o755))
	for i := 1; ; i++ {
		d := base
		if i > 1 {
			d = fmt.Sprintf("%s-%d", base, i)
		}
		err := os.Mkdir(d, 0o755)
		if err == nil {
			return d
		}
		if !errors.Is(err, fs.ErrExist) {
			check(err)
		}
	}
}

// move renames file from to to, or where they're on different filesystems
// (--out's and the cache's), copies it to a temporary name beside to,
// renames that to to, and only then removes from.
func move(from, to string) error {
	err := os.Rename(from, to)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(to), "."+filepath.Base(to)+".*")
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if fi, serr := in.Stat(); serr == nil {
		err = errors.Join(err, out.Chmod(fi.Mode()))
	}
	if err = errors.Join(err, out.Close()); err == nil {
		err = os.Rename(out.Name(), to)
	}
	if err != nil {
		os.Remove(out.Name())
		return err
	}
	return os.Remove(from)
}
