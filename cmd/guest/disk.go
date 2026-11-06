//go:build linux

package main

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Node disks. Each node (see node in faults.go) has a disk of its own, as a
// machine does: an ext4 filesystem (mounted as a host's, data=ordered and a
// commit every 5 s) on a loop device over a sparse file on Docker's disk.
// It holds its containers' own layers, the volumes that no other node
// mounts, and the project's files bind-mounted into its containers alone,
// each bound where Docker keeps it. (Volumes and project files that nodes
// share stay on Docker's disk, which no fault touches.) The loop device
// injects the disk faults (see timeskip_fail and timeskip_cut in
// guest/kernel/timeskip.patch); a container started by a test, or with no
// network of its own, has none.

// A disk is a node's disk.
type disk struct {
	name  string      // the node's owner's name
	dev   string      // its loop device, /dev/loopN
	dir   string      // where it's mounted
	binds [][2]string // bind mounts from it: from (in dir) and onto

	mu     sync.Mutex  // held while it changes: remounted, filled, its faults set
	faulty int         // disk faults in effect
	later  []container // containers that failed to start meanwhile, to start as they heal
	deaths []container // containers whose deaths a grace claimed meanwhile, ditto (see injector.claimedStart)
}

// disks are the node disks, by the ID of each container on them.
var disks = map[string]*disk{}

const disksDir = "/var/lib/docker/timeskip/disks"

// makeDisks gives each node of the project's containers, created and not
// started yet, a disk.
func makeDisks() error {
	cs, err := containers("")
	if err != nil {
		return err
	}
	byID := map[string]container{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	// The volumes and project files each node mounts (or container on the
	// VM's network: it has no disk).
	nodes := map[string][]container{}
	users := map[string]map[string]bool{} // mount key: the nodes that mount it
	for _, c := range cs {
		o, _ := netOwner(c, byID)
		if c.HostConfig.NetworkMode != "host" {
			nodes[o.ID] = append(nodes[o.ID], c)
		}
		for _, m := range c.Mounts {
			if users[m.key()] == nil {
				users[m.key()] = map[string]bool{}
			}
			users[m.key()][o.ID] = true
		}
	}
	if err := os.MkdirAll(disksDir, 0o700); err != nil {
		return err
	}
	moved := map[string]bool{}
	for id, nc := range nodes {
		d := &disk{name: byID[id].Name, dir: filepath.Join(disksDir, byID[id].Name)}
		if err := d.create(); err != nil {
			return fmt.Errorf("%s's disk: %v", d.name, err)
		}
		for _, c := range nc {
			var g struct {
				GraphDriver struct{ Data map[string]string }
			}
			if err := call("GET", "/containers/"+c.ID+"/json", nil, &g); err != nil {
				return err
			}
			if err := d.layer(c, filepath.Dir(g.GraphDriver.Data["UpperDir"])); err != nil {
				return fmt.Errorf("%s's layer: %v", c.Name, err)
			}
			for _, m := range c.Mounts {
				k := m.key()
				if k == "" || len(users[k]) > 1 || moved[k] {
					continue
				}
				moved[k] = true
				if err := d.mount(m); err != nil {
					return fmt.Errorf("%s's %s: %v", c.Name, m.Source, err)
				}
			}
			disks[c.ID] = d
		}
	}
	return nil
}

// key is what identifies mount m among the containers' mounts ("" for one
// that a node disk can't hold: a volume of a driver with options, as a tmpfs
// volume, a tmpfs, or a bind mount of other files than the project's).
func (m mount) key() string {
	switch {
	case m.Type == "volume" && m.Driver == "local":
		var v struct{ Options map[string]string }
		if call("GET", "/volumes/"+m.Name, nil, &v) == nil && len(v.Options) == 0 {
			return m.Source
		}
	case m.Type == "bind" && strings.HasPrefix(m.Source, protocol.BundleDir+"/"):
		return m.Source
	}
	return ""
}

// create makes, attaches and mounts d's filesystem.
func (d *disk) create() error {
	img := d.dir + ".img"
	f, err := os.OpenFile(img, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	// (Sparse, and a new file of zeros, so that only metadata is written,
	// as for Docker's disk in guest/init; no space held back for root, so
	// that a full disk is full for everyone.)
	if err := errors.Join(os.Truncate(img, 64<<30), run("mkfs.ext4", "-q", "-F", "-m", "0", "-E", "assume_storage_prezeroed=1,nodiscard", img)); err != nil {
		return err
	}
	if d.dev, err = attach(img); err != nil {
		return err
	}
	if err := errors.Join(os.Mkdir(d.dir, 0o700), d.mount0()); err != nil {
		return err
	}
	return os.Remove(d.dir + "/lost+found")
}

// mount0 mounts d's filesystem. (Not reading its block bitmaps in the
// background, as ext4 does after a mount: that's I/O and timer wake-ups
// in every timeline, and reads a failing disk would fail without the
// node's doing.)
func (d *disk) mount0() error {
	return syscall.Mount(d.dev, d.dir, "ext4", syscall.MS_NOATIME, "no_prefetch_block_bitmaps")
}

// attach attaches file to a free loop device and returns its name.
func attach(file string) (string, error) {
	const getFree, setFd, setDirectIO = 0x4C82, 0x4C00, 0x4C08 // LOOP_CTL_GET_FREE, LOOP_SET_FD, LOOP_SET_DIRECT_IO
	ctl, err := os.Open("/dev/loop-control")
	if err != nil {
		return "", err
	}
	defer ctl.Close()
	n, _, e := syscall.Syscall(syscall.SYS_IOCTL, ctl.Fd(), getFree, 0)
	if e != 0 {
		return "", e
	}
	dev := fmt.Sprintf("/dev/loop%d", n)
	l, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer l.Close()
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, l.Fd(), setFd, f.Fd()); e != 0 {
		return "", e
	}
	// (Direct I/O to the file: what's cached of the disk is cached once,
	// by its filesystem, not again in the file's.)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, l.Fd(), setDirectIO, 1); e != 0 {
		return "", fmt.Errorf("%s: direct I/O: %v", dev, e)
	}
	return dev, nil
}

