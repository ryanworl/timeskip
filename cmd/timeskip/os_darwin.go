package main

import (
	"os/exec"
	"strings"
	"syscall"
)

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
