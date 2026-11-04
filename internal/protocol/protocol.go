// Package protocol is the control channel between the CLI (cmd/timeskip)
// and the guest agent (cmd/guest), as README.md describes it: lines on the
// VM's console of the form "TS <nonce> VERB BODY". The host's verbs (SETUP,
// GO, CONTINUE, NOTED, BRANCH) have key=value fields; the guest's (BASE, READY,
// CHECKPOINT, EDGES, EXIT) a JSON body.
package protocol

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// BundleDir is where the guest keeps the bundle's files: mounts/ (see
// internal/bundle) holds the files a project bind-mounts.
const BundleDir = "/var/lib/docker/timeskip"

// Line is a control line for nonce's VM.
func Line(nonce, verb, body string) string {
	return "TS " + nonce + " " + verb + " " + body + "\n"
}

// Parse is line's verb and body, if it's a control line for nonce's VM.
func Parse(line, nonce string) (verb, body string, ok bool) {
	s, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "TS "+nonce+" ")
	if !ok {
		return "", "", false
	}
	verb, body, _ = strings.Cut(s, " ")
	return verb, body, true
}

// Fields are a body's (or the kernel command line's) key=value words.
func Fields(body string) map[string]string {
	kv := map[string]string{}
	for _, x := range strings.Fields(body) {
		k, v, _ := strings.Cut(x, "=")
		kv[k] = v
	}
	return kv
}

// Go runs a timeline.
type Go struct {
	Skip     bool   // skip idle time
	Seed     uint64 // its schedule's, and the kernel's random number generator's, reseeded with it
	Template string
	First    string   // the template's first_ command to run (none if empty)
	Firsts   []string // the template's first_ commands, as the host found them
	Drivers  int      // driver commands to launch, when not a singleton_driver
	Timeout  int      // guest seconds before the timeline is stopped
	// Pre are the edits to apply before First, by name: the ones touching
	// or deleting first_ and helper_ files.
	Pre []string
	// Checkpoint: after First, go idle and send CHECKPOINT, for the host to
	// save the VM, then wait. CONTINUE goes on; a GO comes to a VM restored
	// from that checkpoint, which goes on with that GO's seed and inputs.
	Checkpoint bool
	Inputs     string   // where the edits are: none, virtiofs or blk:SIZE (the third drive)
	Faults     float64  // inject faults while the drivers run, at most a gap apart that each timeline draws around Faults seconds (see the guest's inject); 0: none
	Level      string   // which faults, by Antithesis's fault tolerance level: legacy, resilient or high_availability
	Disk       []string // with Faults, disk faults too, of these kinds (fail, full, power), which no fault tolerance level makes
	Profile    bool     // sample the containers' stacks as they use the CPU
	Symbols    bool     // send the instrumented modules' symbol tables
	// Explore: send EDGES as the drivers run, when instrumented code
	// reaches edges new to the VM or SDK assertions reach new states, and
	// take BRANCH.
	Explore bool
}

func (g Go) String() string {
	return fmt.Sprintf("skip=%d seed=%d template=%s first=%s firsts=%s drivers=%d timeout=%d pre=%s checkpoint=%d inputs=%s faults=%g level=%s disk=%s profile=%d symbols=%d explore=%d",
		bit(g.Skip), g.Seed, url.QueryEscape(g.Template), url.QueryEscape(g.First), list(g.Firsts), g.Drivers, g.Timeout,
		list(g.Pre), bit(g.Checkpoint), g.Inputs, g.Faults, g.Level, list(g.Disk), bit(g.Profile), bit(g.Symbols), bit(g.Explore))
}

// ParseGo is the Go in a GO line's body.
func ParseGo(body string) Go {
	kv := Fields(body)
	g := Go{Skip: kv["skip"] == "1", Template: unescape(kv["template"]), First: unescape(kv["first"]), Firsts: unlist(kv["firsts"]),
		Pre: unlist(kv["pre"]), Checkpoint: kv["checkpoint"] == "1", Inputs: kv["inputs"], Level: kv["level"], Disk: unlist(kv["disk"]), Profile: kv["profile"] == "1", Symbols: kv["symbols"] == "1", Explore: kv["explore"] == "1"}
	g.Seed, _ = strconv.ParseUint(kv["seed"], 10, 64)
	g.Drivers, _ = strconv.Atoi(kv["drivers"])
	g.Timeout, _ = strconv.Atoi(kv["timeout"])
	g.Faults, _ = strconv.ParseFloat(kv["faults"], 64)
	return g
}

func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// list and unlist write and read a list of names (any but empty ones) as a
// field's value; escape and unescape one name.
func list(l []string) string {
	var e []string
	for _, s := range l {
		e = append(e, url.QueryEscape(s))
	}
	return strings.Join(e, ",")
}