// layer moves container c's own layer, its overlay upper and work
// directories in Docker's layer directory dir, to d: they're left there as
// links into dir/node, a mount of their new place. (Docker's files about
// the layer stay on Docker's disk, for it to read while d is down.)
func (d *disk) layer(c container, dir string) error {
	to := filepath.Join(d.dir, "layer-"+c.ID[:12])
	if err := os.Mkdir(to, 0o700); err != nil {
		return err
	}
	for _, n := range []string{"diff", "work"} {
		if err := errors.Join(run("cp", "-a", filepath.Join(dir, n), to), os.RemoveAll(filepath.Join(dir, n)), os.Symlink("node/"+n, filepath.Join(dir, n))); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "node"), 0o700); err != nil {
		return err
	}
	return d.bind(to, filepath.Join(dir, "node"))
}

// mount moves volume or project files m to d, bound where they were.
func (d *disk) mount(m mount) error {
	fi, err := os.Stat(m.Source)
	if err != nil {
		return nil // (a bind mount whose source Docker makes)
	}
	to := filepath.Join(d.dir, fmt.Sprintf("mount-%d", len(d.binds)))
	if fi.IsDir() {
		st := fi.Sys().(*syscall.Stat_t)
		err = errors.Join(os.Mkdir(to, 0o700), run("cp", "-a", m.Source+"/.", to), os.Chmod(to, fi.Mode()), os.Lchown(to, int(st.Uid), int(st.Gid)))
	} else {
		err = run("cp", "-a", m.Source, to)
	}
	if err != nil {
		return err
	}
	return d.bind(to, m.Source)
}

