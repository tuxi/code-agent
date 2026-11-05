package shell

import "strings"

// Only file operands belong in the workspace path check. In particular,
// awk's first program operand often starts with '/', but is not a path.
// Recognize POSIX options; unknown options retain the conservative scan.
func awkReadOperands(args []string) []string {
	var files []string
	hasProgram, options := false, true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options {
			switch arg {
			case "--":
				options = false
				continue
			case "-f":
				if i+1 < len(args) {
					i++
					files = append(files, args[i])
				}
				hasProgram = true
				continue
			case "-F", "-v":
				if i+1 < len(args) {
					i++
				}
				continue
			}
			if strings.HasPrefix(arg, "-f") {
				files = append(files, arg[2:])
				hasProgram = true
				continue
			}
			if strings.HasPrefix(arg, "-F") || strings.HasPrefix(arg, "-v") {
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return append(files, args[i:]...)
			}
		}
		options = false
		if !hasProgram {
			hasProgram = true
			continue
		}
		files = append(files, arg)
	}
	return files
}
