// Package bundle packs a Docker Compose project into a timeskip bundle: a tar
// archive, padded to a whole number of MiB so it can be a VM disk, with
//
//	format            the bundle format's version, Format
//	arch              the target architecture (arm64 or amd64)
//	images            "image id [digest...]" per image: what was bundled, with
//	                  registry digests for pulled images
//	compose.yaml      the resolved project, without build: and pull policies,
//	                  naming images pulled by digest as the VM knows them
//	settings          (without Options.Cache) the project's x-timeskip
//	                  block, JSON, which compose.yaml leaves out
//	images.tar        every service's image (docker save, with gzipped
//	                  layers stored inflated, under the same names); with
//	                  Options.Cache, the images the project builds
//	images-amd64.tar  (for arm64) the same, of the images for linux/amd64,
//	                  which the VM runs emulated
//	pulled            (with Options.Cache) the other images, as in
//	                  images: the VM has them already
//	mounts            the digest of mounted/N/: the files (or directory) the
//	                  project bind-mounts, or names as file: configs and
//	                  secrets, N in order of their first use
//	syncs             (with Options.Synced) the sync rules whose files are in
//	                  synced/N/, N their index, and those files' digest
package bundle

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanworl/timeskip/internal/archive"
	"github.com/ryanworl/timeskip/internal/protocol"
)

// Format is the version of the bundle format. A change that an older
// timeskip can't read, or that can't read older bundles, increments it.
const Format = "1"

type service struct {
	Image      string          `json:"image"`
	Build      json.RawMessage `json:"build"`
	PullPolicy string          `json:"pull_policy"`
	Platform   string          `json:"platform"`
	Volumes    []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	} `json:"volumes"`
	Develop struct {
		Watch []watchRule `json:"watch"`
	} `json:"develop"`
}

type project struct {
	Services map[string]service                 `json:"services"`
	Configs  map[string]source                  `json:"configs"`
	Secrets  map[string]source                  `json:"secrets"`
	Networks map[string]struct{ Internal bool } `json:"networks"`
}

// A source is a config's or secret's.
type source struct{ File, Environment string }

// guestPaths are the paths a service may bind-mount: the VM's own, not the
// host's.
var guestPaths = []string{"/var/run/docker.sock", "/run/docker.sock", "/proc", "/sys", "/dev"}

// The build: options bundle supports.
type buildSpec struct {
	Context    string            `json:"context"`
	Dockerfile string            `json:"dockerfile"`
	Args       map[string]string `json:"args"`
	Target     string            `json:"target"`
}

// dockerfile is the path of b's Dockerfile.
func (b *buildSpec) dockerfile() string {
	if f := cmp.Or(b.Dockerfile, "Dockerfile"); !filepath.IsAbs(f) {
		return filepath.Join(b.Context, f)
	} else {
		return f
	}
}

// Options says how Create bundles.
type Options struct {
	// Cache is for timeskip test's bundle of a compose file in the cache: it builds only
	// the images whose build inputs (see buildInputs) changed since they
	// were built (recorded in out.inputs, with what their sync rules' paths
	// held then, and their files' digests in out.sums), and leaves out of
	// images.tar the images the project doesn't build (listed in the
	// pulled file instead), for a VM that has them already. Without
	// Docker (see Create), an image built from unchanged recorded inputs
	// is the one out holds.
	Cache bool
	// Synced packs what each sync rule's path holds, so that the bundle
	// tested elsewhere gets the files a test here would sync.
	Synced bool
	// NoBuild uses the services' images as they are, ignoring build:, as
	// Antithesis does: pulled per pull_policy, or Docker's already.
	NoBuild bool
	// Settings, with Cache, receives the project's x-timeskip block (JSON),
	// which the bundle leaves out: a setting's change changes no image nor
	// setup.
	Settings *string
	// Tmp is where Create works (os.TempDir() if empty).
	Tmp string
	Log io.Writer
}