func (d *disk) bind(from, onto string) error {
	d.binds = append(d.binds, [2]string{from, onto})
	return syscall.Mount(from, onto, "", syscall.MS_BIND, "")
}

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sys reads or, with v, writes d's loop device's sysfs file timeskip_NAME.
func (d *disk) sys(name string, v ...string) (string, error) {
	f := "/sys/block/" + filepath.Base(d.dev) + "/loop/timeskip_" + name
	if len(v) > 0 {
		return "", os.WriteFile(f, []byte(v[0]), 0o644)
	}
	b, err := os.ReadFile(f)
	return strings.TrimSpace(string(b)), err
}

func (d *disk) failed() int64 {
	s, _ := d.sys("failed")
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// shutdown stops d's filesystem at once, as a power cut would: it writes
// nothing more, not even its journal (EXT4_IOC_SHUTDOWN, NOLOGFLUSH).
func (d *disk) shutdown() error {
	f, err := os.Open(d.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	flags := uint32(2) // EXT4_GOING_FLAGS_NOLOGFLUSH
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x8004587D, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return fmt.Errorf("%s's disk: shutdown: %v", d.name, e)
	}
	return nil
}

// setFail sets the parts per million of d's reads and writes to fail.
func (d *disk) setFail(ppm int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sys("fail", strconv.Itoa(ppm))
	return err
}

// remount unmounts d (its node's containers are down) and mounts it again,
// as its machine reboots, holding d.mu: what's in the kernel's caches of it
// is gone. With cut, its power was cut: the writes its cache held are lost
// (see timeskip_cut), and mounting replays its journal, which keeps what
// was committed. Its faults are off meanwhile: the disk boots.
func (d *disk) remount(cut bool) error {
	fail, err := d.sys("fail")
	if err == nil {
		err = d.sys0("fail", "0")
	}
	for i := len(d.binds) - 1; i >= 0 && err == nil; i-- {
		err = unmount(d.binds[i][1])
	}
	if err == nil {
		err = unmount(d.dir)
	}
	if err == nil {
		err = released(d.dev)
	}
	if err == nil && cut {
		err = d.sys0("cut", "1")
	}
	if err == nil {
		err = d.mount0()
	}
	for _, b := range d.binds {
		if err == nil {
			err = syscall.Mount(b[0], b[1], "", syscall.MS_BIND, "")
		}
	}
	if err == nil {
		err = d.sys0("fail", fail)
	}
	if err != nil {
		return fmt.Errorf("%s's disk: %v", d.name, err)
	}
	return nil
}

func (d *disk) sys0(name, v string) error { _, err := d.sys(name, v); return err }

// unmount unmounts path, waiting up to 10 s for what holds it (Docker
// unmounts a container's layer once it has seen it exit).
func unmount(path string) error {
	for t := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		err := syscall.Unmount(path, 0)
		if err != syscall.EBUSY || time.Since(t) > 10*time.Second {
			if err != nil {
				return fmt.Errorf("unmount %s: %v", path, err)
			}
			return nil
		}
	}
}

// released waits up to 10 s until no filesystem holds block device dev:
// unmounted, a filesystem lives on while a mount holds it, as Docker's
// overlay of a container's layer (a private clone of the mount beneath)
// until Docker sees the container exit, or a container's mount namespace
// until its last process is gone. (Opening a block device exclusively
// fails while it's mounted.)
func released(dev string) error {
	for t := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		fd, err := syscall.Open(dev, syscall.O_RDONLY|syscall.O_EXCL|syscall.O_CLOEXEC, 0)
		if err == nil {
			return syscall.Close(fd)
		}
		if err != syscall.EBUSY || time.Since(t) > 10*time.Second {
			return fmt.Errorf("%s: %v", dev, err)
		}
	}
}

// nodeDisks are the disks of the nodes of containers cs (once each).
func nodeDisks(cs []container) []*disk {
	var l []*disk
	for _, c := range cs {
		if d := disks[c.ID]; d != nil && !slices.Contains(l, d) {
			l = append(l, d)
		}
	}
	return l
}

