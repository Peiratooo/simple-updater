package simpleupdater

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// GenerateUpdateScriptFromJSON parses a manifest.json produced by DownloadPatch
// and returns a platform-specific update script.
func GenerateUpdateScriptFromJSON(system string, manifest []byte) (string, error) {
	var files []File
	if err := json.Unmarshal(manifest, &files); err != nil {
		return "", fmt.Errorf("unmarshal manifest: %w", err)
	}
	return GenerateUpdateScript(system, files)
}

// GenerateUpdateScript returns an update script for the requested platform.
//
// The generated script is designed to be streamed to the updater helper over
// stdin; it does not need to be written to disk. The updater helper provides
// runtime context through these environment variables:
//
//	SIMPLE_UPDATER_PID          process id of the application being updated
//	SIMPLE_UPDATER_INSTALL_ROOT installation root to modify
//	SIMPLE_UPDATER_PATCH_ROOT   temporary update directory containing patch files
//	SIMPLE_UPDATER_RESTART_PATH optional executable/.app path to start on success
//
// Lifecycle implemented by the generated script:
//  1. wait for the old app to exit, forcing it down after a short grace period;
//  2. preflight every target before modifying anything;
//  3. back up every existing target into the update directory;
//  4. apply and verify all changes;
//  5. rollback and show a native error dialog on failure;
//  6. on success, remove the entire temporary update directory and restart.
func GenerateUpdateScript(system string, manifest []File) (string, error) {
	files, err := normalizeScriptManifest(system, manifest)
	if err != nil {
		return "", err
	}

	switch strings.ToLower(strings.TrimSpace(system)) {
	case "windows", "win":
		return generatePowerShellUpdateScript(files), nil
	case "darwin", "mac", "macos":
		return generateMacUpdateScript(files), nil
	default:
		return "", fmt.Errorf("unsupported system: %s", system)
	}
}

