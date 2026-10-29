//go:build !unix

package processexec

import "os/exec"

// Non-Unix platforms retain CommandContext's direct-child cancellation. Pipe
// waiting is still bounded, but descendant cleanup needs a platform job API.
func configureTree(cmd *exec.Cmd) func() { return func() {} }
