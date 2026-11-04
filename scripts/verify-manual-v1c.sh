#!/usr/bin/env bash
# Offline candidate verification only. No installation, production Store/grant,
# configured Provider, account change, dependency download, or remote Git access.
set -euo pipefail

task_source_root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
task_output="$(mktemp -d "${TMPDIR:-/tmp}/manual-v1c-verify.XXXXXXXX")"
export GOPROXY=off GOSUMDB=off
export GOCACHE="${GOCACHE:-$task_output/go-cache}"
python3 - "$task_source_root" "$task_output" <<'PY'
import hashlib, json, pathlib, shutil, subprocess, sys
source, output = map(pathlib.Path, sys.argv[1:])
manifest = json.loads((source / 'docs/contracts/manual-collaboration-v1c-candidate.json').read_text())
files = manifest['candidate']['source_files_sha256']
for name, expected in files.items():
    path = pathlib.PurePosixPath(name)
    if path.is_absolute() or '..' in path.parts or '.git' in path.parts:
        raise SystemExit('Invalid candidate source path')
    if hashlib.sha256((source / name).read_bytes()).hexdigest() != expected:
        raise SystemExit('Candidate source hash changed: ' + name)
for version in ['baseline', 'candidate']:
    target = output / version
    subprocess.run(['git', 'clone', '--quiet', '--no-hardlinks', str(source), str(target)], check=True)
    subprocess.run(['git', '-C', str(target), 'checkout', '--quiet', '--detach', manifest['candidate']['base_commit']], check=True)
    selected = files if version == 'candidate' else [manifest['compatibility_fixture']]
    for name in selected:
        dest = target / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source / name, dest)
(output / 'input-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
PY

printf 'Offline verification output: %s\n' "$task_output"
for task_version in baseline candidate; do
  (
    cd "$task_output/$task_version"
    go build -o "$task_output/$task_version-bridge" ./cmd/codeagent-mcp
    go build -o "$task_output/$task_version-runtime-fixture" ./internal/mcpbridge/testdata/v1c_compat
  )
done
export CODEAGENT_COMPAT_OLD_RUNTIME="$task_output/baseline-runtime-fixture"
export CODEAGENT_COMPAT_NEW_RUNTIME="$task_output/candidate-runtime-fixture"
export CODEAGENT_COMPAT_OLD_BRIDGE="$task_output/baseline-bridge"
export CODEAGENT_COMPAT_NEW_BRIDGE="$task_output/candidate-bridge"
(
  cd "$task_output/candidate"
  go test -race ./internal/mcpbridge -run '^TestV1CCrossVersionProcesses$' -count=1 -json > "$task_output/cross-version.jsonl"
  go test -race ./internal/session/... ./internal/conversation ./internal/server ./internal/mcpbridge ./cmd/codeagent-mcp ./internal/executionevidence \
    -run 'TestValidationContract|TestAcceptedValidation|TestDurableValidation|TestManual|TestHandoff|TestEvidence|TestConfirmed|TestDevelopmentEvidence|TestAttention|TestBridgeAttention|TestDaemonContinuesAfterBridgeExit|TestProjectConfig|Test.*Profile' \
    -count=1 -json > "$task_output/regression.jsonl"
  go vet ./internal/session/... ./internal/conversation ./internal/server ./internal/mcpbridge ./cmd/codeagent-mcp ./internal/executionevidence ./internal/mcpbridge/testdata/v1c_compat
  go build -o "$task_output/candidate-codeagentd" ./cmd/codeagentd
)
printf 'Offline checks passed. No deployment performed. Logs: %s\n' "$task_output"
