//go:build linux

// Command guest is timeskip's guest agent, /timeskip-guest in the VM, run by
// /init once dockerd is up. It reports BASE (for the base checkpoint), and on
// SETUP starts the bundle's Compose project, waits for the project's
// setup_complete message, reports READY with the project's test
// templates, and on GO runs a timeline: the host's edits applied, then test
// commands scheduled as Antithesis's Test Composer schedules them (see
// README.md for the protocol).
package main

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ryanworl/timeskip/internal/protocol"
)

const (
	project  = "antithesis"           // (containers are antithesis-SERVICE-N, as in Antithesis)
	bundled  = protocol.BundleDir     // the bundle's files, on the data disk
	sdkDir   = "/run/timeskip-output" // each service's ANTITHESIS_OUTPUT_DIR, in a directory of its own
	sdkInCtr = "/timeskip-output"     // ... as its containers see it
	voidstar = "/usr/local/lib/timeskip/libvoidstar.so"
	override = "/run/timeskip-override.json"
	skipParm = "/sys/module/kernel/parameters/timeskip"
	setupMax = 600 * time.Second // of guest time
)

var nonce string

// console is the line-oriented channel to the host.
var (
	consoleMu sync.Mutex
	in        *bufio.Reader
)

func say(format string, a ...any) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Printf("[guest %.2fs] "+format+"\n", append([]any{uptime().Seconds()}, a...)...)
}

// msg writes a control line.
func msg(verb string, v any) {
	b, _ := json.Marshal(v)
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Print(protocol.Line(nonce, verb, string(b)))
}

// await reads console lines until one of the host's verbs, and returns it
// and its body.
func await(verbs ...string) (verb, body string) {
	for {
		l, err := in.ReadString('\n')
		if err != nil {
			fail("reading the console: %v", err)
		}
		if verb, body, ok := protocol.Parse(l, nonce); ok && slices.Contains(verbs, verb) {
			return verb, body
		}
	}
}

// fail ends a VM whose setup (or a timeline's) can't go on.
func fail(format string, a ...any) {
	err := fmt.Sprintf(format, a...)
	say("failed: %s", err)
	compose("ps", "-a")
	compose("logs", "--tail", "20")
	if err := dumpLogs(); err != nil {
		say("writing the containers' logs: %v", err)
	}
	msg("EXIT", protocol.Exit{Kind: "setup", Error: err})
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

// uptime is the guest's CLOCK_BOOTTIME (/proc/uptime has only centiseconds).
func uptime() time.Duration {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 7, uintptr(unsafe.Pointer(&ts)), 0) // CLOCK_BOOTTIME
	return time.Duration(ts.Nano())
}

func cmdline() map[string]string {
	b, _ := os.ReadFile("/proc/cmdline")
	return protocol.Fields(string(b))
}

