package bundle

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// develop.watch, read as docker compose watch reads it (compose's
// pkg/compose/watch.go and pkg/watch).

type watchRule struct {
	Action  string   `json:"action"`
	Path    string   `json:"path"`
	Target  string   `json:"target"`
	Ignore  []string `json:"ignore"`
	Include []string `json:"include"`
}

// A Sync is a develop.watch sync rule (sync or sync+restart): Path on the
// host, a file or a directory, belongs at Target in Service. Rule is its
// index in the service's develop.watch.
type Sync struct {
	Service, Path, Target, Action string
	Rule                          int
	// Deleted are the files under Path that were there when Service's
	// image was built and aren't any more.
	Deleted []string
	// Skip says whether a host path under Path is left out: a file or, if
	// dir, a whole directory.
	Skip func(path string, dir bool) bool
}

// Files are the files (not directories) the rule syncs.
func (y Sync) Files() ([]string, error) {
	var l []string
	err := Walk(y.Path, y.Skip, func(p string, fi fs.FileInfo) {
		if !fi.IsDir() {
			l = append(l, p)
		}
	})
	return l, err
}

// Walk calls fn with root and each file and directory under it that skip
// doesn't leave out, pruning the directories it leaves out whole. (A root
// that's a file is left out too if skip says so, as compose watch matches
// it.)
func Walk(root string, skip func(string, bool) bool, fn func(string, fs.FileInfo)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return nil
			}
			return err
		}
		if (p != root || !d.IsDir()) && skip != nil && skip(p, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fn(p, fi)
		return nil
	})
}

// ephemeral are the editor and tool temporary files compose watch ignores.
var ephemeral = []string{"**/*___jb_old___", "**/*___jb_tmp___", "**/.idea/**", "**/.#*", "**/#*#",
	"**/4913", "**/*~", "**/.*.swp", "**/.*.swx", "**/.*.swo", "**/.*.swn", "**/.*.kate-swp", "**/*-go-tmp-umask"}

// skipper returns a rule's skip: a file is left out unless it matches the
// rule's include patterns (if any), and if it matches the build context's
// .dockerignore, an editor's temporary file, **/.git or the rule's ignore
// patterns. A directory is left out whole only if an ignore pattern matches
// it and no exclusion (!pattern) could bring back something inside it.
func skipper(w watchRule, b *buildSpec) (func(string, bool) bool, error) {
	var include *patternmatcher.PatternMatcher
	if len(w.Include) > 0 {
		m, err := matcher(w.Path, w.Include, true)
		if err != nil {
			return nil, err
		}
		include = m
	}
	type set struct {
		root     string
		patterns []string
	}
	sets := []set{{"/", ephemeral}, {"/", []string{"**/.git"}}, {w.Path, w.Ignore}}
	if b != nil {
		patterns, err := dockerignore(b)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set{b.Context, patterns})
	}
	var ignore []*patternmatcher.PatternMatcher
	for _, set := range sets {
		m, err := matcher(set.root, set.patterns, true)
		if err != nil {
			return nil, err
		}
		ignore = append(ignore, m)
	}
	return func(p string, dir bool) bool {
		if dir {
			return slices.ContainsFunc(ignore, func(m *patternmatcher.PatternMatcher) bool { return entireDir(m, p) })
		}
		if include != nil {
			if ok, _ := include.MatchesOrParentMatches(p); !ok {
				return true
			}
		}
		return slices.ContainsFunc(ignore, func(m *patternmatcher.PatternMatcher) bool {
			ok, _ := m.MatchesOrParentMatches(p)
			return ok
		})
	}, nil
}

// entireDir says whether m leaves out the directory dir and everything in
// it, as compose watch's MatchesEntireDir does.
func entireDir(m *patternmatcher.PatternMatcher, dir string) bool {
	if ok, _ := m.MatchesOrParentMatches(dir); !ok {
		return false
	}
	for _, p := range m.Patterns() {
		if p.Exclusion() && strings.HasPrefix(p.String(), dir+"/") {
			return false
		}
	}
	return true
}