func normalizeScriptManifest(system string, manifest []File) ([]File, error) {
	normalized := make([]File, 0, len(manifest))
	seen := make(map[string]struct{}, len(manifest))
	windows := strings.EqualFold(strings.TrimSpace(system), "windows") || strings.EqualFold(strings.TrimSpace(system), "win")

	for _, file := range manifest {
		name, err := cleanArchiveName(file.Path)
		if err != nil {
			return nil, err
		}
		if windows && (strings.Contains(name, "\\") || strings.Contains(name, ":")) {
			return nil, fmt.Errorf("invalid Windows manifest path: %s", file.Path)
		}
		if name == ".simple-updater-state" || strings.HasPrefix(name, ".simple-updater-state/") {
			return nil, fmt.Errorf("manifest path uses reserved updater state path: %s", name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate manifest path: %s", name)
		}
		seen[name] = struct{}{}
		file.Path = name

		switch file.fileType() {
		case FileTypeRegular:
			file.Type = FileTypeRegular
		case FileTypeSymlink:
			file.Type = FileTypeSymlink
			if err := validateSymlinkTarget(name, file.LinkTarget); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported file type for %s: %s", name, file.Type)
		}

		normalized = append(normalized, file)
	}

	return normalized, nil
}

func generatePowerShellUpdateScript(files []File) string {
	var b strings.Builder
	b.WriteString(`$ErrorActionPreference = "Stop"
Import-Module Microsoft.PowerShell.Utility -ErrorAction Stop

$PidToWait = 0
if ($env:SIMPLE_UPDATER_PID) {
    [void][int]::TryParse($env:SIMPLE_UPDATER_PID, [ref]$PidToWait)
}
$InstallRoot = $env:SIMPLE_UPDATER_INSTALL_ROOT
$PatchRoot = $env:SIMPLE_UPDATER_PATCH_ROOT
$RestartPath = $env:SIMPLE_UPDATER_RESTART_PATH

if ([string]::IsNullOrWhiteSpace($InstallRoot)) { throw "SIMPLE_UPDATER_INSTALL_ROOT is empty" }
if ([string]::IsNullOrWhiteSpace($PatchRoot)) { throw "SIMPLE_UPDATER_PATCH_ROOT is empty" }

$InstallRoot = [System.IO.Path]::GetFullPath($InstallRoot)
$PatchRoot = [System.IO.Path]::GetFullPath($PatchRoot)
$StateRoot = Join-Path $PatchRoot ".simple-updater-state"
$BackupRoot = Join-Path $StateRoot "backup"
$LogPath = Join-Path $StateRoot "update.log"
$script:UpdateCommitted = $false

function Write-UpdateLog([string]$Message) {
    try {
        $line = "{0:o} {1}" -f (Get-Date), $Message
        Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8 -ErrorAction SilentlyContinue
    } catch {}
}

function Show-UpdateDialog([string]$Title, [string]$Message, [string]$Icon = "Error") {
    try {
        Add-Type -AssemblyName System.Windows.Forms -ErrorAction Stop
        $messageIcon = [System.Windows.Forms.MessageBoxIcon]::$Icon
        [void][System.Windows.Forms.MessageBox]::Show(
            $Message,
            $Title,
            [System.Windows.Forms.MessageBoxButtons]::OK,
            $messageIcon
        )
    } catch {}
}

function Wait-Or-StopProcess([int]$ProcessId) {
    if ($ProcessId -le 0) { return }

    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline) {
        if (-not (Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)) { return }
        Start-Sleep -Milliseconds 200
    }

    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if ($process) {
        Write-UpdateLog "application did not exit in time; forcing process $ProcessId to stop"
        Stop-Process -Id $ProcessId -Force -ErrorAction Stop
    }

    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline) {
        if (-not (Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "application process is still running: $ProcessId"
}

function Get-ExistingParent([string]$Path) {
    $current = Split-Path -Parent $Path
    while ($current -and -not (Test-Path -LiteralPath $current -PathType Container)) {
        $next = Split-Path -Parent $current
        if ($next -eq $current) { break }
        $current = $next
    }
    return $current
}

function Assert-DirectoryWritable([string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) { throw "unable to resolve writable parent directory" }
    $probe = Join-Path $Path (".simple-updater-write-test-" + [Guid]::NewGuid().ToString("N"))
    try {
        [System.IO.File]::WriteAllText($probe, "test")
    } finally {
        Remove-Item -LiteralPath $probe -Force -ErrorAction SilentlyContinue
    }
}

function Assert-FileUnlocked([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer) { throw "target path is a directory: $Path" }
    if ($item.LinkType -eq "SymbolicLink") { return }

    try {
        $stream = [System.IO.File]::Open(
            $Path,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::ReadWrite,
            [System.IO.FileShare]::None
        )
        $stream.Dispose()
    } catch {
        throw "file is still locked or not writable: $Path"
    }
}

function Assert-PatchFile([string]$Relative, [uint64]$ExpectedSize, [string]$ExpectedSHA256) {
    $source = Join-Path $PatchRoot $Relative
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "patch file is missing: $Relative"
    }

    if ($ExpectedSize -gt 0) {
        $actualSize = (Get-Item -LiteralPath $source -Force).Length
        if ([uint64]$actualSize -ne $ExpectedSize) {
            throw "patch file size mismatch: $Relative"
        }
    }

    if (-not [string]::IsNullOrWhiteSpace($ExpectedSHA256)) {
        $actualHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualHash -ne $ExpectedSHA256.ToLowerInvariant()) {
            throw "patch file SHA256 mismatch: $Relative"
        }
    }
}

function Backup-Target([string]$Relative) {
    $destination = Join-Path $InstallRoot $Relative
    if (-not (Test-Path -LiteralPath $destination)) { return $false }

    $backup = Join-Path $BackupRoot $Relative
    $backupParent = Split-Path -Parent $backup
    if ($backupParent) { New-Item -ItemType Directory -Force -Path $backupParent | Out-Null }

    $item = Get-Item -LiteralPath $destination -Force
    if ($item.LinkType -eq "SymbolicLink") {
        $target = [string]$item.Target
        Set-Content -LiteralPath ($backup + ".simple-updater-link") -Value $target -Encoding UTF8 -NoNewline
    } else {
        Copy-Item -LiteralPath $destination -Destination $backup -Force
    }
    return $true
}

function Restore-Target([string]$Relative, [bool]$Existed) {
    $destination = Join-Path $InstallRoot $Relative
    Remove-Item -LiteralPath $destination -Force -Recurse -ErrorAction SilentlyContinue
    if (-not $Existed) { return }

    $backup = Join-Path $BackupRoot $Relative
    $linkMetadata = $backup + ".simple-updater-link"
    $parent = Split-Path -Parent $destination
    if ($parent) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }

    if (Test-Path -LiteralPath $linkMetadata -PathType Leaf) {
        $target = Get-Content -LiteralPath $linkMetadata -Raw
        New-Item -ItemType SymbolicLink -Path $destination -Target $target | Out-Null
    } else {
        Copy-Item -LiteralPath $backup -Destination $destination -Force
    }
}

function Apply-RegularFile([string]$Relative, [uint64]$ExpectedSize, [string]$ExpectedSHA256) {
    $source = Join-Path $PatchRoot $Relative
    $destination = Join-Path $InstallRoot $Relative
    $parent = Split-Path -Parent $destination
    if ($parent) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }

    $temporary = $destination + ".simple-updater-" + [Guid]::NewGuid().ToString("N") + ".tmp"
    try {
        Copy-Item -LiteralPath $source -Destination $temporary -Force
        Remove-Item -LiteralPath $destination -Force -Recurse -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $temporary -Destination $destination -Force
    } finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }

    if ($ExpectedSize -gt 0) {
        $actualSize = (Get-Item -LiteralPath $destination -Force).Length
        if ([uint64]$actualSize -ne $ExpectedSize) { throw "installed file size mismatch: $Relative" }
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedSHA256)) {
        $actualHash = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualHash -ne $ExpectedSHA256.ToLowerInvariant()) { throw "installed file SHA256 mismatch: $Relative" }
    }
}

function Apply-Symlink([string]$Relative, [string]$LinkTarget) {
    $destination = Join-Path $InstallRoot $Relative
    $parent = Split-Path -Parent $destination
    if ($parent) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }
    Remove-Item -LiteralPath $destination -Force -Recurse -ErrorAction SilentlyContinue
    New-Item -ItemType SymbolicLink -Path $destination -Target $LinkTarget | Out-Null
}

function Remove-UpdateDirectory {
    for ($attempt = 0; $attempt -lt 10; $attempt++) {
        try {
            Remove-Item -LiteralPath $PatchRoot -Recurse -Force -ErrorAction Stop
            return $true
        } catch {
            Start-Sleep -Milliseconds 250
        }
    }
    return $false
}

try {
    New-Item -ItemType Directory -Force -Path $PatchRoot | Out-Null
    New-Item -ItemType Directory -Force -Path $StateRoot | Out-Null
    Write-UpdateLog "update started"
    Wait-Or-StopProcess $PidToWait

    # Preflight every target before modifying installation files.
`)

	for _, file := range files {
		windowsPath := strings.ReplaceAll(file.Path, "/", "\\")
		fmt.Fprintf(&b, "    $relative = %s\n", powershellQuote(windowsPath))
		b.WriteString("    $destination = Join-Path $InstallRoot $relative\n")
		b.WriteString("    $existingParent = Get-ExistingParent $destination\n")
		b.WriteString("    Assert-DirectoryWritable $existingParent\n")
		b.WriteString("    Assert-FileUnlocked $destination\n")
		if file.fileType() == FileTypeRegular {
			fmt.Fprintf(&b, "    Assert-PatchFile $relative %d %s\n", file.Size, powershellQuote(file.SHA256))
		}
	}

	if hasSymlink(files) {
		b.WriteString(`    # Verify that this Windows session is allowed to create symbolic links.
    $symlinkProbe = Join-Path $InstallRoot (".simple-updater-symlink-test-" + [Guid]::NewGuid().ToString("N"))
    try {
        New-Item -ItemType SymbolicLink -Path $symlinkProbe -Target "." -ErrorAction Stop | Out-Null
    } finally {
        Remove-Item -LiteralPath $symlinkProbe -Force -ErrorAction SilentlyContinue
    }
`)
	}

	b.WriteString(`
    Remove-Item -LiteralPath $BackupRoot -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $BackupRoot | Out-Null

    # Back up all existing targets before applying the first change.
`)
	for i, file := range files {
		windowsPath := strings.ReplaceAll(file.Path, "/", "\\")
		fmt.Fprintf(&b, "    $had_%d = Backup-Target %s\n", i, powershellQuote(windowsPath))
	}

	b.WriteString(`
    try {
`)
	for _, file := range files {
		windowsPath := strings.ReplaceAll(file.Path, "/", "\\")
		if file.fileType() == FileTypeSymlink {
			fmt.Fprintf(&b, "        Apply-Symlink %s %s\n", powershellQuote(windowsPath), powershellQuote(strings.ReplaceAll(file.LinkTarget, "/", "\\")))
		} else {
			fmt.Fprintf(&b, "        Apply-RegularFile %s %d %s\n", powershellQuote(windowsPath), file.Size, powershellQuote(file.SHA256))
		}
	}
	b.WriteString(`        $script:UpdateCommitted = $true
    } catch {
        $applyError = $_
        Write-UpdateLog ("apply failed: " + $applyError.Exception.Message)
        Write-UpdateLog "rolling back"
`)
	for i := len(files) - 1; i >= 0; i-- {
		windowsPath := strings.ReplaceAll(files[i].Path, "/", "\\")
		fmt.Fprintf(&b, "        try { Restore-Target %s $had_%d } catch { Write-UpdateLog (\"rollback failed for %s: \" + $_.Exception.Message) }\n", powershellQuote(windowsPath), i, escapePowerShellComment(windowsPath))
	}
	b.WriteString(`        throw $applyError
    }

    Write-UpdateLog "update committed"

    # No script file is executing from PatchRoot, so the entire temporary
    # update directory can be removed after a successful commit.
    if (-not (Remove-UpdateDirectory)) {
        Show-UpdateDialog "更新完成" "更新已完成，但临时更新目录无法自动删除。可以稍后手动清理。" "Warning"
    }

    if (-not [string]::IsNullOrWhiteSpace($RestartPath)) {
        Start-Process -FilePath $RestartPath
    }
} catch {
    $message = $_.Exception.Message
    Write-UpdateLog ("update failed: " + $message)
    if (-not $script:UpdateCommitted) {
        Show-UpdateDialog "更新失败" ("无法完成更新。" + [Environment]::NewLine + [Environment]::NewLine + $message + [Environment]::NewLine + [Environment]::NewLine + "临时更新目录已保留，便于重试或排查。") "Error"
    } else {
        Show-UpdateDialog "更新异常" ("更新文件已完成，但后续步骤出现异常。" + [Environment]::NewLine + [Environment]::NewLine + $message) "Warning"
    }
    exit 1
}
`)
	return b.String()
}

func generateMacUpdateScript(files []File) string {
	var b strings.Builder
	b.WriteString(`#!/bin/sh
set -eu

PID_TO_WAIT="${SIMPLE_UPDATER_PID:-0}"
INSTALL_ROOT="${SIMPLE_UPDATER_INSTALL_ROOT:-}"
PATCH_ROOT="${SIMPLE_UPDATER_PATCH_ROOT:-}"
RESTART_PATH="${SIMPLE_UPDATER_RESTART_PATH:-}"

if [ -z "$INSTALL_ROOT" ]; then echo "SIMPLE_UPDATER_INSTALL_ROOT is empty" >&2; exit 2; fi
if [ -z "$PATCH_ROOT" ]; then echo "SIMPLE_UPDATER_PATCH_ROOT is empty" >&2; exit 2; fi

STATE_ROOT="$PATCH_ROOT/.simple-updater-state"
BACKUP_ROOT="$STATE_ROOT/backup"
LOG_PATH="$STATE_ROOT/update.log"
UPDATE_COMMITTED=0

log_update() {
    printf '%s %s\n' "$(/bin/date '+%Y-%m-%dT%H:%M:%S%z')" "$1" >> "$LOG_PATH" 2>/dev/null || true
}

show_dialog() {
    title="$1"
    message="$2"
    icon="$3"
    /usr/bin/osascript \
        -e 'on run argv' \
        -e 'if (item 3 of argv) is "stop" then' \
        -e 'display dialog (item 2 of argv) with title (item 1 of argv) buttons {"确定"} default button "确定" with icon stop' \
        -e 'else' \
        -e 'display dialog (item 2 of argv) with title (item 1 of argv) buttons {"确定"} default button "确定" with icon caution' \
        -e 'end if' \
        -e 'end run' \
        "$title" "$message" "$icon" >/dev/null 2>&1 || true
}

wait_or_stop_process() {
    case "$PID_TO_WAIT" in
        ''|*[!0-9]*) return 0 ;;
    esac
    if [ "$PID_TO_WAIT" -le 0 ]; then return 0; fi

    count=0
    while /bin/kill -0 "$PID_TO_WAIT" 2>/dev/null && [ "$count" -lt 25 ]; do
        /bin/sleep 0.2
        count=$((count + 1))
    done
    if ! /bin/kill -0 "$PID_TO_WAIT" 2>/dev/null; then return 0; fi

    log_update "application did not exit in time; sending TERM to $PID_TO_WAIT"
    /bin/kill -TERM "$PID_TO_WAIT" 2>/dev/null || true
    count=0
    while /bin/kill -0 "$PID_TO_WAIT" 2>/dev/null && [ "$count" -lt 25 ]; do
        /bin/sleep 0.2
        count=$((count + 1))
    done
    if ! /bin/kill -0 "$PID_TO_WAIT" 2>/dev/null; then return 0; fi

    log_update "application still running; sending KILL to $PID_TO_WAIT"
    /bin/kill -KILL "$PID_TO_WAIT" 2>/dev/null || true
    /bin/sleep 0.2
    if /bin/kill -0 "$PID_TO_WAIT" 2>/dev/null; then
        echo "application process is still running: $PID_TO_WAIT" >&2
        return 1
    fi
}

existing_parent() {
    current=$(/usr/bin/dirname "$1")
    while [ ! -d "$current" ]; do
        next=$(/usr/bin/dirname "$current")
        if [ "$next" = "$current" ]; then break; fi
        current="$next"
    done
    printf '%s\n' "$current"
}

assert_directory_writable() {
    dir="$1"
    probe="$dir/.simple-updater-write-test-$$"
    if ! : > "$probe" 2>/dev/null; then
        echo "directory is not writable: $dir" >&2
        return 1
    fi
    /bin/rm -f "$probe"
}

assert_patch_file() {
    relative="$1"
    expected_size="$2"
    expected_sha="$3"
    source="$PATCH_ROOT/$relative"

    if [ ! -f "$source" ]; then
        echo "patch file is missing: $relative" >&2
        return 1
    fi

    if [ "$expected_size" -gt 0 ]; then
        actual_size=$(/usr/bin/stat -f '%z' "$source")
        if [ "$actual_size" != "$expected_size" ]; then
            echo "patch file size mismatch: $relative" >&2
            return 1
        fi
    fi

    if [ -n "$expected_sha" ]; then
        actual_sha=$(/usr/bin/shasum -a 256 "$source" | /usr/bin/awk '{print $1}')
        if [ "$actual_sha" != "$expected_sha" ]; then
            echo "patch file SHA256 mismatch: $relative" >&2
            return 1
        fi
    fi
}

backup_target() {
    relative="$1"
    destination="$INSTALL_ROOT/$relative"
    backup="$BACKUP_ROOT/$relative"
    backup_parent=$(/usr/bin/dirname "$backup")
    /bin/mkdir -p "$backup_parent"

    if [ -L "$destination" ]; then
        /usr/bin/readlink "$destination" > "$backup.simple-updater-link"
        return 0
    fi
    if [ -e "$destination" ]; then
        /bin/cp -p "$destination" "$backup"
        return 0
    fi
    return 1
}

restore_target() {
    relative="$1"
    existed="$2"
    destination="$INSTALL_ROOT/$relative"
    backup="$BACKUP_ROOT/$relative"
    parent=$(/usr/bin/dirname "$destination")

    /bin/rm -rf "$destination" 2>/dev/null || true
    if [ "$existed" != "1" ]; then return 0; fi

    /bin/mkdir -p "$parent"
    if [ -f "$backup.simple-updater-link" ]; then
        target=$(/bin/cat "$backup.simple-updater-link")
        /bin/ln -s "$target" "$destination"
    else
        /bin/cp -p "$backup" "$destination"
    fi
}

apply_regular_file() {
    relative="$1"
    expected_size="$2"
    expected_sha="$3"
    mode="$4"
    source="$PATCH_ROOT/$relative"
    destination="$INSTALL_ROOT/$relative"
    parent=$(/usr/bin/dirname "$destination")
    temporary="$destination.simple-updater-$$.tmp"

    /bin/mkdir -p "$parent" || return 1
    /bin/rm -rf "$temporary"
    /bin/cp "$source" "$temporary" || { /bin/rm -f "$temporary"; return 1; }
    /bin/chmod "$mode" "$temporary" || { /bin/rm -f "$temporary"; return 1; }
    /bin/rm -rf "$destination" || { /bin/rm -f "$temporary"; return 1; }
    /bin/mv "$temporary" "$destination" || { /bin/rm -f "$temporary"; return 1; }

    if [ "$expected_size" -gt 0 ]; then
        actual_size=$(/usr/bin/stat -f '%z' "$destination")
        if [ "$actual_size" != "$expected_size" ]; then
            echo "installed file size mismatch: $relative" >&2
            return 1
        fi
    fi
    if [ -n "$expected_sha" ]; then
        actual_sha=$(/usr/bin/shasum -a 256 "$destination" | /usr/bin/awk '{print $1}')
        if [ "$actual_sha" != "$expected_sha" ]; then
            echo "installed file SHA256 mismatch: $relative" >&2
            return 1
        fi
    fi
}

apply_symlink() {
    relative="$1"
    target="$2"
    destination="$INSTALL_ROOT/$relative"
    parent=$(/usr/bin/dirname "$destination")
    /bin/mkdir -p "$parent"
    /bin/rm -rf "$destination"
    /bin/ln -s "$target" "$destination"
}

remove_update_directory() {
    attempt=0
    while [ "$attempt" -lt 10 ]; do
        if /bin/rm -rf "$PATCH_ROOT" 2>/dev/null && [ ! -e "$PATCH_ROOT" ]; then
            return 0
        fi
        attempt=$((attempt + 1))
        /bin/sleep 0.25
    done
    return 1
}

fail_update() {
    message="$1"
    log_update "update failed: $message"
    if [ "$UPDATE_COMMITTED" = "0" ]; then
        show_dialog "更新失败" "无法完成更新。\n\n$message\n\n临时更新目录已保留，便于重试或排查。" "stop"
    else
        show_dialog "更新异常" "更新文件已完成，但后续步骤出现异常。\n\n$message" "caution"
    fi
    exit 1
}

/bin/mkdir -p "$PATCH_ROOT"
/bin/mkdir -p "$STATE_ROOT"
log_update "update started"
wait_or_stop_process || fail_update "无法关闭正在运行的应用。"

# Preflight every target before modifying installation files.
`)

	for _, file := range files {
		fmt.Fprintf(&b, "RELATIVE=%s\n", shellQuote(file.Path))
		b.WriteString("DESTINATION=\"$INSTALL_ROOT/$RELATIVE\"\n")
		b.WriteString("PARENT=$(existing_parent \"$DESTINATION\")\n")
		b.WriteString("assert_directory_writable \"$PARENT\" || fail_update \"目标目录不可写：$RELATIVE\"\n")
		if file.fileType() == FileTypeRegular {
			fmt.Fprintf(&b, "assert_patch_file \"$RELATIVE\" %d %s || fail_update \"更新文件校验失败：$RELATIVE\"\n", file.Size, shellQuote(strings.ToLower(file.SHA256)))
		}
	}

	b.WriteString(`
/bin/rm -rf "$BACKUP_ROOT" 2>/dev/null || true
/bin/mkdir -p "$BACKUP_ROOT"

# Back up all existing targets before applying the first change.
`)
	for i, file := range files {
		fmt.Fprintf(&b, "if backup_target %s; then HAD_%d=1; else HAD_%d=0; fi\n", shellQuote(file.Path), i, i)
	}

	b.WriteString(`
apply_failed=0
apply_error=""
`)
	for _, file := range files {
		if file.fileType() == FileTypeSymlink {
			fmt.Fprintf(&b, "if [ \"$apply_failed\" = \"0\" ] && ! apply_symlink %s %s; then apply_failed=1; apply_error=%s; fi\n", shellQuote(file.Path), shellQuote(file.LinkTarget), shellQuote("failed to create symlink: "+file.Path))
		} else {
			mode := file.Mode
			if mode == 0 {
				mode = 0o644
			}
			fmt.Fprintf(&b, "if [ \"$apply_failed\" = \"0\" ] && ! apply_regular_file %s %d %s %s; then apply_failed=1; apply_error=%s; fi\n", shellQuote(file.Path), file.Size, shellQuote(strings.ToLower(file.SHA256)), shellQuote(formatFileMode(mode)), shellQuote("failed to replace file: "+file.Path))
		}
	}

	b.WriteString(`
if [ "$apply_failed" != "0" ]; then
    log_update "$apply_error; rolling back"
`)
	for i := len(files) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "    restore_target %s \"$HAD_%d\" || log_update %s\n", shellQuote(files[i].Path), i, shellQuote("rollback failed: "+files[i].Path))
	}
	b.WriteString(`    fail_update "$apply_error"
fi

# A macOS application bundle is only considered committed after its signature
# verifies. This runs only when the restart target is an .app bundle.
case "$RESTART_PATH" in
    *.app)
        if ! /usr/bin/codesign --verify --deep --strict "$RESTART_PATH" >/dev/null 2>&1; then
            log_update "codesign verification failed; rolling back"
`)
	for i := len(files) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "            restore_target %s \"$HAD_%d\" || log_update %s\n", shellQuote(files[i].Path), i, shellQuote("rollback failed: "+files[i].Path))
	}
	b.WriteString(`            fail_update "应用签名校验失败，更新已回滚。"
        fi
        ;;
esac

UPDATE_COMMITTED=1
log_update "update committed"

# The script is executing from stdin, not from PATCH_ROOT, so the complete
# temporary update directory can be deleted safely after a successful update.
if ! remove_update_directory; then
    show_dialog "更新完成" "更新已完成，但临时更新目录无法自动删除。可以稍后手动清理。" "caution"
fi

if [ -n "$RESTART_PATH" ]; then
    /usr/bin/open "$RESTART_PATH" || {
        show_dialog "更新完成" "更新已完成，但应用无法自动重新启动。请手动启动。" "caution"
        exit 1
    }
fi

exit 0
`)
	return b.String()
}

func hasSymlink(files []File) bool {
	for _, file := range files {
		if file.fileType() == FileTypeSymlink {
			return true
		}
	}
	return false
}

func powershellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func formatFileMode(mode uint32) string {
	return "0" + strconv.FormatUint(uint64(mode&0o777), 8)
}

func escapePowerShellComment(value string) string {
	return strings.ReplaceAll(value, "`", "``")
}
