package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// A property is an assertion over every timeline, as Antithesis reports it:
// Always ones must never be false, Sometimes ones true at least once,
// Reachable ones reached and Unreachable ones not; and every test command
// exits 0.
type property struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Pass    bool     `json:"pass"`
	Hits    int      `json:"hits"`
	True    int      `json:"true"`
	Failing []uint64 `json:"failing_seeds,omitempty"` // the timelines that broke it
	Details any      `json:"details,omitempty"`
	key     string
	first   *run // the first run that broke it
}

// broken are the properties (by key) that a run broke by itself: an Always
// assertion false, an Unreachable one reached, a command failed. (Others
// fail only over all runs: a Sometimes assertion never true.)
func broken(r run) map[string]bool {
	m := map[string]bool{}
	for _, c := range r.Commands {
		if c.Exit != 0 && !c.Killed {
			m["cmd "+r.Template+"/"+c.Name] = true
		}
	}
	for _, a := range r.Assertions {
		if a.Type == "always" && a.Hits > a.True || a.Type == "reachability" && !a.MustHit && a.Hits > 0 {
			m["assert "+cmp.Or(a.ID, a.Message)] = true
		}
	}
	return m
}

func properties(runs []run) []*property {
	var l []*property
	by := map[string]*property{}
	kind := map[*property]protocol.Assertion{} // (its assert_type and must_hit)
	get := func(key, name, typ string) *property {
		p := by[key]
		if p == nil {
			p = &property{Name: name, Type: typ, Pass: true, key: key}
			by[key] = p
			l = append(l, p)
		}
		return p
	}
	for i, r := range runs {
		for _, c := range r.Commands {
			p := get("cmd "+r.Template+"/"+c.Name, r.Template+"/"+c.Name+" exits 0", "Always")
			p.Hits++
			if c.Exit == 0 || c.Killed {
				p.True++
			}
		}
		for _, a := range r.Assertions {
			p := get("assert "+cmp.Or(a.ID, a.Message), a.Message, cmp.Or(a.Display, a.Type))
			p.Hits += a.Hits
			p.True += a.True
			k := kind[p]
			k.Type, k.MustHit = a.Type, k.MustHit || a.MustHit
			kind[p] = k
			if broken(r)[p.key] && p.Details == nil && a.Details != nil {
				p.Details = a.Details
			}
		}
		for k := range broken(r) {
			p := by[k]
			p.Pass = false
			p.Failing = append(p.Failing, r.Seed)
			if p.first == nil {
				p.first = &runs[i]
			}
		}
	}
	// As Antithesis's "All commands were run to completion at least once":
	// every template had a timeline that didn't run out of time. (A
	// timeline that did isn't a failure by itself: a command may hang in
	// one timeline and not another.)
	done := map[string]bool{}
	for _, r := range runs {
		if r.Template != "" && !r.setup {
			done[r.Template] = done[r.Template] || !r.TimedOut
		}
	}
	if len(done) > 0 {
		p := get("completed", "All commands were run to completion at least once", "Always")
		for _, d := range done {
			p.Hits++
			if d {
				p.True++
			}
		}
		for _, r := range runs {
			if r.TimedOut && !done[r.Template] {
				p.Failing = append(p.Failing, r.Seed)
			}
		}
		p.Pass = p.True == p.Hits
	}
	// (assert_type and must_hit decide; display_type is only a name.)
	for p, k := range kind {
		switch {
		case k.Type == "sometimes":
			p.Pass = p.True > 0
		case k.Type == "reachability" && k.MustHit:
			p.Pass = p.Hits > 0
		case k.Type == "reachability":
			p.Pass = p.Hits == 0
		default: // always
			p.Pass = p.first == nil && (!k.MustHit || p.Hits > 0)
		}
	}
	return l
}

