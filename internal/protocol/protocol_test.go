package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGoRoundTrip(t *testing.T) {
	for _, g := range []Go{
		{Skip: true, Seed: 1<<63 + 5, Template: "my tests", First: "first_b, c=d.sh", Drivers: 3,
			Timeout: 90, Faults: 2.5, Level: "resilient", Disk: []string{"fail", "power"}, Profile: true, Symbols: true,
			Explore: true},
		{Template: "u"}, // (empty lists come back nil)
	} {
		if got := ParseGo(g.String()); !reflect.DeepEqual(got, g) {
			t.Errorf("ParseGo(%q) = %+v, want %+v", g.String(), got, g)
		}
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		line, verb, body string
		ok               bool
	}{
		{Line("abc", "GO", "seed=1 skip=1"), "GO", "seed=1 skip=1", true},
		{"TS abc NOTED \r\n", "NOTED", "", true},
		{Line("abd", "GO", "seed=1"), "", "", false}, // another VM's
		{"x TS abc GO seed=1\n", "", "", false},      // command output
	} {
		verb, body, ok := Parse(c.line, "abc")
		if verb != c.verb || body != c.body || ok != c.ok {
			t.Errorf("Parse(%q) = %q, %q, %v; want %q, %q, %v", c.line, verb, body, ok, c.verb, c.body, c.ok)
		}
	}
}

func TestExit(t *testing.T) {
	// A setup failure has no timeline fields; a timeline's result comes back.
	b, _ := json.Marshal(Exit{Kind: "setup", Error: "boom"})
	if string(b) != `{"kind":"setup","error":"boom"}` {
		t.Errorf("setup EXIT = %s", b)
	}
	want := Exit{Kind: "timeline", Result: &Result{GuestMs: 7, Commands: []Record{{Name: "x", Exit: 3}}, Errors: []string{"e"}}}
	b, _ = json.Marshal(want)
	var got Exit
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("timeline EXIT %s came back as %+v (%v)", b, got, err)
	}
}
