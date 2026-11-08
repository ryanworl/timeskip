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
	"strconv"
	"strings"

	"github.com/ryanworl/timeskip/internal/bundle"
)

// An edit copies file (a file, or a directory less what skip leaves out)
// to target in service's containers before they start, as a develop.watch
// sync rule does under docker compose watch. A directory merges into what's
// there. deleted are paths under target whose files were deleted on the
// host.
type edit struct {
	service, target, file string
	skip                  func(string, bool) bool
	deleted               []string
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
			check(os.Chmod(dst, 0o755)) // (whatever the umask, as below)
		case h.Typeflag == tar.TypeSymlink:
			check(os.Symlink(h.Linkname, dst))
		default:
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, h.FileInfo().Mode().Perm())
			check(err)
			_, err = io.Copy(w, r)
			check(cmp.Or(err, w.Close(), os.Chmod(dst, h.FileInfo().Mode().Perm())))
		}
	}
	var in []edit
	for i, y := range m.Rules {
		in = append(in, edit{y.Service, y.Target, filepath.Join(d, "synced", strconv.Itoa(i)), nil, nil})
	}
	return in
}

// staged edits, in dir/inputs: copies of the files, with the manifests
// replace and delete (see cmd/guest). On Linux they become an ext4 image,
// img, for the VM's inputs drive.
type staged struct {
	dir, spec, img string
}

func stage(d string, edits []edit) *staged {
	s := &staged{dir: filepath.Join(d, "inputs"), spec: "none"}
	check(os.MkdirAll(s.dir, 0o755))
	var replace, del strings.Builder
	for i, e := range edits {
		name := strconv.Itoa(i)
		for _, p := range e.deleted {
			fmt.Fprintf(&del, "%s\t%s\n", e.service, p)
		}
		// Only what the rule syncs is copied: not a whole .git or
		// node_modules that it leaves out.
		dst := filepath.Join(s.dir, name)
		check(bundle.Walk(e.file, e.skip, func(p string, fi fs.FileInfo) {
			if fi.Mode().Type()&^(fs.ModeDir|fs.ModeSymlink) != 0 {
				return // (sockets, pipes and devices aren't synced)
			}
			rel, _ := filepath.Rel(e.file, p)
			copyEntry(p, fi, filepath.Join(dst, rel))
		}))
		if _, err := os.Lstat(dst); err == nil { // (not if the file is gone or the rule leaves it out)
			fmt.Fprintf(&replace, "%s\t%s\t%s\n", e.service, e.target, name)
		}
	}
	write(filepath.Join(s.dir, "replace"), replace.String())
	write(filepath.Join(s.dir, "delete"), del.String())
	if replace.Len()+del.Len() > 0 {
		s.seal(d)
	}
	return s
}

// syncKey identifies what edits give setup: each rule's service and
// target, the paths, types, modes, link targets and contents of what it
// syncs, and what it deletes.
func syncKey(edits []edit) string {
	h := sha256.New()
	for _, e := range edits {
		fmt.Fprintf(h, "%s %s %q\n", e.service, e.target, e.deleted)
		check(bundle.Walk(e.file, e.skip, func(p string, fi fs.FileInfo) {
			rel, _ := filepath.Rel(e.file, p)
			l, _ := os.Readlink(p)
			if fi.Mode().IsRegular() {
				l = sum(p)
			}
			fmt.Fprintf(h, "%q %v %s\n", rel, fi.Mode(), l)
		}))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// sum is file p's SHA-256.
func sum(p string) string {
	f, err := os.Open(p)
	check(err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	check(err)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// copyEntry copies p, a directory (not what's in it), a symlink or a file, to
// to, with its permissions.
func copyEntry(p string, fi fs.FileInfo, to string) {
	switch {
	case fi.IsDir():
		check(os.Mkdir(to, 0o700))
		check(os.Chmod(to, fi.Mode().Perm()|0o700)) // (and writable, to fill; Chmod: whatever the umask)
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
		_, err = io.Copy(out, in)                                            // (copy_file_range on Linux: a reflink where it can)
		check(errors.Join(err, out.Close(), os.Chmod(to, fi.Mode().Perm()))) // (whatever the umask)
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
