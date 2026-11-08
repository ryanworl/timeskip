//go:build e2e

// Package e2e tests a timeskip kit end to end. ./build.sh compiles it to
// out/e2e.test for the kit's platform, and ./test.sh runs that from the
// repository root, against out/timeskip-*.tgz and the fixtures' bundles in
// out/tests. It needs a Mac, or Linux with KVM, and Docker.
package e2e

import (
	"archive/tar"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each fixture is a project for one of the protocol's cases, with its
// expected exit code in a "# expect: N" line, and arguments for timeskip
// test in a "# args: ..." line, if it needs some. A "# errors: REGEXP" line
// says the timelines fail with errors that match it (joined by "; ") and
// nothing else: every property holds. A "# fails: A; B" line names exactly
// the properties that fail (sorted, joined by "; ").
//
//go:embed testdata/protocol/*.yaml
var fixtures embed.FS

//go:embed testdata/sdk
var sdkProject embed.FS

var ts, work string // the kit's timeskip, and where the tests keep their files (out/test)

func TestMain(m *testing.M) {
	kits, _ := filepath.Glob("out/timeskip-*.tgz")
	if len(kits) != 1 {
		fmt.Fprintln(os.Stderr, "e2e: expected one kit in out/; run ./build.sh")
		os.Exit(2)
	}
	work, _ = filepath.Abs("out/test")
	os.RemoveAll(work)
	os.MkdirAll(work, 0o755)
	if out, err := exec.Command("tar", "-xzf", kits[0], "-C", work).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: extracting %s: %v\n%s", kits[0], err, out)
		os.Exit(2)
	}
	ts = filepath.Join(work, "timeskip", "timeskip")
	os.Setenv("TS_CACHE", filepath.Join(work, "cache"))
	rc := m.Run()
	// The images the projects built (see example), named for this
	// checkout's directories: another checkout's suite would leave more.
	if out, err := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}").Output(); err == nil {
		var built []string
		for _, img := range strings.Fields(string(out)) {
			if repos[strings.Split(img, "/")[0]] {
				built = append(built, img)
			}
		}
		if len(built) > 0 {
			exec.Command("docker", append([]string{"image", "rm", "-f"}, built...)...).Run()
		}
	}
	os.Exit(rc)
}

// result is a run's results.json.
type result struct {
	Pass     bool
	Coverage []struct {
		Module              string
		Edges, Reached, New int
		Progress            []int
	}
	Timelines  []timeline
	Properties []property
}

type property struct {
	Name string
	Pass bool
	Hits int
}

type assertion struct {
	Message      string
	Hits, True   int
	FirstFalseMs int64 `json:"first_false_ms"`
}

type timeline struct {
	Template, First, From string
	Seed                  uint64
	Branch                *struct {
		Seed       uint64
		AtMs       int64 `json:"at_ms"`
		Checkpoint string
	}
	Pass          bool
	Errors        []string
	ContainerLogs string  `json:"container_logs"`
	GuestMs       float64 `json:"guest_ms"`
	Skips         int
	Wall          float64 `json:"wall_s"`
	Commands      []struct {
		Name  string
		CPUMs int64 `json:"cpu_ms"`
	}
	ContainerCPUMs map[string]int64 `json:"container_cpu_ms"`
	Assertions     []assertion
	Faults         []struct {
		Kind, Detail string
		Targets      [][]string
		AtMs         int64 `json:"at_ms"`
		Ms           int64
	}
	FaultMix   string `json:"fault_mix"`
	FaultGapMs int64  `json:"fault_gap_ms"`
}

// commands are the names of the commands a timeline ran, sorted.
func (r result) commands(i int) []string {
	var l []string
	for _, c := range r.Timelines[i].Commands {
		l = append(l, c.Name)
	}
	slices.Sort(l)
	return l
}

