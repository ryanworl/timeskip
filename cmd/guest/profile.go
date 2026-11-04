//go:build linux

package main

import (
	"bufio"
	"cmp"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// A profiler samples the stacks of the containers' threads as they use the
// CPU, as perf record -G would: a perf event of the cpu-clock, every 10 ms
// of CPU time, on the cgroup Docker puts the containers in (and so their
// descendants: containers started again or by a test too). The kernel
// schedules such an event, and its timer, only while one of those threads
// runs, so it never wakes an idle CPU: idle time is still skipped. The
// kernel unwinds user stacks by frame pointers, so code built without them
// shows its innermost function only. Each sample is symbolized as it's
// read, while its process still runs, and counted by stack.
type profiler struct {
	fd, wake int // the event, and a pipe's write end that stops the reader
	ring     []byte
	tail     uint64
	done     chan struct{}
	stacks   map[string]int64 // folded: container;comm;outermost;...;innermost
	lost     uint64
	kernel   []symbol
	elfs     map[string][]symbol // by device and inode
	names    map[string]string   // containers' names, by ID
}

// A symbol is a function: where it starts, its size if known, and its name.
type symbol struct {
	addr, size uint64
	name       string
}

const (
	profilePeriod = 10_000_000 // ns of CPU time between samples
	ringPages     = 64
)

// profile starts a profiler; its stop returns the folded stacks.
func profile() (*profiler, error) {
	cg, err := os.Open("/sys/fs/cgroup/docker")
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	// struct perf_event_attr (PERF_ATTR_SIZE_VER5): a software cpu-clock
	// event, sampling every profilePeriod ns its thread's IDs and call
	// chain, disabled until enabled, waking the reader every 8 samples.
	var attr [112]byte
	le := binary.LittleEndian
	le.PutUint32(attr[0:], 1)                 // type: PERF_TYPE_SOFTWARE
	le.PutUint32(attr[4:], uint32(len(attr))) // size
	le.PutUint64(attr[8:], 0)                 // config: PERF_COUNT_SW_CPU_CLOCK
	le.PutUint64(attr[16:], profilePeriod)    // sample_period
	le.PutUint64(attr[24:], 1<<1|1<<5)        // sample_type: TID, CALLCHAIN
	le.PutUint64(attr[40:], 1)                // flags: disabled
	le.PutUint32(attr[48:], 8)                // wakeup_events
	const flags = 1<<2 | 1<<3                 // PERF_FLAG_PID_CGROUP, PERF_FLAG_FD_CLOEXEC
	fd, _, errno := syscall.Syscall6(syscall.SYS_PERF_EVENT_OPEN, uintptr(unsafe.Pointer(&attr[0])), cg.Fd(), 0, ^uintptr(0), flags, 0)
	if errno != 0 {
		return nil, fmt.Errorf("perf_event_open: %v", errno)
	}
	p := &profiler{fd: int(fd), done: make(chan struct{}), stacks: map[string]int64{}, elfs: map[string][]symbol{}, names: map[string]string{}}
	page := os.Getpagesize()
	if p.ring, err = syscall.Mmap(p.fd, 0, (1+ringPages)*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); err != nil {
		syscall.Close(p.fd)
		return nil, err
	}
	var pipe [2]int
	if err := syscall.Pipe2(pipe[:], syscall.O_CLOEXEC); err != nil {
		syscall.Munmap(p.ring)
		syscall.Close(p.fd)
		return nil, err
	}
	p.wake = pipe[1]
	p.kernel = kallsyms()
	go p.read(pipe[0])
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, 0x2400, 0); errno != 0 { // PERF_EVENT_IOC_ENABLE
		p.stop()
		return nil, fmt.Errorf("enabling the perf event: %v", errno)
	}
	return p, nil
}

// stop stops sampling and returns the stacks and how many samples the
// kernel dropped (the reader lagging behind).
func (p *profiler) stop() (map[string]int64, uint64) {
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(p.fd), 0x2401, 0) // PERF_EVENT_IOC_DISABLE
	syscall.Write(p.wake, []byte{0})
	<-p.done
	syscall.Close(p.wake)
	syscall.Munmap(p.ring)
	syscall.Close(p.fd)
	return p.stacks, p.lost
}

