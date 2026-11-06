// Command build builds the timeskip kit. ../../build.sh runs it in the
// builder container (builder.Dockerfile), with the repository at /src, the
// output directory at /out and the cache volume at /cache.
//
//	build macos-arm64|linux-x86_64   the kit, out/e2e.test and out/tests
//	build lint                       gofmt, go vet and shellcheck
//
// Steps, in parallel: kernel, rootfs (the guest's root filesystem, with the
// guest agent) and the test bundles; then the kit. A step is
// skipped when its inputs are unchanged since it last ran. Its timestamps
// (SOURCE_DATE_EPOCH) are the time of the last commit that touched its
// inputs, so a commit builds the same kit on the same kind of host.
package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanworl/timeskip/internal/archive"
	"github.com/ryanworl/timeskip/internal/bundle"
)

const src, out, dl = "/src", "/out", "/cache/dl"

type target struct {
	name   string
	a      string // the architecture, as in Alpine (aarch64, x86_64)
	g      string // ... as in Go and Docker (arm64, amd64)
	k      string // ... as in the kernel's ARCH (arm64, x86_64)
	goos   string
	image  string // the kernel's make target, and where it's built
	kernel string // the kernel's image in out/ and the kit
}

var targets = map[string]target{
	"macos-arm64":  {"macos-arm64", "aarch64", "arm64", "arm64", "darwin", "arch/arm64/boot/Image", "Image-arm64"},
	"linux-x86_64": {"linux-x86_64", "x86_64", "amd64", "x86_64", "linux", "vmlinux", "vmlinux-x86_64"},
}

// Every step's inputs include these.
var common = []string{"deps.lock", "cmd/build", "go.mod", "go.sum", "vendor", "internal/archive"}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "lint" {
		lint()
		return
	}
	t, ok := targets[os.Args[len(os.Args)-1]]
	if len(os.Args) != 2 || !ok {
		fail("usage: build macos-arm64|linux-x86_64|lint")
	}
	if t.g != runtime.GOARCH {
		fail("the builder must run natively (%s), not on %s", t.g, runtime.GOARCH)
	}
	start := time.Now()
	defer chown()
	lock = deps(t)
	fetch()
	// The Docker CLI, with Compose, for the fixtures' bundles: they're
	// made with the host's Docker, through its socket.
	// (Reinstalled when their deps.lock entries change; key is written last.)
	cli, tools := "/cache/docker-cli-"+t.a, []string{"stable/" + t.a + "/docker-", "docker-compose-linux-" + t.a}
	key := ""
	for _, d := range lock {
		for _, tool := range tools {
			if strings.Contains(d.url, tool) {
				key += d.sum + "\n"
			}
		}
	}
	if old, _ := os.ReadFile(cli + "/key"); string(old) != key {
		os.RemoveAll(cli)
		must(os.MkdirAll(cli+"/cli-plugins", 0o755))
		must(run(os.Stdout, "", nil, "tar", "-xzf", file(tools[0]), "-C", cli, "--strip-components=1", "docker/docker"))
		must(install(file(tools[1]), cli+"/cli-plugins/docker-compose", 0o755))
		must(os.WriteFile(cli+"/key", []byte(key), 0o644))
	}
	os.Setenv("PATH", cli+":"+os.Getenv("PATH"))
	os.Setenv("DOCKER_CONFIG", cli)

	steps := []func() error{
		func() error { return step(t, "kernel", []string{"guest/kernel"}, nil, kernel) },
		func() error {
			return step(t, "tests", append(goDeps("linux", "./internal/bundle"), "e2e/testdata"), nil, tests)
		},
		func() error {
			return step(t, "rootfs", append(goDeps("linux", "./cmd/guest"), "guest/init", "guest/libvoidstar.c"), nil, rootfs)
		},
	}
	if t.goos == "linux" {
		steps = append(steps, func() error { return step(t, "firecracker", []string{"firecracker"}, nil, firecracker) })
	}
	errs := make([]error, len(steps))
	var wg sync.WaitGroup
	for i, fn := range steps {
		wg.Go(func() { errs[i] = fn() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			fail("%v", err)
		}
	}
	// The version (the last tag, see git describe) and source the kit's
	// MANIFEST names, part of the kit's inputs.
	version, err := git("describe", "--tags", "--always", "--abbrev=12")
	must(err)
	commit, err := git("rev-parse", "HEAD")
	must(err)
	source := strings.TrimSpace(version) + "\n" + strings.TrimSpace(commit)
	// Only changes to what the build reads count (hvrun is built by
	// build.sh, from hv/): a file elsewhere (notes, a project under docs/)
	// would make the kit, whose inputs include this text, another kit,
	// whose checkpoints and replays aren't this one's.
	inputs := append([]string{"status", "--porcelain", "--", "guest", "firecracker", "cmd", "internal", "e2e",
		"examples", "hv", "build.sh", ".gitignore", "LICENSE", "THIRD_PARTY.md", "licenses"}, common...)
	if st, _ := git(inputs...); st != "" {
		source += " (with uncommitted changes)"
	}
	must(os.WriteFile(out+"/.source", []byte(source), 0o644))
	must(step(t, "kit", []string{"cmd", "internal", "e2e", "examples", "hv", "LICENSE", "THIRD_PARTY.md", "licenses"},
		[]string{".stamp-kernel", ".stamp-rootfs", ".stamp-firecracker", ".source", "hvrun", "hvrun.toolchain"}, kit))
	sum, _ := os.ReadFile(out + "/timeskip-" + t.name + ".tgz.sha256")
	fmt.Printf("%sbuild: %.0f s\n", sum, time.Since(start).Seconds())
}