// Create bundles the project in composeFile for linux/arch into out, and
// returns its develop.watch sync rules. The same files and images leave out
// as it is, so that what timeskip prepared from it stays valid. If Docker's
// daemon can't be reached, out is used as it is if that needs nothing from
// Docker: no image to build (see Options.Cache), the images it doesn't
// build as out has them, and nothing else changed.
func Create(composeFile, arch, out string, o Options) ([]Sync, error) {
	plat, log := "linux/"+arch, o.Log
	tmp, err := os.MkdirTemp(o.Tmp, "timeskip-bundle")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	// Resolve the project (paths, env, anchors) into one self-contained
	// file, as Antithesis does: with the project's .env, not the shell's
	// variables.
	cmd := []string{"compose", "-f", composeFile, "--profile", "*", "config", "--format", "json"}
	full, err := outputEnv(hermetic(), "docker", cmd...)
	if err != nil {
		return nil, err
	}
	if shell, err := output("docker", cmd...); err == nil && !bytes.Equal(shell, full) {
		fmt.Fprintln(log, "note: the project uses variables from your shell, which Antithesis doesn't see; timeskip ignores them too: set them in the project's .env")
	}
	// (What the host uses of it, unescaped: config writes a literal $ as
	// $$, for Compose to interpolate again, as the guest's does.)
	var p project
	if err := json.Unmarshal(bytes.ReplaceAll(full, []byte("$$"), []byte("$")), &p); err != nil {
		return nil, err
	}
	names := slices.Sorted(maps.Keys(p.Services))
	if o.NoBuild {
		for _, n := range names {
			s := p.Services[n]
			s.Build = nil
			p.Services[n] = s
		}
	}
	// Each image's platform: its services' platform:, else the target's
	// (but see the pulls). On arm64, linux/amd64 images run emulated, so a
	// project for Antithesis, which runs only those, runs here too.
	plats, explicit := map[string]string{}, map[string]bool{}
	for _, n := range names {
		s := p.Services[n]
		ip := cmp.Or(s.Platform, plat)
		if ip != plat && (arch != "arm64" || ip != "linux/amd64") {
			return nil, fmt.Errorf("service %s: platform %s: timeskip runs %s images here", n, ip, plat)
		}
		if q, ok := plats[s.Image]; ok && q != ip {
			return nil, fmt.Errorf("image %s is for platforms %s and %s: give its services one", s.Image, q, ip)
		}
		plats[s.Image], explicit[s.Image] = ip, explicit[s.Image] || s.Platform != ""
	}
	for _, n := range slices.Sorted(maps.Keys(p.Networks)) {
		if p.Networks[n].Internal {
			return nil, fmt.Errorf("network %s is internal, which Antithesis doesn't allow (its containers couldn't reach the Antithesis network): remove internal: true", n)
		}
	}
	// Host files the project mounts go in the bundle, if they're the
	// project's: in its directory, as Antithesis's config image holds them.
	abs, _ := filepath.Abs(composeFile)
	pdir, mounts := filepath.Dir(abs), map[string]string{}
	mount := func(what, src string) error {
		real, err := filepath.EvalSymlinks(src) // (what a symlink mounts, as Docker mounts it)
		if err != nil {
			real = src
		}
		if rel, err := filepath.Rel(pdir, real); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("%s %s, outside the project's directory %s, which the VM doesn't have: move it into the project", what, src, pdir)
		}
		if _, ok := mounts[src]; !ok {
			mounts[src] = "mounted/" + strconv.Itoa(len(mounts))
		}
		return nil
	}
	specs := map[string]*buildSpec{}
	var syncs []Sync
	for _, name := range names {
		s := p.Services[name]
		for _, v := range s.Volumes {
			if v.Type == "bind" && !slices.ContainsFunc(guestPaths, func(g string) bool { return v.Source == g || strings.HasPrefix(v.Source, g+"/") }) {
				if err := mount("service "+name+" bind-mounts", v.Source); err != nil {
					return nil, err
				}
			}
		}
		if s.Build != nil {
			specs[name] = &buildSpec{}
			if err := strict(s.Build, specs[name]); err != nil {
				return nil, fmt.Errorf("service %s: build: takes only context, dockerfile, args and target: %v", name, err)
			}
			if s.Image == "" {
				return nil, fmt.Errorf("service %s has build: but no image: name", name)
			}
			if fi, err := os.Stat(specs[name].Context); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("service %s: build: context %s isn't a directory here", name, specs[name].Context)
			}
		}
		for i, w := range s.Develop.Watch {
			if w.Action == "sync+exec" {
				return nil, fmt.Errorf("service %s: develop.watch: timeskip doesn't run sync+exec hooks: use sync+restart, or sync and a test command", name)
			}
			if !strings.HasPrefix(w.Action, "sync") {
				continue
			}
			skip, err := skipper(w, specs[name])
			if err != nil {
				return nil, fmt.Errorf("service %s: develop.watch: %v", name, err)
			}
			syncs = append(syncs, Sync{Service: name, Path: w.Path, Target: w.Target, Action: w.Action, Rule: i, Skip: skip})
		}
	}

	for _, kind := range []string{"config", "secret"} {
		m := map[string]map[string]source{"config": p.Configs, "secret": p.Secrets}[kind]
		for _, name := range slices.Sorted(maps.Keys(m)) {
			if e := m[name].Environment; e != "" {
				return nil, fmt.Errorf("%s %s comes from the variable %s, which the VM doesn't have: use file: or content: instead", kind, name, e)
			}
			if f := m[name].File; f != "" {
				if err := mount(kind+" "+name+" is the host file", f); err != nil {
					return nil, err
				}
			}
		}
	}

	// Synced is what each sync rule's path held when the image was built,
	// so that a file deleted since is deleted in the container too.
	type record struct {
		Inputs, Image string
		Synced        map[int][]string
	}
	state, sums := map[string]record{}, fileSums{map[string]string{}, map[string]string{}}
	// down is why Docker's daemon can't be reached, if it can't; then had
	// is out's images, each name's line in its images file, which it must
	// already hold (see Create).
	var down error
	had := map[string]string{}
	if o.Cache {
		b, _ := os.ReadFile(out + ".inputs")
		json.Unmarshal(b, &state)
		b, _ = os.ReadFile(out + ".sums")
		json.Unmarshal(b, &sums.was)
	}
	if _, err := output("docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		e := strings.TrimSpace(err.Error())
		down = errors.New(e[strings.LastIndexByte(e, '\n')+1:])
		for _, l := range strings.Split(Files(out)["images"], "\n") {
			if img, id, ok := strings.Cut(l, " "); ok {
				had[img] = id
			}
		}
	}
	needsDocker := func(why string) error {
		return fmt.Errorf("%s, and Docker's daemon isn't reachable (%v): start Docker", why, down)
	}
	for _, name := range names {
		s, b := p.Services[name], specs[name]
		if b == nil {
			continue
		}
		var in string
		if o.Cache {
			if in, err = buildInputs(name, s, b, syncs, &sums); err != nil {
				return nil, fmt.Errorf("service %s: %v", name, err)
			}
			// (Without Docker, the image built from these inputs is the one
			// out holds, recorded when it was built, which build checked
			// reached Docker's images.)
			r := state[name]
			if r.Inputs == in && down != nil && r.Image != "" && r.Image == had[s.Image] {
				continue
			}
			if r.Inputs == in && down == nil && r.Image == imageID(s.Image, plats[s.Image]) {
				continue
			}
		}
		if down != nil {
			return nil, needsDocker(fmt.Sprintf("%s needs building (its build inputs changed, or the cached bundle doesn't hold the image built from them)", s.Image))
		}
		if err := build(s.Image, *b, plats[s.Image], log); err != nil {
			return nil, fmt.Errorf("building %s: %v", s.Image, err)
		}
		r := record{in, imageID(s.Image, plats[s.Image]), map[int][]string{}}
		for _, y := range syncs {
			if y.Service == name {
				if r.Synced[y.Rule], err = y.Files(); err != nil {
					return nil, fmt.Errorf("service %s: develop.watch: %v", name, err)
				}
			}
		}
		state[name] = r
	}
	for i, y := range syncs {
		was := state[y.Service].Synced[y.Rule]
		if len(was) == 0 {
			continue
		}
		now, err := y.Files()
		if err != nil {
			return nil, fmt.Errorf("service %s: develop.watch: %v", y.Service, err)
		}
		for _, f := range was {
			if !slices.Contains(now, f) {
				syncs[i].Deleted = append(syncs[i].Deleted, f)
			}
		}
	}
	if o.Cache {
		b, _ := json.Marshal(state)
		if err := os.WriteFile(out+".inputs", b, 0o644); err != nil {
			return nil, err
		}
		b, _ = json.Marshal(sums.now)
		if err := os.WriteFile(out+".sums", b, 0o644); err != nil {
			return nil, err
		}
	}
	// Images that aren't built are pulled as Compose would, by each
	// service's pull_policy.
	var images []string
	built, policies := map[string]bool{}, map[string][]string{}
	for _, s := range p.Services {
		images = append(images, s.Image)
		if s.Build != nil {
			built[s.Image] = true
		} else {
			policies[s.Image] = append(policies[s.Image], s.PullPolicy)
		}
	}
	slices.Sort(images)
	images = slices.Compact(images)
	var ids []string
	tags := map[string][]string{} // the names images.tar gives images by digest (see normalizeSave)
	for _, img := range images {
		if down != nil { // (as out has it, pulled or built)
			id, ok := had[img]
			if !ok {
				return nil, needsDocker("the cached bundle doesn't hold " + img)
			}
			ids = append(ids, img+" "+id)
			continue
		}
		if !built[img] {
			err := pull(img, plats[img], policies[img], log)
			if err != nil && arch == "arm64" && !explicit[img] && noPlatform(err) {
				// (An image Antithesis could run, for amd64 only.)
				fmt.Fprintf(log, "%s has no linux/arm64 image; its linux/amd64 one runs emulated\n", img)
				plats[img] = "linux/amd64"
				err = pull(img, plats[img], policies[img], log)
			}
			if err != nil {
				return nil, err
			}
		}
		// A built image's digests cover its build's attestations, which
		// change with every build; its ID is its content.
		format := "{{.Id}} {{range .RepoDigests}}{{.}} {{end}}"
		if built[img] {
			format = "{{.Id}}"
		}
		id, err := output("docker", "image", "inspect", "--platform", plats[img], "--format", format, img)
		if err != nil {
			return nil, err
		}
		ids = append(ids, img+" "+strings.TrimSpace(string(id)))
		if g := guestRef(img); g != img {
			tags[g] = []string{strings.Fields(string(id))[0], img[strings.Index(img, "@")+1:]}
		}
	}
	// What the guest needs: the compose file without build: and pull
	// policies (it has no registry or build context).
	var doc map[string]any
	json.Unmarshal(full, &doc)
	for _, s := range doc["services"].(map[string]any) {
		s.(map[string]any)["image"] = guestRef(fmt.Sprint(s.(map[string]any)["image"]))
		delete(s.(map[string]any), "build")
		delete(s.(map[string]any), "pull_policy")
		// (The guest uses only the rules' targets: their paths, relative,
		// keep the host's directory out.)
		dev, _ := s.(map[string]any)["develop"].(map[string]any)
		ws, _ := dev["watch"].([]any)
		for _, w := range ws {
			if rel, err := filepath.Rel(pdir, unescape(w.(map[string]any)["path"])); err == nil {
				w.(map[string]any)["path"] = strings.ReplaceAll(rel, "$", "$$")
			}
		}
		vs, _ := s.(map[string]any)["volumes"].([]any)
		for _, v := range vs {
			if m := mounts[unescape(v.(map[string]any)["source"])]; m != "" && v.(map[string]any)["type"] == "bind" {
				v.(map[string]any)["source"] = protocol.BundleDir + "/" + m
			}
		}
	}
	for _, kind := range []string{"configs", "secrets"} {
		m, _ := doc[kind].(map[string]any)
		for _, c := range m {
			if f := mounts[unescape(c.(map[string]any)["file"])]; f != "" {
				c.(map[string]any)["file"] = protocol.BundleDir + "/" + f
			}
		}
	}
	settings, _ := json.Marshal(doc["x-timeskip"])
	delete(doc, "x-timeskip")
	compose, _ := json.MarshalIndent(doc, "", "  ")
	files := map[string]string{"format": Format + "\n", "arch": arch + "\n", "images": lines(ids), "compose.yaml": string(compose) + "\n"}
	if o.Cache {
		*o.Settings = string(settings)
	} else {
		files["settings"] = string(settings) + "\n"
	}
	dir := filepath.Join(tmp, "b")
	os.Mkdir(dir, 0o755)
	if len(mounts) > 0 {
		digest, err := packMounts(mounts, dir)
		if err != nil {
			return nil, err
		}
		files["mounts"] = digest
	}
	if o.Synced {
		m, err := packSynced(syncs, filepath.Join(dir, "synced"))
		if err != nil {
			return nil, err
		}
		files["syncs"] = m
	}
	saved := images
	if o.Cache {
		saved = nil
		var pulled []string
		for i, img := range images {
			if built[img] {
				saved = append(saved, img)
			} else {
				pulled = append(pulled, ids[i])
			}
		}
		files["pulled"] = lines(pulled)
	}
	if maps.Equal(Files(out), files) {
		fmt.Fprintf(log, "%s is up to date\n", out)
		if down != nil {
			fmt.Fprintf(log, "note: Docker's daemon isn't reachable: the bundle's images are those built from their unchanged build inputs, and the others as they were pulled then\n")
		}
		return syncs, nil
	}
	if down != nil {
		return nil, needsDocker("the cached bundle would change (the project changed)")
	}
	fmt.Fprintf(log, "saving %d images:\n  %s\n", len(saved), strings.Join(saved, "\n  "))
	groups := map[string][]string{plat: nil} // (images.tar, if empty)
	for _, img := range saved {
		groups[plats[img]] = append(groups[plats[img]], img)
	}
	for q, imgs := range groups {
		a := strings.TrimPrefix(q, "linux/")
		name := map[bool]string{true: "images.tar", false: "images-" + a + ".tar"}[q == plat]
		if err := save(filepath.Join(dir, name), q, imgs); err != nil {
			return nil, err
		}
		if err := normalizeSave(filepath.Join(dir, name), filepath.Join(tmp, "img-"+a), q, tags); err != nil {
			return nil, err
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	// (Written aside, in tmp, then renamed: a bundle is either the old one
	// or whole, and what a write that's interrupted leaves goes with tmp.)
	f, err := os.Create(filepath.Join(tmp, "bundle.tsb"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := archive.Tar(f, dir, time.Unix(0, 0)); err != nil {
		return nil, err
	}
	// Disk images must be a whole number of MiB; zero padding is a valid tar end.
	size, _ := f.Seek(0, io.SeekCurrent)
	size = (size>>20 + 1) << 20
	if err := f.Truncate(size); err != nil {
		return nil, err
	}
	if err := f.Chmod(0o644); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "wrote %s (%d MiB, %s)\n", out, size>>20, arch)
	return syncs, place(f.Name(), out)
}

// place renames file p to out, or across file systems copies it beside out
// and renames the copy.
func place(p, out string) error {
	err := os.Rename(p, out)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	src, err := os.Open(p)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.CreateTemp(filepath.Dir(out), ".bundle-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, src)
	if err = cmp.Or(err, f.Chmod(0o644), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), out)
}

// imageID is an image's ID, or "" if there's no such image.
func imageID(img, plat string) string {
	id, _ := output("docker", "image", "inspect", "--platform", plat, "--format", "{{.Id}}", img)
	return strings.TrimSpace(string(id))
}

// save writes images for plat to path with docker save, and checks that the
// archive has every image's config and layers: Docker can have an image's
// metadata without all its content, and then save fails or, unfiltered by
// platform, leaves the content out.
func save(path, plat string, images []string) error {
	if len(images) == 0 { // (docker save wants one; docker load takes none)
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		w := tar.NewWriter(f)
		w.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: 2})
		w.Write([]byte("[]"))
		return cmp.Or(w.Close(), f.Close())
	}
	incomplete := fmt.Sprintf("Docker has the image but not all its content for %s: remove it and pull it again (docker image rm IMAGE, then docker pull --platform %s IMAGE), or rebuild it", plat, plat)
	if _, err := output("docker", append([]string{"save", "--platform", plat, "-o", path}, images...)...); err != nil {
		return fmt.Errorf("%v\n(If it says an image doesn't provide the platform: %s.)", err, incomplete)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var m []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	have := map[string]bool{}
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		have[h.Name] = true
		if h.Name == "manifest.json" {
			if err := json.NewDecoder(r).Decode(&m); err != nil {
				return fmt.Errorf("%s: manifest.json: %v", path, err)
			}
		}
	}
	for _, e := range m {
		for _, b := range append([]string{e.Config}, e.Layers...) {
			if !have[b] {
				return fmt.Errorf("docker save left %s out of %s: %s", b, strings.Join(e.RepoTags, ", "), incomplete)
			}
		}
	}
	return nil
}