// read takes the samples from the ring buffer as the kernel wakes it, until
// stop writes to the pipe it reads from (stop).
func (p *profiler) read(stop int) {
	defer close(p.done)
	defer syscall.Close(stop)
	for {
		var r syscall.FdSet
		r.Bits[p.fd/64] |= 1 << (p.fd % 64)
		r.Bits[stop/64] |= 1 << (stop % 64)
		_, err := syscall.Select(max(p.fd, stop)+1, &r, nil, nil, nil)
		if err != nil && err != syscall.EINTR {
			return
		}
		p.drain()
		if r.Bits[stop/64]&(1<<(stop%64)) != 0 {
			return
		}
	}
}

// drain reads the records the kernel wrote since the last drain: struct
// perf_event_mmap_page's data_head and data_tail at 1024 and 1032, and the
// records after the first page.
func (p *profiler) drain() {
	page := uint64(os.Getpagesize())
	head := atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.ring[1024])))
	data, size := p.ring[page:], uint64(ringPages)*page
	le := binary.LittleEndian
	var procs map[uint32]*process // (this drain's: a process may exec or exit before the next)
	for p.tail < head {
		at := p.tail % size
		hdr := make([]byte, 8)
		copyRing(hdr, data, at)
		typ, n := le.Uint32(hdr), uint64(le.Uint16(hdr[6:]))
		rec := make([]byte, n)
		copyRing(rec, data, at)
		p.tail += n
		switch typ {
		case 9: // PERF_RECORD_SAMPLE: pid, tid, nr, ips[nr]
			if len(rec) < 24 {
				continue
			}
			pid, nr := le.Uint32(rec[8:]), le.Uint64(rec[16:])
			ips := make([]uint64, 0, nr)
			for i := uint64(0); i < nr && 24+8*i+8 <= uint64(len(rec)); i++ {
				ips = append(ips, le.Uint64(rec[24+8*i:]))
			}
			if procs == nil {
				procs = map[uint32]*process{}
			}
			if procs[pid] == nil {
				procs[pid] = p.process(pid)
			}
			p.stacks[p.fold(procs[pid], ips)]++
		case 2: // PERF_RECORD_LOST: id, lost
			if len(rec) >= 24 {
				p.lost += le.Uint64(rec[16:])
			}
		}
	}
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.ring[1032])), p.tail)
}

// copyRing copies len(dst) bytes of the ring buffer data from at, wrapping.
func copyRing(dst, data []byte, at uint64) {
	n := copy(dst, data[at:])
	copy(dst[n:], data)
}

// A process is what symbolizing its samples needs: its container, command
// name and memory mappings.
type process struct {
	pid             uint32
	container, comm string
	maps            []mapping
}

// A mapping is a file mapped in a process: its addresses, the file offset
// they start at, and the file (its path, and its device and inode).
type mapping struct {
	start, end, off uint64
	path, file      string
}

