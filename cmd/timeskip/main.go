// Command timeskip runs a Docker Compose project's test templates in
// isolated, single-vCPU Linux VMs that skip idle time. See usage below and
// README.md.
package main

import (
	"cmp"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/bundle"
)

const usage = `usage:
  timeskip test [compose.yaml|app.tsb] [--template NAME]... [--timelines N] [-j N]
                [--seed N] [--set KEY=VALUE]... [--out DIR] [--profile]
                [--explore] [--branch DIR] [--no-build] [--no-skip]
      Run the project's test templates (/opt/antithesis/test/v1/NAME/ in its
      containers): N timelines of each (default 1), up to -j at a time
      (default: one per CPU core, the most for all runs on the host), each
      from a checkpoint saved once the project wrote setup_complete. A
      compose file (default compose.yaml) is built where it changed (not
      with --no-build: its images as they are, as Antithesis takes them),
      bundled into the cache, and its develop.watch sync rules applied as
      docker compose watch would; a bundle runs as it is.
      The project's x-timeskip block sets how its timelines run, and each
      --set KEY=VALUE overrides one of its keys for this run:
        faults: high_availability (the default), resilient, legacy or none:
          Antithesis's fault tolerance levels, mixed differently in each
          timeline.
        disk_faults: true, or a list of fail, full and power (default: none).
        fault_gap: guest seconds between faults, about (default 10).
        drivers: driver launches per timeline (default 10).
        timeout: guest seconds a timeline may run (default 3600).
        memory: the VMs' memory in GiB (default 4).
      --out DIR writes results.json, each instrumented module's functions
      (coverage/MODULE.functions.tsv) and, with --profile, each timeline's
      stacks sampled every 10 ms of CPU time (profiles/TEMPLATE-SEED.folded).
      --explore branches timelines from moments that reached new edges or
      assertion states; --branch replays one, kept when a timeline from it
      failed. --no-skip runs in real time.
  timeskip bundle [compose.yaml] [--no-build] [--arch arm64|amd64] -o app.tsb
      Pack the project's images and compose file into a bundle (needs Docker).
  timeskip version

Exits 0 when every timeline passes, 1 when one fails, 125 if the project's
setup failed, 130 if interrupted, or 2 for usage and harness errors.

Environment: TS_CACHE (default ~/.cache/timeskip; on APFS, XFS or btrfs VM
disks are instant clones). It keeps the failed timelines' logs of the last
10 runs and their branch points of the last 3; what no run used for 5 days is
removed from it.
`

var (
	home     string // the kit: this executable's directory
	cache    string
	memMB    = 4096 // the VMs' memory (x-timeskip's memory)
	noBuild  bool   // --no-build
	darwin   = runtime.GOOS == "darwin"
	hostArch = runtime.GOARCH
)

// settings applies a project's x-timeskip settings (JSON) to o, each
// --set KEY=VALUE in sets overriding one.
func settings(js string, sets []string, o *options) {
	var x map[string]any
	if json.Unmarshal([]byte(js), &x) != nil || x == nil {
		x = map[string]any{}
	}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			die("--set %s: expected KEY=VALUE", s)
		}
		x[k] = v
	}
	num := func(k string, min float64) float64 {
		f, err := strconv.ParseFloat(fmt.Sprint(x[k]), 64)
		if err != nil || f < min {
			die("x-timeskip: %s is a number, at least %g", k, min)
		}
		return f
	}
	for k, v := range x {
		switch k {
		case "faults":
			switch l := fmt.Sprint(v); l {
			case "legacy", "resilient", "high_availability":
				o.faults = l
			case "none":
				o.faults = ""
			default:
				die("x-timeskip: faults is high_availability, resilient, legacy or none")
			}
		case "disk_faults":
			var kinds []string
			switch v := v.(type) {
			case []any:
				for _, k := range v {
					kinds = append(kinds, fmt.Sprint(k))
				}
			default:
				kinds = strings.Split(fmt.Sprint(v), ",")
			}
			o.diskFaults = nil
			for _, k := range kinds {
				switch k = strings.TrimSpace(k); k {
				case "true":
					o.diskFaults = []string{"all"}
				case "false", "":
				case "fail", "full", "power":
					if !slices.Contains(o.diskFaults, k) {
						o.diskFaults = append(o.diskFaults, k)
					}
				default:
					die("x-timeskip: disk_faults is true, false or a list of fail, full and power")
				}
			}
		case "fault_gap":
			if o.faultGap = num(k, 0); o.faultGap == 0 {
				die("x-timeskip: fault_gap is above 0")
			}
		case "drivers":
			o.drivers = int(num(k, 1))
		case "timeout":
			o.timeout = int(num(k, 1))
		case "memory":
			memMB = int(num(k, 1)) << 10
		default:
			die("x-timeskip: no setting %s (faults, disk_faults, fault_gap, drivers, timeout, memory)", k)
		}
	}
	if len(o.diskFaults) > 0 && o.faults == "" {
		die("x-timeskip: disk_faults needs faults (not none)")
	}
	if slices.Equal(o.diskFaults, []string{"all"}) {
		o.diskFaults = []string{"fail", "full"}
		if o.faults == "high_availability" {
			o.diskFaults = append(o.diskFaults, "power")
		}
	}
	if slices.Contains(o.diskFaults, "power") && o.faults != "high_availability" {
		die("x-timeskip: disk_faults power needs faults high_availability, whose kills it makes power losses")
	}
}

