package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Confirm prompts on a fresh stdin reader. Suitable for one-shot use where
// nothing else is reading stdin.
func Confirm(message string) bool {
	return confirm(bufio.NewReader(os.Stdin), message)
}

// confirm prompts using the provided reader. The REPL shares a single reader
// between its input loop and the approver so two bufio readers never race for
// stdin (the second would swallow buffered bytes meant for the first).
func confirm(r *bufio.Reader, message string) bool {
	fmt.Printf("%s [y/N]: ", message)
	input, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	input = strings.TrimSpace(strings.ToLower(input))
	return input == "y" || input == "yes"
}