// step runs fn, logging to out/log-NAME, unless its inputs are unchanged
// since its last run. Its inputs are the repository paths in paths (plus
// common) and the files in out/ named in outs.
func step(t target, name string, paths, outs []string, fn func(t target, log io.Writer, sde int64) error) error {
	start := time.Now()
	paths = append(paths, common...)
	sdeOut, err := git(append([]string{"log", "-1", "--format=%ct", "--"}, paths...)...)
	if err != nil {
		return err
	}
	sde, _ := strconv.ParseInt(strings.TrimSpace(sdeOut), 10, 64)
	files, err := git(append([]string{"ls-files", "-co", "--exclude-standard", "--"}, paths...)...)
	if err != nil {
		return err
	}
	h := sha256.New()
	fmt.Fprintln(h, t.name, sde)
	names := strings.Fields(files)
	slices.Sort(names)
	for _, f := range names {
		hashFile(h, filepath.Join(src, f))
	}
	for _, f := range outs {
		hashFile(h, filepath.Join(out, f))
	}
	key, stamp := fmt.Sprintf("%x\n", h.Sum(nil)), filepath.Join(out, ".stamp-"+name)
	if b, _ := os.ReadFile(stamp); string(b) == key {
		fmt.Printf("%s: up to date\n", name)
		return nil
	}
	os.Remove(stamp)
	fmt.Printf("%s: building (log in out/log-%s)\n", name, name)
	log, err := os.Create(filepath.Join(out, "log-"+name))
	if err != nil {
		return err
	}
	defer log.Close()
	if err := fn(t, log, sde); err != nil {
		b, _ := os.ReadFile(log.Name())
		l := strings.Split(strings.TrimSpace(string(b)), "\n")
		return fmt.Errorf("%s failed: %v\n%s", name, err, strings.Join(l[max(0, len(l)-20):], "\n"))
	}
	fmt.Printf("%s: built in %.0f s\n", name, time.Since(start).Seconds())
	return os.WriteFile(stamp, []byte(key), 0o644)
}

// Linux 6.12.56 plus the time-skip patch, with the committed config. The
// tree stays in the cache, so a rebuild compiles only what changed.
func kernel(t target, log io.Writer, sde int64) error {
	w := "/cache/kernel-" + t.k
	tree := w + "/linux-6.12.56"
	if err := unpack(log, w, tree, file("linux-6.12.56.tar.xz"), []string{src + "/guest/kernel/timeskip.patch"}); err != nil {
		return err
	}
	// (Rewriting an unchanged .config would rebuild everything.)
	config, _ := os.ReadFile(src + "/guest/kernel/config-" + t.k)
	if old, _ := os.ReadFile(tree + "/.config"); !bytes.Equal(old, config) {
		if err := os.WriteFile(tree+"/.config", config, 0o644); err != nil {
			return err
		}
	}
	env := []string{"KBUILD_BUILD_TIMESTAMP=" + time.Unix(sde, 0).UTC().Format("Mon Jan _2 15:04:05 MST 2006"),
		"KBUILD_BUILD_USER=timeskip", "KBUILD_BUILD_HOST=timeskip", "KBUILD_BUILD_VERSION=1"}
	if err := run(log, tree, nil, "make", "-s", "ARCH="+t.k, "olddefconfig"); err != nil {
		return err
	}
	if err := run(log, tree, env, "make", "-s", "ARCH="+t.k, "-j"+strconv.Itoa(runtime.NumCPU()), filepath.Base(t.image)); err != nil {
		return err
	}
	return install(tree+"/"+t.image, out+"/"+t.kernel, 0o644)
}

