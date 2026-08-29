//go:build windows

package simpleupdater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Peiratooo/simple-updater/updaterstub"
)

func TestWindowsHelperExecutesGeneratedScript(t *testing.T) {
	root := t.TempDir()
	installRoot := filepath.Join(root, "install")
	patchRoot := filepath.Join(root, "patch")
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(patchRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	contents := []byte("updated")
	digest := sha256.Sum256(contents)
	if err := os.WriteFile(filepath.Join(patchRoot, "app.txt"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := GenerateUpdateScript("windows", []File{{
		Path:   "app.txt",
		Size:   uint64(len(contents)),
		SHA256: fmt.Sprintf("%x", digest),
	}})
	if err != nil {
		t.Fatal(err)
	}

	helperPath, err := updaterstub.Build(context.Background(), updaterstub.BuildOptions{
		System:  "windows",
		Output:  filepath.Join(root, "helper.exe"),
		WorkDir: ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helperPath, "0", installRoot, patchRoot)
	cmd.Stdin = bytes.NewBufferString(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(patchRoot, ".simple-updater-state", "update.log"))
		t.Fatalf("run helper: %v: %s\n%s", err, output, log)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		installed, readErr := os.ReadFile(filepath.Join(installRoot, "app.txt"))
		if readErr == nil && bytes.Equal(installed, contents) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not install patch: %v", readErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