// timeskip runs the CLI, keeping its output in work/NAME.log (shown if the
// test fails), and returns its exit code and output. A test run gets
// --set faults=none unless it sets faults, or runs a protocol fixture whose
// x-timeskip does: tests make faults only where they check them.
func timeskip(t *testing.T, name string, args ...string) (int, string) {
	t.Helper()
	if len(args) > 0 && args[0] == "test" && !slices.ContainsFunc(args, func(a string) bool {
		src, _ := fixtures.ReadFile(path.Join("testdata/protocol", strings.TrimSuffix(filepath.Base(a), ".tsb")+".yaml"))
		return strings.HasPrefix(a, "faults=") || strings.Contains(string(src), "\n  faults: ")
	}) {
		args = append(args, "--set", "faults=none")
	}
	log := filepath.Join(work, name+".log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ts, args...)
	cmd.Stdout, cmd.Stderr = f, f
	err = cmd.Run()
	f.Close()
	if cmd.ProcessState == nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	t.Cleanup(func() {
		if t.Failed() {
			l := strings.Split(strings.TrimSpace(string(b)), "\n")
			t.Logf("last lines of %s:\n%s", log, strings.Join(l[max(0, len(l)-25):], "\n"))
		}
	})
	return cmd.ProcessState.ExitCode(), string(b)
}

// run runs timeskip with --out work/NAME.out and returns its exit code,
// result and output.
func run(t *testing.T, name string, args ...string) (int, result, string) {
	t.Helper()
	dir := filepath.Join(work, name+".out")
	os.RemoveAll(dir)
	rc, out := timeskip(t, name, append(args, "--out", dir)...)
	var r result
	b, _ := os.ReadFile(filepath.Join(dir, "results.json"))
	json.Unmarshal(b, &r)
	return rc, r, out
}

func TestDemo(t *testing.T) {
	demo := filepath.Join(work, "pingpong.tsb")
	// (Its test commands' container gets no faults: the tests with faults
	// check what they are, not what the demo gets through.)
	f := project(t, "demo")
	edit(t, f, "    command: [setup.sh]", "    labels: {com.antithesis.exclude_from_faults: \"network,pause,stop,kill,throttle,clock,disk\"}\n    command: [setup.sh]")
	if rc, _ := timeskip(t, "demo-bundle", "bundle", f, "-o", demo); rc != 0 {
		t.Fatalf("bundle: exit %d", rc)
	}
	t.Run("a timeline", func(t *testing.T) {
		rc, r, out := run(t, "demo-1", "test", demo)
		if rc != 0 || len(r.Timelines) != 1 || !r.Pass {
			t.Fatalf("exit %d, %d timelines, pass %v", rc, len(r.Timelines), r.Pass)
		}
		tl := r.Timelines[0]
		if tl.First != "first_handshake.sh" || !strings.Contains(out, "[pingpong/parallel_driver_ping.sh] pong 1 v1") {
			t.Errorf("first %q; no ping in the output", tl.First)
		}
		if tl.GuestMs < 3000 || tl.Skips < 100 {
			t.Errorf("%.0f ms of guest time, %d skips: not skipping", tl.GuestMs, tl.Skips)
		}
		for _, p := range r.Properties {
			if !p.Pass || p.Hits == 0 {
				t.Errorf("property %q: pass %v, %d hits", p.Name, p.Pass, p.Hits)
			}
		}
	})
	t.Run("parallel timelines from the first_ checkpoint", func(t *testing.T) {
		rc, r, _ := run(t, "demo-6", "test", demo, "--timelines", "6", "-j", "6")
		if rc != 0 || len(r.Timelines) != 6 {
			t.Fatalf("exit %d, %d timelines", rc, len(r.Timelines))
		}
		seeds := map[uint64]bool{}
		for _, tl := range r.Timelines {
			seeds[tl.Seed] = true
			if tl.From != "first" || !tl.Pass {
				t.Errorf("timeline seed=%d from %s, pass %v", tl.Seed, tl.From, tl.Pass)
			}
		}
		if len(seeds) != 6 {
			t.Errorf("seeds %v: not all different", seeds)
		}
	})
	t.Run("a seed's schedule", func(t *testing.T) {
		_, a, _ := run(t, "demo-seed-a", "test", demo, "--seed", "42")
		_, b, _ := run(t, "demo-seed-b", "test", demo, "--seed", "42")
		_, c, _ := run(t, "demo-seed-c", "test", demo, "--seed", "43")
		if len(a.Timelines)+len(b.Timelines)+len(c.Timelines) != 3 {
			t.Fatal("no results")
		}
		if !slices.Equal(a.commands(0), b.commands(0)) {
			t.Errorf("seed 42 ran %v, then %v", a.commands(0), b.commands(0))
		}
		if slices.Equal(a.commands(0), c.commands(0)) {
			t.Errorf("seed 43 ran what seed 42 did: %v", c.commands(0))
		}
		// The same faults, too, from a seed that draws node faults. (Sparse
		// ones, sparing the test commands: the demo doesn't get through
		// dense ones, nor the commands through faults of their own, so how
		// many it gets before it ends varies.)
		_, d, _ := run(t, "demo-seed-faults-a", "test", demo, "--seed", "43", "--set", "faults=high_availability", "--set", "fault_gap=100")
		_, e, _ := run(t, "demo-seed-faults-b", "test", demo, "--seed", "43", "--set", "faults=high_availability", "--set", "fault_gap=100")
		if len(d.Timelines)+len(e.Timelines) != 2 || len(d.Timelines[0].Faults) < 2 {
			t.Fatal("no faults")
		}
		plan := func(r result) (l []string) {
			for _, f := range r.Timelines[0].Faults {
				l = append(l, fmt.Sprint(f.Kind, f.Detail, f.Targets))
			}
			return l
		}
		if x, y := plan(d), plan(e); !slices.Equal(x, y) {
			t.Errorf("seed 43 made faults %s, then %s", x, y)
		}
		// With fault_gap 1, the timeline draws a gap of a third of a
		// second to 3 s (a third of that without node faults), and each
		// network or node fault starts within it of the last one's end.
		// (The demo may not get through such faults, hence the timeout.)
		_, g, _ := run(t, "demo-seed-faults-gap", "test", demo, "--seed", "42", "--set", "faults=high_availability", "--set", "fault_gap=1", "--set", "timeout=30")
		if len(g.Timelines) != 1 {
			t.Fatal("no results")
		}
		tl, lo, hi := g.Timelines[0], int64(333), int64(3000)
		if tl.FaultMix == "no node faults" {
			lo, hi = 111, 1000
		}
		if tl.FaultGapMs < lo || tl.FaultGapMs > hi {
			t.Errorf("fault_gap 1 (%s): a gap of %d ms, want %d to %d", tl.FaultMix, tl.FaultGapMs, lo, hi)
		}
		n, end := map[bool]int{}, map[bool]int64{true: -1, false: -1}
		for _, f := range tl.Faults {
			node := f.Kind == "pause" || f.Kind == "stop" || f.Kind == "kill" || f.Kind == "throttle"
			if node || f.Kind == "partition" || f.Kind == "clog" {
				if n[node]++; end[node] >= 0 && f.AtMs-end[node] > tl.FaultGapMs+500 {
					t.Errorf("a %s %d ms after the last one ended, with a gap of %d ms", f.Kind, f.AtMs-end[node], tl.FaultGapMs)
				}
				end[node] = f.AtMs + f.Ms
			}
		}
		if n[false] < 2 || (n[true] < 2) != (tl.FaultMix == "no node faults") {
			t.Errorf("%d network and %d node faults with fault_gap 1 (%s)", n[false], n[true], tl.FaultMix)
		}
		// Antithesis's fault tolerance levels: legacy has no node faults
		// nor clock skips, resilient only pauses, throttles, CPU
		// modulation, slowed or jammed clogs and clock skips.
		for level, ok := range map[string]*regexp.Regexp{"legacy": regexp.MustCompile(`^(latency|partition|clog)`), "resilient": regexp.MustCompile(`^(latency|clog (slowed|jammed)|pause|throttle|modulation|skip)`)} {
			_, r, _ := run(t, "demo-faults"+level, "test", demo, "--seed", "42", "--set", "faults="+level, "--set", "fault_gap=1", "--set", "timeout=30")
			if len(r.Timelines) != 1 || len(r.Timelines[0].Faults) < 3 {
				t.Fatalf("faults %s: no faults", level)
			}
			for _, f := range r.Timelines[0].Faults {
				if !ok.MatchString(f.Kind + " " + f.Detail) {
					t.Errorf("faults %s made a %s fault (%s)", level, f.Kind, f.Detail)
				}
			}
		}
	})
	t.Run("no skipping", func(t *testing.T) {
		rc, r, _ := run(t, "demo-noskip", "test", demo, "--no-skip", "--set", "drivers=2")
		if rc != 0 || len(r.Timelines) != 1 {
			t.Fatalf("exit %d", rc)
		}
		if tl := r.Timelines[0]; tl.Wall*1000 < tl.GuestMs*0.8 {
			t.Errorf("%.0f ms of guest time in %.2f s without skipping", tl.GuestMs, tl.Wall)
		}
	})
}

// project copies the demo's source to a new directory and returns its
// compose file. Its images are named for the directory (see repo): image
// names are the Docker host's, which other projects' builds would retag.
func project(t *testing.T, name string) string { t.Helper(); return example(t, "pingpong", name) }

// example copies the kit's examples/EX to work/NAME, with images of its own,
// and returns its compose file.
func example(t *testing.T, ex, name string) string {
	t.Helper()
	dir := filepath.Join(work, name)
	os.RemoveAll(dir)
	src := filepath.Join(work, "timeskip", "examples", ex)
	if out, err := exec.Command("cp", "-R", src, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	f := filepath.Join(dir, "compose.yaml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, []byte(strings.ReplaceAll(string(b), "image: timeskip/", "image: "+repo(dir)+"/")), 0o644)
	return f
}

var repos = map[string]bool{} // (removed at the end: see TestMain)

// repo is where project's copy in dir names its images.
func repo(dir string) string {
	r := fmt.Sprintf("timeskip-e2e-%x", sha256.Sum256([]byte(dir)))[:25]
	repos[r] = true
	return r
}

func edit(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), old) {
		t.Fatalf("%s: no %q (%v)", path, old, err)
	}
	fi, _ := os.Stat(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), fi.Mode())
}

