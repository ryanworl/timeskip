//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// The sync rules' files arrive in /inputs for setup: on the bundle's
// virtiofs share (macOS), or an ext4 image the host gives the VM as its
// third drive (Firecracker).
//
//	replace     "service<TAB>target<TAB>name" per line: the file or
//	            directory /inputs/<name> belongs at target in service's
//	            containers, as a develop.watch rule with action sync or
//	            sync+restart puts it
//	delete      "service<TAB>target" per line: target was deleted on the
//	            host since service's image was built
type edit struct {
	service, target, name string
	del                   bool
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
			if x := append(strings.Split(l, "\t"), ""); len(x) >= 3 {
				e = append(e, edit{x[0], x[1], x[2], f == "delete"})
			}
		}
	}
	return e
}

// apply applies the edits to the project's containers, created and not yet
// started, so that they start with the host's files as if their images
// held them, as docker compose watch syncs develop.watch sync rules: what
// differs from a container's files (contents, type, mode or link target)
// is copied in, with the host's owners; what was deleted on the host is
// deleted.
func apply(edits []edit) {
	for _, e := range edits {
		cs, err := containers(e.service)
		check(err)
		if len(cs) == 0 {
			fail("can't apply the edit to %s:%s: %s has no container", e.service, e.target, e.service)
		}
		for _, c := range cs {
			root := mountRoot(c)
			// (The target itself as it is, a symlink included, as compose
			// watch replaces or deletes it.)
			dst := filepath.Join(resolve(root, filepath.Dir(e.target)), filepath.Base(e.target))
			src, put := "/inputs/"+e.name, false
			if e.del {
				check(os.RemoveAll(dst))
			} else if put = differs(src, dst); put {
				checkReplacement(e, src, dst)
			}
			check(syscall.Unmount(root, 0))
			if put { // (through Docker, which mounts the container's files itself)
				var buf bytes.Buffer
				check(tarOf(src, filepath.Base(e.target), &buf))
				if err := untar(c.ID, filepath.Dir(e.target), &buf); err != nil {
					fail("copying the edit to %s:%s: %v", e.service, e.target, err)
				}
			}
		}
	}
}

// mountRoot mounts container c's files, as Docker's overlay2 driver does
// when it starts it, at a directory of its own, and returns the directory.
func mountRoot(c container) string {
	layer := filepath.Dir(c.GraphDriver.Data["UpperDir"])
	lower, err := os.ReadFile(filepath.Join(layer, "lower"))
	check(err)
	dir := "/run/timeskip-root"
	check(os.MkdirAll(dir, 0o700))
	// (Its layers' short names, relative to overlay2's directory, as Docker
	// gives them: a mount's options have to fit in a page.)
	id := filepath.Base(layer)
	cmd := exec.Command("mount", "-t", "overlay", "overlay", "-o",
		fmt.Sprintf("index=off,lowerdir=%s,upperdir=%s/diff,workdir=%s/work", strings.TrimSpace(string(lower)), id, id), dir)
	cmd.Dir = filepath.Dir(layer)
	if out, err := cmd.CombinedOutput(); err != nil {
		fail("mounting %s's files: %v: %s", c.Name, err, out)
	}
	return dir
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
			x, err1 := os.ReadFile(p)
			y, err2 := os.ReadFile(filepath.Join(dst, rel))
			d = err1 != nil || err2 != nil || !bytes.Equal(x, y)
		}
		if d {
			return filepath.SkipAll
		}
		return nil
	})
	return d
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
