//go:build linux

package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Docker Engine API, over dockerd's socket: cheaper than a docker CLI
// process per call on one vCPU.
var engine = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
}}}

// call makes an API request with a JSON body (if any) and decodes a JSON
// answer into out (if any).
func call(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	resp, err := do(method, path, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func do(method, path, ctype string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://docker"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := engine.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified { // (304: already so)
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("docker %s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return resp, nil
}

// exits are the project's containers' exits, from Docker's die events, and
// the signals Docker sent them, from its kill events.
var exits struct {
	sync.Mutex
	once sync.Once
	l    []exit
}

// exit is a container's exit, fault if a fault caused it (see claimExit
// and signaledExits); or, if kill, a signal Docker sent it.
type exit struct {
	id, name, code string
	fault, kill    bool
}

// unexpected is whether e counts as Antithesis's "No unexpected container
// exits" counts them: with an exit code other than 0, 137 (SIGKILL) or 143
// (SIGTERM), and not caused by a fault, since a process may exit with
// another code on SIGTERM (as Antithesis filters the exits of containers it
// stopped). So the stops and kills of node faults and restarts don't count,
// and a crash does, restarted or not.
func (e exit) unexpected() bool {
	return !e.fault && !e.kill && e.code != "0" && e.code != "137" && e.code != "143"
}

func (e exit) String() string { return e.name + " exited with code " + e.code }

// watchExits records the exits from now on, from Docker's events.
func watchExits() {
	exits.once.Do(func() {
		f, _ := json.Marshal(map[string][]string{"type": {"container"}, "event": {"die", "kill"}, "label": {"com.docker.compose.project=" + project}})
		resp, err := do("GET", "/events?filters="+url.QueryEscape(string(f)), "", nil)
		check(err)
		go func() {
			for d := json.NewDecoder(resp.Body); ; {
				var e struct {
					Action string
					Actor  struct {
						ID         string
						Attributes map[string]string
					}
				}
				if d.Decode(&e) != nil {
					return
				}
				a := e.Actor.Attributes
				exits.Lock()
				exits.l = append(exits.l, exit{id: e.Actor.ID, name: a["name"], code: a["exitCode"], kill: e.Action == "kill"})
				exits.Unlock()
			}
		}()
	})
}

// exitsSince are the unexpected exits after the first n exits.
func exitsSince(n int) []string {
	exits.Lock()
	defer exits.Unlock()
	var l []string
	for _, e := range exits.l[n:] {
		if e.unexpected() {
			l = append(l, e.String())
		}
	}
	return l
}

// exitCount is how many exits there were so far.
func exitCount() int {
	exits.Lock()
	defer exits.Unlock()
	return len(exits.l)
}

// claimExits marks the unexpected exits of container id after the first
// n, but those with exit codes keep lists, as a fault's doing.
func claimExits(id string, n int, keep ...string) {
	exits.Lock()
	defer exits.Unlock()
	for i := n; i < len(exits.l); i++ {
		if e := &exits.l[i]; e.id == id && e.unexpected() && !slices.Contains(keep, e.code) {
			e.fault = true
		}
	}
}

// signaledExits marks the exit of each of the containers that follows the
// first signal Docker sent it after the first n exits (a stop fault's
// SIGTERM: Docker logs the signal before the exit it causes), as the
// fault's doing, waiting up to 5 s for Docker's events. An exit before the
// signal, a crash's that the stop came after, stays unexpected.
func signaledExits(cs []container, n int) {
	for t := time.Now(); len(cs) > 0 && time.Since(t) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		exits.Lock()
		cs = slices.DeleteFunc(cs, func(c container) bool {
			l := exits.l[n:]
			k := slices.IndexFunc(l, func(e exit) bool { return e.id == c.ID && e.kill })
			if k < 0 {
				return false
			}
			i := slices.IndexFunc(l[k:], func(e exit) bool { return e.id == c.ID && !e.fault && !e.kill })
			if i >= 0 {
				l[k+i].fault = true
			}
			return i >= 0
		})
		exits.Unlock()
	}
}

// catchesTerm is whether container c's process is running (not exited, nor
// a zombie) and catches SIGTERM, so that a stop may make it exit with a code
// of its own.
func catchesTerm(c container) bool {
	var d container
	if call("GET", "/containers/"+c.ID+"/json", nil, &d) != nil || d.State.Pid == 0 {
		return false
	}
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", d.State.Pid))
	var state, caught string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "State:"); ok {
			state = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(l, "SigCgt:"); ok {
			caught = strings.TrimSpace(v)
		}
	}
	m, err := strconv.ParseUint(caught, 16, 64)
	return err == nil && state != "" && state[0] != 'Z' && state[0] != 'X' && m&(1<<(syscall.SIGTERM-1)) != 0
}

// container is what the agent needs of a container's inspect document.
type container struct {
	ID    string
	Name  string
	State struct {
		Status   string
		Pid      int
		ExitCode int
		Health   *struct{ Status string }
	}
	Config          struct{ Labels map[string]string }
	HostConfig      struct{ NetworkMode string }
	Mounts          []mount
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

// A mount is a container's volume, bind mount or tmpfs.
type mount struct{ Type, Source, Name, Driver string }

// ips are the container's addresses, on each of its networks.
func (c container) ips() []string {
	var l []string
	for _, n := range c.NetworkSettings.Networks {
		if n.IPAddress != "" {
			l = append(l, n.IPAddress)
		}
	}
	slices.Sort(l)
	return l
}

// root is the container's filesystem as its processes see it, volumes
// included (none, if it doesn't run).
func (c container) root() string { return fmt.Sprintf("/proc/%d/root", c.State.Pid) }

// containers inspects the project's containers, all of them or service's.
func containers(service string) ([]container, error) {
	f := map[string][]string{"label": {"com.docker.compose.project=" + project}}
	if service != "" {
		f["label"] = append(f["label"], "com.docker.compose.service="+service)
	}
	fj, _ := json.Marshal(f)
	var list []struct{ ID string }
	if err := call("GET", "/containers/json?all=1&filters="+url.QueryEscape(string(fj)), nil, &list); err != nil {
		return nil, err
	}
	var cs []container
	for _, l := range list {
		var c container
		if err := call("GET", "/containers/"+l.ID+"/json", nil, &c); err != nil {
			return nil, err
		}
		c.Name = strings.TrimPrefix(c.Name, "/")
		cs = append(cs, c)
	}
	return cs, nil
}

// execIn runs path in a container as docker exec would, writing its output
// to out, and returns its exit code and the CPU time it used (and what it
// waited for). started gets the process's pid (in the
// VM) once it runs. It doesn't go through Docker: a docker exec costs about
// 57 ms of the VM's one CPU (dockerd, containerd, a shim and runc), most of
// a short test command's cost. nsenter enters the container's namespaces
// (but its cgroup namespace) and root, in its cgroup, as its user, in its
// working directory, with its environment. What only isolates is left out:
// its capability bounds, seccomp and AppArmor profiles.
func execIn(id, path string, out io.Writer, started func(pid int)) (int, time.Duration, error) {
	t, err := target(id)
	if err != nil {
		return 0, 0, err
	}
	// (nsenter opens -w's directory before it enters the mount namespace.
	// It starts as root with the gid and groups, and -S switches to the
	// uid: its -G would drop the groups.)
	cmd := exec.Command("nsenter", "-t", strconv.Itoa(t.pid), "-m", "-u", "-i", "-n", "-p", "-r",
		"-w"+t.dir, "-S", t.uid, "--", path)
	cmd.Env = t.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Gid: t.gid, Groups: t.groups}}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", t.pid)); err == nil {
		if cg, err := os.Open("/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(string(b), "0::"))); err == nil {
			defer cg.Close()
			cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, int(cg.Fd())
		}
	}
	cmd.Stdout, cmd.Stderr = out, out
	// (Output after it exited, from what it left running, isn't waited for.)
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	if started != nil {
		started(cmd.Process.Pid)
	}
	err = cmd.Wait()
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), cpu, nil // (as runc reports it)
	}
	var x *exec.ExitError
	if err != nil && !errors.As(err, &x) && !errors.Is(err, exec.ErrWaitDelay) {
		return 0, cpu, err
	}
	return cmd.ProcessState.ExitCode(), cpu, nil
}