// unpack extracts tarball to tree and applies patches, unless w/source says
// tree already holds them.
func unpack(log io.Writer, w, tree, tarball string, patches []string) error {
	h := sha256.New()
	hashFile(h, tarball)
	for _, p := range patches {
		hashFile(h, p)
	}
	key := fmt.Sprintf("%x", h.Sum(nil))
	if b, _ := os.ReadFile(w + "/source"); string(b) == key {
		return nil
	}
	os.Remove(w + "/source") // (written again once the tree is whole)
	os.RemoveAll(tree)
	if err := os.MkdirAll(tree, 0o755); err != nil {
		return err
	}
	if err := run(log, "", nil, "tar", "-xf", tarball, "-C", tree, "--strip-components=1"); err != nil {
		return err
	}
	for _, p := range patches {
		if err := run(log, "", nil, "patch", "-d", tree, "-p1", "-s", "-i", p); err != nil {
			return err
		}
	}
	return os.WriteFile(w+"/source", []byte(key), 0o644)
}

// Firecracker 1.17.0 with the patches in firecracker/, a static musl binary
// built with the Rust toolchain its rust-toolchain.toml names.
// The tree and cargo's cache stay in the cache volume.
func firecracker(t target, log io.Writer, sde int64) error {
	w := "/cache/firecracker"
	tree := w + "/src"
	patches, _ := filepath.Glob(src + "/firecracker/*.patch")
	env := []string{"RUSTUP_HOME=/cache/rustup", "CARGO_HOME=/cache/cargo", "PATH=/cache/cargo/bin:" + os.Getenv("PATH")}
	if _, err := os.Stat("/cache/cargo/bin/rustup"); err != nil {
		if err := install(file("rustup-init"), "/tmp/rustup-init", 0o755); err != nil {
			return err
		}
		if err := run(log, "", env, "/tmp/rustup-init", "-y", "-q", "--no-modify-path", "--profile", "minimal", "--default-toolchain", "none"); err != nil {
			return err
		}
	}
	if err := unpack(log, w, tree, file("firecracker/archive"), patches); err != nil {
		return err
	}
	toolchain, _ := os.ReadFile(tree + "/rust-toolchain.toml")
	channel := regexp.MustCompile(`(?m)^channel = "(.+)"`).FindSubmatch(toolchain)
	if channel == nil {
		return fmt.Errorf("no channel in firecracker's rust-toolchain.toml")
	}
	triple := t.a + "-unknown-linux-musl"
	if err := run(log, "", env, "/cache/cargo/bin/rustup", "toolchain", "install", "--no-self-update", string(channel[1]), "--profile", "minimal", "--target", triple); err != nil {
		return err
	}
	// The C parts build with musl-gcc, which has no kernel headers of its own.
	cc := strings.ReplaceAll(triple, "-", "_")
	env = append(env, "CARGO_TARGET_DIR="+w+"/target", "CC_"+cc+"=musl-gcc",
		"CFLAGS_"+cc+"=-idirafter /usr/include -idirafter /usr/include/"+t.a+"-linux-gnu")
	if err := run(log, tree, env, "/cache/cargo/bin/cargo", "build", "-q", "--release", "--locked", "--target", triple, "-p", "firecracker"); err != nil {
		return err
	}
	return install(w+"/target/"+triple+"/release/firecracker", out+"/firecracker-"+t.a, 0o755)
}

