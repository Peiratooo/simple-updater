package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const maxScriptSize = 64 << 20 // 64 MiB
const updaterHelperTempPrefix = "simple-updater-helper-"
const updaterHelperTempDirEnv = "SIMPLE_UPDATER_HELPER_TEMP_DIR"

func main() {
	err := run(os.Args[1:], os.Stdin)
	cleanupErr := cleanupTemporaryHelper()

	if err != nil {
		fmt.Fprintln(os.Stderr, "updater:", err)
		if cleanupErr != nil {
			fmt.Fprintln(os.Stderr, "updater cleanup:", cleanupErr)
		}
		os.Exit(1)
	}
	if cleanupErr != nil {
		// The update itself has already completed. A stale temporary helper is a
		// cleanup issue, not an update failure.
		fmt.Fprintln(os.Stderr, "updater cleanup:", cleanupErr)
	}
}

func run(args []string, stdin io.Reader) error {
	if len(args) < 3 || len(args) > 4 {
		return fmt.Errorf("usage: %s <pid> <install-root> <patch-root> [restart-path] < script", filepath.Base(os.Args[0]))
	}

	pid, err := strconv.Atoi(args[0])
	if err != nil || pid < 0 {
		return fmt.Errorf("invalid pid: %s", args[0])
	}

	installRoot, err := filepath.Abs(args[1])
	if err != nil {
		return fmt.Errorf("resolve install root: %w", err)
	}
	patchRoot, err := filepath.Abs(args[2])
	if err != nil {
		return fmt.Errorf("resolve patch root: %w", err)
	}
	if samePath(installRoot, patchRoot) {
		return fmt.Errorf("patch root must not equal install root")
	}

	restartPath := ""
	if len(args) == 4 && strings.TrimSpace(args[3]) != "" {
		restartPath, err = filepath.Abs(args[3])
		if err != nil {
			return fmt.Errorf("resolve restart path: %w", err)
		}
	}

	limited := io.LimitReader(stdin, maxScriptSize+1)
	script, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read update script from stdin: %w", err)
	}
	if len(script) == 0 || len(bytes.TrimSpace(script)) == 0 {
		return fmt.Errorf("update script is empty")
	}
	if len(script) > maxScriptSize {
		return fmt.Errorf("update script exceeds %d bytes", maxScriptSize)
	}
	// This distribution targets unsigned macOS application bundles. The
	// library-generated script may contain the optional codesign commit check;
	// remove that block in the helper so callers only need to replace this
	// executable when packaging an unsigned build.
	script = stripUnsignedMacOSSignatureCheck(script)

	// Read the entire script before starting the interpreter. The parent app can
	// safely exit as soon as this helper has consumed stdin; execution no longer
	// depends on the parent process or a script file on disk.
	runtimeContext := runtimeContext{
		PID:         pid,
		InstallRoot: installRoot,
		PatchRoot:   patchRoot,
		RestartPath: restartPath,
	}
	return runUpdateScript(script, runtimeContext)
}

func stripUnsignedMacOSSignatureCheck(script []byte) []byte {
	if runtime.GOOS != "darwin" {
		return script
	}

	text := string(script)
	const begin = "# A macOS application bundle is only considered committed after its signature\n"
	const end = "\nUPDATE_COMMITTED=1\n"
	start := strings.Index(text, begin)
	if start < 0 {
		return script
	}
	finishOffset := strings.Index(text[start:], end)
	if finishOffset < 0 {
		return script
	}
	finish := start + finishOffset
	stripped := text[:start] + text[finish:]
	return []byte(stripped)
}

type runtimeContext struct {
	PID         int
	InstallRoot string
	PatchRoot   string
	RestartPath string
}

func normalizePath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func samePath(a, b string) bool {
	return normalizePath(a) == normalizePath(b)
}

func pathWithin(candidate, root string) bool {
	candidate = normalizePath(candidate)
	root = normalizePath(root)
	if candidate == root {
		return true
	}
	separator := string(filepath.Separator)
	if !strings.HasSuffix(root, separator) {
		root += separator
	}
	return strings.HasPrefix(candidate, root)
}
