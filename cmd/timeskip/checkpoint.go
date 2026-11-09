package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/bundle"
	"github.com/ryanworl/timeskip/internal/protocol"
)

// A checkpoint is a saved VM (in $TS_CACHE/checkpoints): the base
// checkpoint, saved once dockerd is up in a VM booted for the kit; a
// bundle's created checkpoint, restored from the base (or from an images
// checkpoint, the created checkpoint of a bundle of images only) and saved
// once its images are loaded and its containers created; or its setup
// checkpoint, restored from that and saved once the project wrote
// setup_complete and went idle (or a branch point: see explore.go). Its
// directory holds state and its memory (mem; on macOS, hvrun's
// state.mem), disk (its data disk), meta.json and done (once it's
// complete, and synced). A base, created or setup checkpoint also
// holds bundle.tsb (the bundle's drive: a link to the bundle, or the base's
// placeholder) and inputs.ph (Linux: the inputs
// drive's placeholder) or inputs/ (macOS: the share's).
type checkpoint struct {
	dir   string
	setup string // its setup checkpoint's directory
	meta  meta
}

type meta struct {
	Nonce   string           `json:"nonce"`
	Catalog protocol.Catalog `json:"catalog"`
	Bundle  string           `json:"bundle"` // the path of the bundle it's for
	Kit     string           `json:"kit"`    // the kitID of the kit that saved it
}

func (c *checkpoint) file(name string) string { return filepath.Join(c.dir, name) }
func (c *checkpoint) bundle() string          { return filepath.Join(c.setup, "bundle.tsb") }
func (c *checkpoint) placeholder() string     { return filepath.Join(c.setup, "inputs.ph") }
func (c *checkpoint) done() bool              { return exists(c.file("done")) }

func (c *checkpoint) load() {
	b, err := os.ReadFile(c.file("meta.json"))
	check(err)
	check(json.Unmarshal(b, &c.meta))
}

func (c *checkpoint) publish() {
	// (Errors don't die: exit waits for publishing. A checkpoint without
	// its done file is made again.)
	mem := c.file(map[bool]string{false: "mem", true: "state.mem"}[darwin]) // (hvrun's: see hv)
	b, _ := json.Marshal(c.meta)
	if os.WriteFile(c.file("meta.json"), b, 0o644) != nil {
		return
	}
	sparsify(mem)
	for _, p := range []string{mem, c.file("state"), c.file("disk"), c.file("meta.json")} {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return
		}
	}
	os.WriteFile(c.file("done"), nil, 0o644)
}

// kitID identifies the kit by its MANIFEST, which has every file's SHA-256:
// a checkpoint is only good for the kernel, guest and VMM that saved it,
// on macOS for the OS build (whose Hypervisor.framework may save the GIC
// differently), and on Linux for where the kit is (Firecracker's saved VM
// names its root filesystem's file).
var kitID = sync.OnceValue(func() string {
	m, err := os.ReadFile(filepath.Join(home, "MANIFEST"))
	if err != nil {
		die("the kit has no MANIFEST: %v", err)
	}
	if err := checkKit(home, string(m)); err != nil {
		die("%v", err)
	}
	_, rootfs := guest()
	return hash(osBuild(), string(m), map[bool]string{false: rootfs}[darwin])
})