// buildPolicy makes a build the same on any host from the same files and
// base images: timestamps at 0 (the config's, and with the containerd image
// store the files'; the build's own SOURCE_DATE_EPOCH wins), and no
// provenance attestation. (buildInputs digests it: an image built otherwise
// is built again.)
var buildPolicy = []string{"--output", "type=image,unpack=false,rewrite-timestamp=true", "--provenance=false", "--build-arg", "SOURCE_DATE_EPOCH=0"}

// build builds an image.
func build(img string, b buildSpec, plat string, log io.Writer) error {
	ctx := b.Context
	args := append([]string{"build", "-q"}, buildPolicy...)
	args = append(args, "-t", img, "--platform", plat, "-f", b.dockerfile())
	if b.Target != "" {
		args = append(args, "--target", b.Target)
	}
	for _, k := range slices.Sorted(maps.Keys(b.Args)) {
		args = append(args, "--build-arg", k+"="+b.Args[k])
	}
	fmt.Fprintf(log, "building %s (%s)\n", img, plat)
	id, err := output("docker", append(args, ctx)...)
	if err != nil {
		return err
	}
	// (A builder of its own, not Docker's, keeps the image to itself, and
	// Docker's image of that name, if any, is an old one.)
	if got := imageID(img, plat); got != strings.TrimSpace(string(id)) {
		return fmt.Errorf("the build of %s didn't reach Docker's images (it has %q): use a builder with the docker driver (docker buildx ls, then docker buildx use NAME; and no BUILDX_BUILDER)", img, got)
	}
	return nil
}

