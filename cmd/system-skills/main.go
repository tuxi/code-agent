// Command system-skills validates and materializes the shared CodeAgent system
// skill lock. It is used by release scripts and can optionally sync the generated
// bundle into AgentKit's SwiftPM resources.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"code-agent/internal/systemskills"
)

func main() {
	repoRoot := flag.String("root", ".", "code-agent repository root")
	manifestPath := flag.String("manifest", "skills/system-skills.json", "canonical system skill manifest")
	outputDir := flag.String("output", "internal/systemskills/bundle", "generated embedded bundle directory")
	agentkitDir := flag.String("agentkit-resources", "", "optional AgentKit Resources/skills/.system directory")
	write := flag.Bool("write", false, "update hashes, write the manifest, and regenerate the bundle")
	check := flag.Bool("check", false, "validate canonical sources and the generated bundle")
	flag.Parse()

	if *write == *check {
		fmt.Fprintln(os.Stderr, "error: pass exactly one of --write or --check")
		os.Exit(2)
	}
	root, err := filepath.Abs(*repoRoot)
	if err != nil {
		fail(err)
	}
	manifest := *manifestPath
	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(root, manifest)
	}
	output := *outputDir
	if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}

	if *check {
		if err := systemskills.CheckBundle(root, manifest, output); err != nil {
			fail(err)
		}
		if *agentkitDir != "" {
			if err := systemskills.CheckBundle(root, manifest, *agentkitDir); err != nil {
				fail(fmt.Errorf("AgentKit resources: %w", err))
			}
		}
		fmt.Printf("system skills: valid (%s)\n", manifest)
		return
	}

	if err := systemskills.WriteBundle(root, manifest, output); err != nil {
		fail(err)
	}
	if *agentkitDir != "" {
		if err := systemskills.SyncBundle(output, *agentkitDir); err != nil {
			fail(err)
		}
	}
	fmt.Printf("system skills: generated (%s -> %s)\n", manifest, output)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
