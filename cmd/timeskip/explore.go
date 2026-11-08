package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Exploring (--explore; see docs/reference.md, "Exploring"): a timeline is saved,
// as a branch point, at a moment its instrumented code reached edges that
// no timeline of the run had, and later timelines start from branch points
// with seeds of their own, as Antithesis branches from interesting moments.

// A branchPoint is a timeline saved at such a moment: a checkpoint, which
// refers to its setup checkpoint's bundle and placeholder, with what's
// needed to go on from it (in its branch.json).
type branchPoint struct {
	*checkpoint
	Template string     `json:"template"`
	First    string     `json:"first,omitempty"`
	Firsts   []string   `json:"firsts,omitempty"`
	Setup    string     `json:"setup"` // its setup checkpoint's directory
	Ref      *branchRef `json:"ref"`
	score    float64    // what it had reached new to the run was worth (see explorer.news)
	picks    int        // timelines started from it
	users    int        // ... and still running
	bases    int        // timelines whose next branch point is saved over its memory (see memBase)
	files    string     // where its files are until it's kept (dir links to it; see reserve), or ""
	evicted  bool       // out of the pool: removed once unused
	keep     bool       // a timeline from it failed: kept for replays
	saving   bool       // reserved in the pool, and being saved
}

// A branchRef is where a timeline branched from: a moment of the timeline
// with seed Seed (guest time since it started), which in turn may have
// branched from another. Checkpoint, of a failed timeline's, is kept for
// replays with --branch.
type branchRef struct {
	Seed       uint64     `json:"seed"`
	AtMs       int64      `json:"at_ms"`
	From       *branchRef `json:"from,omitempty"`
	Checkpoint string     `json:"checkpoint,omitempty"`
}

func (b *branchRef) String() string {
	s := fmt.Sprintf("seed %d at %.1f s", b.Seed, float64(b.AtMs)/1000)
	if b.From != nil {
		s = b.From.String() + " > " + s
	}
	return s
}

// poolMax is how many branch points each template keeps: a new one past it
// takes the place of the one least worth branching from, if worth more.
const poolMax = 32

// pointRoom is the room a branch point is saved in memory with (see
// memDir): more than one takes, its memory and data disk.
const pointRoom = 2 << 30

// An explorer is the run's branch points and the edges its timelines
// reached, and decides where each timeline starts.
type explorer struct {
	mu        sync.Mutex
	dir       string                    // the branch points, links to their files until kept
	tmp, mem  string                    // where their files are: directories in the cache, and in memory or "" (see reserve)
	seen      map[string][]byte         // the edges reached, by module
	asserts   map[string]bool           // the SDK assertions' states reached
	best      float64                   // the most a branch point was worth
	pool      map[string][]*branchPoint // by template
	all       []*branchPoint
	fresh     map[string]int // timelines started from setup, by template
	branchd   map[string]int // timelines started from branch points
	freshN    int            // ... of each template
	timelines int            // ... of each template
}

// freshShare is the share of the timelines that start from setup, at least.
const freshShare = 0.5

func newExplorer(o options) *explorer {
	dir := filepath.Join(cache, "branches", time.Now().Format("20060102-150405.000"))
	x := &explorer{dir: dir, seen: map[string][]byte{}, asserts: map[string]bool{}, pool: map[string][]*branchPoint{}, fresh: map[string]int{},
		branchd: map[string]int{}, freshN: int(math.Ceil(float64(o.timelines) * freshShare)), timelines: o.timelines}
	// The branch points' files are in tempIn directories, which exit (or,
	// after a kill -9, sweep) removes, with the links to them.
	check(os.MkdirAll(dir, 0o755))
	hold(dir, syscall.LOCK_EX)
	atExit(func() { unlink(dir) })
	x.tmp = tempDir()
	if m := memDir(math.MinInt64); m != "" { // (whatever room it has: each save checks)
		x.mem = tempIn(m)
	}
	return x
}

// unlink removes the links in a directory of branch points (see reserve)
// and the directory, if that empties it.
func unlink(dir string) {
	points, _ := os.ReadDir(dir)
	for _, p := range points {
		if p.Type() == os.ModeSymlink {
			os.Remove(filepath.Join(dir, p.Name()))
		}
	}
	os.Remove(dir)
}

// worth is how much branching from b is worth: what it reached new to the
// run, less as timelines branch from it.
func (x *explorer) worth(b *branchPoint) float64 { return b.score / float64(1+b.picks) }