// The guest's root filesystem, a read-only EROFS image: Alpine, static
// Docker and Compose, the Alpine packages in deps.lock, /init and the guest
// agent, /timeskip-guest. The VM reads files from it as they're used, so a
// checkpoint holds only the pages of the files its processes map.
func rootfs(t target, log io.Writer, sde int64) error {
	r := "/tmp/rootfs-" + t.a
	os.RemoveAll(r)
	for _, d := range []string{"usr/local/bin", "usr/local/libexec/docker/cli-plugins", "var/lib/docker", "inputs", "dev"} {
		if err := os.MkdirAll(filepath.Join(r, d), 0o755); err != nil {
			return err
		}
	}
	if err := run(log, "", nil, "tar", "-xzpf", file("alpine-minirootfs-3.20.3-"+t.a), "-C", r); err != nil {
		return err
	}
	// (Less containerd's ctr and the docker CLI, which nothing uses.)
	if err := run(log, "", nil, "tar", "-xzf", file("stable/"+t.a+"/docker-"), "--strip-components=1", "--exclude=docker/ctr", "--exclude=docker/docker", "-C", r+"/usr/local/bin"); err != nil {
		return err
	}
	if err := install(file("docker-compose-linux-"+t.a), r+"/usr/local/libexec/docker/cli-plugins/docker-compose", 0o755); err != nil {
		return err
	}
	if err := install(file("crun-1.30.1-linux-"+t.g+"-disable-systemd"), r+"/usr/local/bin/crun", 0o755); err != nil {
		return err
	}
	// (Only Alpine's packages: the guest deps include Alpine's minirootfs
	// and Docker's tarball too.)
	alpine := "https://dl-cdn.alpinelinux.org/alpine/v3.20/"
	for _, d := range lock {
		if d.use == "guest" && strings.HasSuffix(d.url, ".apk") &&
			(strings.HasPrefix(d.url, alpine+"main/"+t.a+"/") || strings.HasPrefix(d.url, alpine+"community/"+t.a+"/")) {
			// (Alpine's packages carry metadata files, and tar headers GNU tar warns about.)
			if err := run(io.Discard, "", nil, "tar", "-xzpf", d.path, "-C", r, "--exclude=.*"); err != nil {
				return err
			}
		}
	}
	if err := install(src+"/guest/init", r+"/init", 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(r+"/usr/local/lib/timeskip", 0o755); err != nil {
		return err
	}
	// (Nothing left undefined: no libc, nor libgcc's helpers, such as the
	// outline atomics GCC calls on arm64.)
	cc := []string{"cc", "-O2", "-fPIC", "-shared", "-nostdlib", "-ffreestanding", "-fno-stack-protector", "-Wl,--build-id=none", "-Wl,-z,defs"}
	if t.g == "arm64" {
		cc = append(cc, "-mno-outline-atomics")
	}
	if err := run(log, src, nil, append(cc, "-o", r+"/usr/local/lib/timeskip/libvoidstar.so", "guest/libvoidstar.c")...); err != nil {
		return err
	}
	if err := run(log, src, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + t.g}, "go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", r+"/timeskip-guest", "./cmd/guest"); err != nil {
		return err
	}
	// (mkfs.erofs orders the files by name, whatever the order on disk;
	// every file gets root's ownership and one time, and the image a fixed
	// UUID.)
	img := out + "/rootfs-" + t.a + ".erofs"
	os.Remove(img)
	return run(log, "", nil, "mkfs.erofs", "--quiet", "-x-1", "--all-root", "--ignore-mtime", fmt.Sprint("-T", sde),
		"-U", "7a3e5c1d-2b4f-4e6a-8c9d-0f1e2d3c4b5a", img, r)
}

func tests(t target, log io.Writer, sde int64) error {
	os.RemoveAll(out + "/tests")
	if err := os.MkdirAll(out+"/tests", 0o755); err != nil {
		return err
	}
	fixtures, _ := filepath.Glob(src + "/e2e/testdata/protocol/*.yaml")
	for _, f := range fixtures {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if _, err := bundle.Create(f, t.g, out+"/tests/"+name+".tsb", bundle.Options{Log: log}); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
	}
	return nil
}

