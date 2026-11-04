package main

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// wantSymbols says whether timelines should send the instrumented modules'
// symbol tables: with --out, until one sent its modules'.
var wantSymbols atomic.Bool

// A moduleCoverage is the runs' coverage of an instrumented module: the
// edges they reached, how many of them no earlier run (with this cache)
// had, and how many were reached after each run, in order: how exploring
// went, Antithesis's "new behaviors discovered over time".
type moduleCoverage struct {
	Module   string `json:"module"`
	Edges    int    `json:"edges"`
	Reached  int    `json:"reached"`
	New      int    `json:"new"`
	Progress []int  `json:"progress"`
	bits     []byte
	symbols  []byte // its symbol table, if a run sent it
}

// coverageOf sums up the runs' coverage, adds it to the cache's, and with
// dir writes each module's functions there, reached or not.
func coverageOf(runs []run, dir string) []*moduleCoverage {
	mods := map[string]*moduleCoverage{}
	for i, r := range runs {
		for name, c := range r.Coverage {
			m := mods[name]
			if m == nil {
				m = &moduleCoverage{Module: name, Progress: make([]int, i)}
				mods[name] = m
			}
			m.Edges = max(m.Edges, c.Edges)
			b, _ := base64.StdEncoding.DecodeString(c.Bits)
			m.bits = or(m.bits, b)
			if s, ok := r.Symbols[name]; ok && m.symbols == nil {
				if z, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(s))); err == nil {
					m.symbols, _ = io.ReadAll(z)
				}
			}
		}
		for _, m := range mods {
			m.Progress = append(m.Progress, count(m.bits))
		}
	}
	l := slices.SortedFunc(maps.Values(mods), func(a, b *moduleCoverage) int { return cmp.Compare(a.Module, b.Module) })
	for _, m := range l {
		m.Reached = count(m.bits)
		f := filepath.Join(cache, "coverage", m.Module)
		was, _ := os.ReadFile(f)
		m.New = m.Reached - count(and(m.bits, was))
		os.MkdirAll(filepath.Dir(f), 0o755)
		write(f, string(or(slices.Clone(m.bits), was)))
		if dir != "" && m.symbols != nil {
			check(os.MkdirAll(dir, 0o755))
			write(filepath.Join(dir, strings.TrimSuffix(m.Module, ".sym.tsv")+".functions.tsv"), m.functions())
		}
	}
	return l
}

// reportCoverage prints each module's coverage, how it grew over the runs,
// and with dir, how many of its functions were never reached.
func reportCoverage(mods []*moduleCoverage, dir string) {
	for _, m := range mods {
		fmt.Fprintf(os.Stderr, "coverage: %s reached %d of %d edges (%.1f%%), %d in no earlier run\n",
			m.Module, m.Reached, m.Edges, 100*float64(m.Reached)/float64(max(m.Edges, 1)), m.New)
		if n := len(m.Progress); n >= 4 {
			var at []string
			for q := 1; q <= 4; q++ {
				at = append(at, strconv.Itoa(m.Progress[n*q/4-1]))
			}
			added := m.Reached - m.Progress[n*3/4-1]
			verdict := "still growing: more timelines may reach more"
			if float64(added) < 0.01*float64(m.Reached) {
				verdict = "levelling off: more of the same timelines won't reach much more; vary what they do"
			}
			fmt.Fprintf(os.Stderr, "  by each quarter of the timelines: %s edges; the last quarter added %d, %s\n", strings.Join(at, " → "), added, verdict)
		}
		if dir == "" {
			continue
		}
		if m.symbols == nil {
			fmt.Fprintf(os.Stderr, "  no symbol table: put %s in /symbols in its image\n", m.Module)
			continue
		}
		total, never := 0, 0
		for _, l := range strings.Split(strings.TrimSpace(m.functions()), "\n")[1:] {
			total++
			if strings.HasSuffix(l, "\t0") {
				never++
			}
		}
		fmt.Fprintf(os.Stderr, "  %d of %d functions never reached: %s\n", never, total, filepath.Join(dir, strings.TrimSuffix(m.Module, ".sym.tsv")+".functions.tsv"))
	}
}

// functions are the module's functions as TSV, by file and line: each one's
// edges and how many were reached, from its symbol table (Antithesis's:
// file, function, begin_line, begin_column, end_line, end_column and the
// edge, address, a line).
func (m *moduleCoverage) functions() string {
	type fn struct {
		file, name     string
		line           int
		edges, reached int
	}
	by := map[string]*fn{}
	s := bufio.NewScanner(bytes.NewReader(m.symbols))
	for s.Scan() {
		f := strings.Split(s.Text(), "\t")
		if len(f) < 7 || strings.HasPrefix(f[0], "#") || f[0] == "file" {
			continue
		}
		edge, err := strconv.Atoi(f[6])
		line, _ := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		k := f[0] + "\t" + f[1]
		if by[k] == nil {
			by[k] = &fn{file: f[0], name: f[1], line: line}
		}
		by[k].line = min(by[k].line, line)
		by[k].edges++
		if edge/8 < len(m.bits) && m.bits[edge/8]&(1<<(edge%8)) != 0 {
			by[k].reached++
		}
	}
	fns := slices.SortedFunc(maps.Values(by), func(a, b *fn) int { return cmp.Or(cmp.Compare(a.file, b.file), cmp.Compare(a.line, b.line)) })
	var b strings.Builder
	b.WriteString("file\tfunction\tline\tedges\treached\n")
	for _, f := range fns {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\n", f.file, f.name, f.line, f.edges, f.reached)
	}
	return b.String()
}

// or ORs b into a (growing it) and returns it.
func or(a, b []byte) []byte {
	for len(a) < len(b) {
		a = append(a, 0)
	}
	for i, x := range b {
		a[i] |= x
	}
	return a
}

func and(a, b []byte) []byte {
	c := make([]byte, min(len(a), len(b)))
	for i := range c {
		c[i] = a[i] & b[i]
	}
	return c
}

func count(b []byte) (n int) {
	for _, x := range b {
		n += bits.OnesCount8(x)
	}
	return n
}
