package sandbox

import "strings"

// NetworkCommandPrefixes are commands whose primary purpose is reaching the
// network. The approval-mode layer uses them to decide that a run_command needs
// a human even in "auto" tier (which auto-runs in-workspace commands): a
// network-capable command's blast radius is the machine's credentials and
// external services, not just the workspace tree. Matched on word boundaries
// like CommandPolicy prefixes, so "git push" matches "git push origin main"
// but not "git pushd".
//
// The list is deliberately curated, not exhaustive: an unrecognized command
// already falls through to Confirm (human) in the policy, so a missed entry
// only makes auto tier slightly more permissive, never less safe. The hard
// Block list and protected paths stay in force regardless of mode.
var NetworkCommandPrefixes = []string{
	// Direct network clients.
	"curl", "wget", "ssh", "scp", "sftp", "telnet", "nc", "ncat", "socat",
	// Git network operations (fetch is auto-allowed today; included for completeness).
	"git push", "git pull", "git fetch", "git clone", "git remote add", "git submodule update", "git lfs fetch", "git lfs pull",
	// Package managers reach registries.
	"npm install", "npm ci", "npm publish", "npm login", "npm logout",
	"yarn add", "yarn install", "yarn publish", "pnpm add", "pnpm install", "pnpm publish",
	"bun install", "bun add", "npx",
	"pip install", "pip3 install", "pip download", "pip3 download", "pipx install",
	"go get", "go install", "go mod download",
	"cargo add", "cargo publish", "cargo update", "cargo fetch", "cargo install",
	"mvn dependency:get", "gradle dependencies", "pod install", "pod update",
	// Container runtimes pull and publish images.
	"docker pull", "docker push", "docker login", "docker logout", "docker build", "docker run", "docker compose pull",
	// System package managers.
	"brew install", "brew update", "brew upgrade", "brew tap",
	"apt-get install", "apt-get update", "apt install", "apt update",
	"yum install", "yum update", "dnf install", "dnf update",
}

// IsNetworkCommand reports whether command is (or starts with) a known
// network-reaching command, matched on word boundaries. "" and unknown
// commands report false.
func IsNetworkCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	for _, p := range NetworkCommandPrefixes {
		if matchesPrefix(cmd, p) {
			return true
		}
	}
	return false
}