// checkKit: the kit's programs and guest (the CLI, bin/ and share/) are the
// files its MANIFEST lists. The cache's checkpoints are the
// MANIFEST's, so a file changed in place (a debugging hvrun copied into a
// kit, say) would use and save that kit's as its own.
func checkKit(dir, manifest string) error {
	_, files, _ := strings.Cut(manifest, "\n\n")
	for _, l := range strings.Split(files, "\n") {
		sum, name, ok := strings.Cut(l, "  ./")
		if !ok || name != "timeskip" && !strings.HasPrefix(name, "bin/") && !strings.HasPrefix(name, "share/") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("the kit's %s: %v", name, err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("the kit's %s: %v", name, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != sum {
			return fmt.Errorf("the kit's %s isn't the file its MANIFEST lists (SHA-256 %s, not %s): the cache keeps checkpoints by the MANIFEST, so a changed file would use another kit's. To run it, put its SHA-256 on its line in %s, which makes it a kit of its own", name, got, sum, filepath.Join(dir, "MANIFEST"))
		}
	}
	return nil
}

// setupCheckpoint returns the setup checkpoint of bundle b (b is, or will
// be, at origin) with edits applied before its containers start, saving it
// first if there isn't one, or why setup failed. It holds the checkpoint (a
// shared lock) until this process exits.
func setupCheckpoint(b, origin string, edits []edit) (*checkpoint, error) {
	c, err := saved(hash(bundle.ID(b), kitID(), memMB, slackArg(), syncKey(edits)), func(c *checkpoint) error {
		from, err := createdCheckpoint(b, origin)
		if err != nil {
			return err
		}
		return build(c, b, origin, from, "SETUP", edits)
	})
	if err != nil {
		return nil, err
	}
	prune(origin, c.dir, filepath.Join(cache, "checkpoints", createdID(b)))
	return c, nil
}

// prune removes the checkpoints of the bundle at origin but keep's: older
// versions aren't needed any more. And other kits' checkpoints (a cache
// keeps the last kit's).
func prune(origin string, keep ...string) {
	others, _ := filepath.Glob(filepath.Join(cache, "checkpoints", "*", "meta.json"))
	for _, o := range others {
		var m meta
		if raw, _ := os.ReadFile(o); json.Unmarshal(raw, &m) == nil && (m.Bundle == origin || m.Kit != kitID()) && !slices.Contains(keep, filepath.Dir(o)) {
			remove(filepath.Dir(o))
		}
	}
}

// createdCheckpoint returns the created checkpoint of bundle b (b is, or
// will be, at origin): its images loaded and its containers created, not
// started, which its setups start from. It saves it first if there isn't
// one, or returns why it couldn't, and holds it until this process exits.
func createdCheckpoint(b, origin string) (*checkpoint, error) {
	return saved(createdID(b), func(c *checkpoint) error {
		// (A project's bundle in the cache leaves out the images it doesn't
		// build: they're loaded in an images checkpoint, kept for its next
		// builds.)
		from := baseCheckpoint()
		var pulled []string
		for _, l := range strings.Split(bundleFile(b, "pulled"), "\n") {
			if f := strings.Fields(l); len(f) > 0 {
				pulled = append(pulled, f[0])
			}
		}
		if len(pulled) > 0 {
			var err error
			if from, err = imagesCheckpoint(pulled); err != nil {
				return err
			}
		}
		return build(c, b, origin, from, "CREATE", nil)
	})
}

func createdID(b string) string { return hash(bundle.ID(b), kitID(), memMB, slackArg()) }

// imagesCheckpoint is the created checkpoint of a project of images:
// bundled from a compose file listing them in a profile that's never
// started, so that they're loaded and nothing runs. Or why it couldn't be
// made. A project's created checkpoint starts from one, given the images
// it doesn't build.
func imagesCheckpoint(images []string) (*checkpoint, error) {
	images = slices.Compact(slices.Sorted(slices.Values(images)))
	dir := filepath.Join(cache, "images", hash(images))
	var c strings.Builder
	c.WriteString("services:\n")
	for i, img := range images {
		fmt.Fprintf(&c, "  image-%d:\n    image: %s\n    profiles: [images]\n", i, strconv.Quote(img))
	}
	f := filepath.Join(dir, "compose.yaml")
	// (Written under the lock: another run may be bundling f, or trimming
	// it.)
	mine, b, _, err := cachedBundle(f, nil, func() { check(os.MkdirAll(dir, 0o755)); write(f, c.String()) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "timeskip: bundling %s: %v\n", strings.Join(images, " "), err)
		return nil, err
	}
	cp, err := createdCheckpoint(mine, b)
	if err == nil {
		prune(b, cp.dir)
	}
	return cp, err
}

// baseCheckpoint returns the base checkpoint, which every bundle's setup
// starts from: a VM booted with a blank data disk and dockerd up, saved once
// for the kit (and the VM's memory size and timer slack). It holds it (a shared lock) until
// this process exits.
func baseCheckpoint() *checkpoint {
	c, _ := saved(hash("base", kitID(), memMB, slackArg()), func(c *checkpoint) error { buildBase(c); return nil })
	return c
}

// saved returns checkpoint id, saving it first with create if there isn't
// one, and holds it (a shared lock) until this process exits; or create's
// error. Checkpoints are kept in $TS_CACHE/checkpoints.
func saved(id string, create func(*checkpoint) error) (*checkpoint, error) {
	dir := filepath.Join(cache, "checkpoints", id)
	c := &checkpoint{dir: dir, setup: dir}
	check(os.MkdirAll(filepath.Dir(dir), 0o755))
	// (Saving it takes the lock exclusively; runs from it share it. Turning
	// a shared lock exclusive gives it up first, so two can't deadlock.)
	l := lock(dir+".lock", syscall.LOCK_SH)
	used(dir + ".lock")
	created := false
	if !c.done() {
		check(syscall.Flock(l, syscall.LOCK_EX))
		if !locked(l, dir+".lock") {
			syscall.Close(l)
			return saved(id, create)
		}
		if c.done() {
			check(syscall.Flock(l, syscall.LOCK_SH))
		} else {
			os.RemoveAll(c.dir)
			check(os.MkdirAll(c.dir, 0o755))
			// (A run that exits while making it removes what it made;
			// after a SIGKILL, trim does.)
			atExit(func() {
				if !c.done() {
					os.RemoveAll(c.dir)
				}
			})
			if err := create(c); err != nil {
				os.RemoveAll(c.dir)
				syscall.Close(l)
				return nil, err
			}
			// It's usable now. Publishing it (making its memory file
			// sparse, syncing it) goes on in the background, with the lock
			// held exclusively until it's done.
			created = true
			published := make(chan struct{})
			go func() {
				c.publish()
				syscall.Flock(l, syscall.LOCK_SH)
				close(published)
			}()
			atExit(func() { <-published })
		}
	}
	if !created {
		c.load()
	}
	return c, nil
}

// remove deletes checkpoint dir unless someone is using it.
func remove(dir string) {
	if l, ok := tryLock(dir+".lock", syscall.LOCK_EX); ok {
		os.RemoveAll(dir)
		os.Remove(dir + ".lock")
		syscall.Close(l)
	}
}

// unused is how long the cache keeps what no run used: checkpoints and
// cached bundles, whose lock files' times say when they were last used (at
// most an hour ago, as in Go's build cache), failed timelines' logs and the
// branch points they started from.
const unused = 5 * 24 * time.Hour

// used records that the cache entry whose lock file is p was just used.
func used(p string) {
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
		now := time.Now()
		os.Chtimes(p, now, now)
	}
}

// keepLast is, by kind, how many runs' failed timelines' logs (or setup
// logs), and branch points, the cache keeps: a run's branch points take
// gigabytes.
var keepLast = map[string]int{"logs": 10, "branches": 3}

// trim removes what the cache kept unused for longer than unused, unless
// a run is using it; of the logs and branch points runs kept, all but the
// last runs' (keepLast); cached bundles whose project is gone, and
// checkpoints whose bundle is; branch points whose setup checkpoint is
// gone; and images projects whose bundle is gone (see imagesCheckpoint).
func trim() {
	old := func(p string) bool { fi, err := os.Stat(p); return err == nil && time.Since(fi.ModTime()) > unused }
	locks, _ := filepath.Glob(filepath.Join(cache, "checkpoints", "*.lock"))
	for _, l := range locks {
		// (Unused, or never finished: a run killed while it made it.)
		if c := strings.TrimSuffix(l, ".lock"); old(l) || !exists(filepath.Join(c, "done")) {
			remove(c)
		}
	}
	locks, _ = filepath.Glob(filepath.Join(cache, "bundles", "*.tsb.lock"))
	for _, l := range locks {
		project, _ := os.ReadFile(l) // (see cachedBundle)
		if fd, ok := tryLock(l, syscall.LOCK_EX); ok && (old(l) || len(project) > 0 && !exists(string(project))) {
			b := strings.TrimSuffix(l, ".lock")
			os.Remove(b)
			os.Remove(b + ".inputs")
			os.Remove(b + ".sums")
			os.Remove(l)
			syscall.Close(fd)
		} else if ok {
			syscall.Close(fd)
		}
	}
	metas, _ := filepath.Glob(filepath.Join(cache, "checkpoints", "*", "meta.json"))
	for _, p := range metas {
		var m meta
		if raw, err := os.ReadFile(p); err == nil && json.Unmarshal(raw, &m) == nil && m.Bundle != "" && !exists(m.Bundle) {
			remove(filepath.Dir(p))
		}
	}
	images, _ := filepath.Glob(filepath.Join(cache, "images", "*", "compose.yaml"))
	for _, f := range images {
		b := filepath.Join(cache, "bundles", hash(f)+".tsb")
		if fd, ok := tryLock(b+".lock", syscall.LOCK_EX); ok {
			if !exists(b) {
				os.RemoveAll(filepath.Dir(f))
				os.Remove(b + ".lock")
			}
			syscall.Close(fd)
		}
	}
	points, _ := filepath.Glob(filepath.Join(cache, "branches", "*", "*", "branch.json"))
	for _, p := range points {
		var b struct{ Setup string }
		if raw, err := os.ReadFile(p); err == nil && json.Unmarshal(raw, &b) == nil && !exists(b.Setup) && !held(filepath.Dir(filepath.Dir(p))) {
			os.RemoveAll(filepath.Dir(p))
			os.Remove(filepath.Dir(filepath.Dir(p))) // (if empty)
		}
	}
	for kind, n := range keepLast {
		runs, _ := filepath.Glob(filepath.Join(cache, kind, "*"))
		mtime := func(p string) (t time.Time) {
			if fi, err := os.Stat(p); err == nil {
				t = fi.ModTime()
			}
			return t
		}
		slices.SortFunc(runs, func(a, b string) int { return mtime(b).Compare(mtime(a)) }) // (newest first)
		for i, p := range runs {
			if (i >= n || old(p)) && !held(p) {
				os.RemoveAll(p)
			}
		}
	}
	coverage, _ := filepath.Glob(filepath.Join(cache, "coverage", "*"))
	for _, p := range coverage {
		if old(p) {
			os.Remove(p)
		}
	}
}

// buildBase saves the base checkpoint: a VM booted with a placeholder for
// the bundle's drive and a blank data disk, once it reports BASE.
func buildBase(c *checkpoint) {
	sparse(c.bundle(), 1<<20)
	sparse(c.file("disk"), 64<<30) // (sparse: room for big images costs nothing)
	disks := []disk{{c.bundle(), true}, {c.file("disk"), false}}
	if darwin {
		check(os.MkdirAll(c.file("inputs"), 0o755))
	} else {
		sparse(c.placeholder(), 1<<20)
		disks = append(disks, disk{c.placeholder(), true})
	}
	n := nonce()
	v := start(c.file("vm"), n, disks, c.file("inputs"), nil, "", "", false)
	if l := v.next(); l.verb != "BASE" {
		v.kill()
		showTail(v.file("console.log"), 40)
		die("the VM didn't start (%s)", l.verb)
	}
	v.save(c.dir, c.file("disk"), c.placeholder(), false)
	go v.kill() // (waited for at exit)
	c.meta = meta{Nonce: n, Kit: kitID()}
}

// build saves one of bundle b's checkpoints (b is, or will be, at origin):
// a VM restored from checkpoint from and sent verb. Given CREATE (from is
// the base or an images checkpoint), the bundle's created checkpoint, once
// it reports CREATED; given SETUP (from is the created checkpoint), its
// setup checkpoint with edits applied, once it reports READY. If it fails,
// it shows why and returns the error.
func build(c *checkpoint, b, origin string, from *checkpoint, verb string, edits []edit) error {
	if os.Link(b, c.bundle()) != nil {
		clone(b, c.bundle())
	}
	if verb == "CREATE" {
		if f := bundleFile(b, "format"); f != bundle.Format {
			die("%s is a bundle of format %q, and this timeskip reads format %s: bundle the project again with this timeskip", b, f, bundle.Format)
		}
		a := bundleFile(b, "arch")
		if a != hostArch {
			die("%s is for linux/%s VMs, and this machine runs linux/%s ones (bundle it with --arch %s)", b, a, hostArch, hostArch)
		}
		fmt.Fprintf(os.Stderr, "timeskip: loading %s (a checkpoint for each version of a bundle)\n", filepath.Base(origin))
	} else {
		fmt.Fprintf(os.Stderr, "timeskip: setting up %s (a checkpoint for each version of its synced files)\n", filepath.Base(origin))
	}
	clone(from.file("disk"), c.file("disk"))
	go writeOut(c.file("disk"))
	if !darwin {
		sparse(c.placeholder(), 1<<20)
	}
	in := stage(c.dir, edits) // (in the share, c's inputs, on macOS)
	v := start(c.file("vm"), from.meta.Nonce, nil, in.dir, from, c.file("disk"), cmp.Or(in.img, c.placeholder()), true)
	// (On Linux the save writes the pages written since the restore over a
	// copy of the base's memory, made meanwhile.)
	copied := make(chan struct{})
	go func() {
		if !darwin {
			clone(from.file("mem"), c.file("mem"))
		}
		close(copied)
	}()
	// (On Linux its timelines' first drive is its own link to the bundle.)
	if !darwin {
		v.check(v.api("PATCH", "/drives/d0", map[string]string{"drive_id": "d0", "path_on_host": c.bundle()}))
	}
	switch {
	case verb == "SETUP":
		v.send(verb, "inputs="+in.spec)
	case darwin:
		check(os.Link(c.bundle(), c.file("inputs/bundle.tsb")))
		v.send(verb, "bundle=virtiofs")
	default:
		st, err := os.Stat(c.bundle())
		check(err)
		v.send(verb, fmt.Sprintf("bundle=blk:%d", st.Size()))
	}
	l := v.next()
	log := v.file("console.log")
	if l.verb != map[string]string{"CREATE": "CREATED", "SETUP": "READY"}[verb] {
		v.kill()
		showTail(log, 40)
		err := errors.New("the VM stopped")
		if r := exitOf(l); r.Error != "" {
			err = errors.New(r.Error)
		}
		fmt.Fprintf(os.Stderr, "timeskip: setup failed: %v\n", err)
		keep := filepath.Join(cache, "logs", time.Now().Format("20060102-150405.000")+"-setup", "setup")
		if os.MkdirAll(filepath.Dir(keep), 0o755) == nil && os.Rename(log, keep+".log") == nil {
			fmt.Fprintf(os.Stderr, "timeskip: its log is %s.log\n", keep)
		}
		if containerLogs(c.file("disk"), keep+".containers.log") {
			fmt.Fprintf(os.Stderr, "timeskip: its containers' logs are %s.containers.log\n", keep)
		}
		return err
	}
	var ready protocol.Ready
	if verb == "SETUP" {
		check(json.Unmarshal([]byte(l.body), &ready))
	}
	<-copied
	if in.img != "" { // (its timelines get the placeholder)
		v.check(v.api("PATCH", "/drives/d2", map[string]string{"drive_id": "d2", "path_on_host": c.placeholder()}))
	}
	v.save(c.dir, c.file("disk"), c.placeholder(), false)
	go v.kill()
	// (Its share, empty, is its timelines'.)
	check(os.RemoveAll(in.dir))
	os.Remove(in.img)
	check(os.Mkdir(in.dir, 0o755))
	c.meta = meta{Nonce: from.meta.Nonce, Catalog: ready.Catalog, Bundle: origin, Kit: from.meta.Kit}
	return nil
}

// lock takes a lock (flock) on path, held until this process exits or the
// returned descriptor is closed.
func lock(path string, how int) int {
	fd, err := flock(path, how)
	check(err)
	return fd
}

// tryLock takes a lock on path (how: LOCK_EX or LOCK_SH) if no one holds
// one that excludes it (and it can be opened).
func tryLock(path string, how int) (int, bool) {
	fd, err := flock(path, how|syscall.LOCK_NB)
	return fd, err == nil
}

func flock(path string, how int) (int, error) {
	for {
		fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o644)
		if err != nil {
			return 0, err
		}
		if err := syscall.Flock(fd, how); err != nil {
			syscall.Close(fd)
			return 0, err
		}
		if locked(fd, path) {
			return fd, nil
		}
		syscall.Close(fd)
	}
}

// locked is whether fd, just locked, is still the file at path: remove
// deletes a lock file while holding it, so one that waited for it may hold a
// lock on a file that's gone, while another run locks a new one.
func locked(fd int, path string) bool {
	var a, b syscall.Stat_t
	return syscall.Fstat(fd, &a) == nil && syscall.Stat(path, &b) == nil && a.Dev == b.Dev && a.Ino == b.Ino
}

func showTail(log string, n int) {
	b, _ := os.ReadFile(log)
	l := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	fmt.Fprintln(os.Stderr, strings.Join(l[max(0, len(l)-n):], "\n"))
}
