//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appendLog appends a json-file entry to the log at path, and gives the
// log's size after it.
func appendLog(t *testing.T, path, log, stream string, at time.Duration) int64 {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"log": log, "stream": stream, "time": time.Unix(0, int64(at)).UTC()})
	f.Write(append(b, '\n'))
	st, _ := f.Stat()
	return st.Size()
}

// mergeLogs merges the logs at paths as dumpLogs does, checking every
// container's logStream after each step, and gives the lines written.
func mergeLogs(t *testing.T, paths []string, segs []clockSeg, check func(*logStream)) []string {
	var streams []*logStream
	for _, p := range paths {
		s, f, _, err := openLog(filepath.Base(p), p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if s.next(segs); s.line != "" {
			streams = append(streams, s)
		}
		check(s)
	}
	var out []string
	for len(streams) > 0 {
		i := 0
		for j, s := range streams {
			if s.t < streams[i].t {
				i = j
			}
		}
		s := streams[i]
		out = append(out, strings.TrimSuffix(s.line, "\n"))
		if s.next(segs); s.line == "" {
			streams = append(streams[:i], streams[i+1:]...)
		}
		check(s)
	}
	return out
}

// A log written as the merge reads it: the merge stops at its size when
// opened, and ends the line in part there.
func TestLogsGrowing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c")
	appendLog(t, p, "a", "stdout", time.Second)
	appendLog(t, p, "b", "stdout", time.Second)
	s, f, _, err := openLog("c", p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for range 1000 {
		appendLog(t, p, "c", "stdout", time.Second)
	}
	var got []string
	for s.next([]clockSeg{{}}); s.line != ""; s.next([]clockSeg{{}}) {
		got = append(got, s.line)
	}
	if want := []string{"+1.000s c stdout ab\n"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("lines %q, want %q", got, want)
	}
}

// Containers each with a long unended line and stderr lines behind it:
// what the merge holds of each (summed here from its lines in part, read
// ahead and to write) stays within logsHeldMax and a piece, and
// every byte goes out, in order.
func TestLogsHeld(t *testing.T) {
	piece := strings.Repeat("x", 100)
	dir := t.TempDir()
	var paths []string
	for c := range 5 {
		p := filepath.Join(dir, fmt.Sprint("c", c))
		for i := range 300 {
			if i%3 == 0 {
				appendLog(t, p, piece, "stdout", time.Duration(1+c)*time.Second)
			}
			appendLog(t, p, fmt.Sprintf("e%d\n", i), "stderr", time.Duration(10+i)*time.Second)
		}
		paths = append(paths, p)
	}
	for _, lim := range [][2]int{{1000, 2000}, {5000, 2000}, {logLineMax, logsHeldMax}} {
		t.Run(fmt.Sprint(lim), func(t *testing.T) {
			defer func(l, h int) { logLineMax, logsHeldMax = l, h }(logLineMax, logsHeldMax)
			logLineMax, logsHeldMax = lim[0], lim[1]
			out, n := map[string]string{}, map[string]int{}
			for _, l := range mergeLogs(t, paths, []clockSeg{{}}, func(s *logStream) {
				held := len(s.line)
				for _, p := range s.parts {
					held += p.log.Len()
				}
				for _, l := range s.ready {
					held += len(l.line)
				}
				if held > logsHeldMax+256 {
					t.Fatalf("%s: %d bytes held, over %d", s.name, held, logsHeldMax+256)
				}
			}) {
				f := strings.SplitN(l, " ", 4)
				if f[2] == "stdout" {
					out[f[1]] += f[3]
					n[f[1]]++
				} else {
					out[f[1]+" stderr"] += f[3] + ","
				}
			}
			var e string
			for i := range 300 {
				e += fmt.Sprintf("e%d,", i)
			}
			for _, p := range paths {
				c := filepath.Base(p)
				if out[c] != strings.Repeat(piece, 100) {
					t.Errorf("%s's stdout is %d bytes, want %d", c, len(out[c]), 100*len(piece))
				}
				if out[c+" stderr"] != e {
					t.Errorf("%s's stderr %q, want %q", c, out[c+" stderr"], e)
				}
				if lim[0] > 1<<16 && n[c] != 1 {
					t.Errorf("%s's stdout in %d lines, want 1", c, n[c])
				}
			}
		})
	}
}

// A line out in chunks across two clock steps: every chunk has the time
// of the line's first piece.
func TestLogsChunkTime(t *testing.T) {
	defer func(l int) { logLineMax = l }(logLineMax)
	logLineMax = 1000
	p := filepath.Join(t.TempDir(), "c")
	at := 100 * time.Second
	var size [30]int64
	for i := range 30 {
		size[i] = appendLog(t, p, strings.Repeat("x", 100)+map[bool]string{true: "\n"}[i == 29], "stdout", at)
	}
	// Steps of +10 s once 10 pieces were written and +10 s at 20 (a piece
	// read past the second, alone, would be timed 80 s).
	segs := []clockSeg{{0, 0, nil}, {85 * time.Second, 10 * time.Second, map[string]int64{p: size[9]}}, {90 * time.Second, 20 * time.Second, map[string]int64{p: size[19]}}}
	got := mergeLogs(t, []string{p}, segs, func(*logStream) {})
	if len(got) != 3 {
		t.Fatalf("%d lines, want 3 chunks of 1000 bytes", len(got))
	}
	for _, l := range got {
		if !strings.HasPrefix(l, "+100.000s c stdout ") {
			t.Errorf("chunk %.40q, want it at +100.000s", l)
		}
	}
}
