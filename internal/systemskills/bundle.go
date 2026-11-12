package systemskills

import (
	"embed"
	"io/fs"
)

//go:embed bundle
var embedded embed.FS

// Embedded returns the immutable system skill bundle compiled into codeagentd,
// codeagent, and the embedded Runtime.
func Embedded() fs.FS {
	sub, err := fs.Sub(embedded, "bundle")
	if err != nil {
		panic("system skill bundle is missing: " + err.Error())
	}
	return sub
}