func main() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	check(err)
	home = filepath.Dir(exe)
	if cache = os.Getenv("TS_CACHE"); cache == "" {
		h, err := os.UserHomeDir()
		check(err)
		cache = filepath.Join(h, ".cache", "timeskip")
	}
	cache = abs(cache)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		go func() { <-sig; os.Exit(130) }() // (a second one doesn't wait for the cleanups)
		exit(130)
	}()

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	switch cmd {
	case "bundle":
		arch, out := fs.String("arch", hostArch, ""), fs.String("o", "", "")
		fs.BoolVar(&noBuild, "no-build", false, "")
		pos := parse(fs, args, -1)
		if *out == "" || len(pos) > 1 {
			die("usage: timeskip bundle [compose.yaml] [--no-build] [--arch arm64|amd64] -o app.tsb")
		}
		_, err := bundle.Create(project(pos), *arch, *out, bundle.Options{Synced: true, NoBuild: noBuild, Tmp: tempDir(), Log: os.Stdout})
		check(err)
	case "test":
		o := options{timelines: 1, jobs: cores(), drivers: 10, timeout: 3600, skip: true, faults: "high_availability", faultGap: 10}
		fs.Func("set", "", func(v string) error { o.sets = append(o.sets, v); return nil })
		fs.Func("template", "", func(v string) error { o.templates = append(o.templates, v); return nil })
		fs.BoolFunc("no-skip", "", func(string) error { o.skip = false; return nil })
		fs.BoolVar(&noBuild, "no-build", false, "")
		fs.IntVar(&o.timelines, "timelines", o.timelines, "")
		fs.IntVar(&o.jobs, "j", o.jobs, "")
		fs.Uint64Var(&o.seed, "seed", 0, "")
		fs.StringVar(&o.out, "out", "", "")
		fs.BoolVar(&o.profile, "profile", false, "")
		fs.BoolVar(&o.explore, "explore", false, "")
		fs.StringVar(&o.branch, "branch", "", "")
		pos := parse(fs, args, -1)
		if len(pos) > 1 {
			fs.Usage()
			exit(2)
		}
		wantSymbols.Store(o.out != "")
		if o.seed == 0 {
			o.seed = uint64(rand.Int64N(1e6)) + 1
		}
		if o.timelines < 1 || o.jobs < 1 {
			die("--timelines and -j must be at least 1")
		}
		if o.profile && o.out == "" {
			die("--profile writes to --out DIR")
		}
		if d := filepath.Dir(abs(o.branch)); o.branch != "" && filepath.Dir(d) == filepath.Join(cache, "branches") {
			// (Its run's branch points, which trim keeps for the last runs:
			// they're used now, and kept while they are.)
			hold(d, syscall.LOCK_SH)
			now := time.Now()
			os.Chtimes(d, now, now)
		}
		var edits []edit
		var setup *checkpoint
		if f := abs(project(pos)); bundle.Files(f) != nil {
			settings(bundleFile(f, "settings"), o.sets, &o)
			setup = mustSetup(setupCheckpoint(f, f))
			edits = syncedEdits(f)
		} else {
			// The project's bundle, in the cache, with the images it builds;
			// the VM has the others already (see setupCheckpoint). A run
			// takes a link to it, which another run's can't change.
			var js string
			mine, b, syncs, err := cachedBundle(f, &js, nil)
			check(err)
			for _, s := range syncs {
				e := edit{s.Service, s.Target, s.Path, s.Action, s.Skip, nil}
				for _, p := range s.Deleted {
					e.deleted = append(e.deleted, s.Target+strings.TrimPrefix(p, s.Path))
				}
				edits = append(edits, e)
			}
			settings(js, o.sets, &o)
			setup = mustSetup(setupCheckpoint(mine, b))
		}
		cmdTest(setup, edits, o)
	case "version":
		parse(fs, args, 0)
		m, err := os.ReadFile(filepath.Join(home, "MANIFEST"))
		check(err)
		fmt.Println(strings.SplitN(string(m), "\n\n", 2)[0]) // (the header, before the files)
	default:
		fmt.Fprint(os.Stderr, usage)
		exit(2)
	}
	exit(0)
}