// report prints the timelines and the properties, writes results.json, and says
// whether everything passed. A failed property gets a seed to replay.
func report(runs []run, o options) bool {
	props := properties(runs)
	ok := true
	passed := 0
	for _, r := range runs {
		status := "ok"
		if !r.Pass {
			status, ok = "FAIL", false
		} else {
			passed++
		}
		faults := ""
		if o.faults != "" {
			faults = fmt.Sprintf(", %d faults", len(r.Faults))
			if r.FaultMix == "no node faults" {
				faults += " (no node faults)"
			}
		}
		from := r.From
		if r.Branch != nil {
			from += ": " + r.Branch.String()
		}
		fmt.Fprintf(os.Stderr, "timeline %s#%d seed=%d %-4s %d commands%s, %.1f s of guest time in %.2f s (from %s)\n",
			r.Template, r.i, r.Seed, status, len(r.Commands), faults, float64(r.GuestMs)/1000, r.Wall, from)
		for _, e := range r.Errors {
			fmt.Fprintf(os.Stderr, "  %s\n", e)
		}
		if r.TimedOut {
			fmt.Fprintf(os.Stderr, "  ran out of time (timeout: %d guest seconds); what still ran was killed\n", o.timeout)
		}
		for _, c := range r.Commands {
			if c.Exit != 0 && !c.Killed {
				fmt.Fprintf(os.Stderr, "  %s exited %d\n", c.Name, c.Exit)
			}
		}
	}
	mostCPU(runs)
	if o.explore {
		saves, secs, branched, from := 0, 0.0, 0, map[*branchPoint]bool{}
		for _, r := range runs {
			saves, secs = saves+r.Saves, secs+r.SaveS
			if r.from != nil {
				branched++
				from[r.from] = true
			}
		}
		fmt.Fprintf(os.Stderr, "exploring: %d branch points saved (in %.1f s), %d timelines started from %d of them\n", saves, secs, branched, len(from))
	}
	if o.profile {
		profiles(runs, filepath.Join(o.out, "profiles"))
	}
	covDir := map[bool]string{true: filepath.Join(o.out, "coverage")}[o.out != ""]
	cov := coverageOf(runs, covDir)
	reportCoverage(cov, covDir)
	for i := range runs {
		runs[i].Coverage, runs[i].Symbols = nil, nil
	}
	failed := 0
	for _, p := range props {
		status := "ok  "
		if !p.Pass {
			status, ok = "FAIL", false
			failed++
		}
		fmt.Fprintf(os.Stderr, "%s %-20s %s (%d of %d true)\n", status, p.Type, p.Name, p.True, p.Hits)
		if b := p.first; b != nil && b.Branch != nil {
			fmt.Fprintf(os.Stderr, "     broken in %d timelines; replay: --branch %s --seed %d\n", len(p.Failing), b.Branch.Checkpoint, b.Seed)
		} else if b != nil {
			replay := fmt.Sprintf("--seed %d", b.Seed)
			if b.Template != "" {
				replay = "--template " + b.Template + " " + replay
			}
			for _, s := range o.sets {
				replay += " --set " + s
			}
			fmt.Fprintf(os.Stderr, "     broken in %d timelines; replay: %s\n", len(p.Failing), replay)
		}
	}
	if o.out != "" {
		b, _ := json.MarshalIndent(map[string]any{"pass": ok, "timelines": runs, "properties": props, "coverage": cov}, "", "  ")
		check(os.MkdirAll(o.out, 0o755))
		write(filepath.Join(o.out, "results.json"), string(b)+"\n")
	}
	fmt.Fprintf(os.Stderr, "timeskip: %d of %d timelines passed, %d of %d properties failed\n", passed, len(runs), failed, len(props))
	return ok
}

// mostCPU prints where the guests' CPU time went, summed over the runs: the
// five test commands and the five containers that used the most.
func mostCPU(runs []run) {
	cmds, ctrs := map[string]int64{}, map[string]int64{}
	for _, r := range runs {
		for _, c := range r.Commands {
			cmds[r.Template+"/"+c.Name] += c.CPUMs
		}
		for n, ms := range r.ContainerCPUMs {
			ctrs[n] += ms
		}
	}
	mostIn("commands", cmds)
	mostIn("containers", ctrs)
}

// mostIn prints the five things in ms that used the most CPU time (ms).
func mostIn(what string, ms map[string]int64) {
	names := slices.SortedFunc(maps.Keys(ms), func(a, b string) int { return cmp.Or(cmp.Compare(ms[b], ms[a]), cmp.Compare(a, b)) })
	var l []string
	for _, n := range names[:min(5, len(names))] {
		if ms[n] >= 10 {
			l = append(l, fmt.Sprintf("%s %.2f s", n, float64(ms[n])/1000))
		}
	}
	if len(l) > 0 {
		fmt.Fprintf(os.Stderr, "most CPU (%s): %s\n", what, strings.Join(l, ", "))
	}
}

// profiles writes each run's profile to dir, as TEMPLATE-SEED.folded (a
// folded stack and its samples a line, as flamegraph.pl, inferno and
// speedscope read them), prints the functions the samples were most in,
// and leaves the profiles out of the runs (and results.json).
func profiles(runs []run, dir string) {
	check(os.MkdirAll(dir, 0o755))
	self := map[string]int64{}
	for i, r := range runs {
		var b strings.Builder
		for _, s := range slices.Sorted(maps.Keys(r.Profile)) {
			fmt.Fprintf(&b, "%s %d\n", s, r.Profile[s])
			self[s[strings.LastIndexByte(s, ';')+1:]] += r.Profile[s] * 10
		}
		write(filepath.Join(dir, fmt.Sprintf("%s-%d.folded", r.Template, r.Seed)), b.String())
		runs[i].Profile = nil
	}
	mostIn("functions", self)
	fmt.Fprintf(os.Stderr, "profiles (folded stacks) in %s\n", dir)
}
