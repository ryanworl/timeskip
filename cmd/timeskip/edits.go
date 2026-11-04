package main

import (
	"archive/tar"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ryanworl/timeskip/internal/bundle"
)

// An edit copies file (a file, or a directory less what skip leaves out)
// to target in service's containers, then takes action, as a develop.watch
// sync rule does under docker compose watch. A directory
// merges into what's there. deleted are paths under target whose files
// were deleted on the host.
type edit struct {
	service, target, file, action string
	skip                          func(string, bool) bool
	deleted                       []string
}

// syncedEdits are the edits of the sync rules packed into bundle b (see
// bundle.Options.Synced), whose files go to a temporary directory.
func syncedEdits(b string) []edit {
	f, err := os.Open(b)
	check(err)
	defer f.Close()
	var m struct{ Rules []bundle.SyncedRule }
	d := tempDir()
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		check(err)
		dst := filepath.Join(d, filepath.FromSlash(h.Name))
		switch {
		case h.Name == "syncs":
			check(json.NewDecoder(r).Decode(&m))
		case !strings.HasPrefix(h.Name, "synced/"):
		case h.Typeflag == tar.TypeDir:
			check(os.MkdirAll(dst, 0o755))
		case h.Typeflag == tar.TypeSymlink:
			check(os.Symlink(h.Linkname, dst))
		default:
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, h.FileInfo().Mode().Perm())
			check(err)
			_, err = io.Copy(w, r)
			check(cmp.Or(err, w.Close()))
		}
	}
	var in []edit
	for i, y := range m.Rules {
		in = append(in, edit{y.Service, y.Target, filepath.Join(d, "synced", strconv.Itoa(i)), y.Action, nil, nil})
	}
	return in
}

// staged edits, in dir/inputs: copies of the files, with the manifests
// replace and delete (see cmd/guest). On Linux they become an ext4 image,
// img, for the VMs' inputs drive.
type staged struct {
	dir, spec, img string
	edits          []edit
	paths          [][]string // each edit's paths in its containers
}

func stage(d string, edits []edit) *staged {
	s := &staged{dir: filepath.Join(d, "inputs"), spec: "none", edits: edits}
	check(os.MkdirAll(s.dir, 0o755))
	var replace, del strings.Builder
	for i, e := range edits {
		name := strconv.Itoa(i)
		for _, p := range e.deleted {
			fmt.Fprintf(&del, "%s\t%s\t%s\t%s\n", e.service, p, name, e.action)
		}
		// Only what the rule syncs is copied: not a whole .git or
		// node_modules that it leaves out.
		var paths []string
		dst := filepath.Join(s.dir, name)
		check(bundle.Walk(e.file, e.skip, func(p string, fi fs.FileInfo) {
			if fi.Mode().Type()&^(fs.ModeDir|fs.ModeSymlink) != 0 {
				return // (sockets, pipes and devices aren't synced)
			}
			rel, _ := filepath.Rel(e.file, p)
			copyEntry(p, fi, filepath.Join(dst, rel))
			if !fi.IsDir() {
				paths = append(paths, filepath.Join(e.target, rel))
			}
		}))
		if _, err := os.Lstat(dst); err == nil { // (not if the file is gone or the rule leaves it out)
			fmt.Fprintf(&replace, "%s\t%s\t%s\t%s\n", e.service, e.target, name, e.action)
		}
		s.paths = append(s.paths, paths)
	}
	write(filepath.Join(s.dir, "replace"), replace.String())
	write(filepath.Join(s.dir, "delete"), del.String())
	if replace.Len()+del.Len() > 0 {
		s.seal(d)
	}
	return s
}

// copyEntry copies p, a directory (not what's in it), a symlink or a file, to
// to, with its permissions.
func copyEntry(p string, fi fs.FileInfo, to string) {
	switch {
	case fi.IsDir():
		check(os.Mkdir(to, fi.Mode().Perm()|0o700)) // (and writable, to fill)
	case fi.Mode().Type()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		check(err)
		check(os.Symlink(l, to))
	default:
		in, err := os.Open(p)
		check(err)
		defer in.Close()
		out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		check(err)
		_, err = io.Copy(out, in) // (copy_file_range on Linux: a reflink where it can)
		check(errors.Join(err, out.Close()))
	}
}

// seal makes the staged files the VMs' inputs: a virtiofs share (macOS), or
// an ext4 image in d (Linux).
func (s *staged) seal(d string) {
	if darwin {
		s.spec = "virtiofs"
		return
	}
	s.img = filepath.Join(d, "inputs.img")
	var kb int64
	filepath.WalkDir(s.dir, func(_ string, e fs.DirEntry, _ error) error {
		if fi, err := e.Info(); err == nil {
			kb += fi.Size()/1024 + 4
		}
		return nil
	})
	os.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/sbin") // (mkfs.ext4 is often there)
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-d", s.dir, "-O", "^has_journal", "-E", "root_owner=0:0", s.img, fmt.Sprintf("%dk", kb*11/10+4096))
	if out, err := cmd.CombinedOutput(); err != nil {
		die("mkfs.ext4: %v\n%s", err, out)
	}
	fi, err := os.Stat(s.img)
	check(err)
	s.spec = fmt.Sprintf("blk:%d", fi.Size())
}

// templates are the test templates the edits add files to.
func (s *staged) templates() (l []string) {
	for _, paths := range s.paths {
		for _, p := range paths {
			if rel, ok := strings.CutPrefix(p, "/opt/antithesis/test/v1/"); ok && strings.Contains(rel, "/") {
				l = append(l, strings.Split(rel, "/")[0])
			}
		}
	}
	return l
}

// pre is what of the edits a template's first_ command can see: the edits
// that touch (or delete) its first_ or helper_ files (their names, to
// apply before it), and a digest of those files, for its checkpoint's name.
func (s *staged) pre(tmpl string) (names []string, digest string) {
	h := sha256.New()
	dir := "/opt/antithesis/test/v1/" + tmpl + "/"
	seen := func(p string) bool {
		rel, ok := strings.CutPrefix(p, dir)
		return ok && (strings.HasPrefix(rel, "first_") || strings.HasPrefix(rel, "helper_"))
	}
	for i, paths := range s.paths {
		e := s.edits[i]
		gone := slices.DeleteFunc(slices.Clone(e.deleted), func(p string) bool { return !seen(p) })
		if !slices.ContainsFunc(paths, seen) && len(gone) == 0 {
			continue
		}
		names = append(names, strconv.Itoa(i))
		for _, p := range gone {
			fmt.Fprintln(h, e.service, p, "deleted")
		}
		root := filepath.Join(s.dir, strconv.Itoa(i))
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			rel, _ := filepath.Rel(root, p)
			if err == nil && !d.IsDir() && seen(filepath.Join(e.target, rel)) {
				fi, _ := d.Info()
				fmt.Fprintln(h, e.service, filepath.Join(e.target, rel), fi.Mode())
				f, _ := os.Open(p)
				io.Copy(h, f)
				f.Close()
			}
			return nil
		})
	}
	return names, fmt.Sprintf("%x", h.Sum(nil))[:16]
}
