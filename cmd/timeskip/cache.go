package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// A cacheEntry is what the cache keeps of one thing (see trim): a
// checkpoint, a cached bundle, a run's logs or branch points, or a
// module's coverage.
type cacheEntry struct {
	kind, what string
	paths      []string
	used       time.Time // (lock files' times move at most hourly: see used)
}

// cacheEntries lists the cache's entries, cached bundles first (a
// checkpoint's bundle.tsb is a link to one).
func cacheEntries() []cacheEntry {
	var es []cacheEntry
	mtime := func(p string) time.Time {
		fi, _ := os.Stat(p)
		if fi == nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
	// of is what a bundle is of: its project (a compose file), or the
	// images of an images checkpoint's project; a bundle tested by path is
	// its own.
	of := func(b string) string {
		project, _ := os.ReadFile(b + ".lock") // (see cachedBundle)
		p := string(project)
		if p == "" {
			return b
		}
		if filepath.Dir(filepath.Dir(p)) != filepath.Join(cache, "images") {
			return p
		}
		var imgs []string
		c, _ := os.ReadFile(p)
		for _, l := range strings.Split(string(c), "\n") {
			if img, ok := strings.CutPrefix(strings.TrimSpace(l), "image: "); ok {
				if u, err := strconv.Unquote(img); err == nil {
					imgs = append(imgs, u)
				}
			}
		}
		return "images " + strings.Join(imgs, " ")
	}
	bundles, _ := filepath.Glob(filepath.Join(cache, "bundles", "*.tsb"))
	for _, b := range bundles {
		es = append(es, cacheEntry{"bundle", of(b), []string{b, b + ".inputs", b + ".sums"}, mtime(b + ".lock")})
	}
	locks, _ := filepath.Glob(filepath.Join(cache, "checkpoints", "*.lock"))
	for _, l := range locks {
		d := strings.TrimSuffix(l, ".lock")
		e := cacheEntry{"unfinished", "", []string{d}, mtime(l)}
		var m meta
		if raw, err := os.ReadFile(filepath.Join(d, "meta.json")); err == nil && json.Unmarshal(raw, &m) == nil && exists(filepath.Join(d, "done")) {
			e.kind, e.what = "setup", of(m.Bundle)
			switch {
			case m.Bundle == "":
				e.kind, e.what = "base", "every bundle"
			case strings.HasPrefix(e.what, "images "):
				e.kind, e.what = "images", strings.TrimPrefix(e.what, "images ")
			case len(m.Catalog) == 0:
				e.kind = "created"
			}
		}
		es = append(es, e)
	}
	for kind, dir := range map[string]string{"branches": "branch points", "logs": "logs", "coverage": "coverage"} {
		runs, _ := filepath.Glob(filepath.Join(cache, kind, "*"))
		for _, r := range runs {
			es = append(es, cacheEntry{dir, filepath.Base(r), []string{r}, mtime(r)})
		}
	}
	return es
}

// sizes are the bytes an entry's files allocate, and of those its own:
// what removing it alone frees. Checkpoints are copy-on-write clones of
// those they start from (on APFS, XFS and btrfs), and a block clones share
// is no one's own. A file seen (another link to it) counts no more.
func (e cacheEntry) sizes(seen map[[2]uint64]bool) (own, alloc int64) {
	for _, p := range e.paths {
		filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			var st syscall.Stat_t
			if syscall.Stat(f, &st) != nil || seen[[2]uint64{uint64(st.Dev), st.Ino}] {
				return nil
			}
			seen[[2]uint64{uint64(st.Dev), st.Ino}] = true
			a := st.Blocks * 512
			own += min(ownSize(f, a), a)
			alloc += a
			return nil
		})
	}
	return own, alloc
}

// cacheList prints the cache's entries, the last used first.
func cacheList() {
	es := cacheEntries()
	seen := map[[2]uint64]bool{}
	type row struct {
		e          cacheEntry
		own, alloc int64
	}
	var rows []row
	var own, alloc int64
	for _, e := range es {
		o, a := e.sizes(seen)
		rows = append(rows, row{e, o, a})
		own, alloc = own+o, alloc+a
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.e.used.Compare(a.e.used) })
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "      OWN\tALLOCATED\tLAST USED\tKIND\tFOR")
	for _, r := range rows {
		fmt.Fprintf(w, "%9s\t%9s\t%s\t%s\t%s\n", size(r.own), size(r.alloc), r.e.used.Format("2006-01-02 15:04"), r.e.kind, r.e.what)
	}
	w.Flush()
	fmt.Printf(`
%s: %s own, %s allocated, in %d entries. Own is what removing an
entry alone frees. Checkpoints are copy-on-write clones of the ones they
start from (on APFS, XFS and btrfs): the blocks they share are no entry's
own, and du counts them in each. "timeskip cache prune" removes what no
run is using.
`, cache, size(own), size(alloc), len(es))
}

// cachePrune removes the cache's entries that no run is using (all, or
// those no run used for longer than age, given as older), and reports what
// that freed: the disk's free space before and after.
func cachePrune(age time.Duration, older string) {
	free := func() int64 {
		syscall.Sync()
		var st syscall.Statfs_t
		var last int64 = -1
		// (Freed blocks show in a moment: until two looks agree.)
		for i := 0; i < 20; i++ {
			check(syscall.Statfs(cache, &st))
			n := int64(st.Bavail) * int64(st.Bsize)
			if n == last {
				break
			}
			last = n
			time.Sleep(500 * time.Millisecond)
		}
		return last
	}
	before, n := free(), len(cacheEntries())
	sweep()
	trim(age)
	after, left := free(), len(cacheEntries())
	fmt.Printf("timeskip: removed %d of %d cache entries (%d kept: in use", n-left, n, left)
	if age > 0 {
		fmt.Printf(", or used in the last %s", older)
	}
	fmt.Printf("); the disk's free space grew by %s\n", size(after-before))
}

// size is n bytes in GiB, or MiB if less.
func size(n int64) string {
	if n < 1<<30 && n > -1<<30 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

// parseAge is --older-than's duration: Go's (90m, 12h) or days (7d).
func parseAge(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(d, 64)
		return time.Duration(n * 24 * float64(time.Hour)), err
	}
	return time.ParseDuration(s)
}
