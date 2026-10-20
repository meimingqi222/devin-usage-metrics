package updater

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Apply hands off to an out-of-process, bounded-wait replacement helper. It
// never exits the caller. A successful return requires the caller to exit within
// 120 seconds. Backups are deliberately retained for recovery, not overwritten.
func Apply(payload string) error {
	if _, err := assetName(runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	current, err := os.Executable()
	if err != nil {
		return err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return err
	}
	payload, err = filepath.Abs(payload)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(filepath.ToSlash(current), "/") {
		if p == "target" || p == "go-build" || strings.HasPrefix(p, "go-build") {
			return errors.New("refusing update of development build")
		}
	}
	if runtime.GOOS == "darwin" {
		suffix := filepath.Join("Agent Usage Metrics.app", "Contents", "MacOS", filepath.Base(current))
		if filepath.Base(current) != "Agent Usage Metrics" && filepath.Base(current) != "devin-usage-metrics" {
			return errors.New("unsupported app executable")
		}
		if !strings.HasSuffix(current, string(filepath.Separator)+suffix) {
			return errors.New("not running from the release .app bundle")
		}
		current = strings.TrimSuffix(current, filepath.Join("Contents", "MacOS", filepath.Base(current)))
		current = filepath.Clean(current)
	} else if filepath.Base(current) != "devin-usage-metrics.exe" && filepath.Base(current) != "Agent Usage Metrics.exe" {
		return errors.New("not running the packaged Windows executable")
	}
	if runtime.GOOS == "darwin" && filepath.Base(payload) != filepath.Base(current) {
		return errors.New("payload is not the supported release packaging")
	}
	if runtime.GOOS == "windows" {
		if filepath.Base(payload) != "Agent Usage Metrics.exe" && filepath.Base(payload) != "devin-usage-metrics.exe" {
			return errors.New("unsupported executable payload")
		}
		lib := filepath.Join(filepath.Dir(payload), "mygo-sqlite3.dll")
		if s, err := os.Lstat(lib); err != nil || !s.Mode().IsRegular() {
			return errors.New("update is missing its SQLite runtime")
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(current), "mygo-sqlite3.dll.old")); !os.IsNotExist(err) {
			return errors.New("previous SQLite backup exists")
		}
	}
	s, err := os.Lstat(payload)
	if err != nil {
		return err
	}
	if s.Mode()&os.ModeSymlink != 0 || (runtime.GOOS == "darwin" && !s.IsDir()) || (runtime.GOOS == "windows" && !s.Mode().IsRegular()) {
		return errors.New("invalid update payload")
	}
	resolved, err := filepath.EvalSymlinks(payload)
	if err != nil {
		return err
	}
	if strings.EqualFold(resolved, current) {
		return errors.New("payload is the running application")
	}
	if _, err = os.Lstat(current + ".old"); !os.IsNotExist(err) {
		return errors.New("previous update backup exists; remove it after verifying the installed version")
	}
	probe, err := os.CreateTemp(filepath.Dir(current), ".aum-update-probe-")
	if err != nil {
		return err
	}
	probe.Close()
	os.Remove(probe.Name())
	// Keep scripts and their local copy in a private directory on the destination
	// filesystem, so final renames cannot fail due to cross-device moves.
	stage, err := os.MkdirTemp(filepath.Dir(current), ".aum-install-")
	if err != nil {
		return err
	}
	script := macHelper
	name := "helper.sh"
	if runtime.GOOS == "windows" {
		script = windowsHelper
		name = "helper.ps1"
	}
	scriptPath := filepath.Join(stage, name)
	if err = os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		os.RemoveAll(stage)
		return err
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("/bin/sh", scriptPath, strconv.Itoa(os.Getpid()), current, resolved, stage)
	} else {
		// Values travel through the environment, never interpolated as script code.
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
		cmd.Env = append(os.Environ(), "AUM_PID="+strconv.Itoa(os.Getpid()), "AUM_CURRENT="+current, "AUM_PAYLOAD="+resolved, "AUM_STAGE="+stage)
	}
	if err = cmd.Start(); err != nil {
		os.RemoveAll(stage)
		return err
	}
	go cmd.Wait()
	return nil
}

const macHelper = `#!/bin/sh
set -eu
pid=$1; current=$2; payload=$3; stage=$4
trap 'rm -rf "$stage"' EXIT
/bin/cp -R "$payload" "$stage/new.app"
i=0
while kill -0 "$pid" 2>/dev/null; do
 i=$((i+1)); [ "$i" -lt 120 ] || exit 1; sleep 1
done
[ ! -e "$current.old" ] || exit 1
/bin/mv "$current" "$current.old"
if ! /bin/mv "$stage/new.app" "$current"; then
 /bin/mv "$current.old" "$current"
 /usr/bin/open "$current"
 exit 1
fi
/usr/bin/open "$current"
`
const windowsHelper = `$ErrorActionPreference = 'Stop'
$current = $env:AUM_CURRENT
$stage = $env:AUM_STAGE
$library = Join-Path (Split-Path -Parent $current) 'mygo-sqlite3.dll'
$libraryMoved = $false
$executableMoved = $false
try {
 Copy-Item -LiteralPath $env:AUM_PAYLOAD -Destination (Join-Path $stage 'new.exe')
 Copy-Item -LiteralPath (Join-Path (Split-Path -Parent $env:AUM_PAYLOAD) 'mygo-sqlite3.dll') -Destination (Join-Path $stage 'new.dll')
 $parent = Get-Process -Id ([int]$env:AUM_PID) -ErrorAction SilentlyContinue
 if ($parent -and -not $parent.WaitForExit(120000)) { throw 'Application did not exit' }
 if ((Test-Path -LiteralPath ($current + '.old')) -or (Test-Path -LiteralPath ($library + '.old'))) { throw 'Backup already exists' }
 Move-Item -LiteralPath $current -Destination ($current + '.old')
 $executableMoved = $true
 if (Test-Path -LiteralPath $library) { Move-Item -LiteralPath $library -Destination ($library + '.old'); $libraryMoved = $true }
 Move-Item -LiteralPath (Join-Path $stage 'new.dll') -Destination $library
 Move-Item -LiteralPath (Join-Path $stage 'new.exe') -Destination $current
 Start-Process -FilePath $current
} catch {
 if ($executableMoved) {
  Remove-Item -LiteralPath $current -Force -ErrorAction SilentlyContinue
  Move-Item -LiteralPath ($current + '.old') -Destination $current
  if ($libraryMoved) {
   Remove-Item -LiteralPath $library -Force -ErrorAction SilentlyContinue
   Move-Item -LiteralPath ($library + '.old') -Destination $library
  }
  Start-Process -FilePath $current
 }
 throw
} finally { Remove-Item -LiteralPath $stage -Recurse -Force }
`
