package simpleupdater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProductManifestExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	write := func(name string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt")
	write(filepath.Join("cache", "skip.txt"))
	write(filepath.Join("cache-old", "keep.txt"))
	write(filepath.Join("vendor", "skip.txt"))

	all, err := ReadProductManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("old call returned %d files, want 4", len(all))
	}

	files, err := ReadProductManifest(
		root,
		"",
		filepath.Join("cache", "."),
		filepath.Join(root, "cache"),
		filepath.Join(root, "vendor"),
		filepath.Join(filepath.Dir(root), "outside"),
		filepath.Join(root, "vendor"),
	)
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		paths[file.Path] = true
	}
	for _, want := range []string{"keep.txt", filepath.Join("cache-old", "keep.txt")} {
		want = filepath.ToSlash(want)
		if !paths[want] {
			t.Errorf("excluded manifest missing %q", want)
		}
	}
	for _, unwanted := range []string{"cache/skip.txt", "vendor/skip.txt"} {
		if paths[unwanted] {
			t.Errorf("excluded manifest contains %q", unwanted)
		}
	}
}
