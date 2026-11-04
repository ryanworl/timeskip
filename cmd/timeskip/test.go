package main

import (
	"cmp"
	"encoding/json"
	"fmt"
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
	out                      string   // where to write results.json, coverage/ and profiles/, if anywhere
	profile                  bool
	explore                  bool   // branch timelines from moments that did something new (see explore.go)
	branch                   string // a branch point's checkpoint to run the timelines from (a replay)
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
	Wall     float64    `json:"wall_s"`
	From     string     `json:"from"` // the checkpoint it started from: setup, first or branch
	Branch   *branchRef `json:"branch,omitempty"`
	Saves    int        `json:"saves,omitempty"`  // branch points saved from it (exploring)
	SaveS    float64    `json:"save_s,omitempty"` // ... and the wall time it was paused for them
	protocol.Result
	Log   string `json:"log,omitempty"`
	i     int
	setup bool // its edits or new images couldn't be applied
	from  *branchPoint
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
	// The timelines, up to -j at a time.
	results := make([]run, len(plans))
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan struct{}, o.jobs)
	var x *explorer
	if o.explore && replay == nil {
		x = newExplorer(o)
	}
	for i, p := range plans {
		wg.Go(func() {
			jobs <- struct{}{}
			defer func() { <-jobs }()
			if x != nil {
				p.from = x.start(p.tmpl)
			}
			for {
				r, newCat := timeline(setup, p, in, o, x, runs, len(plans) == 1)
				if newCat == nil {
					if x != nil && p.from != nil {
						x.done(p.from, !r.Pass)
					}
					if r.Pass || len(plans) == 1 { // (a log that isn't kept: see below)
						os.RemoveAll(filepath.Dir(r.Log))
						r.Log = ""
					}
					results[i] = r
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
	if x != nil {
		x.keepFailed(results)
	}
	// Keep the logs of the timelines that failed (with one timeline, the
	// console streamed).
	keep := filepath.Join(cache, "logs", time.Now().Format("20060102-150405.000"))
	for i, r := range results {
		if r.Log != "" {
			check(os.MkdirAll(keep, 0o755))
			results[i].Log = filepath.Join(keep, filepath.Base(filepath.Dir(r.Log))+".log")
			check(os.Rename(r.Log, results[i].Log))
		}
	}
	ok := report(results, o)
	if exists(keep) {
		fmt.Fprintf(os.Stderr, "timeskip: the failed timelines' logs are in %s\n", keep)
	}
	trim()
	if !ok {
		if !slices.ContainsFunc(results, func(r run) bool { return !r.setup }) {
			exit(125)
		}
		exit(1)
	}
}

// timeline runs one timeline from setup, or from the checkpoint after its
// first_ command (it saves that checkpoint on the way if no one has), or
// from p's branch point. Exploring, x saves branch points from it. A new
// catalog comes back if the edits changed the template's first_ commands.
func timeline(setup *checkpoint, p *plan, in *staged, o options, x *explorer, runs string, echo bool) (run, protocol.Catalog) {
	dir := filepath.Join(runs, fmt.Sprintf("%s-%d", p.tmpl, p.i))
	os.RemoveAll(dir)
	from, saveAs, saveLock := setup, (*checkpoint)(nil), 0
	pre, digest := in.pre(p.tmpl)
	if p.from != nil {
		from, p.first, p.firsts = p.from.checkpoint, p.from.First, p.from.Firsts
	} else if p.first != "" {
		// (Timelines from it share its lock; saving or removing it takes it
		// exclusively. A new version of the first_ and helper_ files
		// replaces the one before, as a new version of a bundle replaces
		// its checkpoint.)
		name := filepath.Join(setup.dir, "first-"+hash(p.tmpl, p.first, p.firsts))
		k := &checkpoint{dir: name + "-" + hash(digest), setup: setup.dir}
		if l, ok := tryLock(k.dir+".lock", syscall.LOCK_SH); ok {
			defer syscall.Close(l)
			switch {
			case k.done():
				from = k
				used(k.dir + ".lock")
			case syscall.Flock(l, syscall.LOCK_EX|syscall.LOCK_NB) == nil:
				os.RemoveAll(k.dir)
				check(os.MkdirAll(k.dir, 0o755))
				saveAs, saveLock = k, l
				others, _ := filepath.Glob(name + "-*.lock")
				for _, o := range others {
					if o != k.dir+".lock" {
						remove(strings.TrimSuffix(o, ".lock"))
					}
				}
			}
		}
	}
	check(os.MkdirAll(dir, 0o755))
	data := filepath.Join(dir, "disk")
	clone(from.file("disk"), data)
	defer os.Remove(data) // (once the VM is gone: a run may have thousands)
	// (On Linux the first_ checkpoint's save writes the pages written since
	// the restore over a copy of the memory file, made meanwhile.)
	copied := make(chan struct{})
	go func() {
		if saveAs != nil && !darwin {
			clone(from.file("mem"), saveAs.file("mem"))
		}
		close(copied)
	}()
	v := start(dir, setup.meta.Nonce, nil, in.dir, from, data, in.img, saveAs != nil || x != nil)
	defer v.kill()
	if echo {
		v.echo = os.Stdout
	}
	r := run{Template: p.tmpl, Seed: p.seed, First: p.first, From: "setup", Log: v.file("console.log"), i: p.i}
	t0, paused := time.Now(), time.Duration(0)
	if p.from != nil {
		ref := *p.from.Ref
		r.From, r.Branch, r.from = "branch", &ref, p.from
		v.send("BRANCH", fmt.Sprintf("seed=%d", p.seed))
	} else {
		if from != setup {
			r.From = "first"
		}
		v.send("GO", protocol.Go{Skip: o.skip, Seed: p.seed, Template: p.tmpl, First: p.first, Firsts: p.firsts, Drivers: o.drivers,
			Timeout: o.timeout, Pre: pre, Checkpoint: saveAs != nil, Inputs: in.spec,
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
		l := v.next()
		switch l.verb {
		case "EDGES": // (a replay's too, from a branch point saved exploring: x is nil then)
			var e protocol.Edges
			json.Unmarshal([]byte(l.body), &e)
			if x != nil {
				if news += x.news(e); news > 0 && e.AtMs-lastSave >= spacing {
					if b := x.reserve(p, setup, e, news); b != nil {
						t := time.Now()
						x.save(v, b, data, in.img, base)
						paused += time.Since(t)
						r.Saves, r.SaveS = r.Saves+1, r.SaveS+time.Since(t).Seconds()
						news, lastSave, saved = 0, e.AtMs, append(saved, b)
					}
				}
			}
			v.send("NOTED", "")
			continue
		case "CHECKPOINT":
			t := time.Now()
			<-copied
			v.save(saveAs.dir, data, in.img, setup.placeholder(), false)
			base.file = saveAs.file("mem")
			published := make(chan struct{})
			go func() {
				saveAs.publish()
				syscall.Flock(saveLock, syscall.LOCK_SH) // (others may start from it now)
				close(published)
			}()
			defer func() { <-published }() // (before its lock goes)
			v.resume()
			v.send("CONTINUE", "")
			paused += time.Since(t)
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
				if x != nil && !r.Pass {
					x.withdraw(saved, cmp.Or(r.FailedMs, r.GuestMs))
				}
				return r, nil
			case "setup":
				r.Errors, r.setup = append(r.Errors, "setup failed: "+e.Error), true
			default:
				die("the guest's EXIT makes no sense: %s", l.body)
			}
		default: // EXITED
			r.Errors = append(r.Errors, "the VM stopped without a result")
		}
		return r, nil
	}
}