// project is the compose file or bundle named by the positional arguments:
// compose.yaml, or in a directory named, by default.
func project(pos []string) string {
	p := cmp.Or(append(pos, "compose.yaml")...)
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		p = filepath.Join(p, "compose.yaml")
	}
	return p
}

// mustSetup is a setup checkpoint, or exits 125 if setup failed (which it
// showed). Either way it first removes what killed runs left, now that
// this run holds what it uses; and the cache is trimmed as the run ends.
func mustSetup(c *checkpoint, err error) *checkpoint {
	sweep()
	if err != nil {
		trim()
		exit(125)
	}
	return c
}

// bundleFile is one of bundle b's files (see internal/bundle), "" if it
// has none.
func bundleFile(b, name string) string {
	files := bundle.Files(b)
	if files == nil {
		die("%s: can't read the bundle", b)
	}
	return strings.TrimSpace(files[name])
}

// cachedBundle bundles project (a compose file) into the cache, with
// bundle.Options.Cache if asked for its settings, and returns a link to the bundle in a
// tempDir (that later bundling can't change), the bundle's path and its
// sync rules. prepare, if given, runs first, under the bundle's lock.
func cachedBundle(project string, settings *string, prepare func()) (mine, b string, syncs []bundle.Sync, err error) {
	b = filepath.Join(cache, "bundles", hash(project)+".tsb")
	check(os.MkdirAll(filepath.Dir(b), 0o755))
	l := lock(b+".lock", syscall.LOCK_EX)
	defer syscall.Close(l)
	write(b+".lock", project) // (when it was last used, and for which project: see trim)
	if prepare != nil {
		prepare()
	}
	d := tempDir()
	if syncs, err = bundle.Create(project, hostArch, b, bundle.Options{Cache: settings != nil, Settings: settings, NoBuild: noBuild, Tmp: d, Log: os.Stderr}); err != nil {
		return "", "", nil, err
	}
	mine = filepath.Join(d, "bundle.tsb")
	if os.Link(b, mine) != nil {
		clone(b, mine)
	}
	return mine, b, syncs, nil
}

// tempDir makes a directory in the cache that is removed on exit.
func tempDir() string { return tempIn(cache) }

// tempIn makes a directory run-* in parent (made if need be) that is
// removed on exit, or by sweep if this process dies first.
func tempIn(parent string) string {
	check(os.MkdirAll(parent, 0o755))
	d, err := os.MkdirTemp(parent, "run-")
	check(err)
	hold(d, syscall.LOCK_EX)
	atExit(func() { os.RemoveAll(d) })
	return d
}

