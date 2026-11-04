// Package archive writes reproducible tar archives: entries sorted by name,
// owned by root and stamped with one fixed time.
package archive

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// walk calls fn for every path under root except root itself, in lexical
// order, with its path relative to root.
func walk(root string, fn func(rel string, fi fs.FileInfo) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		return fn(filepath.ToSlash(rel), fi)
	})
}

// Tar writes the tree under root to w.
func Tar(w io.Writer, root string, mtime time.Time) error {
	tw := tar.NewWriter(w)
	err := walk(root, func(rel string, fi fs.FileInfo) error {
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			var err error
			if link, err = os.Readlink(filepath.Join(root, rel)); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name, h.ModTime, h.Format = rel, mtime, tar.FormatGNU
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
		if fi.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			return copyFile(tw, filepath.Join(root, rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func copyFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
