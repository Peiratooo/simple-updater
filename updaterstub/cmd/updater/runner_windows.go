//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const elevatedPowerShellLauncher = `$scriptPath = '"' + $env:SIMPLE_UPDATER_SCRIPT_PATH + '"'
$arguments = "-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $scriptPath"
$process = Start-Process -FilePath "powershell.exe" -ArgumentList $arguments -WorkingDirectory $env:SIMPLE_UPDATER_INSTALL_ROOT -Verb RunAs -Wait -PassThru
exit $process.ExitCode`

func runUpdateScript(script []byte, runtime runtimeContext) error {
	scriptFile, err := os.CreateTemp("", "simple-updater-script-*.ps1")
	if err != nil {
		return fmt.Errorf("create powershell update script: %w", err)
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	// Windows PowerShell 5.1 needs the BOM to decode UTF-8 script files reliably.
	if _, err := scriptFile.Write(append([]byte{0xef, 0xbb, 0xbf}, script...)); err != nil {
		_ = scriptFile.Close()
		return fmt.Errorf("write powershell update script: %w", err)
	}
	if err := scriptFile.Close(); err != nil {
		return fmt.Errorf("close powershell update script: %w", err)
	}

	env := append(os.Environ(),
		"SIMPLE_UPDATER_PID="+strconv.Itoa(runtime.PID),
		"SIMPLE_UPDATER_INSTALL_ROOT="+runtime.InstallRoot,
		"SIMPLE_UPDATER_PATCH_ROOT="+runtime.PatchRoot,
		// Restart from this non-elevated helper after the privileged update
		// finishes so the application does not inherit the administrator token.
		"SIMPLE_UPDATER_RESTART_PATH=",
		"SIMPLE_UPDATER_SCRIPT_PATH="+scriptPath,
	)
	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", elevatedPowerShellLauncher,
	)
	cmd.Dir = runtime.InstallRoot
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("elevated powershell update failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	if strings.TrimSpace(runtime.RestartPath) == "" {
		return nil
	}
	restart := exec.Command(runtime.RestartPath)
	restart.Dir = filepath.Dir(runtime.RestartPath)
	if err := restart.Start(); err != nil {
		return fmt.Errorf("restart application: %w", err)
	}
	return restart.Process.Release()
}