// dockerignore reads a build's ignore file: DOCKERFILE.dockerignore, or else
// .dockerignore, in its context.
func dockerignore(b *buildSpec) ([]string, error) {
	f, err := os.Open(b.dockerfile() + ".dockerignore")
	if os.IsNotExist(err) {
		f, err = os.Open(filepath.Join(b.Context, ".dockerignore"))
	}
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	return ignorefile.ReadAll(f)
}

// matcher makes .dockerignore-style patterns relative to root absolute.
// For watch, as compose watch does, including its rule that a "*" pattern
// leaves only the exclusions; otherwise as docker build does.
func matcher(root string, patterns []string, watch bool) (*patternmatcher.PatternMatcher, error) {
	if watch && slices.Contains(patterns, "*") {
		patterns = slices.DeleteFunc(slices.Clone(patterns), func(p string) bool { return p != "" && p[0] != '!' })
	}
	var abs []string
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		p = filepath.Clean(p)
		not := ""
		if p[0] == '!' {
			not, p = "!", p[1:]
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		abs = append(abs, not+p)
	}
	return patternmatcher.New(abs)
}

// buildInputs digests what building s reads, as far as timeskip can tell
// without building: its build options, and the path, mode and contents of
// its Dockerfile, of each file in its context less what .dockerignore
// excludes and its sync rules deliver (sync, not a new image, brings
// those), and of each file under its rebuild rules' paths; and the path
// and mode of each directory among those.
func buildInputs(name string, s service, b *buildSpec, syncs []Sync, sums *fileSums) (string, error) {
	h := sha256.New()
	opts, _ := json.Marshal(b)
	h.Write(opts)
	synced := func(p string, dir bool) bool {
		return slices.ContainsFunc(syncs, func(y Sync) bool {
			return y.Service == name && (p == y.Path || strings.HasPrefix(p, y.Path+"/")) && !y.Skip(p, dir)
		})
	}
	var errs []error
	add := func(root string, skip func(string, bool) bool) error {
		return Walk(root, skip, func(p string, fi fs.FileInfo) {
			if fi.IsDir() {
				if !synced(p, true) {
					fmt.Fprintf(h, "%s %o\n", p, fi.Mode())
				}
				return
			}
			sum, err := sums.of(p, fi)
			errs = append(errs, err)
			fmt.Fprintf(h, "%s %o %s\n", p, fi.Mode(), sum)
		})
	}
	patterns, err := dockerignore(b)
	if err != nil {
		return "", err
	}
	ignore, err := matcher(b.Context, patterns, false)
	if err != nil {
		return "", err
	}
	err = add(b.Context, func(p string, dir bool) bool {
		if dir {
			return entireDir(ignore, p)
		}
		if ok, _ := ignore.MatchesOrParentMatches(p); ok {
			return true
		}
		return synced(p, false)
	})
	if err == nil {
		err = add(b.dockerfile(), nil)
	}
	for _, w := range s.Develop.Watch {
		if w.Action != "rebuild" || err != nil {
			continue
		}
		var skip func(string, bool) bool
		if skip, err = skipper(w, b); err == nil {
			err = add(w.Path, skip)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), cmp.Or(err, errors.Join(errs...))
}

// fileSums are files' content digests by path, size, and modification and
// change times (the change time moves with any write, even one that keeps
// the size and modification time), so that a file is read only if one of
// those changed since the last bundle. was is that bundle's; now, this
// one's, less files changed in the last seconds: a write in the same
// timestamp tick would change none of those (as git's racily clean files).
type fileSums struct{ was, now map[string]string }

func (c *fileSums) of(p string, fi fs.FileInfo) (string, error) {
	k := fmt.Sprintf("%s %d %d %d", p, fi.Size(), fi.ModTime().UnixNano(), ctime(fi).UnixNano())
	sum, ok := c.was[k]
	if !ok {
		h := sha256.New()
		if fi.Mode()&fs.ModeSymlink != 0 {
			l, err := os.Readlink(p)
			if err != nil {
				return "", err
			}
			h.Write([]byte(l))
		} else {
			f, err := os.Open(p)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
		sum = fmt.Sprintf("%x", h.Sum(nil))
	}
	if time.Since(ctime(fi)) > 2*time.Second {
		c.now[k] = sum
	}
	return sum, nil
}

func ctime(fi fs.FileInfo) time.Time { return ctimeOf(fi.Sys().(*syscall.Stat_t)) }