func unlist(s string) []string {
	if s == "" {
		return nil
	}
	l := strings.Split(s, ",")
	for i := range l {
		l[i] = unescape(l[i])
	}
	return l
}

func unescape(s string) string {
	u, _ := url.QueryUnescape(s)
	return u
}

// Edges is EDGES's body: what a timeline did for the first time in the VM
// as it goes (see Go's Explore).
type Edges struct {
	AtMs  int64            `json:"at_ms"` // guest time since the timeline started
	Edges map[string][]int `json:"edges"` // edges instrumented code reached, by module
	// Assertions are SDK assertions' states: "reached KEY" and, of a
	// Sometimes assertion, "true KEY", KEY its ID or else its message.
	Assertions []string `json:"assertions,omitempty"`
}

// A Command is a test command, as the guest found it.
type Command struct {
	Name string `json:"name"` // the file name, e.g. parallel_driver_put.sh
}

// A Catalog is the test commands by template, each sorted by name.
type Catalog map[string][]Command

// Ready is READY's body.
type Ready struct {
	Catalog Catalog `json:"catalog"`
}

// Exit is EXIT's body.
type Exit struct {
	Kind    string  `json:"kind"`              // timeline, setup or replan
	Error   string  `json:"error,omitempty"`   // (setup) what failed
	Catalog Catalog `json:"catalog,omitempty"` // (replan) the test commands after the edits
	*Result         // (timeline)
}

// A Result is a timeline's.
type Result struct {
	GuestMs    int64       `json:"guest_ms"`
	Skips      int         `json:"skips"` // idle periods skipped
	Commands   []Record    `json:"commands"`
	Assertions []Assertion `json:"assertions"`
	Errors     []string    `json:"errors"` // what went wrong besides
	Faults     []Fault     `json:"faults,omitempty"`
	FaultMix   string      `json:"fault_mix,omitempty"`    // with a level that makes node faults, the half the timeline drew: "node faults" or "no node faults"
	FaultGapMs int64       `json:"fault_gap_ms,omitempty"` // with faults, the most guest time between two that the timeline drew
	TimedOut   bool        `json:"timed_out,omitempty"`    // it ran out of time: what ran was killed
	// The guest time it first failed, as the guest agent saw it: a command
	// failed, or, exploring, an Always assertion was false or an
	// Unreachable one reached, or a container exited unexpectedly.
	FailedMs int64 `json:"failed_ms,omitempty"`
	// The CPU time each container used (its cgroup's, test commands'
	// included), by name, in ms.
	ContainerCPUMs map[string]int64 `json:"container_cpu_ms,omitempty"`
	// With Go's Profile: the stacks sampled, folded (container;command;
	// outermost frame;...;innermost), each every 10 ms of CPU time.
	Profile map[string]int64 `json:"profile,omitempty"`
	// The edges instrumented code reached (setup's included), by module
	// (its symbol table's name); with Go's Symbols, the symbol tables,
	// gzipped, in base64.
	Coverage map[string]Coverage `json:"coverage,omitempty"`
	Symbols  map[string]string   `json:"symbols,omitempty"`
}

// Coverage is a module's edges, 0 to Edges, and a bit per edge reached, in
// base64 (edge e is bit e%8 of byte e/8).
type Coverage struct {
	Edges int    `json:"edges"`
	Bits  string `json:"bits"`
}

// A Record is a test command's run.
type Record struct {
	Name   string `json:"name"`
	Exit   int    `json:"exit"`
	Killed bool   `json:"killed,omitempty"`
	Ms     int64  `json:"ms"`
	CPUMs  int64  `json:"cpu_ms"` // the CPU time it and what it waited for used
}

// An Assertion sums up an Antithesis SDK assertion's evaluations.
type Assertion struct {
	ID      string          `json:"id"`
	Message string          `json:"message"`
	Type    string          `json:"type"`    // always, sometimes or reachability
	Display string          `json:"display"` // e.g. AlwaysOrUnreachable
	MustHit bool            `json:"must_hit"`
	Hits    int             `json:"hits"`
	True    int             `json:"true"`
	Details json.RawMessage `json:"details,omitempty"` // of the first false evaluation
	// FirstFalseMs is the guest time (since the timeline started) it was
	// first false (an Always one) or reached (an Unreachable one), when
	// exploring.
	FirstFalseMs int64 `json:"first_false_ms,omitempty"`
}

// A Fault is one that the guest's injector made.
type Fault struct {
	Kind    string     `json:"kind"`    // latency, partition, clog, pause, stop, kill, throttle, modulation or skip (the clock's)
	Detail  string     `json:"detail"`  // e.g. "stopped, one way" or "slowed: delay 300ms 75ms"
	Targets [][]string `json:"targets"` // a partition's two sides (one way: from the first), a clog's or pause's containers
	AtMs    int64      `json:"at_ms"`   // guest time since the timeline started
	Ms      int64      `json:"ms"`
}
