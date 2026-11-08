package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// A vm is a VMM process (Firecracker, or hvrun on macOS), a child of this
// one: it dies with it. Its console goes to dir/console.log, and its
// control lines ("TS <nonce> VERB JSON") to ctl.
type vm struct {
	dir, nonce string
	sock       string // Firecracker's API socket
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	ctl        chan ctlLine
	saved      chan struct{} // hvrun saved the VM (macOS)
	gone       chan struct{} // closed once the VMM exited and was waited for
	logged     chan struct{} // closed once its console was read to the end
	stop       chan struct{} // closed by kill: no one reads ctl any more
	echo       io.Writer     // when set, the console is copied there too
	cut        chan struct{} // when closed, next returns CUT, as the run stops early (--max-failures)
	client     *http.Client
	killed     atomic.Bool // by kill
}

type ctlLine struct {
	verb string // EXITED when the VMM exited without a guest EXIT line; CUT (see vm.cut)
	body string
}

// disk is a VM disk: vda, vdb, ... in the order given.
type disk struct {
	path string
	ro   bool
}

// guest is the kit's kernel and the guest's root filesystem, a read-only
// image whose files the VM reads as it uses them (see rootfs in cmd/build).
func guest() (kernel, rootfs string) {
	if darwin {
		return filepath.Join(home, "share", "Image-arm64"), filepath.Join(home, "share", "rootfs-aarch64.erofs")
	}
	return filepath.Join(home, "share", "vmlinux-x86_64"), filepath.Join(home, "share", "rootfs-x86_64.erofs")
}

