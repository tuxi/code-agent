#!/bin/sh
# Inventory only: no sudo, network, credential reads, setup or isolation probes.
set -eu
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH

usage() {
    printf '%s\n' 'Usage: /bin/sh inspect-managed-host.sh [--label NAME] [--workspace /absolute/path] [--app /absolute/Talkify.app]'
    printf '%s\n' 'Outputs metadata only. This does not establish isolation; missing evidence remains unknown.'
}

label=unassigned
workspace=
app=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --help) usage; exit 0 ;;
        --label|--workspace|--app)
            option=$1
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$2" in *'
'*|*''*) printf '%s\n' 'Control characters are not accepted.' >&2; exit 2 ;; esac
            case "$option" in
                --label) label=$2 ;;
                --workspace|--app)
                    case "$2" in /*) ;; *) printf '%s\n' 'Paths must be absolute.' >&2; exit 2 ;; esac
                    if [ "$option" = --workspace ]; then workspace=$2; else app=$2; fi ;;
            esac
            shift 2 ;;
        *) usage >&2; exit 2 ;;
    esac
done

metadata() {
    target=$1
    printf 'path=%s\n' "$target"
    if [ -e "$target" ] || [ -L "$target" ]; then
        /usr/bin/stat -f 'type=%HT uid=%u gid=%g mode=%Sp inode=%i device=%d links=%l' "$target"
        if [ -L "$target" ]; then printf '%s\n' 'symlink=true'; else printf '%s\n' 'symlink=false'; fi
        /bin/ls -lde "$target"
    else
        printf '%s\n' 'exists=false'
    fi
}

printf 'schema=managed-host-readonly-inventory/v1\nlabel=%s\n' "$label"
printf 'collected_at_utc=%s\n' "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')"
if [ "$(/usr/bin/uname -s)" != Darwin ]; then
    printf '%s\n' 'unsupported_os=true' 'isolation_verified=false'
    exit 3
fi
/usr/bin/sw_vers
printf 'architecture=%s\neuid=%s\negid=%s\ngroups=%s\n' "$(/usr/bin/uname -m)" "$(/usr/bin/id -u)" "$(/usr/bin/id -g)" "$(/usr/bin/id -G)"
if [ -n "${SSH_AUTH_SOCK:-}" ]; then printf '%s\n' 'ssh_agent_environment_present=true'; else printf '%s\n' 'ssh_agent_environment_present=false'; fi
if [ -n "${DYLD_INSERT_LIBRARIES:-}" ]; then printf '%s\n' 'dyld_injection_environment_present=true'; else printf '%s\n' 'dyld_injection_environment_present=false'; fi

printf '%s\n' 'section=explicit_paths'
metadata "$HOME"
metadata /Users/Shared
metadata /private/tmp
metadata "$HOME/.codeagent"
metadata "$HOME/Library/Application Support/Talkify/CodeAgentMCP"
printf '%s\n' 'section=system_tool_metadata_no_execution'
for tool in /usr/bin/git /usr/bin/python3 /opt/homebrew/bin/git /opt/homebrew/bin/python3; do metadata "$tool"; done
printf '%s\n' 'section=developer_directory'
/usr/bin/xcode-select -p 2>/dev/null || printf '%s\n' 'developer_directory=unknown_or_not_configured'

printf '%s\n' 'section=service_disable_overrides_only'
# A missing override does not prove that a service is off or unreachable.
/bin/launchctl print-disabled system 2>/dev/null | /usr/bin/grep -E 'com\.openssh\.sshd|com\.apple\.(smbd|screensharing|RemoteDesktop)' || printf '%s\n' 'service_disable_overrides=unknown_or_not_listed'
printf '%s\n' 'remote_access_verified=false'

if [ -n "$workspace" ]; then
    printf '%s\n' 'section=workspace_and_ancestors'
    target=$workspace
    while :; do
        metadata "$target"
        [ "$target" = / ] && break
        target=$(/usr/bin/dirname "$target")
    done
else
    printf '%s\n' 'workspace=not_supplied'
fi

if [ -n "$app" ]; then
    printf '%s\n' 'section=explicit_app_metadata'
    metadata "$app"
    if [ -d "$app" ]; then
        /usr/bin/codesign -d --entitlements :- "$app" 2>/dev/null | /usr/bin/plutil -p - 2>/dev/null | /usr/bin/grep -E '"com\.apple\.security\.(app-sandbox|get-task-allow|inherit|cs\.disable-library-validation)"' || printf '%s\n' 'selected_entitlements=unknown_or_absent'
        binary="$app/Contents/Resources/codeagentd"
        metadata "$binary"
        if [ -f "$binary" ] && [ ! -L "$binary" ]; then /usr/bin/shasum -a 256 "$binary"; fi
    fi
else
    printf '%s\n' 'app=not_supplied'
fi

printf '%s\n' 'credential_contents_read=false' 'network_probes_run=false' 'negative_isolation_probes_run=false' 'device_independence_verified=false' 'isolation_verified=false'
