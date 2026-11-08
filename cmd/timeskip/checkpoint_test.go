package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTrim checks that the cache drops what no run used for longer than
// unused, keeps what's recent or in use, and that a use counts; and that
// it keeps the last runs' logs and branch points, and drops what can't be
// used any more.
func TestTrim(t *testing.T) {
	cache = t.TempDir()
	long := time.Now().Add(-unused - time.Hour)
	entry := func(lock string, files ...string) string {
		p := filepath.Join(cache, lock)
		os.MkdirAll(filepath.Dir(p), 0o755)
		for _, f := range append(files, lock) {
			if err := os.WriteFile(filepath.Join(cache, f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	backdate := func(p string) { os.Chtimes(p, long, long) }
	for _, d := range []string{"old", "held", "recent", "reused", "orphan"} {
		os.MkdirAll(filepath.Join(cache, "checkpoints", d), 0o755)
		os.WriteFile(filepath.Join(cache, "checkpoints", d, "done"), nil, 0o644)
	}
	// One a killed run left unfinished, which none uses.
	os.MkdirAll(filepath.Join(cache, "checkpoints", "unfinished"), 0o755)
	entry("checkpoints/unfinished.lock")
	backdate(entry("checkpoints/old.lock"))
	backdate(entry("checkpoints/held.lock"))
	entry("checkpoints/recent.lock")
	entry("checkpoints/orphan.lock") // (of a bundle that's gone)
	os.WriteFile(filepath.Join(cache, "checkpoints", "orphan", "meta.json"), fmt.Appendf(nil, `{"bundle": %q}`, filepath.Join(cache, "gone.tsb")), 0o644)
	reused := entry("checkpoints/reused.lock")
	backdate(reused)
	used(reused)
	backdate(entry("bundles/p.tsb.lock", "bundles/p.tsb", "bundles/p.tsb.inputs", "bundles/p.tsb.sums"))
	entry("bundles/q.tsb.lock", "bundles/q.tsb")
	entry("bundles/gone.tsb.lock", "bundles/gone.tsb") // (a project that's gone)
	os.WriteFile(filepath.Join(cache, "bundles/gone.tsb.lock"), []byte(filepath.Join(cache, "gone/compose.yaml")), 0o644)
	os.MkdirAll(filepath.Join(cache, "logs", "old"), 0o755)
	backdate(filepath.Join(cache, "logs", "old"))
	// Branch points whose setup checkpoint is gone are no use.
	point := func(dir, setup string) {
		os.MkdirAll(filepath.Join(cache, dir), 0o755)
		os.WriteFile(filepath.Join(cache, dir, "branch.json"), fmt.Appendf(nil, `{"setup": %q}`, filepath.Join(cache, setup)), 0o644)
	}
	point("branches/111/1", "checkpoints/recent")
	point("branches/111/2", "checkpoints/gone")
	point("branches/110/1", "checkpoints/gone")
	// Of runs' logs and branch points, the newest (10 and 3) are kept (by
	// time, not name: 111 is the newest), and what a run uses (holds),
	// whatever its age.
	for i := range 12 {
		at := time.Now().Add(time.Duration(i-12) * time.Minute)
		for _, p := range []string{fmt.Sprint("logs/", 100+i, "-setup.log"), fmt.Sprint("branches/", 100+i)} {
			if !strings.HasPrefix(p, "logs") {
				os.MkdirAll(filepath.Join(cache, p), 0o755)
			} else {
				os.WriteFile(filepath.Join(cache, p), nil, 0o644)
			}
			os.Chtimes(filepath.Join(cache, p), at, at)
		}
	}
	hold(filepath.Join(cache, "branches/100"), syscall.LOCK_SH)
	// An images project's compose file goes with its bundle.
	for _, d := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(cache, "images", d), 0o755)
		os.WriteFile(filepath.Join(cache, "images", d, "compose.yaml"), nil, 0o644)
	}
	entry("bundles/"+hash(filepath.Join(cache, "images", "a", "compose.yaml"))+".tsb.lock",
		"bundles/"+hash(filepath.Join(cache, "images", "a", "compose.yaml"))+".tsb")
	os.MkdirAll(filepath.Join(cache, "coverage"), 0o755)
	backdate(entry("coverage/old"))
	entry("coverage/new")

	fd, ok := tryLock(filepath.Join(cache, "checkpoints", "held.lock"), syscall.LOCK_EX) // (a run using it)
	if !ok {
		t.Fatal("can't lock held")
	}
	defer syscall.Close(fd)
	trim()

	for p, keep := range map[string]bool{
		"checkpoints/old": false, "checkpoints/old.lock": false,
		"checkpoints/held": true, "checkpoints/recent": true, "checkpoints/reused": true, "checkpoints/orphan": false,
		"checkpoints/unfinished": false, "checkpoints/unfinished.lock": false,
		"bundles/p.tsb": false, "bundles/p.tsb.inputs": false, "bundles/p.tsb.sums": false, "bundles/p.tsb.lock": false,
		"bundles/q.tsb": true, "bundles/gone.tsb": false, "logs/old": false,
		"logs/102-setup.log": true, "logs/101-setup.log": false, "logs/100-setup.log": false,
		"branches/108": true, "branches/107": false, "branches/100": true, // (110 went: see above)
		"branches/111/1": true, "branches/111/2": false, "branches/110": false,
		"images/a": true, "images/b": false, "coverage/old": false, "coverage/new": true,
	} {
		if _, err := os.Stat(filepath.Join(cache, p)); (err == nil) != keep {
			t.Errorf("%s: kept %v, want %v", p, err == nil, keep)
		}
	}
}
