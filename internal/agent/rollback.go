package agent

import (
	"strings"
)

func BuildRollbackHint(paths []string) string {
	if len(paths) == 0 {
		return "Rollback hint unavailable: no touched files were detected from the patch."
	}

	var b strings.Builder

	b.WriteString("Rollback hint:\n")
	b.WriteString("This patch touched:\n")
	for _, path := range paths {
		b.WriteString("- ")
		b.WriteString(path)
		b.WriteString("\n")
	}

	b.WriteString("\nReview the patch-scoped diff:\n")
	for _, path := range paths {
		b.WriteString("git diff -- ")
		b.WriteString(path)
		b.WriteString("\n")
	}

	b.WriteString("\nManual rollback command:\n")
	for _, path := range paths {
		b.WriteString("git checkout -- ")
		b.WriteString(path)
		b.WriteString("\n")
	}

	b.WriteString("\nWarning:\n")
	b.WriteString("Only run the rollback command if you are sure these files did not contain important uncommitted changes before this patch. ")
	b.WriteString("A safer rollback strategy will later store pre-apply snapshots and restore only the Agent transaction.\n")

	return b.String()
}
