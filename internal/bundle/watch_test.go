package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A sync rule leaves out what docker compose watch leaves out.
func TestSkip(t *testing.T) {
	ctx := t.TempDir()
	os.WriteFile(filepath.Join(ctx, ".dockerignore"), []byte("*.log\n!keep.log\nbuild/\n"), 0o644)
	all, err := skipper(watchRule{Path: ctx, Ignore: []string{"node_modules/", "**/*.tmp"}}, &buildSpec{Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	goOnly, err := skipper(watchRule{Path: ctx, Include: []string{"*.go"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		skip func(string, bool) bool
		path string
		want bool
	}{
		{all, "main.go", false},
		{all, "app.log", true},           // .dockerignore
		{all, "keep.log", false},         // its exclusion
		{all, "sub/app.log", false},      // its patterns are anchored at the context
		{all, "build/x/y", true},         // inside an ignored directory
		{all, "node_modules/a.js", true}, // the rule's ignore, relative to its path
		{all, "a/b/c.tmp", true},         // **
		{all, ".git/HEAD", true},
		{all, "sub/.git/config", true},
		{all, "sub/.main.go.swp", true}, // an editor's temporary file
		{all, "main.go~", true},
		{goOnly, "main.go", false},
		{goOnly, "README.md", true}, // not included
		{goOnly, "sub/x.go", true},  // include patterns are anchored too
	} {
		if got := c.skip(filepath.Join(ctx, c.path), false); got != c.want {
			t.Errorf("%s: skipped %v, want %v", c.path, got, c.want)
		}
	}
}

// A directory is pruned only when nothing under it can be synced: not for
// failing to match an include pattern, nor when an exclusion brings back
// something inside it.
func TestFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"pkg/server.go", "pkg/README.md", "gen/out.txt", "gen/keep/k.txt"} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), nil, 0o644)
	}
	for _, c := range []struct {
		rule watchRule
		want []string
	}{
		{watchRule{Path: root, Include: []string{"**/*.go"}}, []string{"pkg/server.go"}},
		{watchRule{Path: root, Ignore: []string{"gen", "!gen/keep"}}, []string{"gen/keep/k.txt", "pkg/README.md", "pkg/server.go"}},
	} {
		skip, err := skipper(c.rule, nil)
		if err != nil {
			t.Fatal(err)
		}
		files, err := (Sync{Path: root, Skip: skip}).Files()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range files {
			r, _ := filepath.Rel(root, f)
			got = append(got, r)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%+v: synced %v, want %v", c.rule, got, c.want)
		}
	}
}

// A build context's .dockerignore keeps docker build's meaning: "*" with
// exclusions is a whitelist. Rewriting a file with the same contents
// changes nothing; a new directory, even empty, or a directory's mode
// does, unless a sync rule delivers it.
func TestBuildInputs(t *testing.T) {
	ctx := t.TempDir()
	os.WriteFile(filepath.Join(ctx, ".dockerignore"), []byte("*\n!src\n"), 0o644)
	os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	os.Mkdir(filepath.Join(ctx, "src"), 0o755)
	os.WriteFile(filepath.Join(ctx, "src", "a"), []byte("a"), 0o644)
	b := &buildSpec{Context: ctx}
	sums := fileSums{map[string]string{}, map[string]string{}}
	var syncs []Sync
	inputs := func() string {
		t.Helper()
		in, err := buildInputs("s", service{}, b, syncs, &sums)
		if err != nil {
			t.Fatal(err)
		}
		sums = fileSums{sums.now, map[string]string{}}
		return in
	}
	before := inputs()
	os.WriteFile(filepath.Join(ctx, "notes.txt"), []byte("ignored"), 0o644)
	if inputs() != before {
		t.Error("a file .dockerignore leaves out changed the build inputs")
	}
	os.WriteFile(filepath.Join(ctx, "src", "a"), []byte("a"), 0o644)
	if inputs() != before {
		t.Error("rewriting a file with the same contents changed the build inputs")
	}
	os.WriteFile(filepath.Join(ctx, "src", "a"), []byte("b"), 0o644)
	if inputs() == before {
		t.Error("a change in the context didn't change the build inputs")
	}
	before = inputs()
	os.Mkdir(filepath.Join(ctx, "src", "empty"), 0o755)
	if inputs() == before {
		t.Error("a new empty directory didn't change the build inputs")
	}
	before = inputs()
	os.Chmod(filepath.Join(ctx, "src"), 0o700)
	if inputs() == before {
		t.Error("a directory's mode didn't change the build inputs")
	}
	syncs = []Sync{{Service: "s", Path: filepath.Join(ctx, "src"), Skip: func(string, bool) bool { return false }}}
	before = inputs()
	os.Mkdir(filepath.Join(ctx, "src", "new"), 0o755)
	os.WriteFile(filepath.Join(ctx, "src", "new", "b"), []byte("b"), 0o644)
	if inputs() != before {
		t.Error("what a sync rule delivers changed the build inputs")
	}
}

// A rule whose path is a file syncs it unless the rule leaves it out, as
// compose watch matches a file event against the rule.
func TestFilesOfAFile(t *testing.T) {
	ctx := t.TempDir()
	os.WriteFile(filepath.Join(ctx, ".dockerignore"), []byte("secret.env\n"), 0o644)
	for _, f := range []string{"secret.env", "app.env"} {
		os.WriteFile(filepath.Join(ctx, f), nil, 0o644)
		p := filepath.Join(ctx, f)
		skip, err := skipper(watchRule{Path: p}, &buildSpec{Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
		files, err := (Sync{Path: p, Skip: skip}).Files()
		if want := map[bool]int{true: 0, false: 1}[f == "secret.env"]; err != nil || len(files) != want {
			t.Errorf("%s: synced %q (%v), want %d files", f, files, err, want)
		}
	}
}

// A directory that can't be read fails the listing, rather than listing
// nothing (whose files would then be deleted in the containers).
func TestFilesUnreadable(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "f"), nil, 0o644)
	os.Chmod(sub, 0)
	defer os.Chmod(sub, 0o755)
	if f, err := os.Open(sub); err == nil {
		f.Close()
		t.Skip("running as root: the directory is readable")
	}
	if files, err := (Sync{Path: root}).Files(); err == nil {
		t.Errorf("listed %q, want an error", files)
	}
}
