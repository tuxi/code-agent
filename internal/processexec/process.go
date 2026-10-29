// Package processexec bounds subprocess cancellation and inherited output pipes.
package processexec

import (
	"os/exec"
	"time"
)

// Configure must be called before Start/Run. Cancel terminates the owned process
// group where supported; WaitDelay bounds pipes held open after the parent exits.
// Call the returned cleanup after Wait/Run, including on a normal parent exit.
func Configure(cmd *exec.Cmd) func() {
	cmd.WaitDelay = time.Second
	return configureTree(cmd)
}
