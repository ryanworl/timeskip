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

// Tar writes the tree under root to w.
func Tar(w io.Writer, root string, mtime time.Time) error {
	tw := tar.NewWriter(w)
	// (WalkDir goes in lexical order.)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name, h.ModTime, h.Format = filepath.ToSlash(rel), mtime, tar.FormatGNU
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
		if fi.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