// start says where tmpl's next timeline starts: a branch point, or setup
// (nil). Half the timelines (freshShare) start from setup, the
// others from the branch point worth the most (timelines from it still
// running lower its worth as the ones that ended do), interleaved with
// those from setup in that proportion; but with none to start from (none
// yet, or all still being saved), from setup: a timeline never waits.
func (x *explorer) start(tmpl string) *branchPoint {
	x.mu.Lock()
	defer x.mu.Unlock()
	var best *branchPoint
	for _, b := range x.pool[tmpl] {
		if !b.saving && (best == nil || x.worth(b) >= x.worth(best)) { // (the newest, of equals)
			best = b
		}
	}
	if best == nil || float64(x.branchd[tmpl])*freshShare >= float64(x.fresh[tmpl])*(1-freshShare) {
		x.fresh[tmpl]++
		return nil
	}
	best.picks++
	best.users++
	x.branchd[tmpl]++
	return best
}

// done notes that a timeline from b ended, and whether it failed.
func (x *explorer) done(b *branchPoint, failed bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	b.users--
	b.keep = b.keep || failed
	x.prune(b)
}

// news marks edges and assertions' states reached, and says what those no
// timeline had are worth: 1 each, and an assertion's state as much as the
// most a branch point was (at least 1), since its author called it out.
func (x *explorer) news(e protocol.Edges) float64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0.0
	for _, a := range e.Assertions {
		if !x.asserts[a] {
			x.asserts[a] = true
			n += max(x.best, 1)
		}
	}
	for mod, l := range e.Edges {
		bits := x.seen[mod]
		for _, i := range l {
			if i/8 >= len(bits) {
				bits = append(bits, make([]byte, i/8+1-len(bits))...)
			}
			if bits[i/8]&(1<<(i%8)) == 0 {
				bits[i/8] |= 1 << (i % 8)
				n++
			}
		}
		x.seen[mod] = bits
	}
	return n
}

// reserve puts a branch point of p's at the moment e says, worth score, in
// its template's pool for the timeline to save, if it's worth keeping (see
// worst), in place of the one worth the least if the pool is full; or
// returns nil. (Reserved
// before it's saved, so that timelines saving at once don't each save for
// the same place, all but one in vain.)
// Its directory in the run's links to its files, in a directory of the
// run's in the cache, or on Linux in memory where there's room (see
// memDir), until keepFailed keeps it.
func (x *explorer) reserve(p *plan, setup *checkpoint, e protocol.Edges, score float64) *branchPoint {
	x.mu.Lock()
	defer x.mu.Unlock()
	i := x.worst(p.tmpl, score)
	if i < 0 {
		return nil
	}
	n := fmt.Sprint(len(x.all) + 1)
	d := filepath.Join(x.dir, n)
	b := &branchPoint{checkpoint: &checkpoint{dir: d, setup: setup.dir, meta: setup.meta}, Template: p.tmpl, First: p.first, Firsts: p.firsts,
		Setup: setup.dir, Ref: &branchRef{Seed: p.seed, AtMs: e.AtMs}, score: score, saving: true}
	if p.from != nil {
		b.Ref.From = p.from.Ref
	}
	b.files = filepath.Join(x.tmp, n)
	if x.mem != "" && memDir(pointRoom) != "" {
		b.files = filepath.Join(x.mem, n)
	}
	check(os.Mkdir(b.files, 0o755))
	check(os.Symlink(b.files, d))
	if pool := x.pool[p.tmpl]; i < len(pool) {
		pool[i].evicted = true
		x.prune(pool[i])
		pool[i] = b
	} else {
		x.pool[p.tmpl] = append(pool, b)
	}
	x.all = append(x.all, b)
	x.best = max(x.best, score)
	return b
}

// worst is the index of the branch point in tmpl's pool that one worth
// score would take the place of (len: none, there's room), or -1: when
// the timelines left to start from branch points could all start from
// ones worth as much (each pick lowers a point's worth: see worth), or the
// pool is full of ones worth as much or being saved.
func (x *explorer) worst(tmpl string, score float64) int {
	p := x.pool[tmpl]
	taken := 0 // (picks of points worth as much: k more of b while b.score/(1+picks+k) >= score)
	for _, b := range p {
		taken += max(0, int(b.score/score)-b.picks)
	}
	if taken >= x.timelines-max(x.freshN, x.fresh[tmpl])-x.branchd[tmpl] {
		return -1
	}
	if len(p) < poolMax {
		return len(p)
	}
	w := -1
	for i, b := range p {
		if !b.saving && (w < 0 || x.worth(b) < x.worth(p[w])) {
			w = i
		}
	}
	if w < 0 || x.worth(p[w]) >= score {
		return -1
	}
	return w
}