// normalizeSave re-packs a docker save archive reproducibly: docker lists
// the images in random order and stamps the entries with the current time.
// It also names images that docker leaves unnamed, those pulled by digest
// (name@sha256:...): names has the name to give each, and the digests that
// may lead to its config (its ID and its reference's digest; which one
// does depends on Docker's image store).
func normalizeSave(path, dir, plat string, names map[string][]string) error {
	os.MkdirAll(dir, 0o755)
	if _, err := output("tar", "-xf", path, "-C", dir); err != nil {
		return err
	}
	get := func(v any, keys ...string) string {
		for _, k := range keys {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		return fmt.Sprint(v)
	}
	sortBy := func(l []any, keys ...string) {
		slices.SortFunc(l, func(a, b any) int { return strings.Compare(get(a, keys...), get(b, keys...)) })
	}
	tags := map[string][]any{}
	for name, digests := range names {
		for _, d := range digests {
			if c := configOf(dir, d, plat); c != "" {
				tags[c] = append(tags[c], name)
				break
			}
		}
	}
	err := editJSON(filepath.Join(dir, "manifest.json"), func(v any) {
		for _, e := range v.([]any) {
			e := e.(map[string]any)
			// (Sorted: tags comes in a map's order, Docker's in its own.)
			l, _ := e["RepoTags"].([]any)
			if l = append(l, tags[get(e, "Config")]...); l != nil {
				slices.SortFunc(l, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
				e["RepoTags"] = slices.Compact(l)
			}
		}
		sortBy(v.([]any), "RepoTags")
	})
	if err != nil {
		return err
	}
	err = editJSON(filepath.Join(dir, "index.json"), func(v any) {
		if l, ok := v.(map[string]any)["manifests"].([]any); ok {
			sortBy(l, "annotations", "io.containerd.image.name")
		}
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := inflate(dir); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := archive.Tar(f, dir, time.Unix(0, 0)); err != nil {
		return err
	}
	return f.Close()
}

// inflate stores an extracted docker save archive's gzipped layers inflated,
// under the same names: docker load inflates what it finds either way (and
// checks the layers' uncompressed digests), and the VM, which has one CPU,
// would otherwise spend most of a setup inflating them.
func inflate(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var m []struct{ Layers []string }
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	done := map[string]bool{}
	for _, e := range m {
		for _, l := range e.Layers {
			if done[l] {
				continue
			}
			done[l] = true
			if err := inflateFile(filepath.Join(dir, l)); err != nil {
				return fmt.Errorf("%s: %v", l, err)
			}
		}
	}
	return nil
}

func inflateFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return nil
	}
	f.Seek(0, io.SeekStart)
	z, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	o, err := os.Create(path + ".inflated")
	if err != nil {
		return err
	}
	defer o.Close()
	if _, err := io.Copy(o, z); err != nil {
		return err
	}
	if err := o.Close(); err != nil {
		return err
	}
	return os.Rename(path+".inflated", path)
}

// configOf follows digest d in an extracted docker save archive, through an
// index's manifest for plat, to an image's config file: "blobs/sha256/HEX",
// or "" if the archive doesn't have it.
func configOf(dir, d, plat string) string {
	f := "blobs/sha256/" + strings.TrimPrefix(d, "sha256:")
	var v struct {
		Config    struct{ Digest string }
		Manifests []struct {
			Digest   string
			Platform struct{ OS, Architecture string }
		}
		RootFS any `json:"rootfs"`
	}
	b, err := os.ReadFile(filepath.Join(dir, f))
	if err != nil || json.Unmarshal(b, &v) != nil {
		return ""
	}
	switch {
	case v.RootFS != nil:
		return f
	case v.Config.Digest != "":
		return configOf(dir, v.Config.Digest, plat)
	}
	for _, m := range v.Manifests {
		if m.Platform.OS+"/"+m.Platform.Architecture == plat {
			return configOf(dir, m.Digest, plat)
		}
	}
	return ""
}

// guestRef is the name the VM's Docker knows image img by: img, or for one
// pulled by digest, which docker save and load don't keep, its repository
// with the digest as its tag (redis@sha256:HEX -> redis:sha256-HEX).
func guestRef(img string) string {
	at := strings.Index(img, "@")
	if at < 0 {
		return img
	}
	repo := img[:at]
	if c := strings.LastIndex(repo, ":"); c > strings.LastIndex(repo, "/") {
		repo = repo[:c]
	}
	return repo + ":" + strings.Replace(img[at+1:], ":", "-", 1)
}

// ID identifies the bundle at path by what its VMs' setup sees: its files
// besides images.tar, which name its images by ID, the synced files and
// the settings.
func ID(path string) string {
	files := Files(path)
	delete(files, "syncs")
	delete(files, "settings")
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%s %d\n%s", name, len(files[name]), files[name])
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// images says whether a bundle's file is an images archive, images.tar or
// images-ARCH.tar.
func images(name string) bool {
	return strings.HasPrefix(name, "images") && strings.HasSuffix(name, ".tar") && !strings.Contains(name, "/")
}

// unescape is a string of config's output as the host uses it (see Create).
func unescape(v any) string { return strings.ReplaceAll(fmt.Sprint(v), "$$", "$") }

// Files reads a bundle's files besides its images archives, synced/ and mounted/ (nil if
// there's no bundle, or it can't be read).
func Files(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r, files := tar.NewReader(f), map[string]string{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			return nil
		}
		if images(h.Name) || strings.HasPrefix(h.Name, "synced/") || strings.HasPrefix(h.Name, "mounted/") {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return nil
		}
		files[h.Name] = string(b)
	}
}

// SyncedRule is a sync rule packed into a bundle (see Options.Synced).
type SyncedRule struct {
	Service, Target, Action string
}

// packSynced copies what each sync rule's path holds, less what it leaves
// out, to dir/N, and returns the bundle's syncs file: the rules, and the
// files' digest.
func packSynced(syncs []Sync, dir string) (string, error) {
	var m struct {
		Rules  []SyncedRule
		Digest string
	}
	h := sha256.New()
	for i, y := range syncs {
		m.Rules = append(m.Rules, SyncedRule{y.Service, y.Target, y.Action})
		var errs []error
		err := Walk(y.Path, y.Skip, func(p string, fi fs.FileInfo) {
			if fi.IsDir() {
				return
			}
			rel, _ := filepath.Rel(y.Path, p)
			dst := filepath.Join(dir, strconv.Itoa(i), rel)
			errs = append(errs, os.MkdirAll(filepath.Dir(dst), 0o755))
			fmt.Fprintf(h, "%d %s %v\n", i, rel, fi.Mode())
			if fi.Mode()&fs.ModeSymlink != 0 {
				l, err := os.Readlink(p)
				fmt.Fprintln(h, l)
				errs = append(errs, err, os.Symlink(l, dst))
				return
			}
			b, err := os.ReadFile(p)
			h.Write(b)
			errs = append(errs, err, os.WriteFile(dst, b, fi.Mode().Perm()))
		})
		if err = cmp.Or(err, errors.Join(errs...)); err != nil {
			return "", fmt.Errorf("service %s: develop.watch: %v", y.Service, err)
		}
	}
	m.Digest = fmt.Sprintf("%x", h.Sum(nil))
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b) + "\n", nil
}

// packMounts copies each mount's host path (a file or a directory, or
// nothing yet: Docker makes an empty directory) to its name under dir, and
// returns a digest of them.
func packMounts(mounts map[string]string, dir string) (string, error) {
	h := sha256.New()
	for _, src := range slices.Sorted(maps.Keys(mounts)) {
		dst := filepath.Join(dir, mounts[src])
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			fmt.Fprintf(h, "%s (none)\n", mounts[src])
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return "", err
			}
			continue
		}
		root, err := filepath.EvalSymlinks(src) // (what a symlink mounts)
		if err != nil {
			return "", err
		}
		// (Modes as they are, past the umask: a directory's once it's
		// filled.)
		mode := func(fi fs.FileInfo) fs.FileMode {
			return fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		}
		var errs []error
		dirs := map[string]fs.FileMode{}
		err = Walk(root, nil, func(p string, fi fs.FileInfo) {
			rel, _ := filepath.Rel(root, p)
			to := filepath.Join(dst, rel)
			fmt.Fprintf(h, "%s %s %v\n", mounts[src], rel, fi.Mode())
			switch {
			case fi.IsDir():
				dirs[to] = mode(fi)
				errs = append(errs, os.MkdirAll(to, 0o700))
			case fi.Mode()&fs.ModeSymlink != 0:
				l, err := os.Readlink(p)
				fmt.Fprintln(h, l)
				errs = append(errs, err, os.MkdirAll(filepath.Dir(to), 0o755), os.Symlink(l, to))
			case fi.Mode().IsRegular():
				b, err := os.ReadFile(p)
				h.Write(b)
				errs = append(errs, err, os.MkdirAll(filepath.Dir(to), 0o755), os.WriteFile(to, b, 0o600), os.Chmod(to, mode(fi)))
			}
		})
		for d, m := range dirs {
			errs = append(errs, os.Chmod(d, m))
		}
		if err = cmp.Or(err, errors.Join(errs...)); err != nil {
			return "", fmt.Errorf("packing %s: %v", src, err)
		}
	}
	return fmt.Sprintf("%x\n", h.Sum(nil)), nil
}

