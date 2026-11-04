//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// The test templates, as Antithesis's Test Composer finds them: directories
// /opt/antithesis/test/v1/<template>/ in any running container, holding
// executables named <prefix>_<name>. Subdirectories and helper_ files are
// left alone; other files are invalid commands.
const templates = "/opt/antithesis/test/v1"

var kinds = []string{"first", "parallel_driver", "singleton_driver", "serial_driver", "anytime", "eventually", "finally"}

// A command is a test command in a container.
type command struct {
	protocol.Command
	kind      string
	container string
	path      string // in the container
}

// catalog is the test commands by template, each sorted by name.
type catalog map[string][]command

func kindOf(name string) string {
	for _, k := range kinds {
		if strings.HasPrefix(name, k+"_") {
			return k
		}
	}
	return ""
}

// discover finds the test commands, and the invalid ones: files without a
// known prefix, or that can't be executed.
func discover() (cat catalog, invalid []string, err error) {
	cs, err := containers("")
	if err != nil {
		return nil, nil, err
	}
	cat = catalog{}
	for _, c := range cs {
		if c.State.Status != "running" {
			continue
		}
		dirs, _ := os.ReadDir(resolve(c.root(), templates))
		for _, d := range dirs {
			t := d.Name()
			files, _ := os.ReadDir(resolve(c.root(), templates+"/"+t))
			for _, f := range files {
				n, p := f.Name(), templates+"/"+t+"/"+f.Name()
				fi, err := os.Stat(resolve(c.root(), p))
				switch k := kindOf(n); {
				case strings.HasPrefix(n, "helper_") || err == nil && fi.IsDir():
				case k == "":
					invalid = append(invalid, fmt.Sprintf("%s in %s: no test command prefix", p, c.Name))
				case err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0:
					invalid = append(invalid, fmt.Sprintf("%s in %s: not an executable file", p, c.Name))
				default:
					cat[t] = append(cat[t], command{protocol.Command{Name: n}, k, c.ID, p})
				}
			}
		}
	}
	for _, l := range cat {
		sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	}
	slices.Sort(invalid)
	return cat, invalid, nil
}

// resolve is where path (in a container whose filesystem is at root) is in
// the VM, following symlinks within the container (and, as there, no
// further up than its /).
func resolve(root, path string) string {
	p := "/"
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		next := filepath.Join(p, part)
		for range 10 {
			l, err := os.Readlink(filepath.Join(root, next))
			if err != nil {
				break
			}
			if !filepath.IsAbs(l) {
				l = filepath.Join(filepath.Dir(next), l)
			}
			next = filepath.Join("/", l)
		}
		p = next
	}
	return filepath.Join(root, p)
}

// wire is the catalog as the host gets it.
func (cat catalog) wire() protocol.Catalog {
	w := protocol.Catalog{}
	for t, cs := range cat {
		w[t] = make([]protocol.Command, len(cs))
		for i, c := range cs {
			w[t][i] = c.Command
		}
	}
	return w
}

func (cat catalog) firsts(t string) []string {
	var f []string
	for _, c := range cat[t] {
		if c.kind == "first" {
			f = append(f, c.Name)
		}
	}
	return f
}

func (cat catalog) of(t, kind string) []command {
	return slices.DeleteFunc(slices.Clone(cat[t]), func(c command) bool { return c.kind != kind })
}
