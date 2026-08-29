//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

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

	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
	)
	cmd.Dir = runtime.InstallRoot
	cmd.Env = append(os.Environ(),
		"SIMPLE_UPDATER_PID="+strconv.Itoa(runtime.PID),
		"SIMPLE_UPDATER_INSTALL_ROOT="+runtime.InstallRoot,
		"SIMPLE_UPDATER_PATCH_ROOT="+runtime.PatchRoot,
		"SIMPLE_UPDATER_RESTART_PATH="+runtime.RestartPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("powershell update failed: %w: %s", err, output)
	}
	return nil
}
