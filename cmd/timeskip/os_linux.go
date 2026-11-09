package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const seekData, seekHole = 3, 4

// cores is the number of CPU cores: CPUs less hardware threads that share a
// core (x86_64's SMT has two a core). A VM on one core's thread takes up to
// twice the CPU time for the same work as one with the core to itself.
func cores() int {
	if b, _ := os.ReadFile("/sys/devices/system/cpu/smt/active"); strings.TrimSpace(string(b)) == "1" {
		return max(1, runtime.NumCPU()/2)
	}
	return runtime.NumCPU()
}

// clone copies file src to dst: a copy-on-write clone where the filesystem
// has them (XFS, btrfs), else its data, in the kernel (copy_file_range, or
// across file systems sendfile), its holes left holes.
func clone(src, dst string) {
	s, err := os.Open(src)
	check(err)
	defer s.Close()
	fi, err := s.Stat()
	check(err)
	os.Remove(dst) // (not through a link of another file's)
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	check(err)
	defer func() { check(d.Close()) }()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), 0x40049409, s.Fd()); e == 0 { // FICLONE
		return
	}
	check(d.Truncate(fi.Size()))
	copyData(d, s)
}

// overlay writes file src's data, not its holes, over dst's at the same
// offsets.
func overlay(dst, src string) {
	d, err := os.OpenFile(dst, os.O_WRONLY, 0)
	check(err)
	defer func() { check(d.Close()) }()
	s, err := os.Open(src)
	check(err)
	defer s.Close()
	copyData(d, s)
}

// copyData copies src's data, not its holes, to the same offsets in dst.
// (Go's copy_file_range falls back to reading and writing across file
// systems, as between /dev/shm and the cache: sendfile copies in the
// kernel.)
func copyData(dst, src *os.File) {
	var ds, ss syscall.Stat_t
	check(syscall.Fstat(int(dst.Fd()), &ds))
	check(syscall.Fstat(int(src.Fd()), &ss))
	for off := int64(0); ; {
		start, err := src.Seek(off, seekData)
		if err != nil { // (ENXIO: no data after off)
			return
		}
		end, err := src.Seek(start, seekHole)
		check(err)
		_, err = dst.Seek(start, io.SeekStart)
		check(err)
		if ds.Dev != ss.Dev {
			for o := start; o < end; {
				n, err := syscall.Sendfile(int(dst.Fd()), int(src.Fd()), &o, int(min(end-o, 1<<30)))
				check(err)
				if n == 0 {
					die("copying %s: it ended at %d", src.Name(), o)
				}
			}
		} else {
			_, err = src.Seek(start, io.SeekStart)
			check(err)
			_, err = io.CopyN(dst, src, end-start)
			check(err)
		}
		off = end
	}
}

// sparsify punches holes in a file where it's zero (in 4 KiB blocks), as
// in a saved VM's memory, where Firecracker writes every page the guest
// touched. It reads only the file's data, not its holes.
func sparsify(path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	fd := int(f.Fd())
	zero, buf := make([]byte, 4096), make([]byte, 1<<20)
	for off := int64(0); ; {
		start, err := f.Seek(off, seekData)
		if err != nil { // (ENXIO: no data after off)
			return
		}
		end, _ := f.Seek(start, seekHole)
		hole := int64(-1)
		punch := func(at int64) {
			if hole >= 0 {
				syscall.Fallocate(fd, 0x01|0x02, hole, at-hole) // KEEP_SIZE|PUNCH_HOLE
				hole = -1
			}
		}
		for at := start; at < end; {
			n, err := f.ReadAt(buf[:min(int64(len(buf)), end-at)], at)
			for i := 0; i+4096 <= n; i += 4096 {
				if bytes.Equal(buf[i:i+4096], zero) {
					if hole < 0 {
						hole = at + int64(i)
					}
				} else {
					punch(at + int64(i))
				}
			}
			at += int64(n)
			if err != nil || n == 0 {
				end = at
				break
			}
		}
		punch(end - end%4096)
		off = end
	}
}

// memRoot is /dev/shm/timeskip-UID, a directory in memory (tmpfs) for
// branch points (see memDir).
func memRoot() string { return fmt.Sprintf("/dev/shm/timeskip-%d", os.Getuid()) }

// memDir is memRoot, if it has room for need bytes more and the host would
// still have a quarter of its memory available; or "". (On a disk branch
// points would be written out, the file system's dirty pages having
// reached the host's limit or their age, gigabytes a minute when
// exploring: the saves would wait for the disk.)
func memDir(need int64) string {
	var s syscall.Statfs_t
	if syscall.Statfs("/dev/shm", &s) != nil || s.Type != 0x01021994 || int64(s.Bavail)*s.Bsize < need { // TMPFS_MAGIC
		return ""
	}
	kb := map[string]int64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var n int64
		k, v, _ := strings.Cut(sc.Text(), ":")
		fmt.Sscan(v, &n)
		kb[k] = n
	}
	if (kb["MemAvailable"]-kb["MemTotal"]/4)<<10 < need {
		return ""
	}
	d := memRoot()
	if os.MkdirAll(d, 0o700) != nil {
		return ""
	}
	return d
}

// childAttr makes the VMM die with this process, and keeps it out of the
// terminal's process group: Ctrl-C is this process's to handle (see exit).
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

// osBuild: KVM's saved state doesn't depend on the host's kernel.
func osBuild() string { return "" }

// ownSize is the bytes of file f's that no clone shares (its extents
// without FIEMAP_EXTENT_SHARED: XFS's and btrfs's reflinks), or alloc, its
// allocated bytes, if they can't be read.
func ownSize(f string, alloc int64) int64 {
	file, err := os.Open(f)
	if err != nil {
		return alloc
	}
	defer file.Close()
	// struct fiemap, then fm_extent_count struct fiemap_extents.
	const n = 256
	buf := make([]uint64, (32+56*n)/8)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(buf)*8)
	var start, shared uint64
	for {
		clear(b)
		binary.LittleEndian.PutUint64(b[0:], start)
		binary.LittleEndian.PutUint64(b[8:], ^uint64(0)-start)
		binary.LittleEndian.PutUint32(b[24:], n)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), 0xc020660b, uintptr(unsafe.Pointer(&b[0]))); e != 0 { // FS_IOC_FIEMAP
			return alloc
		}
		mapped := int(binary.LittleEndian.Uint32(b[20:]))
		last := mapped == 0
		for i := range mapped {
			x := b[32+56*i:]
			length, flags := binary.LittleEndian.Uint64(x[16:]), binary.LittleEndian.Uint32(x[40:])
			if flags&0x2000 != 0 { // FIEMAP_EXTENT_SHARED
				shared += length
			}
			last = last || flags&1 != 0 // FIEMAP_EXTENT_LAST
			start = binary.LittleEndian.Uint64(x[0:]) + length
		}
		if last {
			return max(alloc-int64(shared), 0)
		}
	}
}