// Disk faults (GO's disk=fail,full), as node faults on a schedule of their
// own, on the disks of the nodes not excluded from them: a failing disk
// (fail) or a full one (full). (Power losses, disk=power, are a kind of
// kill: see power.go.) A failing disk fails its data's reads and writes
// and its filesystem's metadata operations, not the filesystem's own I/O
// (see timeskip_fail in guest/kernel/timeskip.patch), so it stays whole.
// Its nodes whose reads or writes failed may be rebooted as it heals, so
// nodes excluded from kills don't get one.
func (in *injector) disk(rng *rand.Rand, _ func()) (*protocol.Fault, func() error, error) {
	kinds := slices.DeleteFunc(slices.Clone(in.diskKinds), func(k string) bool { return k == "power" })
	kind := kinds[0]
	if len(kinds) > 1 { // (a draw only then, so that other seeds' faults stay as they were)
		kind = kinds[rng.IntN(len(kinds))]
	}
	ex := []string{"disk"}
	if kind == "fail" {
		ex = append(ex, "kill")
	}
	scope, levels, err := in.pick(rng, ex...)
	if err != nil || len(levels) == 0 {
		return nil, nil, err
	}
	cs := slices.Concat(levels...)
	ds := nodeDisks(cs)
	if len(ds) == 0 {
		return nil, nil, nil
	}
	// (Its crashes come after seq, read before it begins, as a process may
	// die at once: see the grace below.)
	seq := in.r.crashSeq()
	n := exitCount()
	// (While it lasts, other faults' heals leave its containers that
	// can't start to it: see injector.restart.)
	for _, d := range ds {
		d.mu.Lock()
		d.faulty++
		d.mu.Unlock()
	}
	release := func() (later, deaths []container) {
		for _, d := range ds {
			d.mu.Lock()
			d.faulty--
			later, deaths = append(later, d.later...), append(deaths, d.deaths...)
			d.later, d.deaths = nil, nil
			d.mu.Unlock()
		}
		return later, deaths
	}
	f := &protocol.Fault{Kind: "disk", Targets: [][]string{names(cs)}}
	var lift func() error
	var hit func(d *disk) bool // whether d's reads or writes failed
	reboot := false            // whether its nodes so hit are rebooted as it heals
	if kind == "fail" {
		p := logUniformF(rng, 0.01, 1)
		// (Half the time its nodes are rebooted as it heals, for systems
		// that recover only by restarting; else they run on, for those
		// that recover by themselves.)
		reboot = rng.IntN(2) == 0
		lift = func() error {
			var errs []error
			for _, d := range ds {
				errs = append(errs, d.setFail(0))
			}
			return errors.Join(errs...)
		}
		before := map[*disk]int64{}
		for _, d := range ds {
			before[d] = d.failed()
			if err := d.setFail(int(p * 1e6)); err != nil {
				release()
				return nil, nil, errors.Join(err, lift())
			}
		}
		f.Detail = fmt.Sprintf("failing, %.3g%% of reads and writes", p*100)
		if reboot {
			f.Detail += ", a reboot as it heals"
		}
		hit = func(d *disk) bool { return d.failed() > before[d] }
	} else {
		f.Detail = "full"
		lift = func() error {
			var errs []error
			for _, d := range ds {
				d.mu.Lock()
				if err := os.Remove(d.dir + "/full"); err != nil && !os.IsNotExist(err) {
					errs = append(errs, err)
				}
				// (Committed, so that a power loss soon after doesn't
				// bring the file back, and the disk stays full for good.)
				errs = append(errs, syncDir(d.dir))
				d.mu.Unlock()
			}
			return errors.Join(errs...)
		}
		hit = func(*disk) bool { return true } // (writes may have, for want of space)
		for _, d := range ds {
			if err := d.fill(); err != nil {
				release()
				return nil, nil, errors.Join(err, lift())
			}
		}
	}
	if scope != "" {
		f.Detail = scope + ", " + f.Detail
	}
	heal := func() error {
		var errs []error
		var start []container
		// The containers whose reads or writes failed and that exited on
		// an error (not a fault's stop or kill), as systems that stop on a
		// failed write do, meanwhile or in the second after, are started
		// again, and their exits are the fault's doing, not unexpected
		// ones. (Not exits on 132 nor 139, SIGILL and SIGSEGV: no system
		// stops that way on an error. SIGBUS, 135, is how an mmap's read
		// fails.) If the fault reboots, its nodes whose reads or writes
		// failed are rebooted first, as a machine is after a disk failure.
		// (Every container on its disks, those down when it began
		// included, as another fault's heal may have started them since.)
		claimed := map[string]bool{} // (whose deaths it claimed, not those its reboots took down: see restart)
		scan := func(now map[string]container) {
			for _, c := range slices.SortedFunc(maps.Values(now), func(a, b container) int { return strings.Compare(a.Name, b.Name) }) {
				if d := disks[c.ID]; d == nil || !slices.Contains(ds, d) || !hit(d) {
					continue
				}
				// (Every such exit since the fault began, if another
				// fault started it again or took it down since.)
				claimExits(c.ID, n, "132", "139")
				s := now[c.ID].State
				if errExit(now[c.ID]) && !in.r.isDown(c.ID) && !slices.ContainsFunc(start, func(o container) bool { return o.ID == c.ID }) {
					claimDeath(c, n)
					say("fault: starting %s again (it exited with code %d on a %s disk)", c.Name, s.ExitCode, map[bool]string{true: "failing", false: "full"}[kind == "fail"])
					start = append(start, c)
					claimed[c.ID] = true
				}
			}
		}
		// (Its nodes are rebooted before the fault lifts, and those whose
		// reads or writes failed just before it lifted right after.)
		now, err := current()
		if err != nil {
			release()
			return errors.Join(lift(), err)
		}
		scan(now)
		booted := map[*disk]bool{}
		boot := func() {
			for _, d := range ds {
				if reboot && !booted[d] && hit(d) {
					booted[d] = true
					errs = append(errs, in.reboot(d, &start))
				}
			}
		}
		boot()
		errs = append(errs, lift())
		lifted := time.Now()
		boot()
		// (On a loaded host an exit may take longer than that: once the
		// VM idles, it came.)
		for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if time.Now().Add(100*time.Millisecond).After(end) && in.skip {
				idle()
			}
			if now, err = current(); err != nil {
				release()
				return errors.Join(append(errs, err)...)
			}
			scan(now)
		}
		// (Only now, once it's rebooted, may other faults' heals start
		// its containers: those that tried are started here, and those
		// whose deaths other faults' graces claimed.)
		later, deaths := release()
		for _, c := range slices.Concat(later, deaths) {
			if !slices.ContainsFunc(start, func(o container) bool { return o.ID == c.ID }) {
				start = append(start, c)
			}
		}
		for _, c := range deaths {
			claimed[c.ID] = !slices.ContainsFunc(later, func(o container) bool { return o.ID == c.ID })
		}
		// (One whose death it claimed it starts only if nothing took it
		// over since, and leaves its down state alone, which is a node
		// fault's; those its reboots took down, or that other faults' heals
		// left to it, are its own.)
		var started sync.Map // (each() starts them at once)
		restart := func(c container) error {
			if claimed[c.ID] {
				ok, err := in.claimedStart(c)
				started.Store(c.ID, ok && err == nil)
				return err
			}
			err := in.restart(c)
			in.r.downed(c, false)
			started.Store(c.ID, err == nil)
			return err
		}
		// (Level by level, as a node fault's heal: a container that shares
		// another's network starts once that one runs.)
		restartAll := func(l []container) error {
			var levels [][]container
			for _, c := range l {
				_, h := netOwner(c, now)
				for len(levels) <= h {
					levels = append(levels, nil)
				}
				levels[h] = append(levels[h], c)
			}
			var errs []error
			for _, l := range levels {
				_, err := each(l, restart)
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		}
		errs = append(errs, restartAll(start))
		// (One started again may exit at once on an error the fault left,
		// as a failed write's that the next fsync reports: that's the
		// fault's doing too, and it's started once more.)
		var again []container
		for end := time.Now().Add(time.Second); len(start) > 0 && time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if time.Now().Add(100*time.Millisecond).After(end) && in.skip {
				idle()
			}
			if now, err = current(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			for _, c := range start {
				if ok, _ := started.Load(c.ID); ok != true {
					continue
				}
				if errExit(now[c.ID]) && !in.r.isDown(c.ID) && !slices.ContainsFunc(again, func(o container) bool { return o.ID == c.ID }) {
					claimDeath(now[c.ID], n)
					say("fault: starting %s once more (it exited with code %d as it started)", c.Name, now[c.ID].State.ExitCode)
					again = append(again, now[c.ID])
					claimed[c.ID] = true
				}
			}
		}
		errs = append(errs, restartAll(again))
		// A system may take a while to die of an error the fault left it
		// (ClickHouse Keeper's crash handler reports a failed write's
		// abort, then goes on for seconds before the process dies of
		// SIGTRAP): for 10 s after the fault lifts, a container whose
		// reads or writes failed that exits on an error is the fault's
		// doing too, and started again, once (see injector.claimedStart).
		// A container idle for a second (its cgroup used no CPU) is let
		// go, and the grace ends once none is left. Then the crashes since
		// the fault began in those containers are its doing too, one for
		// each of their deaths by that signal since then. (In the
		// background: the heal, and the next disk fault, don't wait for
		// it; the timeline's end does, see runner.finish.)
		in.r.graces.Add(1)
		go func() {
			defer in.r.graces.Done()
			var err error
			watch := map[string]bool{}
			for id := range now {
				if d := disks[id]; d != nil && slices.Contains(ds, d) && hit(d) && !slices.ContainsFunc(again, func(o container) bool { return o.ID == id }) {
					watch[id] = true
				}
			}
			used, quiet := map[string]uint64{}, map[string]int{}
			for end := lifted.Add(10 * time.Second); len(watch) > 0 && time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
				if now, err = current(); err != nil {
					in.errorf("disk fault's grace: %v", err)
					break
				}
				cpu := cgroupCPU()
				for _, id := range slices.Sorted(maps.Keys(watch)) {
					c := now[id]
					switch {
					case errExit(c) && !in.r.isDown(id):
						claimDeath(c, n)
						say("fault: starting %s again (it exited with code %d %.1f s after the %s disk healed)", c.Name, c.State.ExitCode, time.Since(lifted).Seconds(), map[bool]string{true: "failing", false: "full"}[kind == "fail"])
						if _, err := in.claimedStart(c); err != nil {
							in.errorf("starting %s again: %v", c.Name, err)
						}
						delete(watch, id)
					case !up(c):
						delete(watch, id)
					case cpu[id].us == used[id]:
						if quiet[id]++; quiet[id] >= 10 {
							delete(watch, id)
						}
					default:
						quiet[id] = 0
					}
					used[id] = cpu[id].us
				}
			}
			// (Each crash is matched to an exit, by its signal (128+N), of
			// the container whose cgroup its process was in (the kernel logs
			// the cgroup's name, the container's ID), one whose reads or
			// writes failed: one crash an exit, whatever fault the exit was
			// taken as, such as a stop's that came as the crash handler ran,
			// and no exit matched by another disk fault's grace too. So a
			// crash in another container, or of a process whose container
			// ran on, stays a crash. A kill of the fault's reboot may have
			// cut a crash short, its process dying as the reboot came, as
			// TigerBeetle's on a latent sector error: it matches one crash
			// in its container of any signal, logged before the kill was
			// sent; see markBoot.)
			code := func(c crash) func(exit) bool {
				return func(e exit) bool { return e.code == strconv.Itoa(128+c.sig) }
			}
			cut := func(c crash) func(exit) bool {
				return func(e exit) bool { return c.seq < e.boot }
			}
			claim := func(c crash) bool {
				for _, match := range []func(crash) func(exit) bool{code, cut} {
					if d := disks[c.cgroup]; d != nil && slices.Contains(ds, d) && hit(d) && takeExit(c.cgroup, n, match(c)) {
						return true
					}
				}
				return false
			}
			for _, c := range in.r.claimCrashes(seq, claim) {
				say("fault: %s, on a %s disk", c, map[bool]string{true: "failing", false: "full"}[kind == "fail"])
			}
		}()
		return errors.Join(errs...)
	}
	return f, heal, nil
}