func TestBundles(t *testing.T) {
	arch := runtime.GOARCH
	t.Run("a new version of a bundle replaces its checkpoint", func(t *testing.T) {
		f, b := project(t, "versions"), filepath.Join(work, "versions.tsb")
		for i, v := range []string{"v1", "v2"} {
			if i > 0 {
				edit(t, filepath.Join(filepath.Dir(f), "pong", "pong.sh"), "v1", v)
			}
			if rc, _ := timeskip(t, "versions-bundle-"+v, "bundle", f, "--arch", arch, "-o", b); rc != 0 {
				t.Fatalf("bundle %s: exit %d", v, rc)
			}
			if rc, out := timeskip(t, "versions-"+v, "test", b); rc != 0 || !strings.Contains(out, "pong 1 "+v) {
				t.Fatalf("%s: exit %d", v, rc)
			}
		}
		n := 0
		ms, _ := filepath.Glob(filepath.Join(work, "cache", "checkpoints", "*", "meta.json"))
		for _, m := range ms {
			if raw, _ := os.ReadFile(m); strings.Contains(string(raw), `"bundle":"`+b+`"`) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d checkpoints for %s, want 1", n, b)
		}
	})
	t.Run("a bundle carries its synced files and is reproducible", func(t *testing.T) {
		// A sync rule for a file the image doesn't have: like a binary built
		// on the host.
		f := project(t, "export")
		dir := filepath.Dir(f)
		os.MkdirAll(filepath.Join(dir, "host"), 0o755)
		os.WriteFile(filepath.Join(dir, "host", "singleton_driver_x.sh"), []byte("#!/bin/sh\necho synced driver\n"), 0o755)
		edit(t, f, "        - {action: sync, path: ./workload/test,", "        - {action: sync, path: ./host, target: /opt/antithesis/test/v1/extra}\n        - {action: sync, path: ./workload/test,")
		var sums []string
		for _, name := range []string{"export-a", "export-b"} {
			b := filepath.Join(work, name+".tsb")
			if rc, _ := timeskip(t, name+"-bundle", "bundle", f, "-o", b); rc != 0 {
				t.Fatalf("bundle: exit %d", rc)
			}
			raw, _ := os.ReadFile(b)
			sums = append(sums, fmt.Sprintf("%x", sha256.Sum256(raw)))
		}
		if sums[0] != sums[1] {
			t.Errorf("two bundles of the same project differ: %v", sums)
		}
		if rc, out := timeskip(t, "export", "test", "--template", "extra", filepath.Join(work, "export-a.tsb")); rc != 0 || !strings.Contains(out, "synced driver") {
			t.Errorf("exit %d; want 0 and the synced command's output", rc)
		}
	})
	t.Run("a bundle for another architecture", func(t *testing.T) {
		other := map[string]string{"arm64": "amd64", "amd64": "arm64"}[arch]
		b := filepath.Join(work, "other.tsb")
		if rc, _ := timeskip(t, "other-bundle", "bundle", project(t, "other"), "--arch", other, "-o", b); rc != 0 {
			t.Fatalf("bundle: exit %d", rc)
		}
		if rc, out := timeskip(t, "other", "test", b); rc != 2 || !strings.Contains(out, "--arch "+arch) {
			t.Errorf("exit %d, want 2 and a hint", rc)
		}
	})
	t.Run("a bundle of another format is refused", func(t *testing.T) {
		in, err := os.Open(filepath.Join("out", "tests", "ok.tsb"))
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		b := filepath.Join(work, "format.tsb")
		out, _ := os.Create(b)
		r, w := tar.NewReader(in), tar.NewWriter(out)
		for h, err := r.Next(); err == nil; h, err = r.Next() {
			body, _ := io.ReadAll(r)
			if h.Name == "format" {
				body = []byte("0\n")
				h.Size = int64(len(body))
			}
			w.WriteHeader(h)
			w.Write(body)
		}
		w.Close()
		out.Close()
		if rc, out := timeskip(t, "format", "test", b); rc != 2 || !strings.Contains(out, "bundle the project again") {
			t.Errorf("exit %d, want 2 and a hint", rc)
		}
	})
	t.Run("test -f follows edits as compose watch does", func(t *testing.T) {
		f := project(t, "edits")
		dir := filepath.Dir(f)
		// (And a service whose image the project doesn't build.)
		b, _ := os.ReadFile(f)
		os.WriteFile(f, append(b, "\n  sidecar:\n    image: busybox:1.36\n    command: [sleep, \"2147483647\"]\n"...), 0o644)
		step := func(name string, want ...string) result {
			t.Helper()
			rc, r, out := run(t, "edits-"+name, "test", f)
			if rc != 0 {
				t.Fatalf("%s: exit %d", name, rc)
			}
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("%s: no %q in the output", name, w)
				}
			}
			return r
		}
		r := step("first", "pong 1 v1")
		if !slices.Contains(r.commands(0), "finally_tally.sh") {
			t.Errorf("no finally_ command: %v", r.commands(0))
		}
		// A sync+restart rule: pong restarts with the new pong.sh.
		pong := filepath.Join(dir, "pong", "pong.sh")
		edit(t, pong, "v1", "v2")
		step("sync+restart", "pong 1 v2", "restarted pong")
		// A file that can't stand in for the one it replaces fails the
		// run before anything restarts: one no longer executable, or a
		// binary for another architecture.
		v2, _ := os.ReadFile(pong)
		os.Chmod(pong, 0o644)
		if rc, _ := timeskip(t, "edits-noexec", "test", f); rc != 125 {
			t.Errorf("not executable: exit %d, want 125", rc)
		}
		other := map[string][]byte{"amd64": {0xb7, 0}, "arm64": {0x3e, 0}}[runtime.GOARCH]
		os.WriteFile(pong, append(append([]byte("\x7fELF"), make([]byte, 14)...), other...), 0o755)
		os.Chmod(pong, 0o755)
		if rc, _ := timeskip(t, "edits-elf", "test", f); rc != 125 {
			t.Errorf("another architecture's binary: exit %d, want 125", rc)
		}
		os.WriteFile(pong, v2, 0o755)
		// A sync rule: a test command changes; one is deleted.
		edit(t, filepath.Join(dir, "workload", "test", "parallel_driver_ping.sh"), "for n in", `echo "edited driver"; for n in`)
		os.Remove(filepath.Join(dir, "workload", "test", "finally_tally.sh"))
		r = step("sync", "edited driver")
		if slices.Contains(r.commands(0), "finally_tally.sh") || r.Timelines[0].From != "first" {
			t.Errorf("ran %v from %s; want no finally_ command, from the first_ checkpoint", r.commands(0), r.Timelines[0].From)
		}
		// An edit to the first_ command makes a new first_ checkpoint.
		edit(t, filepath.Join(dir, "workload", "test", "first_handshake.sh"), "r=$(", `echo "edited first"; r=$(`)
		if r = step("first_", "edited first"); r.Timelines[0].From != "setup" {
			t.Errorf("from %s, want setup", r.Timelines[0].From)
		}
		if r = step("first_ again"); r.Timelines[0].From != "first" {
			t.Errorf("from %s, want first", r.Timelines[0].From)
		}
		// ... which took the place of the one before.
		ms, _ := filepath.Glob(filepath.Join(work, "cache", "checkpoints", "*", "meta.json"))
		for _, m := range ms {
			if raw, _ := os.ReadFile(m); strings.Contains(string(raw), fmt.Sprintf("%x", sha256.Sum256([]byte(f)))[:16]+".tsb") {
				if firsts, _ := filepath.Glob(filepath.Join(filepath.Dir(m), "first-*", "done")); len(firsts) != 1 {
					t.Errorf("%d first_ checkpoints, want 1: %v", len(firsts), firsts)
				}
			}
		}
		// A new image: the project is set up again with it, from the
		// checkpoint with the images it doesn't build; then not again.
		edit(t, filepath.Join(dir, "pong", "Dockerfile"), "COPY pong.sh", "RUN echo v3 > /version\nCOPY pong.sh")
		if rc, _, out := run(t, "edits-image", "test", f); rc != 0 || strings.Count(out, "timeskip: setting up") != 1 {
			t.Errorf("exit %d, set up %d times; want 0, once", rc, strings.Count(out, "timeskip: setting up"))
		}
		if _, _, out := run(t, "edits-same", "test", f); strings.Contains(out, "setting up") {
			t.Error("set up again with the same images")
		}
		// A setting changes neither the images nor the setup.
		c, _ := os.ReadFile(f)
		os.WriteFile(f, append([]byte("x-timeskip:\n  drivers: 3\n"), c...), 0o644)
		if rc, _, out := run(t, "edits-setting", "test", f); rc != 0 || strings.Contains(out, "setting up") || strings.Contains(out, "saving") {
			t.Errorf("exit %d, or set up or saved images again for a setting", rc)
		}
	})
	t.Run("a template that edits add", func(t *testing.T) {
		f := project(t, "added")
		extra := filepath.Join(filepath.Dir(f), "workload", "extra")
		edit(t, f, "        - {action: sync, path: ./workload/test,", "        - {action: sync, path: ./workload/extra, target: /opt/antithesis/test/v1/extra}\n        - {action: sync, path: ./workload/test,")
		os.Mkdir(extra, 0o755)
		os.WriteFile(filepath.Join(extra, "helper_lib.sh"), nil, 0o644)
		// With only a helper_ file, it isn't a template.
		if rc, out := timeskip(t, "added-helper", "test", f, "--template", "extra"); rc != 125 || !strings.Contains(out, "no test template extra") {
			t.Errorf("exit %d, want 125 and no test template extra", rc)
		}
		// Its first_ command, which the host can't know of, runs first.
		os.WriteFile(filepath.Join(extra, "first_hello.sh"), []byte("#!/bin/sh\necho hello\n"), 0o755)
		os.WriteFile(filepath.Join(extra, "singleton_driver_hi.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
		rc, r, _ := run(t, "added", "test", f, "--template", "extra")
		if rc != 0 || len(r.Timelines) != 1 || r.Timelines[0].First != "first_hello.sh" || !slices.Equal(r.commands(0), []string{"first_hello.sh", "singleton_driver_hi.sh"}) {
			t.Errorf("exit %d, %+v; want 0, first_hello.sh then singleton_driver_hi.sh", rc, r.Timelines)
		}
	})
	t.Run("bind mounts of the project's files, and only those", func(t *testing.T) {
		// (pong.sh bind-mounted over the image's: an edit to it shows.)
		f := project(t, "bind")
		edit(t, f, "    init: true              # so that", "    volumes: [\"./pong/pong.sh:/usr/local/bin/pong.sh\"]\n    init: true              # so that")
		edit(t, filepath.Join(filepath.Dir(f), "pong", "pong.sh"), "v1", "mounted")
		if rc, out := timeskip(t, "bind", "test", f); rc != 0 || !strings.Contains(out, "pong 1 mounted") {
			t.Errorf("exit %d, want 0 and the mounted pong.sh's answers", rc)
		}
		edit(t, f, "./pong/pong.sh:", "/etc/hosts:")
		if rc, out := timeskip(t, "bind-outside", "test", f); rc != 2 || !strings.Contains(out, "outside the project's directory") {
			t.Errorf("exit %d, want 2 and the reason", rc)
		}
	})
	t.Run("Antithesis's Python SDK", func(t *testing.T) {
		dir := filepath.Join(work, "sdk")
		os.RemoveAll(dir)
		sub, _ := fs.Sub(sdkProject, "testdata/sdk")
		if err := os.CopyFS(dir, sub); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "compose.yaml")
		cov := filepath.Join(work, "sdk.out", "coverage")
		rc, r, _ := run(t, "sdk", "test", f, "--template", "t", "--timelines", "3", "--set", "drivers=1")
		var drew []string
		for _, p := range r.Properties {
			if strings.HasPrefix(p.Name, "drew ") {
				drew = append(drew, p.Name)
			} else if !p.Pass || !slices.Contains([]string{"a non-root container asserts", "the driver drew a number", "t/parallel_driver_draw.py exits 0", "All commands were run to completion at least once"}, p.Name) {
				t.Errorf("property %q (pass %v)", p.Name, p.Pass)
			}
		}
		if rc != 0 || len(r.Properties) != 4+len(drew) || len(drew) != 3 {
			t.Fatalf("exit %d, %d properties, %d numbers drawn in 3 timelines", rc, len(r.Properties), len(drew))
		}
		// Its coverage: edge 8 in setup, then edge 1 and one of 2 to 4 in
		// each timeline; so f3 (5 and 6) never.
		if len(r.Coverage) != 1 || r.Coverage[0].Edges != 8 || r.Coverage[0].Reached < 3 || r.Coverage[0].Reached > 5 ||
			len(r.Coverage[0].Progress) != 3 || r.Coverage[0].Progress[0] < 3 || !slices.IsSorted(r.Coverage[0].Progress) {
			t.Errorf("coverage %+v", r.Coverage)
		}
		fns, _ := os.ReadFile(filepath.Join(cov, "e2e-cov.functions.tsv"))
		for _, want := range []string{"/src/a.go\tf1\t1\t2\t", "/src/a.go\tf3\t9\t2\t0\n", "/src/b.go\tf4\t1\t2\t1\n"} {
			if !strings.Contains(string(fns), want) {
				t.Errorf("e2e-cov.functions.tsv has no %q:\n%s", want, fns)
			}
		}
		// (Drawn from the timeline's seed: a replay draws the same.)
		_, rr, _ := run(t, "sdk-replay", "test", f, "--template", "t", "--seed", fmt.Sprint(r.Timelines[0].Seed), "--set", "drivers=1")
		if n := slices.IndexFunc(rr.Properties, func(p property) bool {
			return strings.HasPrefix(p.Name, "drew ")
		}); n < 0 || !slices.Contains(drew, rr.Properties[n].Name) {
			t.Errorf("a replay drew another number than %v", drew)
		}
		// (Edges the first run reached, which the cache keeps.)
		if len(rr.Coverage) != 1 || rr.Coverage[0].New != 0 {
			t.Errorf("a replay's coverage %+v, want nothing new", rr.Coverage)
		}

		// Exploring: the explore template's driver reaches an edge new to
		// the run in the first timeline, which is saved there; the second
		// and fourth branch from it, so they drew what it drew before, and
		// numbers of their own after.
		rc, r, _ = run(t, "sdk-explore", "test", f, "--template", "explore", "--explore", "--timelines", "4", "-j", "1", "--set", "drivers=1")
		drawn := func(tl timeline, when string) string {
			for _, a := range tl.Assertions {
				if strings.HasPrefix(a.Message, when+": ") {
					return a.Message
				}
			}
			return ""
		}
		seeds, befores, afters := map[uint64]timeline{}, map[string]bool{}, map[string]bool{}
		for _, tl := range r.Timelines {
			seeds[tl.Seed] = tl
			befores[drawn(tl, "before")], afters[drawn(tl, "after")] = true, true
			if !slices.ContainsFunc(tl.Assertions, func(a assertion) bool {
				return a.Message == "ABI v2 grants a lease" && a.True == 1
			}) {
				t.Errorf("timeline %d: no leases through ABI v2 (%+v)", tl.Seed, tl.Assertions)
			}
		}
		var branched []timeline
		for _, tl := range r.Timelines {
			if tl.From != "branch" {
				continue
			}
			branched = append(branched, tl)
			if p, ok := seeds[tl.Branch.Seed]; !ok || p.From != "setup" || tl.Branch.AtMs <= 0 || drawn(tl, "before") != drawn(p, "before") {
				t.Errorf("timeline %d branched from %+v, which drew %q before; it drew %q", tl.Seed, tl.Branch, drawn(p, "before"), drawn(tl, "before"))
			}
		}
		if rc != 1 || len(r.Timelines) != 4 || len(branched) != 2 || len(befores) != 2 || len(afters) != 4 || afters[""] {
			t.Fatalf("exit %d, %d timelines, %d branched; drew %d numbers before the edge and %d after; want 1, 4, 2, 2 and 4", rc, len(r.Timelines), len(branched), len(befores), len(afters))
		}
		// The assertion 2 s in is one too; but not
		// the new ones at the end: the one with the timeline's failure (a
		// timeline saves none once it failed), nor the one 0.1 s before it
		// (a failed timeline's branch points of its last second are
		// withdrawn). Each branch fails after its branch point, as
		// first_false_ms says.
		rc, r, _ = run(t, "sdk-assertions", "test", f, "--template", "explore", "--explore", "--timelines", "4", "-j", "1", "--set", "drivers=1")
		if rc != 1 || len(r.Timelines) != 4 || !slices.ContainsFunc(r.Timelines, func(tl timeline) bool { return tl.Branch != nil && tl.Branch.AtMs > 2000 }) {
			t.Errorf("exit %d, %d timelines, none branched after the assertion 2 s in; want 1, 4 and one", rc, len(r.Timelines))
		}
		for _, tl := range r.Timelines {
			i := slices.IndexFunc(tl.Assertions, func(a assertion) bool {
				return a.Message == "the timeline fails"
			})
			if i < 0 || tl.Branch != nil && (tl.Branch.AtMs > 3000 || tl.Assertions[i].FirstFalseMs <= tl.Branch.AtMs) {
				t.Errorf("timeline %d (branched from %+v) failed at %+v; want a branch from before its parent failed, and its own failure after", tl.Seed, tl.Branch, tl.Assertions)
			}
		}
		// The replan template's ending comes at a moment drawn as its
		// drivers start, after the first timeline's branch point (its first
		// tick): each timeline branched from there draws it anew.
		_, r, _ = run(t, "sdk-replan", "test", f, "--template", "replan", "--explore", "--timelines", "12", "-j", "1", "--set", "drivers=20", "--seed", "7")
		lo, hi, n := math.Inf(1), math.Inf(-1), 0
		for _, tl := range r.Timelines {
			for _, a := range tl.Assertions {
				if up, ok := strings.CutPrefix(a.Message, "ending at "); ok && tl.From == "branch" {
					x, _ := strconv.ParseFloat(up, 64)
					lo, hi, n = min(lo, x), max(hi, x), n+1
				}
			}
		}
		if n < 5 || hi-lo < 1 {
			t.Errorf("%d branched timelines' endings %.2f s apart; want 5 or more, over 1 s", n, hi-lo)
		}
		// A timeline never waits for a branch point: with 4 at once, none
		// is saved yet as they start, so all 4 start from setup.
		rc, r, _ = run(t, "sdk-nowait", "test", f, "--template", "explore", "--explore", "--timelines", "4", "-j", "4", "--set", "drivers=1")
		if n := slices.IndexFunc(r.Timelines, func(tl timeline) bool { return tl.From != "setup" }); rc != 1 || len(r.Timelines) != 4 || n >= 0 {
			t.Errorf("4 timelines at once: exit %d, %d timelines, one (%d) not from setup; want 1, 4, all from setup", rc, len(r.Timelines), n)
		}
		// (A failed timeline's branch point is kept: a replay draws the same.)
		b := branched[0]
		rc, rb, _ := run(t, "sdk-branch", "test", f, "--branch", b.Branch.Checkpoint, "--seed", fmt.Sprint(b.Seed), "--set", "drivers=1")
		if rc != 1 || len(rb.Timelines) != 1 {
			t.Fatalf("a replay from %s: exit %d, %d timelines", b.Branch.Checkpoint, rc, len(rb.Timelines))
		}
		if tl := rb.Timelines[0]; tl.From != "branch" || drawn(tl, "after") != drawn(b, "after") {
			t.Errorf("a replay from %s (from %s) drew %q after the edge, want %q", b.Branch.Checkpoint, tl.From, drawn(tl, "after"), drawn(b, "after"))
		}
	})
	// The examples as they ship pass under the default faults (as README's
	// "Try it" runs them), lockd once fixed.
	t.Run("the examples, with the default faults", func(t *testing.T) {
		if rc, r, _ := run(t, "defaults", "test", example(t, "pingpong", "defaults"), "--timelines", "4", "--set", "faults=high_availability"); rc != 0 || len(r.Timelines) != 4 {
			t.Errorf("pingpong: exit %d, %d timelines; want 0, 4", rc, len(r.Timelines))
		}
		f := example(t, "lockd", "lockd")
		os.WriteFile(filepath.Join(filepath.Dir(f), ".env"), []byte("LOCKD_FIX=1\n"), 0o644)
		if rc, r, _ := run(t, "lockd", "test", f, "--timelines", "2", "--set", "drivers=3", "--set", "faults=high_availability"); rc != 0 || len(r.Properties) < 2 {
			t.Errorf("lockd, fixed: exit %d, %d properties; want 0, the client's", rc, len(r.Properties))
		}
	})
	t.Run("timelines get their own random numbers, and their replays the same", func(t *testing.T) {
		f := project(t, "random")
		test := filepath.Join(filepath.Dir(f), "workload", "test")
		os.WriteFile(filepath.Join(test, "parallel_driver_ping.sh"), []byte(`#!/bin/sh
. "$(dirname "$0")/helper_assert.sh"
assert sometimes "uuid $(cat /proc/sys/kernel/random/uuid)" true
`), 0o755)
		os.Remove(filepath.Join(test, "anytime_reachable.sh")) // (whose processes would race it for random numbers)
		rc, r, _ := run(t, "random", "test", f, "--timelines", "4", "--set", "drivers=1")
		if rc != 0 {
			t.Fatalf("exit %d", rc)
		}
		n := 0
		for _, p := range r.Properties {
			if strings.HasPrefix(p.Name, "uuid ") {
				n++
			}
		}
		if n != 4 {
			t.Errorf("%d different uuids in 4 timelines", n)
		}
		// A replay of the timeline that ran the first_ command and saved its
		// checkpoint, from that checkpoint, draws the same numbers.
		uuid := func(r result, i int) string {
			for _, a := range r.Timelines[i].Assertions {
				if strings.HasPrefix(a.Message, "uuid ") {
					return a.Message
				}
			}
			return ""
		}
		i := max(0, slices.IndexFunc(r.Timelines, func(tl timeline) bool { return tl.From == "setup" }))
		_, rr, _ := run(t, "random-replay", "test", f, "--seed", fmt.Sprint(r.Timelines[i].Seed), "--set", "drivers=1")
		if len(rr.Timelines) != 1 || uuid(rr, 0) != uuid(r, i) {
			t.Errorf("seed %d (from %s) drew %q, then %q", r.Timelines[i].Seed, r.Timelines[i].From, uuid(r, i), uuid(rr, 0))
		}
	})
}

