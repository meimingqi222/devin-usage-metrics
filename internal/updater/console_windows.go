package updater

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps the console helper from opening a window: the GUI
// parent has no console to share, so Windows would otherwise allocate one.
const createNoWindow = 0x08000000

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
