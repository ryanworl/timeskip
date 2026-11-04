//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// The host's edits arrive in /inputs: a virtiofs share (macOS), or an ext4
// image the host swaps in for the third drive (Firecracker) before GO.
//
//	replace     "service<TAB>target<TAB>name<TAB>action" per line: the
//	            file or directory /inputs/<name> belongs at target in
//	            service's containers, as a develop.watch rule with action
//	            sync or sync+restart does
//	delete      "service<TAB>target<TAB>name<TAB>action" per line: target
//	            was deleted on the host, as part of the edit named name
//	            (replace's, if it has a file)
type edit struct {
	service, target, name, action string
	del                           bool
}

func mountInputs(spec string) {
	switch {
	case spec == "virtiofs":
		check(syscall.Mount("inputs", "/inputs", "virtiofs", syscall.MS_RDONLY, ""))
	case strings.HasPrefix(spec, "blk:"):
		waitDrive("vdc", spec)
		exec.Command("blockdev", "--flushbufs", "/dev/vdc").Run()
		check(syscall.Mount("/dev/vdc", "/inputs", "ext4", syscall.MS_RDONLY, ""))
	}
}

func readEdits() []edit {
	var e []edit
	for _, f := range []string{"replace", "delete"} {
		b, err := os.ReadFile("/inputs/" + f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			x := strings.Split(l, "\t")
			if len(x) == 4 {
				e = append(e, edit{x[0], x[1], x[2], x[3], f == "delete"})
			}
		}
	}
	return e
}

// apply applies edits as docker compose watch applies develop.watch sync
// rules: what differs from a container's files (contents, type, mode or
// link target) is copied in, with the host's owners; what was deleted on the
// host is deleted. Then the services with a changed sync+restart rule
// restart (and, as compose does, their dependents that set depends_on
// restart: true), until ctx is done.
func apply(ctx context.Context, edits []edit) {
	var restart []string
	for _, e := range edits {
		cs, err := containers(e.service)
		check(err)
		if len(cs) == 0 {
			fail("can't apply the edit to %s:%s: %s has no container", e.service, e.target, e.service)
		}
		changed := false
		for _, c := range cs {
			// (The target itself as it is, a symlink included, as compose
			// watch replaces or deletes it.)
			dst := filepath.Join(resolve(c.root(), filepath.Dir(e.target)), filepath.Base(e.target))
			if e.del {
				if _, err := os.Lstat(dst); err == nil {
					check(os.RemoveAll(dst))
					changed = true
				}
				continue
			}
			src := "/inputs/" + e.name
			if !differs(src, dst) {
				continue
			}
			checkReplacement(e, src, dst)
			var buf bytes.Buffer
			check(tarOf(src, filepath.Base(e.target), &buf))
			if err := untar(c.ID, filepath.Dir(e.target), &buf); err != nil {
				fail("copying the edit to %s:%s: %v", e.service, e.target, err)
			}
			changed = true
		}
		if changed && e.action == "sync+restart" {
			restart = append(restart, e.service)
		}
	}
	slices.Sort(restart)
	if restart = slices.Compact(restart); len(restart) > 0 {
		check(composeCtx(ctx, append([]string{"restart"}, restart...)...))
		say("restarted %s", strings.Join(restart, " "))
		check(started())
	}
}

// differs reports whether src (a file or directory) isn't the same at dst.
func differs(src, dst string) bool {
	d := false
	filepath.WalkDir(src, func(p string, _ fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(src, p)
		a, err1 := os.Lstat(p)
		b, err2 := os.Lstat(filepath.Join(dst, rel))
		switch {
		case err != nil || err1 != nil || err2 != nil, a.Mode() != b.Mode():
			d = true
		case a.Mode()&fs.ModeSymlink != 0:
			la, _ := os.Readlink(p)
			lb, _ := os.Readlink(filepath.Join(dst, rel))
			d = la != lb
		case a.Mode().IsRegular():
			d = a.Size() != b.Size() || !sameContents(p, filepath.Join(dst, rel))
		}
		if d {
			return filepath.SkipAll
		}
		return nil
	})
	return d
}

func sameContents(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// tarOf archives src as name (a file, or a directory and its contents).
func tarOf(src, name string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(src, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		h.Name = filepath.Join(name, rel)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// checkReplacement fails, before anything restarts, if a file can't stand
// in for the one at dst: a binary for another OS or architecture, or not
// executable where the original is.
func checkReplacement(e edit, src, dst string) {
	fi, err := os.Stat(src)
	if err != nil || fi.IsDir() {
		return
	}
	f, err := os.Open(src)
	check(err)
	head := make([]byte, 20)
	io.ReadFull(f, head)
	f.Close()
	machine := map[string]uint16{"amd64": 0x3e, "arm64": 0xb7}[runtime.GOARCH]
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")) && uint16(head[18])|uint16(head[19])<<8 != machine:
		fail("the replacement for %s:%s is a Linux binary for another architecture than %s", e.service, e.target, runtime.GOARCH)
	case bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}):
		fail("the replacement for %s:%s is a macOS binary; build it for Linux", e.service, e.target)
	}
	if old, err := os.Stat(dst); err == nil && old.Mode()&0o111 != 0 && fi.Mode()&0o111 == 0 {
		fail("the replacement for %s:%s isn't executable, and the file it replaces is", e.service, e.target)
	}
}