func TestProtocol(t *testing.T) {
	entries, _ := fs.ReadDir(fixtures, "testdata/protocol")
	expect, args, errs := regexp.MustCompile(`# expect: (\d+)`), regexp.MustCompile(`(?m)^# args: (.*)$`), regexp.MustCompile(`(?m)^# errors: (.*)$`)
	fails := regexp.MustCompile(`(?m)^# fails: (.*)$`)
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		src, _ := fixtures.ReadFile(path.Join("testdata/protocol", e.Name()))
		want, _ := strconv.Atoi(expect.FindStringSubmatch(string(src))[1])
		var extra []string
		if m := args.FindStringSubmatch(string(src)); m != nil {
			extra = strings.Fields(m[1])
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rc, r, out := run(t, "protocol-"+name, append([]string{"test", filepath.Join("out", "tests", name+".tsb")}, extra...)...)
			if rc != want {
				t.Errorf("exit %d, want %d", rc, want)
			}
			if m := errs.FindStringSubmatch(string(src)); m != nil {
				if len(r.Timelines) == 0 {
					t.Errorf("no timelines")
				}
				for _, tl := range r.Timelines {
					if e := strings.Join(tl.Errors, "; "); !regexp.MustCompile("^(?:" + m[1] + ")$").MatchString(e) {
						t.Errorf("timeline %d: errors %q, want %s", tl.Seed, e, m[1])
					}
				}
				for _, p := range r.Properties {
					if !p.Pass {
						t.Errorf("property %q failed", p.Name)
					}
				}
			}
			if m := fails.FindStringSubmatch(string(src)); m != nil {
				var failed []string
				for _, p := range r.Properties {
					if !p.Pass {
						failed = append(failed, p.Name)
					}
				}
				slices.Sort(failed)
				if got := strings.Join(failed, "; "); got != m[1] {
					t.Errorf("failed properties %q, want %q", got, m[1])
				}
			}
			// faultcrashrace.yaml: the timelines whose racer reached "racer
			// exits 1", and only those, failed with its crash.
			if name == "faultcrashrace" {
				for _, tl := range r.Timelines {
					crashed := false
					for _, a := range tl.Assertions {
						crashed = crashed || a.Message == "racer exits 1" && a.Hits > 0
					}
					if crashed != (len(tl.Errors) > 0) {
						t.Errorf("timeline %d: racer exited 1: %v, but errors %q", tl.Seed, crashed, tl.Errors)
					}
				}
			}
			// faultstop.yaml: a stop starts with its SIGTERM, which the
			// server ignores, so it lasts the 10 s until Docker's SIGKILL
			// and more, even when ANTITHESIS_STOP_FAULTS heals it at once
			// (else it would seem to start when the server exited, inside
			// the quiet time).
			if name == "faultstop" {
				stops := 0
				for _, tl := range r.Timelines {
					for _, f := range tl.Faults {
						if f.Kind == "stop" {
							stops++
							if f.Ms < 10000 {
								t.Errorf("timeline %d: a stop at %d ms lasted %d ms, want 10000 or more", tl.Seed, f.AtMs, f.Ms)
							}
						}
					}
				}
				if stops == 0 {
					t.Errorf("no stop faults")
				}
			}
			// containerlogs.yaml: the ticker's stderr from setup, before
			// the timeline's start; and its ticks, whose guest times less
			// the uptime they print stay put (but for the time Docker took to
			// read them) while the wall clock they print jumps.
			if name == "containerlogs" && len(r.Timelines) == 1 {
				b, _ := os.ReadFile(r.Timelines[0].ContainerLogs)
				logs := string(b)
				if !regexp.MustCompile(`(?m)^-\d+\.\d{3}s ticker-1 stderr ticker starts$`).MatchString(logs) {
					t.Errorf("no stderr line from setup in the containers' logs %q:\n%.2000s", r.Timelines[0].ContainerLogs, logs)
				}
				var lag, jump []float64
				for _, m := range regexp.MustCompile(`(?m)^\+(\d+\.\d{3})s ticker-1 stdout tick (\d+\.\d+) (\d+)$`).FindAllStringSubmatch(logs, -1) {
					at, _ := strconv.ParseFloat(m[1], 64)
					up, _ := strconv.ParseFloat(m[2], 64)
					wall, _ := strconv.ParseFloat(m[3], 64)
					lag, jump = append(lag, at-up), append(jump, wall-up)
				}
				if len(lag) < 20 {
					t.Errorf("%d ticks in the timeline's containers' logs, want 20 or more", len(lag))
				} else if slices.Max(lag)-slices.Min(lag) > 0.5 || slices.Max(jump)-slices.Min(jump) < 2 {
					t.Errorf("guest time less uptime from %.3f to %.3f s (want within 0.5 s), wall clock less uptime from %.0f to %.0f s (want a jump of 2 s or more)",
						slices.Min(lag), slices.Max(lag), slices.Min(jump), slices.Max(jump))
				}
			}
			// cpu.yaml: a command's CPU time counts its child's; a sleep
			// uses next to none; a container's counts its commands'.
			if name == "cpu" && len(r.Timelines) == 1 {
				tl, cpu := r.Timelines[0], map[string]int64{}
				for _, c := range tl.Commands {
					cpu[c.Name] = c.CPUMs
				}
				// The spin's guest second is less CPU time as the host
				// takes the vCPU away (the guest counts that as stolen), so
				// the spin is judged against the kernel's account of its
				// child (in 10 ms ticks), which counts the same way: all of
				// it (none if its child's didn't count), and not twice.
				// The sleep, and the idle container, are judged against
				// the spin.
				var child int64 = -1
				if m := regexp.MustCompile(`the child's CPU time: (\d+) ms`).FindStringSubmatch(out); m != nil {
					child, _ = strconv.ParseInt(m[1], 10, 64)
				}
				if spin, sleep := cpu["singleton_driver_spin"], cpu["finally_sleep"]; child < 300 || spin < child-10 || spin > child*7/4 || sleep > spin/5 {
					t.Errorf("commands' cpu_ms %v, want the spin's child's %d ms and less than as much again for the spin, and next to none for the sleep", cpu, child)
				}
				if w, i := tl.ContainerCPUMs["antithesis-workload-1"], tl.ContainerCPUMs["antithesis-idle-1"]; w < cpu["singleton_driver_spin"] || i > cpu["singleton_driver_spin"]/5 {
					t.Errorf("container_cpu_ms %v", tl.ContainerCPUMs)
				}
			}
			// profile.yaml: samples of dd's CPU time, symbolized, none of the
			// idle container, and idle time still skipped.
			if name == "profile" && len(r.Timelines) == 1 {
				tl := r.Timelines[0]
				b, _ := os.ReadFile(filepath.Join(work, "protocol-profile.out", "profiles", fmt.Sprintf("t-%d.folded", tl.Seed)))
				dd, read, idle := 0, false, 0
				for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					stack, n, _ := strings.Cut(l, " ")
					k, _ := strconv.Atoi(n)
					if strings.HasPrefix(stack, "antithesis-workload-1;dd;") {
						dd += k
						read = read || strings.Contains(stack, ";vfs_read_[k]")
					}
					if strings.HasPrefix(stack, "antithesis-idle-1;") {
						idle += k
					}
				}
				// (A sample each 10 ms of CPU time, so many as the command
				// used: the time the host took the vCPU away is stolen, not
				// the command's, and nothing samples it.)
				var cpu int64
				for _, c := range tl.Commands {
					cpu += c.CPUMs
				}
				if cpu < 500 || int64(dd)*10 < cpu/2 || int64(dd)*10 > cpu*3/2 || !read || idle > 0 {
					t.Errorf("profile: %d samples of dd for %d ms of CPU time (vfs_read among them: %v), %d of the idle container:\n%s", dd, cpu, read, idle, b)
				}
				if tl.GuestMs < 31000 || tl.Wall > 15 {
					t.Errorf("profiling, %.0f ms of guest time took %.1f s", tl.GuestMs, tl.Wall)
				}
			}
			// faultmix.yaml: both halves of the mixture, node faults only in
			// one; each timeline's gap drawn from a third of --fault-gap's
			// to three times it, a third of that without node faults; and
			// the network faults that far apart at most (by the median of
			// their gaps over it, as making a fault can take a while).
			if name == "faultmix" {
				halves, gaps, ratios := map[string]int{}, map[int64]bool{}, []float64{}
				for _, tl := range r.Timelines {
					halves[tl.FaultMix]++
					gaps[tl.FaultGapMs] = true
					lo, hi := int64(1000), int64(9000)
					if tl.FaultMix == "no node faults" {
						lo, hi = 333, 3000
					}
					if tl.FaultGapMs < lo || tl.FaultGapMs > hi {
						t.Errorf("timeline %d (%s): a gap of %d ms, want %d to %d", tl.Seed, tl.FaultMix, tl.FaultGapMs, lo, hi)
					}
					node, last := 0, int64(-1)
					for _, f := range tl.Faults {
						switch f.Kind {
						case "pause", "stop", "kill", "throttle":
							node++
						case "partition", "clog":
							if last >= 0 {
								ratios = append(ratios, float64(f.AtMs-last)/float64(tl.FaultGapMs))
							}
							last = f.AtMs + f.Ms
						}
					}
					if (tl.FaultMix == "no node faults") != (node == 0) {
						t.Errorf("timeline %d (%s): %d node faults", tl.Seed, tl.FaultMix, node)
					}
				}
				if halves["node faults"] == 0 || halves["no node faults"] == 0 || len(halves) != 2 || len(gaps) < len(r.Timelines) {
					t.Errorf("halves %v and gaps %v, want both halves and a gap of each timeline's own", halves, gaps)
				}
				if slices.Sort(ratios); len(ratios) == 0 || ratios[len(ratios)/2] > 0.5 {
					t.Errorf("network faults' gaps over their timeline's, sorted: %.2f; want a median under 0.5", ratios)
				}
			}
			// zones.yaml: a fault that takes a zone or region takes its
			// nodes, and a partition sets them apart from the others' only.
			if name == "zones" {
				kinds, scope := map[string]int{}, regexp.MustCompile(`(zone|region) \w+`)
				nodes := map[string][]string{"zone a": {"a1", "a2"}, "zone b": {"b1", "b2"}, "zone c": {"c1", "c2"}, "region r1": {"a1", "a2", "b1", "b2"}, "region r2": {"c1", "c2"}}
				for _, tl := range r.Timelines {
					for _, f := range tl.Faults {
						m := scope.FindStringSubmatch(f.Detail)
						if m == nil {
							continue
						}
						kinds[f.Kind]++
						kinds[m[1]]++
						var want []string
						for _, n := range nodes[m[0]] {
							want = append(want, "antithesis-"+n+"-1")
						}
						in := func(s string) bool { return slices.Contains(want, s) }
						if got := slices.Sorted(slices.Values(f.Targets[0])); !slices.Equal(got, want) || len(f.Targets) > 1 && slices.ContainsFunc(f.Targets[1], in) {
							t.Errorf("%s (%s) targets %v", f.Kind, f.Detail, f.Targets)
						}
					}
				}
				if kinds["zone"] == 0 || kinds["region"] == 0 {
					t.Errorf("zone and region faults: %v, want both", kinds)
				}
				if kinds["partition"]+kinds["clog"] == 0 || kinds["pause"]+kinds["stop"]+kinds["kill"]+kinds["throttle"] == 0 {
					t.Errorf("zone faults: %v, want network and node faults", kinds)
				}
			}
		})
	}
}