// pull pulls img for plat if its services' pull_policy says to (see
// mustPull).
func pull(img, plat string, policies []string, log io.Writer) error {
	if p, err := mustPull(img, plat, policies); err != nil || !p {
		return err
	}
	fmt.Fprintf(log, "pulling %s (%s)\n", img, plat)
	_, err := output("docker", "pull", "-q", "--platform", plat, img)
	return err
}

// noPlatform says whether err is a registry's: no image for that platform.
func noPlatform(err error) bool {
	return strings.Contains(err.Error(), "no matching manifest") || strings.Contains(err.Error(), "does not match the specified platform")
}

// mustPull says whether to pull img, given the pull_policy of each service
// that uses it: always; missing (the default), if_not_present or build,
// when it isn't local; never, when it must be local. (Time-based policies
// aren't supported.)
func mustPull(img, plat string, policies []string) (bool, error) {
	_, err := output("docker", "image", "inspect", "--platform", plat, "--format", "{{.Id}}", img)
	local := err == nil
	for _, p := range policies {
		switch p {
		case "always":
			return true, nil
		case "never":
			if !local {
				return false, fmt.Errorf("%s isn't local, and its pull_policy is never", img)
			}
		case "", "missing", "if_not_present", "build":
			if !local {
				return true, nil
			}
		default:
			return false, fmt.Errorf("%s: pull_policy %s isn't supported: use always, missing or never", img, p)
		}
	}
	return false, nil
}

// strict decodes JSON (if any) into v, rejecting fields v doesn't have.
func strict(raw json.RawMessage, v any) error {
	if raw == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func editJSON(path string, edit func(any)) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	edit(v)
	b, _ = json.Marshal(v)
	return os.WriteFile(path, b, 0o644)
}

func lines(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return strings.Join(l, "\n") + "\n"
}

// output runs a command and returns its stdout; its error includes stderr.
func output(name string, args ...string) ([]byte, error) { return outputEnv(nil, name, args...) }

// outputEnv is output with only the environment env (nil: this process's).
func outputEnv(env []string, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr, cmd.Env = &stderr, env
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v\n%s", name, args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// hermetic is the environment Compose renders the project in: what the
// Docker CLI needs to reach the daemon, and none of the shell's variables.
func hermetic() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || k == "TMPDIR" || k == "XDG_RUNTIME_DIR" || strings.HasPrefix(k, "DOCKER_") {
			env = append(env, kv)
		}
	}
	return env
}
