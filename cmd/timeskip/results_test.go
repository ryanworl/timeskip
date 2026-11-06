package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// TestProperties checks Antithesis's semantics over timelines: Always and
// Unreachable broken by one timeline, Sometimes and Reachable judged over
// all, must_hit, and test commands that fail (unless killed).
func TestProperties(t *testing.T) {
	a := func(id, typ, display string, mustHit bool, hits, tru int) protocol.Assertion {
		return protocol.Assertion{ID: id, Message: id + " msg", Type: typ, Display: display, MustHit: mustHit, Hits: hits, True: tru}
	}
	bad := a("always", "always", "Always", true, 1, 0)
	bad.Details = json.RawMessage(`{"k":1}`)
	unreached := a("", "reachability", "Unreachable", false, 1, 1) // (no ID: keyed by its message)
	unreached.Message = "unreachable"
	runs := []run{
		{Template: "t", Seed: 1, Result: protocol.Result{
			Commands: []protocol.Record{{Name: "parallel_driver_x"}},
			Assertions: []protocol.Assertion{
				a("always", "always", "Always", true, 2, 2),
				a("sometimes", "sometimes", "Sometimes", true, 3, 0),
				a("never true", "sometimes", "Sometimes", true, 2, 0),
				{Message: "unreachable", Type: "reachability", Display: "Unreachable"},
				a("reachable", "reachability", "Reachable", true, 0, 0),
				a("or unreachable", "always", "AlwaysOrUnreachable", false, 0, 0),
				a("must hit", "always", "Always", true, 0, 0),
				a("raw", "sometimes", "Eventually true", true, 2, 0), // (judged by assert_type)
			}}},
		{Template: "t", Seed: 2, Result: protocol.Result{
			Commands:   []protocol.Record{{Name: "parallel_driver_x", Exit: 3}, {Name: "eventually_y", Exit: -1, Killed: true}},
			Assertions: []protocol.Assertion{bad, a("sometimes", "sometimes", "Sometimes", true, 1, 1), unreached},
			TimedOut:   true, // (t completed in seed 1)
		}},
		{Template: "u", Seed: 3, Result: protocol.Result{TimedOut: true}}, // (u never did)
	}
	want := map[string]string{ // name: pass hits true failing
		"t/parallel_driver_x exits 0": "false 2 1 [2]",
		"t/eventually_y exits 0":      "true 1 1 []",
		"always msg":                  "false 3 2 [2]",
		"sometimes msg":               "true 4 1 []",
		"never true msg":              "false 2 0 []",
		"unreachable":                 "false 1 1 [2]",
		"reachable msg":               "false 0 0 []",
		"or unreachable msg":          "true 0 0 []",
		"must hit msg":                "false 0 0 []",
		"raw msg":                     "false 2 0 []",
		"All commands were run to completion at least once": "false 2 1 [3]",
	}
	props := properties(runs, false)
	if len(props) != len(want) {
		t.Errorf("%d properties, want %d", len(props), len(want))
	}
	for _, p := range props {
		if got := fmt.Sprint(p.Pass, p.Hits, p.True, p.Failing); got != want[p.Name] {
			t.Errorf("%s: %s, want %s", p.Name, got, want[p.Name])
		}
		if p.Name == "always msg" && string(p.Details.(json.RawMessage)) != `{"k":1}` {
			t.Errorf("always: details %s, want those of the false evaluation", p.Details)
		}
		if p.Name == "always msg" && (p.first == nil || p.first.Seed != 2) {
			t.Errorf("always: replay %v, want seed 2", p.first)
		}
	}
	if broken := broken(runs[0]); len(broken) != 0 {
		t.Errorf("seed 1 broke %v by itself, want nothing", broken)
	}
	// A replay's one timeline doesn't judge what only all timelines can
	// fail, but still what it broke itself; a random one judges all.
	unjudged := map[string]bool{}
	for _, p := range properties(runs[:1], true) {
		if p.Unjudged {
			unjudged[p.Name] = true
		}
	}
	if want := map[string]bool{"sometimes msg": true, "never true msg": true, "reachable msg": true, "must hit msg": true, "raw msg": true}; fmt.Sprint(unjudged) != fmt.Sprint(want) {
		t.Errorf("seed 1 alone: unjudged %v, want %v", unjudged, want)
	}
	if one := properties(runs[1:2], true); slices.ContainsFunc(one, func(p *property) bool { return p.Unjudged }) {
		t.Errorf("seed 2 alone: unjudged properties, want none: it broke them itself")
	}
	if slices.ContainsFunc(properties(runs[:1], false), func(p *property) bool { return p.Unjudged }) {
		t.Errorf("seed 1 alone, not a replay: unjudged properties, want none")
	}
	// (A lone timeline that ran out of time still fails completion.)
	for _, p := range properties(runs[2:3], true) {
		if p.Name == "All commands were run to completion at least once" && (p.Pass || p.Unjudged) {
			t.Errorf("seed 3 alone: completion pass %v unjudged %v, want a failure", p.Pass, p.Unjudged)
		}
	}
}
