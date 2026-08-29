package simpleupdater

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const updaterHelperTempPrefix = "simple-updater-helper-"
const updaterHelperTempDirEnv = "SIMPLE_UPDATER_HELPER_TEMP_DIR"

// UpdaterLaunchOptions describes one handoff from the running application to
// the tiny updater helper. Script stays in memory and is delivered over stdin.
type UpdaterLaunchOptions struct {
	UpdaterPath string
	PID         int
	InstallRoot string
	PatchRoot   string
	RestartPath string
	Script      []byte
	// Archive is an optional gzip-compressed tar stream. When set, StartUpdater
	// extracts it into a temporary patch root and generates the update script.
	// PatchRoot and Script must be empty in archive mode.
	Archive io.Reader
}

// StartUpdater starts a temporary copy of the updater helper, synchronously
// writes the complete generated update script to its stdin, closes stdin, and
// then detaches from the helper process. Running an isolated copy allows the
// updater binary in the application install directory to be replaced by the
// same update transaction.
//
// Once this function returns successfully, the calling application may exit
// immediately without truncating the update script.
func StartUpdater(options UpdaterLaunchOptions) (int, error) {
	if options.Archive != nil {
		if strings.TrimSpace(options.PatchRoot) != "" || len(options.Script) != 0 {
			return 0, errors.New("archive mode cannot be combined with patch root or script")
		}

		patchRoot, script, err := prepareArchiveUpdate(options.Archive)
		if err != nil {
			return 0, err
		}
		options.PatchRoot = patchRoot
		options.Script = script

		pid, err := startUpdater(options)
		if err != nil {
			_ = os.RemoveAll(patchRoot)
			return 0, err
		}
		return pid, nil
	}

	return startUpdater(options)
}

func startUpdater(options UpdaterLaunchOptions) (int, error) {
	if strings.TrimSpace(options.UpdaterPath) == "" {
		return 0, errors.New("updater path is empty")
	}
	if options.PID < 0 {
		return 0, fmt.Errorf("invalid pid: %d", options.PID)
	}
	if len(options.Script) == 0 || len(strings.TrimSpace(string(options.Script))) == 0 {
		return 0, errors.New("update script is empty")
	}

	updaterPath, err := filepath.Abs(options.UpdaterPath)
	if err != nil {
		return 0, fmt.Errorf("resolve updater path: %w", err)
	}
	updaterInfo, err := os.Stat(updaterPath)
	if err != nil {
		return 0, fmt.Errorf("stat updater: %w", err)
	}
	if !updaterInfo.Mode().IsRegular() {
		return 0, errors.New("updater path is not a regular file")
	}

	installRoot, err := filepath.Abs(options.InstallRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve install root: %w", err)
	}
	patchRoot, err := filepath.Abs(options.PatchRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve patch root: %w", err)
	}
	if sameFilesystemPath(installRoot, patchRoot) {
		return 0, errors.New("patch root must not equal install root")
	}
	if pathWithin(updaterPath, patchRoot) {
		return 0, errors.New("updater executable must not be inside patch root")
	}

	args := []string{
		strconv.Itoa(options.PID),
		installRoot,
		patchRoot,
	}
	if strings.TrimSpace(options.RestartPath) != "" {
		restartPath, err := filepath.Abs(options.RestartPath)
		if err != nil {
			return 0, fmt.Errorf("resolve restart path: %w", err)
		}
		args = append(args, restartPath)
	}

	helperPath, helperDir, err := copyUpdaterToTemp(updaterPath)
	if err != nil {
		return 0, err
	}
	cleanupHelper := true
	defer func() {
		if cleanupHelper {
			_ = os.RemoveAll(helperDir)
		}
	}()

	cmd := exec.Command(helperPath, args...)
	// Keep the helper's working directory outside its temporary directory so
	// macOS can remove the temporary helper directory while the process exits.
	cmd.Dir = installRoot
	cmd.Env = append(os.Environ(), updaterHelperTempDirEnv+"="+helperDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("open updater stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return 0, fmt.Errorf("start updater: %w", err)
	}

	pid := cmd.Process.Pid
	if _, err := writeAll(stdin, options.Script); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("send update script to updater: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, fmt.Errorf("close updater stdin: %w", err)
	}

	// The temporary helper owns the rest of the update lifecycle and cleans its
	// own temporary copy after the script finishes.
	if err := cmd.Process.Release(); err != nil {
		return 0, fmt.Errorf("release updater process: %w", err)
	}
	cleanupHelper = false
	return pid, nil
}

func copyUpdaterToTemp(source string) (string, string, error) {
	sourceFile, err := os.Open(source)
	if err != nil {
		return "", "", fmt.Errorf("open updater: %w", err)
	}
	defer sourceFile.Close()

	info, err := sourceFile.Stat()
	if err != nil {
		return "", "", fmt.Errorf("stat updater: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("updater path is not a regular file")
	}

	tempDir, err := os.MkdirTemp("", updaterHelperTempPrefix)
	if err != nil {
		return "", "", fmt.Errorf("create updater temp directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tempDir)
		}
	}()

	name := filepath.Base(source)
	if runtime.GOOS == "windows" {
		// A neutral name avoids Windows installer detection treating an
		// unmanifested updater.exe as an application that requires elevation.
		name = "helper.exe"
	}
	destination := filepath.Join(tempDir, name)
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", "", fmt.Errorf("create temporary updater: %w", err)
	}

	copyErr := func() error {
		defer destinationFile.Close()
		if _, err := io.Copy(destinationFile, sourceFile); err != nil {
			return err
		}
		if err := destinationFile.Sync(); err != nil {
			return err
		}
		return destinationFile.Chmod(0o700)
	}()
	if copyErr != nil {
		return "", "", fmt.Errorf("copy updater to temporary directory: %w", copyErr)
	}

	ok = true
	return destination, tempDir, nil
}

func writeAll(writer io.Writer, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		n, err := writer.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func normalizeFilesystemPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func sameFilesystemPath(a, b string) bool {
	return normalizeFilesystemPath(a) == normalizeFilesystemPath(b)
}

func pathWithin(candidate, root string) bool {
	candidate = normalizeFilesystemPath(candidate)
	root = normalizeFilesystemPath(root)
	if candidate == root {
		return true
	}
	separator := string(filepath.Separator)
	if !strings.HasSuffix(root, separator) {
		root += separator
	}
	return strings.HasPrefix(candidate, root)
}
