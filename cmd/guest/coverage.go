//go:build linux

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Coverage: instrumented code (Antithesis's Go instrumentor, say) reports
// the edges it reaches to libvoidstar (guest/libvoidstar.c), which sets
// their bits in a file per module in covDir, shared by every container
// (covInCtr). The files are in the checkpoints too, so a timeline starts
// with what setup (and its first_ command) reached.
const (
	covDir   = "/run/timeskip-coverage"
	covInCtr = "/timeskip-coverage"
)

// coverage is each module's edges reached so far, by its symbol table's
// name; with symbols, the symbol tables too, gzipped, from the /symbols of
// the containers that have them.
func coverage(symbols bool) (map[string]protocol.Coverage, map[string]string) {
	mods := modules()
	if len(mods) == 0 {
		return nil, nil
	}
	cov, syms := map[string]protocol.Coverage{}, map[string]string{}
	var cs []container
	if symbols {
		cs, _ = containers("")
	}
	for name, m := range mods {
		cov[name] = protocol.Coverage{Edges: m.edges, Bits: base64.StdEncoding.EncodeToString(m.bits)}
		for _, c := range cs {
			if t, err := symbolTable(c, name); err == nil {
				syms[name] = t
				break
			}
		}
	}
	return cov, syms
}

// A module is a module's file in covDir (see guest/libvoidstar.c): its
// edges, and a bit per edge (reached or not).
type module struct {
	edges int
	bits  []byte
}

// modules are the modules' files, by their names.
func modules() map[string]module {
	files, _ := os.ReadDir(covDir)
	mods := map[string]module{}
	for _, f := range files {
		if !f.Type().IsRegular() { // (newEdges, a FIFO)
			continue
		}
		b, err := os.ReadFile(filepath.Join(covDir, f.Name()))
		if err != nil || len(b) < 8 {
			continue
		}
		edges := binary.LittleEndian.Uint64(b)
		if n := 8 + (edges/64+1)*8; uint64(len(b)) >= n {
			mods[f.Name()] = module{int(edges), b[8:n]}
		}
	}
	return mods
}

// newEdges is a FIFO in covDir that libvoidstar writes a byte to when it
// sets an edge's bit that wasn't set: for watchEdges, which keeps it open.
const newEdges = ".new"

// noting is held from EDGES until the host replies (NOTED, or BRANCH in a
// VM restored there), which it does once it saved the VM, or not: so that
// no EXIT comes meanwhile, and nothing is saved after it.
var (
	noting sync.Mutex
	noted  = make(chan struct{}, 1)
	// Watching, the host's LOGS comes to watch's reader too, which passes
	// it on.
	watching  bool
	logsAsked = make(chan struct{}, 1)
)

// watch sends EDGES, for the host to branch timelines from (see README.md,
// "Exploring"), when the timeline does something the VM hadn't since
// watching began: instrumented code reaches edges
// (once libvoidstar says so), or SDK assertions reach states (once
// containers append to sdk.jsonl). It sends them 10 ms (of guest time)
// after a wake-up, which may bring more; but none once the timeline
// failed or its ending came (see runner.over), as a branch from there
// would inherit the failure, or could do nothing new. A goroutine reads
// the host's replies.
func watch(r *runner, g protocol.Go) {
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	edges, sdk := watchEdges(poke), watchSDK(r, poke)
	watching = true
	go func() {
		for {
			verb, body := await("NOTED", "BRANCH", "LOGS")
			if verb == "LOGS" {
				logsAsked <- struct{}{}
				return
			}
			if verb == "BRANCH" {
				f := protocol.Fields(body)
				seed, _ := strconv.ParseUint(f["seed"], 10, 64)
				say("branch: seed %d", seed)
				branch(seed)
			}
			noted <- struct{}{}
		}
	}()
	go func() {
		for range wake {
			time.Sleep(10 * time.Millisecond)
			noting.Lock()
			e := protocol.Edges{AtMs: (uptime() - r.t0).Milliseconds(), Edges: edges(), Assertions: sdk()}
			if (len(e.Edges) > 0 || len(e.Assertions) > 0) && !r.over() {
				// (Skipping meanwhile, idle time would go by at once as the
				// host saves the VM, and a timeline from there would go on
				// with this one's seed until BRANCH comes.)
				setSkip(false)
				msg("EDGES", e)
				<-noted
				setSkip(g.Skip)
			}
			noting.Unlock()
		}
	}()
}

// watchEdges pokes when libvoidstar says instrumented code reached edges
// new to the VM, and returns the function that says which since it last
// did (or since watchEdges).
func watchEdges(poke func()) func() map[string][]int {
	none := func() map[string][]int { return nil }
	fifo := filepath.Join(covDir, newEdges)
	if err := syscall.Mkfifo(fifo, 0o666); err != nil && !os.IsExist(err) {
		say("exploring: %v", err)
		return none
	}
	os.Chmod(fifo, 0o666) // (past the umask, for other users' processes)
	f, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		say("exploring: %v", err)
		return none
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			poke()
			time.Sleep(10 * time.Millisecond) // (what comes meanwhile, in one read)
		}
	}()
	seen := map[string][]byte{}
	scan := func() map[string][]int {
		edges := map[string][]int{}
		for name, m := range modules() {
			b, was := m.bits, seen[name]
			for i, x := range b {
				if i < len(was) {
					x &^= was[i]
				}
				for j := range 8 {
					if x&(1<<j) != 0 {
						edges[name] = append(edges[name], i*8+j)
					}
				}
			}
			seen[name] = b
		}
		return edges
	}
	scan()
	return scan
}