// slot waits for one of the host's VM slots, one per core (see cores),
// across every run on the host: a VM that waits for a core counts the wait
// as CPU time, and as guest time. It returns the slot's lock.
func slot() int {
	d := fmt.Sprintf("/tmp/timeskip-slots-%d", os.Getuid()) // (not $TMPDIR, which a process can change)
	check(os.MkdirAll(d, 0o700))
	for {
		for i := range cores() {
			if fd, ok := tryLock(filepath.Join(d, strconv.Itoa(i)), syscall.LOCK_EX); ok {
				return fd
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cmdline is the guest kernel's command line, with root its root disk.
func cmdline(nonce, root string) string {
	// (no-kvmapf: without KVM's async page faults, a page of guest memory
	// the host has to read in, from a checkpoint's memory file, stalls the
	// vCPU, as a slow memory access would. With them, the guest idles
	// meanwhile, and skips ahead through its timers: a heal that touched
	// cold memory took minutes of guest time on a host whose disk was busy.)
	c := "ttyS0 no-kvmapf"
	if darwin {
		c = "hvc0"
	}
	return fmt.Sprintf("console=%s root=%s ro rootfstype=erofs init=/init quiet loglevel=1 reboot=k panic=1 timeskip=1 ts.nonce=%s%s", c, root, nonce, slackArg())
}

// defaultSlackNS is the guest's timer slack unless x-timeskip's timer_slack
// sets it (see guest/init).
const defaultSlackNS = 1000000

// slackArg is the guest kernel's command-line argument that sets its timer
// slack, which init applies before anything else starts, or "" for the
// default. It's in the names of the checkpoints a boot made too, so those
// of the default keep theirs.
func slackArg() string {
	if slackNS == defaultSlackNS {
		return ""
	}
	return fmt.Sprintf(" ts.slack=%d", slackNS)
}

// start starts a VMM. Booting, it gets disks, then the root filesystem
// (the last disk), and on macOS share (the inputs directory); restoring
// from state (and on Linux mem), the VM gets data as its data disk and, on
// Linux, img (or c's placeholder) as the inputs drive, and with track,
// Firecracker tracks the pages it writes (for a save over a copy of mem;
// see save).
func start(dir, nonce string, disks []disk, share string, from *checkpoint, data, img string, track bool) *vm {
	slot := slot() // (first: sweep takes a socket's directory a minute old with no socket for a dead run's)
	check(os.MkdirAll(dir, 0o755))
	v := &vm{dir: dir, nonce: nonce, ctl: make(chan ctlLine, 16), saved: make(chan struct{}, 4), gone: make(chan struct{}), logged: make(chan struct{}), stop: make(chan struct{})}
	k, rootfs := guest()
	if darwin && from != nil {
		disks = []disk{{from.bundle(), true}, {data, false}}
	}
	disks = append(disks, disk{rootfs, true})
	root := fmt.Sprintf("/dev/vd%c", 'a'+len(disks)-1)
	if darwin {
		args := []string{"--mem-mb", strconv.Itoa(memMB), "--kernel", k, "--cmdline", cmdline(nonce, root)}
		if from != nil {
			args = append(args, "--restore", from.file("state"))
		}
		for _, x := range disks {
			args = append(args, map[bool]string{true: "--disk-ro", false: "--disk"}[x.ro], x.path)
		}
		v.cmd = exec.Command(filepath.Join(home, "bin", "hvrun"), append(args, "--share", "inputs="+share)...)
	} else {
		// (In a short directory of its own: a socket's path has a length
		// limit, which one in the cache can exceed.)
		d, err := os.MkdirTemp("", "timeskip-")
		check(err)
		v.sock = filepath.Join(d, "api.sock")
		args := []string{"--api-sock", v.sock, "--level", "Error", "--no-seccomp"}
		if from == nil {
			var drives []any
			for i, x := range disks {
				drives = append(drives, map[string]any{"drive_id": fmt.Sprint("d", i), "path_on_host": x.path, "is_root_device": false, "is_read_only": x.ro})
			}
			b, _ := json.Marshal(map[string]any{
				"boot-source":    map[string]any{"kernel_image_path": k, "boot_args": cmdline(nonce, root)},
				"machine-config": machine(),
				"drives":         drives,
			})
			write(v.file("vm.json"), string(b))
			args = append(args, "--config-file", v.file("vm.json"))
		}
		v.cmd = exec.Command(filepath.Join(home, "bin", "firecracker"), args...)
	}
	v.cmd.SysProcAttr = childAttr()
	// (The VMM is reaped whatever its console's readers do: a console
	// echo blocked on stdout mustn't keep kill waiting.)
	var err error
	v.stdin, err = v.cmd.StdinPipe()
	check(err)
	out, outW, err := os.Pipe()
	check(err)
	errs, errsW, err := os.Pipe()
	check(err)
	v.cmd.Stdout, v.cmd.Stderr = outW, errsW
	log, err := os.Create(v.file("console.log"))
	check(err)
	if from != nil { // (a branch point's: the timeline's log until then)
		if b, err := os.ReadFile(from.file("console.log")); err == nil {
			log.Write(b)
		}
	}
	check(v.cmd.Start())
	outW.Close()
	errsW.Close()
	go func() {
		v.cmd.Wait()
		syscall.Close(slot)
		close(v.gone)
	}()
	atExit(v.reap) // (after the above: once exit began, it reaps at once)
	errsRead := make(chan struct{})
	go func() {
		defer close(errsRead)
		defer errs.Close()
		s := bufio.NewScanner(errs)
		for s.Scan() {
			if l := s.Text(); l == "hvrun: saved" {
				v.saved <- struct{}{}
			} else {
				fmt.Fprintln(log, l)
			}
		}
	}()
	go func() {
		r := bufio.NewReaderSize(out, 1<<16)
		for {
			l, err := r.ReadString('\n')
			if l != "" {
				log.WriteString(l)
				verb, body, ok := protocol.Parse(l, nonce)
				if v.echo != nil && !ok {
					v.echo.Write([]byte(l))
				}
				if ok {
					select {
					case v.ctl <- ctlLine{verb, body}:
					case <-v.stop:
					}
				}
			}
			if err != nil {
				out.Close()
				<-errsRead
				log.Close()
				close(v.logged)
				<-v.gone
				select {
				case v.ctl <- ctlLine{verb: "EXITED"}:
				case <-v.stop:
				}
				return
			}
		}
	}()
	if from != nil && !darwin {
		// (Until Firecracker listens: its socket's file comes a moment before.)
		for t := time.Now(); ; time.Sleep(time.Millisecond) {
			if c, err := net.Dial("unix", v.sock); err == nil {
				c.Close()
				break
			}
			if time.Since(t) > 10*time.Second {
				showTail(v.file("console.log"), 20)
				die("firecracker didn't start")
			}
		}
		check(v.api("PUT", "/snapshot/load", map[string]any{"snapshot_path": from.file("state"),
			"mem_backend": map[string]string{"backend_type": "File", "backend_path": from.file("mem")}, "track_dirty_pages": track, "resume_vm": false, "huge_pages": "None"}))
		v.drives(data, cmp.Or(img, from.placeholder()))
		check(v.api("PATCH", "/vm", map[string]string{"state": "Resumed"}))
	}
	return v
}

// machine is a booting Firecracker VM's machine configuration. Its memory
// gets transparent huge pages where the host has them: a new VM touches
// hundreds of megabytes (the kernel's page structures, then what it reads),
// and 2 MiB host pages make that a 512th of the page faults. (A restored
// VM maps its memory file in 4 KiB pages, copy-on-write.)
func machine() map[string]any {
	m := map[string]any{"vcpu_count": 1, "mem_size_mib": memMB}
	if exists("/sys/kernel/mm/transparent_hugepage") {
		m["huge_pages"] = "Transparent"
	}
	return m
}

func (v *vm) file(name string) string { return filepath.Join(v.dir, name) }

// next is the VM's next control line. A VMM that was killed (say, by the
// OOM killer) or failed, rather than exiting as the guest rebooted or
// powered off, is a harness error. Once exit began, a VMM that exits was
// killed for it, and next never returns.
func (v *vm) next() ctlLine {
	var l ctlLine
	select {
	case l = <-v.ctl:
	case <-v.cut:
		return ctlLine{verb: "CUT"}
	}
	if l.verb == "EXITED" && stopping() {
		select {}
	}
	if l.verb == "EXITED" && !v.killed.Load() && !v.cmd.ProcessState.Success() {
		showTail(v.file("console.log"), 20)
		die("the VMM stopped (%s)", v.cmd.ProcessState)
	}
	return l
}

// send writes a control line to the VM's console.
func (v *vm) send(verb, fields string) {
	io.WriteString(v.stdin, protocol.Line(v.nonce, verb, fields))
}

// save pauses the VM and saves it to dir: state (and on Linux mem), and
// disk, a copy of its data disk, data, taken while it's paused. The saved
// VM refers to dir/disk, and on Linux to placeholder for the inputs drive
// (the running VM has img, if any). On Linux, a booted VM saves the memory
// it has touched to dir/mem; a restored one (tracking its writes) saves only
// the pages written since its restore (or its last save), over dir/mem, a
// copy of the memory file it was restored from (or last saved); or with
// diff, to dir/mem.diff, for the caller to put over such a copy (see
// explorer.save).
func (v *vm) save(dir, data, img, placeholder string, diff bool) {
	disk := filepath.Join(dir, "disk")
	if darwin {
		fmt.Fprintf(v.stdin, "@save %s\n", filepath.Join(dir, "state"))
		select {
		case <-v.saved:
		case l := <-v.ctl:
			showTail(v.file("console.log"), 20)
			die("the VM stopped while being saved (%s)", l.verb)
		}
		if data != disk {
			clone(data, disk)
		}
		return
	}
	// (A diff snapshot without dirty-page tracking, of a booted VM, saves
	// the pages it touched: the rest is zero, a hole in the file.)
	check(v.api("PATCH", "/vm", map[string]string{"state": "Paused"}))
	mem := filepath.Join(dir, "mem")
	if diff {
		mem += ".diff"
	}
	if data != disk {
		clone(data, disk)
		go writeOut(disk)
		v.drives(disk, placeholder)
		defer v.drives(data, cmp.Or(img, placeholder))
	}
	// (Unsynced: a crash leaves a checkpoint without its "done" file, which
	// is then made again. publish syncs it.)
	check(v.api("PUT", "/snapshot/create", map[string]any{"snapshot_type": "Diff", "sync_snapshot_files": false,
		"snapshot_path": filepath.Join(dir, "state"), "mem_file_path": mem}))
}

// writeOut writes a file's data out to the disk, so that publish, which
// syncs it, needn't wait for it all (a copied data disk can be gigabytes).
func writeOut(path string) {
	if f, err := os.Open(path); err == nil {
		f.Sync()
		f.Close()
	}
}

func (v *vm) resume() {
	if darwin {
		fmt.Fprintln(v.stdin, "@resume")
		return
	}
	check(v.api("PATCH", "/vm", map[string]string{"state": "Resumed"}))
}

// drives points a Firecracker VM's data (d1) and inputs (d2) drives at files.
func (v *vm) drives(data, inputs string) {
	check(v.api("PATCH", "/drives/d1", map[string]string{"drive_id": "d1", "path_on_host": data}))
	check(v.api("PATCH", "/drives/d2", map[string]string{"drive_id": "d2", "path_on_host": inputs}))
}

// kill kills the VMM and waits for it, and for its console to be logged.
func (v *vm) kill() {
	v.reap()
	<-v.logged
}

// reap kills the VMM and waits for it, so that its CPU time counts as this
// process's children's (as time(1) reports it), and removes its socket's
// directory. (At exit it doesn't wait for the console's readers.)
func (v *vm) reap() {
	if !v.killed.Swap(true) {
		close(v.stop)
	}
	v.cmd.Process.Kill()
	<-v.gone
	if v.sock != "" {
		os.RemoveAll(filepath.Dir(v.sock))
	}
}

// api makes a Firecracker API call.
func (v *vm) api(method, path string, body any) error {
	if v.client == nil { // (one connection: Firecracker takes few)
		v.client = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{MaxConnsPerHost: 1,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", v.sock)
			}}}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
