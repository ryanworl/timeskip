package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPre checks which edits a first_ command sees, and that its
// checkpoint's name changes with them and only them.
func TestPre(t *testing.T) {
	tmpl := "/opt/antithesis/test/v1/t"
	staging := func(first, driver string) *staged {
		s := &staged{dir: t.TempDir(), edits: []edit{
			{service: "w", target: tmpl},
			{service: "w", target: "/app"},
			{service: "w", target: tmpl, deleted: []string{tmpl + "/helper_x"}},
		}}
		for f, data := range map[string]string{"0/first_a": first, "0/parallel_driver_b": driver, "1/main.go": "package main"} {
			p := filepath.Join(s.dir, f)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		s.paths = [][]string{{tmpl + "/first_a", tmpl + "/parallel_driver_b"}, {"/app/main.go"}, nil}
		return s
	}
	names, digest := staging("v1", "v1").pre("t")
	if !slices.Equal(names, []string{"0", "2"}) {
		t.Errorf("pre names %v, want [0 2]: the edit with a first_ file and the one deleting a helper_ file", names)
	}
	if _, d := staging("v1", "v2").pre("t"); d != digest {
		t.Errorf("a parallel_driver_ edit changed the first_ checkpoint's digest")
	}
	if _, d := staging("v2", "v1").pre("t"); d == digest {
		t.Errorf("a first_ edit left the first_ checkpoint's digest as it was")
	}
	if names, _ := staging("v1", "v1").pre("u"); names != nil {
		t.Errorf("template u's pre names %v, want none", names)
	}
}