// The kit, as a gzipped tar with sorted entries, fixed owners and times,
// and no name or time in the gzip header. And the e2e test binary.
func kit(t target, log io.Writer, sde int64) error {
	root := "/tmp/kit"
	k := root + "/timeskip"
	os.RemoveAll(root)
	for _, d := range []string{"bin", "share", "examples"} {
		if err := os.MkdirAll(k+"/"+d, 0o755); err != nil {
			return err
		}
	}
	// The unit tests first (the end-to-end ones need the kit: test.sh).
	if err := run(log, src, nil, "go", "test", "./..."); err != nil {
		return err
	}
	env := []string{"CGO_ENABLED=0", "GOOS=" + t.goos, "GOARCH=" + t.g}
	if err := run(log, src, env, "go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", k+"/timeskip", "./cmd/timeskip"); err != nil {
		return err
	}
	if err := run(log, src, env, "go", "test", "-c", "-tags", "e2e", "-trimpath", "-ldflags=-s -w -buildid=", "-o", out+"/e2e.test", "./e2e"); err != nil {
		return err
	}
	if t.goos == "darwin" {
		if err := install(out+"/hvrun", k+"/bin/hvrun", 0o755); err != nil {
			return err
		}
	} else if err := install(out+"/firecracker-"+t.a, k+"/bin/firecracker", 0o755); err != nil {
		return err
	}
	files := map[string]string{
		out + "/" + t.kernel:              "share/" + t.kernel,
		out + "/rootfs-" + t.a + ".erofs": "share/rootfs-" + t.a + ".erofs",
		src + "/LICENSE":                  "LICENSE",
		src + "/THIRD_PARTY.md":           "THIRD_PARTY.md",
	}
	// (The committed files only, not whatever else is in the checkout.)
	tracked, err := git("ls-files", "--", "examples", "licenses")
	if err != nil {
		return err
	}
	for _, f := range strings.Fields(tracked) {
		files[src+"/"+f] = f
	}
	for from, to := range files {
		if err := install(from, k+"/"+to, 0); err != nil {
			return err
		}
	}

	b, _ := os.ReadFile(out + "/.source")
	version, source, _ := strings.Cut(string(b), "\n")
	manifest := "timeskip " + version + " " + t.name + "\nsource: " + source
	if t.goos == "darwin" {
		tc, _ := os.ReadFile(out + "/hvrun.toolchain")
		manifest += "\nhvrun: " + strings.TrimSpace(string(tc))
	}
	manifest += "\n\n"
	err = filepath.WalkDir(k, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			manifest += fmt.Sprintf("%x  ./%s\n", sha256.Sum256(b), p[len(k)+1:])
			return err
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(k+"/MANIFEST", []byte(manifest), 0o644); err != nil {
		return err
	}

	name := "timeskip-" + t.name + ".tgz"
	f, err := os.Create(out + "/" + name)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	gz := exec.Command("pigz", "-6", "-n") // (gzip, in parallel)
	gz.Stdout, gz.Stderr = io.MultiWriter(f, h), log
	w, _ := gz.StdinPipe()
	if err := gz.Start(); err != nil {
		return err
	}
	err = archive.Tar(w, root, time.Unix(sde, 0))
	w.Close()
	if err := cmp.Or(err, gz.Wait()); err != nil {
		return err
	}
	return os.WriteFile(out+"/"+name+".sha256", fmt.Appendf(nil, "%x  %s\n", h.Sum(nil), name), 0o644)
}

func lint() {
	if out, _ := exec.Command("gofmt", "-l", ".").Output(); len(out) > 0 {
		fail("gofmt: these files need gofmt -w:\n%s", out)
	}
	for _, goos := range []string{"darwin", "linux"} {
		must(run(os.Stdout, "", []string{"GOOS=" + goos, "GOARCH=arm64"}, "go", "vet", "-tags", "e2e", "./..."))
	}
	scripts := []string{"guest/init"}
	must(filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == "out" || p == ".git") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(p, ".sh") {
			scripts = append(scripts, p)
		}
		return nil
	}))
	must(run(os.Stdout, "", nil, append([]string{"shellcheck", "-S", "warning"}, scripts...)...))
	fmt.Println("lint: ok")
}

// ---- downloads ----------------------------------------------------------------

type dep struct{ sum, use, url, path string }

var lock []dep // the target's deps

var archRE = regexp.MustCompile(`aarch64|x86_64|amd64|arm64`)