func skips() int {
	b, _ := os.ReadFile("/sys/module/kernel/parameters/timeskip_count")
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func setSkip(on bool) {
	v := "N"
	if on {
		v = "Y"
	}
	check(os.WriteFile(skipParm, []byte(v), 0o644))
}

func main() {
	if filepath.Base(os.Args[0]) == "stop-faults" {
		stopFaults()
		return
	}
	cl := cmdline()
	nonce = cl["ts.nonce"]
	con, err := os.Open("/dev/" + cl["console"])
	check(err)
	exec.Command("stty", "-F", con.Name(), "-echo").Run()
	in = bufio.NewReaderSize(con, 1<<16)
	os.Setenv("COMPOSE_PROJECT_NAME", project)
	os.Setenv("COMPOSE_ANSI", "never")
	os.Setenv("COMPOSE_STATUS_STDOUT", "1")
	os.Setenv("COMPOSE_FILE", bundled+"/compose.yaml:"+override)

	base() // (the only boot: the base checkpoint's)
	// Restored from a checkpoint: set up a bundle, or run a timeline.
	for {
		verb, body := await("SETUP", "GO")
		if verb == "GO" {
			timeline(protocol.ParseGo(body))
			return
		}
		setup(protocol.Fields(body)["bundle"])
	}
}

// base reports BASE, once dockerd is up and the VM idle, for the host to
// save the VM as the base checkpoint, which every bundle's setup starts
// from.
func base() {
	for t := uptime(); call("GET", "/_ping", nil, nil) != nil; time.Sleep(time.Millisecond) {
		if uptime()-t > 60*time.Second {
			log, _ := exec.Command("tail", "-5", "/run/dockerd.log").Output()
			fail("dockerd didn't start:\n%s", log)
		}
	}
	idle()
	check(os.MkdirAll(sdkDir, 0o777))
	check(os.Chmod(sdkDir, 0o777))
	check(os.MkdirAll(faultsDir, 0o755))
	check(os.WriteFile(faultsDir+"/stop-faults", nil, 0o755))
	check(syscall.Mount("/timeskip-guest", faultsDir+"/stop-faults", "", syscall.MS_BIND, ""))
	freeze(false)
	msg("BASE", struct{}{})
}

// setup, given SETUP in a VM restored from the base checkpoint or another
// bundle's (an images checkpoint, whose images it keeps), loads the bundle,
// starts the project, waits for its setup_complete and an idle moment, and
// reports READY, for the host to save the VM. bundle is SETUP's bundle=.
func setup(bundle string) {
	setSkip(true)
	logsT0 = uptime()
	// (Zeroing freed pages, as from boot, so that the save holds only live
	// memory; freeze stops it again.)
	os.WriteFile("/sys/module/kernel/parameters/timeskip_zero_free", []byte("Y"), 0o644)
	say("loading the bundle")
	check(os.RemoveAll(bundled))
	check(os.MkdirAll(bundled, 0o755))
	check(untarFile(bundleFile(bundle), bundled))
	if bundle == "virtiofs" {
		syscall.Unmount("/inputs", 0)
	}
	system, err := writeOverride()
	check(err)
	watchExits()
	check(os.WriteFile("/proc/sys/kernel/print-fatal-signals", []byte("1"), 0o644)) // (see crashes)
	if system {
		say("compose up")
		start := uptime()
		ctx, cancel := context.WithTimeoutCause(context.Background(), setupMax, fmt.Errorf("not done after %v", setupMax))
		// (Created first, so that each node gets its disk before it starts:
		// see makeDisks.)
		check(composeCtx(ctx, "create", "--quiet-pull"))
		check(makeDisks())
		check(composeCtx(ctx, "up", "-d", "--quiet-pull"))
		cancel()
		// Poll every 50 ms, every second after 10 s: polls are wake-ups.
		for !setupComplete() {
			t := uptime() - start
			if t > setupMax {
				fail("no setup_complete after %v: a container must write {\"antithesis_setup\": {\"status\": \"complete\", \"details\": null}} to $ANTITHESIS_OUTPUT_DIR/sdk.jsonl once the system is ready", setupMax)
			}
			checkExits()
			time.Sleep(map[bool]time.Duration{true: 50 * time.Millisecond, false: time.Second}[t < 10*time.Second])
		}
		say("setup complete")
	}
	idle()
	checkExits()
	sumTargets()
	cat, _, err := discover()
	check(err)
	freeze(true)
	msg("READY", protocol.Ready{Catalog: cat.wire()})
}

// bundleFile is where SETUP's bundle is: on a virtiofs share (macOS), or
// the first drive, swapped for it (Firecracker).
func bundleFile(bundle string) string {
	if bundle == "virtiofs" {
		check(syscall.Mount("inputs", "/inputs", "virtiofs", syscall.MS_RDONLY, ""))
		return "/inputs/bundle.tsb"
	}
	waitDrive("vda", bundle)
	return "/dev/vda"
}

// waitDrive waits until drive dev shows the file the host swapped in, given
// as blk:SIZE: its new size shows once the guest has seen it.
func waitDrive(dev, spec string) {
	want, _ := strconv.ParseInt(strings.TrimPrefix(spec, "blk:"), 10, 64)
	for t := uptime(); ; time.Sleep(time.Millisecond) {
		b, _ := os.ReadFile("/sys/block/" + dev + "/size")
		if n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); n*512 >= want {
			return
		}
		if uptime()-t > 10*time.Second {
			fail("the drive %s didn't change", dev)
		}
	}
}

// idle waits for a moment when every thread in the VM sleeps (the clock
// skips), so that a checkpoint doesn't catch work in progress, which every
// timeline would then redo.
func idle() {
	n, t := skips(), uptime()
	for skips() == n && uptime()-t < 10*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
}

// freeze readies the VM to be saved: no skipping, disks synced, and the
// page cache dropped. During setup, guest/init has the kernel zero pages as
// they're freed, so free memory is zero or was never touched and a whole
// checkpoint saves only live memory; after setup the zeroing stops.
func freeze(setup bool) {
	setSkip(false)
	syscall.Sync()
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644)
	// What every timeline runs from the root disk is read back in whole: the
	// agent and busybox (nsenter). Reading it from the disk in a timeline
	// would take time that changes how the timeline's processes interleave.
	for _, f := range []string{"/timeskip-guest", "/bin/busybox"} {
		if b, err := os.Open(f); err == nil {
			io.Copy(io.Discard, b)
			b.Close()
		}
	}
	if setup {
		os.WriteFile("/sys/module/kernel/parameters/timeskip_zero_free", []byte("N"), 0o644)
	}
}

