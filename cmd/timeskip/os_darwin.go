package main

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// cores is the number of performance cores: those of every perflevel but
// the last, the efficiency cores, which take two or three times the CPU
// time for the same work (an M6 has two levels above them: 2 super and 4
// performance cores).
func cores() int {
	n := 0
	for i := range max(1, sysctlInt("hw.nperflevels")-1) {
		n += sysctlInt(fmt.Sprintf("hw.perflevel%d.physicalcpu", i))
	}
	if n == 0 {
		return runtime.NumCPU()
	}
	return n
}

// sysctlInt is a 32-bit sysctl's value, or 0.
func sysctlInt(name string) int {
	b, err := syscall.Sysctl(name)
	if err != nil || len(b) > 4 {
		return 0
	}
	return int(binary.LittleEndian.Uint32([]byte(b + "\x00\x00\x00\x00")))
}

// clone copies file src to dst: a copy-on-write clone (APFS).
func clone(src, dst string) {
	if out, err := exec.Command("cp", "-c", src, dst).CombinedOutput(); err != nil {
		die("copying %s: %v: %s", src, err, strings.TrimSpace(string(out)))
	}
}

// overlay is Linux's (see explorer.save).
func overlay(string, string) {}

// sparsify does nothing on macOS: hvrun writes only the pages that aren't
// zero.
func sparsify(string) {}

// memDir: branch points stay in the cache on macOS (see explorer.reserve).
func memDir(int64) string { return "" }

func memRoot() string { return "" }

// childAttr keeps hvrun out of the terminal's process group: Ctrl-C is
// this process's to handle (see exit). hvrun exits when its stdin, from
// this process, closes.
func childAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// osBuild is the macOS build, e.g. 25G332.
func osBuild() string {
	b, _ := syscall.Sysctl("kern.osversion")
	return b
}

// ownSize is the bytes of file f's that no clone shares (APFS's private
// size), or alloc, its allocated bytes, if that can't be read.
func ownSize(f string, alloc int64) int64 {
	// getattrlist(2) with FSOPT_ATTR_CMN_EXTENDED: ATTR_CMNEXT_PRIVATESIZE
	// (a fork attribute), after the returned length.
	a := struct {
		bitmapcount, reserved        uint16
		common, vol, dir, file, fork uint32
	}{bitmapcount: 5, fork: 0x8}
	var buf [16]byte
	p, err := syscall.BytePtrFromString(f)
	if err != nil {
		return alloc
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&a)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0x20|0x1, 0); e != 0 {
		return alloc
	}
	return int64(binary.LittleEndian.Uint64(buf[4:12]))
}