// deps are the deps.lock entries for t's architecture (the builder's too:
// it runs natively), with where they're downloaded to.
func deps(t target) []dep {
	b, err := os.ReadFile(src + "/deps.lock")
	must(err)
	var ds []dep
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if a := archRE.FindString(f[2]); a != "" && a != t.a && a != t.g {
			continue
		}
		ds = append(ds, dep{f[0], f[1], f[2], dl + "/" + strings.ReplaceAll(strings.TrimPrefix(f[2], "https://"), "/", "_")})
	}
	return ds
}

// fetch downloads and verifies the deps, in parallel.
func fetch() {
	client := &http.Client{Timeout: 30 * time.Minute}
	must(os.MkdirAll(dl, 0o755))
	var wg sync.WaitGroup
	for _, d := range lock {
		if ok, _ := os.ReadFile(d.path + ".ok"); string(ok) == d.sum {
			continue
		}
		wg.Go(func() {
			fmt.Println("fetching", d.url)
			resp, err := client.Get(d.url)
			if err == nil && resp.StatusCode != 200 {
				err = fmt.Errorf("%s", resp.Status)
			}
			if err != nil {
				fail("fetching %s: %v", d.url, err)
			}
			defer resp.Body.Close()
			f, err := os.Create(d.path + ".part")
			must(err)
			h := sha256.New()
			_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
			must(err)
			must(f.Close())
			if sum := fmt.Sprintf("%x", h.Sum(nil)); sum != d.sum {
				os.Remove(d.path + ".part")
				fail("checksum mismatch: %s: got %s", d.url, sum)
			}
			must(os.Rename(d.path+".part", d.path))
			must(os.WriteFile(d.path+".ok", []byte(d.sum), 0o644))
		})
	}
	wg.Wait()
}

// file is the path of the downloaded dep whose URL contains pattern.
func file(pattern string) string {
	for _, d := range lock {
		if strings.Contains(d.url, pattern) {
			return d.path
		}
	}
	fail("no deps.lock entry for %s", pattern)
	return ""
}

// ---- helpers ------------------------------------------------------------------

// run runs a command in dir (if not ""), with env added to its environment
// and its output going to log.
func run(log io.Writer, dir string, env []string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %v", strings.Join(args, " "), err)
	}
	return nil
}

// goDeps are the repository's directories of the packages pkg builds from
// for goos, less vendor/ (in common).
func goDeps(goos, pkg string) []string {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", pkg)
	cmd.Dir, cmd.Env, cmd.Stderr = src, append(os.Environ(), "GOOS="+goos), os.Stderr
	b, err := cmd.Output()
	must(err)
	var dirs []string
	for _, d := range strings.Fields(string(b)) {
		if d = strings.TrimPrefix(d, src+"/"); !strings.HasPrefix(d, "vendor/") {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func git(args ...string) (string, error) {
	b, err := exec.Command("git", append([]string{"-c", "safe.directory=*", "-C", src}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v", args[0], err)
	}
	return string(b), nil
}

// install copies a file. Mode 0 means 0755 if the file is executable,
// otherwise 0644, whatever the checkout's umask.
func install(from, to string, mode fs.FileMode) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if mode == 0 {
		fi, _ := os.Stat(from)
		mode = 0o644
		if fi.Mode()&0o100 != 0 {
			mode = 0o755
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	os.Remove(to)
	if err := os.WriteFile(to, b, mode); err != nil {
		return err
	}
	return os.Chmod(to, mode)
}

// hashFile adds a file's path, mode and contents to a cache key.
func hashFile(h io.Writer, p string) {
	fi, _ := os.Stat(p)
	if fmt.Fprintln(h, p); fi != nil {
		fmt.Fprintln(h, fi.Mode())
	}
	if f, err := os.Open(p); err == nil {
		io.Copy(h, f)
		f.Close()
	}
}

// chown gives what the build wrote to the host user (OWNER, uid:gid).
func chown() {
	var uid, gid int
	if _, err := fmt.Sscanf(os.Getenv("OWNER"), "%d:%d", &uid, &gid); err != nil {
		return
	}
	filepath.WalkDir(out, func(p string, _ fs.DirEntry, _ error) error { os.Lchown(p, uid, gid); return nil })
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "build: "+format+"\n", a...)
	chown()
	os.Exit(1)
}