// An execTarget is what execIn needs of a running container: its main
// process (and when that started), and an exec's working directory (as the
// VM sees it), user, group, groups and environment.
type execTarget struct {
	pid             int
	start, dir, uid string
	gid             uint32
	groups          []uint32
	env             []string
}

// targets are the containers' exec targets, by container ID: a test command
// doesn't cost an inspect (dockerd's CPU and the agent's) until the
// container's main process changes.
var targets sync.Map

func target(id string) (*execTarget, error) {
	if t, ok := targets.Load(id); ok && procStart(t.(*execTarget).pid) == t.(*execTarget).start {
		return t.(*execTarget), nil
	}
	var c struct {
		State  struct{ Pid int }
		Config struct {
			Env                        []string
			User, WorkingDir, Hostname string
		}
		HostConfig struct{ GroupAdd []string }
	}
	if err := call("GET", "/containers/"+id+"/json", nil, &c); err != nil {
		return nil, err
	}
	if c.State.Pid == 0 {
		return nil, fmt.Errorf("container %s is not running", id)
	}
	root := fmt.Sprintf("/proc/%d/root", c.State.Pid)
	t := &execTarget{pid: c.State.Pid, start: procStart(c.State.Pid),
		dir: root + "/" + strings.TrimPrefix(cmp.Or(c.Config.WorkingDir, "/"), "/")}
	uid, gid, home, groups := lookupUser(root, c.Config.User, c.HostConfig.GroupAdd)
	t.uid = uid
	for _, g := range append([]string{gid}, groups...) { // (Docker's groups include the gid)
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("container %s: no group %s", id, g)
		}
		t.groups = append(t.groups, uint32(n))
	}
	t.gid = t.groups[0]
	// (As dockerd and runc make an exec's environment.)
	t.env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOSTNAME=" + c.Config.Hostname}
	for _, e := range c.Config.Env {
		k, _, _ := strings.Cut(e, "=")
		t.env = slices.DeleteFunc(t.env, func(x string) bool { return strings.HasPrefix(x, k+"=") })
		t.env = append(t.env, e)
	}
	if !slices.ContainsFunc(t.env, func(x string) bool { return strings.HasPrefix(x, "HOME=") }) {
		t.env = append(t.env, "HOME="+home)
	}
	targets.Store(id, t)
	return t, nil
}