// TestCleanup checks that runs leave nothing behind (but the logs and
// branch points they keep) however they end, and that the next run removes
// what a killed one left.
func TestCleanup(t *testing.T) {
	f := example(t, "lockd", "cleanup") // (long timelines; exploring, it saves branch points)
	tmp := filepath.Join(work, "cleanup-tmp")
	os.MkdirAll(tmp, 0o755)
	t.Setenv("TMPDIR", tmp) // (where Firecracker's sockets go)
	cache, shm, t0 := filepath.Join(work, "cache"), fmt.Sprintf("/dev/shm/timeskip-%d", os.Getuid()), time.Now()
	// leftovers lists what runs left: their directories, in the cache and
	// in memory (unless a run holds it: another suite's or CI's), socket
	// directories, and links to branch points.
	leftovers := func() []string {
		l, _ := filepath.Glob(filepath.Join(cache, "run-*"))
		socks, _ := filepath.Glob(filepath.Join(tmp, "*"))
		mem, _ := filepath.Glob(filepath.Join(shm, "run-*"))
		for _, d := range mem {
			fd, err := syscall.Open(d, syscall.O_RDONLY, 0)
			if fi, _ := os.Stat(d); err == nil && fi.ModTime().After(t0) && syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				l = append(l, d)
			}
			syscall.Close(fd)
		}
		points, _ := filepath.Glob(filepath.Join(cache, "branches", "*", "*"))
		for _, p := range points {
			if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				l = append(l, p)
			}
		}
		return append(l, socks...)
	}
	// stop starts a run and signals it (or its process group, as Ctrl-C
	// does) after 3 s, and returns its exit code and output.
	stop := func(name string, sig syscall.Signal, group bool) (int, string) {
		t.Helper()
		log := filepath.Join(work, name+".log")
		out, _ := os.Create(log)
		defer out.Close()
		cmd := exec.Command(ts, "test", f, "--explore", "--timelines", "40", "-j", "4")
		cmd.Stdout, cmd.Stderr, cmd.SysProcAttr = out, out, &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		pid := cmd.Process.Pid
		if group {
			pid = -pid
		}
		syscall.Kill(pid, sig)
		cmd.Wait()
		b, _ := os.ReadFile(log)
		return cmd.ProcessState.ExitCode(), string(b)
	}
	if rc, _ := timeskip(t, "cleanup", "test", f, "--explore", "--timelines", "8", "-j", "4", "--set", "faults=high_availability"); rc != 0 && rc != 1 {
		t.Fatalf("exit %d", rc)
	}
	if l := leftovers(); len(l) > 0 {
		t.Errorf("a run left %v", l)
	}
	for _, c := range []struct {
		name  string
		sig   syscall.Signal
		group bool
	}{{"ctrl-c", syscall.SIGINT, true}, {"sigterm", syscall.SIGTERM, false}, {"sighup", syscall.SIGHUP, false}} {
		rc, out := stop("cleanup-"+c.name, c.sig, c.group)
		if rc != 130 || strings.Contains(out, "timeskip: ") {
			t.Errorf("%s: exit %d, output ending %q; want 130 and no errors", c.name, rc, out[max(0, len(out)-300):])
		}
		if l := leftovers(); len(l) > 0 {
			t.Errorf("%s: left %v", c.name, l)
		}
	}
	// A run killed with SIGKILL leaves its files, which the next run
	// removes once they're a minute old.
	if rc, _ := stop("cleanup-kill", syscall.SIGKILL, false); rc != -1 {
		t.Errorf("killed: exit %d", rc)
	}
	l := leftovers()
	if len(l) == 0 {
		t.Error("a killed run left nothing to remove")
	}
	old := time.Now().Add(-2 * time.Minute)
	for _, p := range l {
		os.Chtimes(p, old, old)
		os.Chtimes(filepath.Dir(p), old, old) // (a link's directory)
	}
	if rc, _ := timeskip(t, "cleanup-next", "test", f, "--timelines", "1"); rc != 0 && rc != 1 {
		t.Errorf("the next run: exit %d", rc)
	}
	if l := leftovers(); len(l) > 0 {
		t.Errorf("after a killed run, the next left %v", l)
	}
}