// writeOverride gives every service what Antithesis gives its containers:
// an output directory of its own, ANTITHESIS_OUTPUT_DIR naming it, and
// /usr/lib/libvoidstar.so (guest/libvoidstar.c), through which the SDKs
// write their output to its sdk.jsonl; ANTITHESIS_SDK_LOCAL_OUTPUT naming
// that file too, for the SDKs that can't load the library (static
// binaries, Java); ANTITHESIS_STOP_FAULTS (see faults.go); and the
// directory where libvoidstar keeps coverage (see coverage.go). It reports
// whether the project has services that docker compose up starts: services
// in no profile. (An images checkpoint's project has none.)
func writeOverride() (system bool, err error) {
	b, err := os.ReadFile(bundled + "/compose.yaml")
	if err != nil {
		return false, err
	}
	var p struct {
		Services map[string]struct {
			Image    string
			Profiles []string
		}
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return false, err
	}
	if err := errors.Join(os.MkdirAll(covDir, 0o777), os.Chmod(covDir, 0o777)); err != nil {
		return false, err
	}
	svc := map[string]any{}
	for name, s := range p.Services {
		system = system || len(s.Profiles) == 0
		// (Every container appends to sdk.jsonl, as whatever user it runs
		// as; the SDKs create it 0644 and drop what they can't write.)
		dir := filepath.Join(sdkDir, name)
		if err := errors.Join(os.MkdirAll(dir, 0o777), os.Chmod(dir, 0o777), os.WriteFile(dir+"/sdk.jsonl", nil, 0o666), os.Chmod(dir+"/sdk.jsonl", 0o666)); err != nil {
			return false, err
		}
		vs := []any{
			map[string]any{"type": "bind", "source": dir, "target": sdkInCtr},
			map[string]any{"type": "bind", "source": faultsDir, "target": faultsInCtr, "read_only": true},
			map[string]any{"type": "bind", "source": covDir, "target": covInCtr},
		}
		// (Not into an emulated image's containers, whose processes can't
		// load it: their SDKs write to ANTITHESIS_SDK_LOCAL_OUTPUT.)
		var img struct{ Architecture string }
		if call("GET", "/images/"+s.Image+"/json", nil, &img) != nil || img.Architecture == runtime.GOARCH {
			vs = append(vs, map[string]any{"type": "bind", "source": voidstar, "target": "/usr/lib/libvoidstar.so", "read_only": true})
		}
		svc[name] = map[string]any{
			"volumes": vs,
			"environment": map[string]string{"ANTITHESIS_OUTPUT_DIR": sdkInCtr, "ANTITHESIS_SDK_LOCAL_OUTPUT": sdkInCtr + "/sdk.jsonl",
				"ANTITHESIS_STOP_FAULTS": faultsInCtr + "/stop-faults"},
		}
	}
	o, _ := json.Marshal(map[string]any{"services": svc})
	return system, os.WriteFile(override, o, 0o644)
}

// setupComplete reports whether a container has written the setup_complete
// message.
func setupComplete() bool {
	for _, l := range sdkLines() {
		var m struct {
			Setup *struct{ Status string } `json:"antithesis_setup"`
		}
		if json.Unmarshal(l, &m) == nil && m.Setup != nil && m.Setup.Status == "complete" {
			return true
		}
	}
	return false
}

// sdkLines are the services' sdk.jsonl lines.
func sdkLines() [][]byte {
	files, _ := filepath.Glob(sdkDir + "/*/sdk.jsonl")
	var l [][]byte
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, x := range strings.Split(string(b), "\n") {
			if x = strings.TrimSpace(x); x != "" {
				l = append(l, []byte(x))
			}
		}
	}
	return l
}

// checkExits fails setup if a container exited unexpectedly (see exits):
// Antithesis's setup guide says none may exit with an error before setup
// completes.
func checkExits() {
	if l := exitsSince(0); len(l) > 0 {
		fail("%s during setup", strings.Join(l, "; "))
	}
}

// started waits until every container is past starting: created ones
// running, and ones with a healthcheck past its start period.
func started() error {
	for t := uptime(); ; {
		cs, err := containers("")
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(cs, func(c container) bool {
			return c.State.Status == "created" || c.State.Health != nil && c.State.Health.Status == "starting"
		}) {
			return nil
		}
		if uptime()-t > setupMax {
			return fmt.Errorf("services not up after %v", setupMax)
		}
		time.Sleep(map[bool]time.Duration{true: 50 * time.Millisecond, false: time.Second}[uptime()-t < 10*time.Second])
	}
}

// extract writes a tar entry, a directory, file or symlink, to path.
func extract(h *tar.Header, r io.Reader, path string) error {
	// (Modes as they are, past the umask.)
	mode := h.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	switch h.Typeflag {
	case tar.TypeDir:
		return errors.Join(os.MkdirAll(path, 0o755), os.Chmod(path, mode))
	case tar.TypeSymlink:
		return os.Symlink(h.Linkname, path)
	case tar.TypeReg:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		return errors.Join(err, f.Close(), os.Chmod(path, mode))
	}
	return nil
}

// untarFile extracts what the guest uses of the bundle (a tar archive, at
// the start of a disk): its compose file and what the project bind-mounts,
// loading its images archives into Docker as it reads them.
func untarFile(path, dir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("reading the bundle: %v", err)
		}
		switch {
		case strings.HasPrefix(h.Name, "images") && strings.HasSuffix(h.Name, ".tar") && !strings.Contains(h.Name, "/"):
			if err := load(r); err != nil {
				return fmt.Errorf("loading the images: %v", err)
			}
		case h.Name == "compose.yaml" || strings.HasPrefix(h.Name, "mounted/"):
			if err := extract(h, r, filepath.Join(dir, h.Name)); err != nil {
				return err
			}
		}
	}
}