func (p *profiler) process(pid uint32) *process {
	pr := &process{pid: pid, container: "?", comm: "[exited]"}
	dir := fmt.Sprintf("/proc/%d/", pid)
	if b, err := os.ReadFile(dir + "comm"); err == nil {
		pr.comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(dir + "cgroup"); err == nil {
		id := filepath.Base(strings.TrimSpace(string(b)))
		if p.names[id] == "" {
			if cs, err := containers(""); err == nil {
				for _, c := range cs {
					p.names[c.ID] = c.Name
				}
			}
		}
		if n := p.names[id]; n != "" {
			pr.container = n
		}
	}
	f, err := os.Open(dir + "maps")
	if err != nil {
		return pr
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		// start-end perms offset dev inode path
		fs := strings.Fields(s.Text())
		if len(fs) < 6 || !strings.Contains(fs[1], "x") {
			continue
		}
		start, end, _ := strings.Cut(fs[0], "-")
		var m mapping
		m.start, _ = strconv.ParseUint(start, 16, 64)
		m.end, _ = strconv.ParseUint(end, 16, 64)
		m.off, _ = strconv.ParseUint(fs[2], 16, 64)
		m.path, m.file = fs[5], fs[3]+":"+fs[4]
		pr.maps = append(pr.maps, m)
	}
	return pr
}

// fold is a sample's stack, outermost frame first, after its container's
// and command's names. A call chain is innermost first: kernel frames, then
// user ones, each after a context marker.
func (p *profiler) fold(pr *process, ips []uint64) string {
	const contextMax, contextKernel = ^uint64(4095 - 1), ^uint64(128 - 1) // PERF_CONTEXT_MAX, PERF_CONTEXT_KERNEL
	var frames []string
	kernel, leaf := false, true
	for _, ip := range ips {
		if ip >= contextMax {
			kernel, leaf = ip == contextKernel, true
			continue
		}
		if !leaf {
			ip-- // (a return address: the call is before it)
		}
		leaf = false
		if kernel {
			frames = append(frames, lookup(p.kernel, ip, "[kernel]")+"_[k]")
		} else {
			frames = append(frames, p.user(pr, ip))
		}
	}
	slices.Reverse(frames)
	return strings.Join(append([]string{pr.container, pr.comm}, frames...), ";")
}

// user names the function at ip in a process.
func (p *profiler) user(pr *process, ip uint64) string {
	i := slices.IndexFunc(pr.maps, func(m mapping) bool { return m.start <= ip && ip < m.end })
	if i < 0 {
		return "[unknown]"
	}
	m := pr.maps[i]
	if !strings.HasPrefix(m.path, "/") {
		return m.path // [vdso] and the like
	}
	off := ip - m.start + m.off
	syms, ok := p.elfs[m.file]
	if !ok {
		syms = elfSymbols(fmt.Sprintf("/proc/%d/root%s", pr.pid, m.path))
		p.elfs[m.file] = syms
	}
	return lookup(syms, off, fmt.Sprintf("%s+0x%x", filepath.Base(m.path), off))
}

// lookup is the name of the function in syms (sorted) at addr, or else.
func lookup(syms []symbol, addr uint64, or string) string {
	i, _ := slices.BinarySearchFunc(syms, addr+1, func(s symbol, a uint64) int { return cmp.Compare(s.addr, a) })
	if i == 0 || syms[i-1].size > 0 && addr >= syms[i-1].addr+syms[i-1].size {
		return or
	}
	return syms[i-1].name
}

// elfSymbols are an ELF file's functions, by file offset (what a mapping's
// offset is in), sorted: its symbol table's, or else its dynamic symbols'.
func elfSymbols(path string) []symbol {
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil || len(syms) == 0 {
		syms, _ = f.DynamicSymbols()
	}
	var l []symbol
	for _, s := range syms {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC || s.Value == 0 {
			continue
		}
		for _, pr := range f.Progs {
			if pr.Type == elf.PT_LOAD && pr.Vaddr <= s.Value && s.Value < pr.Vaddr+pr.Memsz {
				l = append(l, symbol{s.Value - pr.Vaddr + pr.Off, s.Size, s.Name})
				break
			}
		}
	}
	slices.SortFunc(l, func(a, b symbol) int { return cmp.Compare(a.addr, b.addr) })
	return l
}

// kallsyms are the kernel's functions, sorted.
func kallsyms() []symbol {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return nil
	}
	defer f.Close()
	var l []symbol
	s := bufio.NewScanner(f)
	for s.Scan() {
		fs := strings.Fields(s.Text())
		if len(fs) < 3 || (fs[1] != "t" && fs[1] != "T") {
			continue
		}
		a, err := strconv.ParseUint(fs[0], 16, 64)
		if err == nil && a != 0 {
			l = append(l, symbol{a, 0, fs[2]})
		}
	}
	slices.SortFunc(l, func(a, b symbol) int { return cmp.Compare(a.addr, b.addr) })
	return l
}