// sdkScan is watchSDK's function, which finish calls once more (with
// noting held, as watch does).
var sdkScan = func() []string { return nil }

// firstFalse is the guest time (since the timeline started) each SDK
// assertion, by its key (as in assertions), was first false (an Always
// one) or reached (an Unreachable one), as watchSDK saw it.
var firstFalse = map[string]int64{}

// watchSDK pokes when containers append to their sdk.jsonl, and returns
// the function (also sdkScan) that says which states of SDK assertions the
// appends reached for the first time since it last did (or since
// watchSDK): "reached KEY", an assertion's first hit, and "true KEY", a
// Sometimes assertion's first true, KEY its ID or else its message (as in
// assertions). It notes in firstFalse, and as r's failure, assertions that
// fail.
func watchSDK(r *runner, poke func()) func() []string {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		say("exploring: %v", err)
		return sdkScan
	}
	files, _ := filepath.Glob(sdkDir + "/*/sdk.jsonl")
	for _, f := range files {
		syscall.InotifyAddWatch(fd, f, syscall.IN_MODIFY)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := syscall.Read(fd, buf); err != nil && err != syscall.EINTR {
				return
			}
			poke()
			time.Sleep(10 * time.Millisecond) // (what comes meanwhile, in one read)
		}
	}()
	read, seen := map[string]int64{}, map[string]bool{}
	scan := func() []string {
		var l []string
		ms := (uptime() - r.t0).Milliseconds()
		for _, name := range files {
			f, err := os.Open(name)
			if err != nil {
				continue
			}
			f.Seek(read[name], io.SeekStart)
			b, _ := io.ReadAll(f)
			f.Close()
			i := bytes.LastIndexByte(b, '\n') // (whole lines only)
			if i < 0 {
				continue
			}
			read[name] += int64(i + 1)
			for _, line := range bytes.Split(b[:i], []byte("\n")) {
				a, key := parseAssert(line)
				if a == nil || !a.Hit {
					continue
				}
				if _, ok := firstFalse[key]; !ok && (a.Type == "always" && !a.Condition || a.Type == "reachability" && !a.MustHit) {
					firstFalse[key] = ms
					r.failed()
				}
				states := []string{"reached " + key}
				if a.Type == "sometimes" && a.Condition {
					states = append(states, "true "+key)
				}
				for _, s := range states {
					if !seen[s] {
						seen[s] = true
						l = append(l, s)
					}
				}
			}
		}
		return l
	}
	scan()
	firstFalse = map[string]int64{} // (none before watching)
	sdkScan = scan
	return scan
}

// symbolTable is /symbols/name in container c, gzipped and in base64.
func symbolTable(c container, name string) (string, error) {
	b, err := os.ReadFile(resolve(c.root(), "/symbols/"+name))
	if err != nil {
		return "", err
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	w.Write(b)
	w.Close()
	return base64.StdEncoding.EncodeToString(z.Bytes()), nil
}