// hold locks directory d (how: LOCK_EX, or LOCK_SH to share it) until
// this process exits, so that sweep and trim leave it alone.
func hold(d string, how int) {
	fd, err := syscall.Open(d, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	check(err)
	check(syscall.Flock(fd, how))
}

// held is whether a process holds directory d (or it's gone).
func held(d string) bool {
	fd, err := syscall.Open(d, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return true
	}
	defer syscall.Close(fd)
	return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// sweep removes what runs that died (killed, say) left: their tempIn
// directories that no process holds, unless they were just made (and are
// about to be held), in the cache and in memory (see memDir); the links in
// their branch point directories (see explorer.reserve), and those
// directories if that empties them; and Firecracker socket directories
// (see vm) whose socket no one listens on.
func sweep() {
	old := func(d string) bool {
		fi, err := os.Lstat(d)
		return err == nil && time.Since(fi.ModTime()) > time.Minute
	}
	socks, _ := filepath.Glob(filepath.Join(os.TempDir(), "timeskip-[0-9]*"))
	for _, d := range socks {
		if !old(d) {
			continue
		}
		if c, err := net.Dial("unix", filepath.Join(d, "api.sock")); err == nil {
			c.Close()
		} else {
			os.RemoveAll(d)
		}
	}
	runs, _ := filepath.Glob(filepath.Join(cache, "run-*"))
	if m := memRoot(); m != "" {
		mem, _ := filepath.Glob(filepath.Join(m, "run-*"))
		runs = append(runs, mem...)
	}
	for _, d := range runs {
		if old(d) && !held(d) {
			os.RemoveAll(d)
		}
	}
	branches, _ := filepath.Glob(filepath.Join(cache, "branches", "*"))
	for _, d := range branches {
		if old(d) && !held(d) {
			unlink(d)
		}
	}
}

// parse parses flags and n positional arguments (any number if n < 0), in
// any order.
func parse(fs *flag.FlagSet, args []string, n int) []string {
	var pos []string
	for {
		if fs.Parse(args) != nil {
			exit(2)
		}
		if args = fs.Args(); len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if n >= 0 && len(pos) != n {
		fs.Usage()
		exit(2)
	}
	return pos
}

// ---- exiting ----------------------------------------------------------------

var (
	mu       sync.Mutex
	cleanups []func()
	exiting  bool
	late     sync.RWMutex // (held by cleanups run late: see atExit)
)

// atExit registers fn to run however the process exits (through exit).
// Once exit began, it runs fn at once and never returns: the other
// goroutines go on meanwhile (a timeline may start as exit kills the VM of
// the one before), and what they make is undone and nothing more is made.
func atExit(fn func()) {
	mu.Lock()
	if exiting {
		mu.Unlock()
		late.RLock()
		fn()
		late.RUnlock()
		select {}
	}
	cleanups = append(cleanups, fn)
	mu.Unlock()
}

// stopping is whether exit began.
func stopping() bool { mu.Lock(); defer mu.Unlock(); return exiting }

// exit runs the cleanups, newest first, and exits. Once it began, a second
// exit (from a signal, or an error the cleanups caused) waits for it.
func exit(code int) {
	mu.Lock()
	if exiting {
		mu.Unlock()
		select {}
	}
	exiting = true
	mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	late.Lock()
	os.Exit(code)
}

// die reports an error and exits; once exit began, the error is one the
// cleanups caused, and it only waits.
func die(format string, a ...any) {
	if !stopping() {
		fmt.Fprintf(os.Stderr, "timeskip: "+format+"\n", a...)
	}
	exit(2)
}

func check(err error) {
	if err != nil {
		die("%v", err)
	}
}

// ---- small helpers ----------------------------------------------------------

func abs(p string) string { a, err := filepath.Abs(p); check(err); return a }

func nonce() string { b := make([]byte, 6); crand.Read(b); return hex.EncodeToString(b) }

// hash is a short digest of its arguments, for cache keys.
func hash(a ...any) string { return fmt.Sprintf("%x", sha256.Sum256(fmt.Append(nil, a...)))[:16] }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func write(p, s string) { check(os.WriteFile(p, []byte(s), 0o644)) }

// sparse creates a file of size bytes that uses no disk space.
func sparse(p string, size int64) {
	f, err := os.Create(p)
	check(err)
	check(f.Truncate(size))
	f.Close()
}