// procStart is when a process started (in clock ticks since boot), or "" if
// there's no such process.
func procStart(pid int) string {
	if f := procStat(strconv.Itoa(pid)); f != nil {
		return f[19] // (field 22)
	}
	return ""
}

// procStat is /proc/PID/stat's fields from the third (after the command),
// or nil if there's no such process.
func procStat(pid string) []string {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
}

// lookupUser resolves a container's user (name or uid, and maybe :group
// or :gid) in its /etc/passwd and /etc/group, under root: its uid, gid,
// home directory and supplementary groups, as Docker sets them: the groups
// that list the user (unless a group is given) and groupAdd's.
// Like runc, an unknown numeric uid is fine; its gid is 0 and its home /.
func lookupUser(root, user string, groupAdd []string) (uid, gid, home string, groups []string) {
	u, g, _ := strings.Cut(user, ":")
	uid, gid, home = cmp.Or(u, "0"), "0", "/"
	name := ""
	if b, err := os.ReadFile(root + "/etc/passwd"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) >= 6 && (f[0] == uid || f[2] == uid) {
				name, uid, gid, home = f[0], f[2], f[3], f[5]
				break
			}
		}
	}
	b, _ := os.ReadFile(root + "/etc/group")
	var entries [][]string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Split(l, ":"); len(f) >= 4 {
			entries = append(entries, f)
		}
	}
	gidOf := func(g string) string {
		for _, f := range entries {
			if f[0] == g {
				return f[2]
			}
		}
		return g
	}
	if g != "" {
		gid = gidOf(g)
	} else if name != "" {
		for _, f := range entries {
			if slices.Contains(strings.Split(f[3], ","), name) {
				groups = append(groups, f[2])
			}
		}
	}
	for _, a := range groupAdd {
		groups = append(groups, gidOf(a))
	}
	return uid, gid, home, groups
}

// untar extracts a tar archive at dir in a container, with the archive's
// owners, as docker cp -a and docker compose watch do.
func untar(id, dir string, tarball io.Reader) error {
	resp, err := do("PUT", "/containers/"+id+"/archive?copyUIDGID=true&path="+url.QueryEscape(dir), "application/x-tar", tarball)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// load loads the images in a docker save archive.
func load(archive io.Reader) error {
	resp, err := do("POST", "/images/load?quiet=1", "application/x-tar", archive)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for d := json.NewDecoder(resp.Body); ; {
		var m struct{ Error string }
		if err := d.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

// compose runs Docker Compose (standalone: no docker CLI process in
// between), its output going to the console.
func compose(args ...string) error { return composeCtx(context.Background(), args...) }

// composeCtx runs Docker Compose until ctx is done, then kills it.
func composeCtx(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/local/libexec/docker/cli-plugins/docker-compose", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return fmt.Errorf("docker compose %s: %v", strings.Join(args, " "), err)
	}
	return nil
}