// fill fills d (a file, full, as large as there's room for).
func (d *disk) fill() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(d.dir+"/full", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.dir, &st); err != nil {
		return err
	}
	// (ext4 allocates in groups: what's left after the largest fits is
	// filled in halves, down to 64 KiB.)
	off := int64(0)
	for n := int64(st.Bavail) * st.Bsize; n >= 64<<10; {
		if syscall.Fallocate(int(f.Fd()), 0, off, n) == nil {
			off += n
		} else {
			n /= 2
		}
	}
	// (Committed, so that a power loss meanwhile doesn't free the space.)
	return errors.Join(f.Sync(), syncDir(d.dir))
}

// syncDir commits what changed in directory dir (its entries).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// reboot kills the running containers of d's node, remounts d (see
// remount) and adds them to start, as a machine reboots after a disk
// failure: a system may hold on to an error until it restarts. (Those
// another fault took down, if still running, as a stop's SIGTERM may leave
// them, are killed too, but left to its heal to start.)
func (in *injector) reboot(d *disk, start *[]container) error {
	say("fault: rebooting %s's node (its disk failed)", d.name)
	// (Holding d.mu: no other fault's heal starts its containers until
	// it's up again.)
	d.mu.Lock()
	defer d.mu.Unlock()
	now, err := current()
	if err != nil {
		return err
	}
	var cs []container
	for id, dd := range disks {
		if c := now[id]; dd == d && up(c) {
			cs = append(cs, c)
		}
	}
	slices.SortFunc(cs, func(a, b container) int { return strings.Compare(a.Name, b.Name) })
	for _, c := range cs {
		if !in.r.isDown(c.ID) {
			in.r.downed(c, true)
			*start = append(*start, c)
		}
	}
	_, err = each(cs, in.rebootKill)
	if err == nil {
		if err = d.sys0("fail", "0"); err == nil { // (its fault is over)
			err = d.remount(false)
		}
	}
	return err
}

// rebootKill kills container c for a reboot (see killWait) and marks its
// exit as the reboot's, with the kernel log's position just before the
// kill: a crash logged by then may be one it cut short (see markBoot).
func (in *injector) rebootKill(c container) error {
	bound, m := in.r.crashSeq(), exitCount()
	err := call("POST", "/containers/"+c.ID+"/kill", nil, nil)
	sent := err == nil
	if err == nil || exited(err) { // (it exited meanwhile)
		err = call("POST", "/containers/"+c.ID+"/wait?condition=not-running", nil, nil)
	}
	if sent && err == nil {
		markBoot(c.ID, m, bound)
	}
	return err
}

// errExit is whether container c exited on an error: not 0, nor killed
// (137) or stopped (143), nor 132 or 139 (SIGILL, SIGSEGV), as no system
// stops on an I/O error.
func errExit(c container) bool {
	return c.State.Status == "exited" && !slices.Contains([]int{0, 132, 137, 139, 143}, c.State.ExitCode)
}

// up is whether container c runs, paused or not.
func up(c container) bool { return c.State.Status == "running" || c.State.Status == "paused" }
