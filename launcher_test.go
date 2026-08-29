package simpleupdater

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCopyUpdaterToTempUsesNeutralWindowsName(t *testing.T) {
	source := filepath.Join(t.TempDir(), "updater.exe")
	content := []byte("updater")
	if err := os.WriteFile(source, content, 0o700); err != nil {
		t.Fatal(err)
	}

	helperPath, helperDir, err := copyUpdaterToTemp(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(helperDir) })

	wantName := filepath.Base(source)
	if runtime.GOOS == "windows" {
		wantName = "helper.exe"
	}
	if filepath.Base(helperPath) != wantName {
		t.Fatalf("helper name = %q, want %q", filepath.Base(helperPath), wantName)
	}
	copied, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != string(content) {
		t.Fatal("temporary helper content differs from source")
	}
}