// failWindowMs: a branch point saved less than this (guest time) before
// its timeline failed is withdrawn (see withdraw).
const failWindowMs = 1000

// withdraw takes out of the pool the branch points in saved, a failed
// timeline's, from less than failWindowMs before it failed, at ms: by then
// the failure was mostly settled, if not yet checked, and timelines from
// there would find it again.
func (x *explorer) withdraw(saved []*branchPoint, ms int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, b := range saved {
		if ms-b.Ref.AtMs >= failWindowMs || b.evicted {
			continue
		}
		if pool := x.pool[b.Template]; slices.Contains(pool, b) {
			x.pool[b.Template] = slices.DeleteFunc(pool, func(c *branchPoint) bool { return c == b })
		}
		b.evicted = true
		x.prune(b)
	}
}

// prune removes b's checkpoint if it's out of the pool, unused and not
// kept.
func (x *explorer) prune(b *branchPoint) {
	if b.evicted && b.users == 0 && b.bases == 0 && !b.keep {
		b.remove()
	}
}

func (b *branchPoint) remove() {
	os.RemoveAll(b.dir)
	os.RemoveAll(b.files)
}

// release notes that b's memory is no longer the base of a timeline's next
// branch point (see memBase).
func (x *explorer) release(b *branchPoint) {
	if b == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	b.bases--
	x.prune(b)
}

// keepFailed removes the branch points no failed timeline started from,
// and notes in those runs where theirs are: they stay in the cache (as
// failed timelines' logs do), for replays, moved there in place of their
// links (copied from memory). (A saved VM names its disk's path, so
// they're saved there.)
func (x *explorer) keepFailed(runs []run) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, b := range x.all {
		switch {
		case !b.keep:
			b.remove()
		case b.files != "":
			os.Remove(b.dir)
			if os.Rename(b.files, b.dir) != nil {
				check(os.Mkdir(b.dir, 0o755))
				fs, _ := os.ReadDir(b.files)
				for _, f := range fs {
					clone(filepath.Join(b.files, f.Name()), b.file(f.Name()))
				}
				os.RemoveAll(b.files)
			}
			b.files = ""
		}
		for i := range runs {
			if runs[i].from == b {
				runs[i].Branch.Checkpoint = b.dir
			}
		}
	}
	os.Remove(x.dir) // (if empty)
}

// A memBase is the memory file a timeline's next branch point is saved
// over (see explorer.save): the one it was restored from or last saved, a
// branch point's if point (held till then: see release), once made is
// closed.
type memBase struct {
	file  string
	point *branchPoint
	made  chan struct{}
}

// save saves v as b, a branch point reserve put in the pool, and resumes
// it with NOTED. data is its data disk. On Linux the VM goes on once its
// snapshot has the pages written since its restore or its last
// save (see vm.save); then in the background, once base is made, b's
// memory file is made of them over a copy of it, and b can be picked; and
// b's becomes base. On macOS hvrun saves it all.
func (x *explorer) save(v *vm, b *branchPoint, data string, base *memBase) {
	v.save(b.dir, data, b.placeholder(), !darwin)
	clone(v.file("console.log"), b.file("console.log")) // (what the guest printed until the save)
	// (NOTED while it's paused: the guest skips no idle time until it
	// comes, so its clock would run through the host's work after the save.)
	v.send("NOTED", "")
	v.resume()
	j, _ := json.Marshal(b)
	write(b.file("branch.json"), string(j))
	ready := func() {
		x.mu.Lock()
		b.saving = false
		x.mu.Unlock()
	}
	if darwin {
		ready()
		return
	}
	x.mu.Lock()
	b.bases++
	x.mu.Unlock()
	prev, cur := *base, memBase{b.file("mem"), b, make(chan struct{})}
	*base = cur
	go func() {
		defer close(cur.made)
		<-prev.made
		clone(prev.file, b.file("mem"))
		overlay(b.file("mem"), b.file("mem.diff"))
		os.Remove(b.file("mem.diff"))
		x.release(prev.point)
		ready()
	}()
}

// loadBranch is the branch point in dir, saved by a run from setup.
func loadBranch(dir string, setup *checkpoint) *branchPoint {
	b := &branchPoint{checkpoint: &checkpoint{dir: dir, setup: setup.dir, meta: setup.meta}}
	raw, err := os.ReadFile(b.file("branch.json"))
	if err != nil {
		die("--branch: %v", err)
	}
	check(json.Unmarshal(raw, b))
	if b.Setup != setup.dir {
		die("--branch: %s was saved from another version of the project (or another kit's)", dir)
	}
	b.Ref.Checkpoint = dir
	return b
}
