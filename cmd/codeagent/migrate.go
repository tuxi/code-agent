package main

import (
	"fmt"
	"os"

	"code-agent/internal/app"
)

// runMigrate migrates legacy config.yaml files into the settings.json layer
// (design-config-settings-merge.md stage C). It reads ~/.codeagent/config.yaml
// and <cwd>/.codeagent/config.yaml and writes the merged settings into
// settings.json (user + project scope). config.yaml files are left in place;
// the user removes them after verifying the migration.
func runMigrate(args []string) error {
	var dryRun bool
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-dry-run":
			dryRun = true
		default:
			remaining = append(remaining, args[i])
		}
	}
	if len(remaining) > 0 {
		return fmt.Errorf("unexpected argument: %s", remaining[0])
	}

	root, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	if dryRun {
		// Report what would be migrated without writing anything.
		userPath := home + "/.codeagent/config.yaml"
		projPath := root + "/.codeagent/config.yaml"
		if _, err := os.Stat(userPath); err == nil {
			fmt.Printf("would migrate user config: %s\n", userPath)
		}
		if _, err := os.Stat(projPath); err == nil {
			fmt.Printf("would migrate project config: %s\n", projPath)
		}
		if _, err := os.Stat(userPath); err != nil && os.IsNotExist(err) {
			if _, err2 := os.Stat(projPath); err2 != nil && os.IsNotExist(err2) {
				fmt.Println("no config.yaml found; nothing to migrate")
			}
		}
		return nil
	}

	if err := app.MigrateConfigToSettings(root, home); err != nil {
		return err
	}
	fmt.Println("migrated config.yaml → settings.json")
	fmt.Println("  user:    ~/.codeagent/settings.json")
	fmt.Println("  project: <cwd>/.codeagent/settings.json (when the project had config.yaml)")
	fmt.Println("config.yaml files were left in place; verify and remove them when ready.")
	return nil
}
