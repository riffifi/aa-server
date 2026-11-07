#!/usr/bin/env bash
if [ -z "${BASH_VERSION:-}" ]; then printf '%s\n' 'Bash is required: bash migrate-go.sh --help' >&2; exit 2; fi
set +x
set -euo pipefail
umask 077
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
source scripts/installer-ui.sh
source scripts/installer-system.sh
source scripts/installer-env.sh
snapshot= master= signing= env_file=.env.go project=antiattendance-go explicit_project=0 timeout=240
configure_nginx=0 tls=0 install_deps=1 mode=migration build=1
while (($#)); do
  if [[ $1 == --*=* ]]; then set -- "${1%%=*}" "${1#*=}" "${@:2}"; fi
  case "$1" in
    --snapshot) ui_value "$@"; snapshot=$2; shift 2 ;;
    --master-key) ui_value "$@"; master=$2; shift 2 ;;
    --signing-key) ui_value "$@"; signing=$2; shift 2 ;;
    --env-file|-e) ui_value "$@"; env_file=$2; shift 2 ;;
    --project|-p) ui_value "$@"; project=$2; explicit_project=1; shift 2 ;;
    --timeout) ui_value "$@"; timeout=$2; shift 2 ;;
    --no-build) build=0; shift ;;
    --no-install-deps) install_deps=0; shift ;;
    --install-deps) install_deps=1; shift ;;
    --plain) UI_PLAIN=1; shift ;;
    --no-color) UI_NO_COLOR=1; shift ;;
    --log-level) ui_value "$@"; ui_set_log_level "$2"; shift 2 ;;
    --quiet|-q) ui_set_log_level error; shift ;;
    --verbose|-v|--debug) ui_set_log_level debug; shift ;;
    --log-file) ui_value "$@"; UI_LOG_FILE=$2; shift 2 ;;
    --help|-h)
      printf '%s\n' 'AntiAttendance · SQLite to PostgreSQL migration' \
        'bash migrate-go.sh --snapshot FILE --master-key FILE --signing-key FILE [options]' \
        'Options: --env-file FILE, --project NAME, --timeout SECONDS, --no-build,' \
        '           --no-install-deps, --plain, --no-color, --log-level error|warn|info|debug,' \
        '           --quiet (-q), --verbose (-v), --debug, --log-file FILE.' \
        'Prepare settings: bash install.sh install --no-start.' \
        'Stop writes to the source databases and export a snapshot first. The target database must be empty.' \
        'Migration preserves the source databases, keys and panel password; host nginx is not switched automatically.'
      exit 0 ;;
    *) ui_die "Unknown option: $1. Help: --help" ;;
  esac
done
for file in "$snapshot" "$master" "$signing" "$env_file"; do
  [[ -n $file && -f $file ]] || ui_die "Existing --snapshot, --master-key, --signing-key and --env-file files are required."
done
if ((!explicit_project)); then
  saved_project=$(installer_env_value "$env_file" GO_INSTALL_PROJECT)
  project=${saved_project:-$project}
fi
installer_project "$project"
[[ $timeout =~ ^[0-9]{1,4}$ ]] && ((10#$timeout >= 1 && 10#$timeout <= 3600)) || ui_die "--timeout: between 1 and 3600."
public_url=$(installer_env_value "$env_file" GO_PUBLIC_URL); public_url=${public_url:-https://aa.ytc-srv.org}
aaid_url=$(installer_env_value "$env_file" GO_AAID_PUBLIC_URL); aaid_url=${aaid_url:-https://aaid.ytc-srv.org}
site_port=$(installer_env_value "$env_file" GO_PORT); site_port=${site_port:-18080}
aaid_port=$(installer_env_value "$env_file" GO_AAID_PORT); aaid_port=${aaid_port:-18091}
panel_port=$(installer_env_value "$env_file" GO_PANEL_PORT); panel_port=${panel_port:-18092}
installer_origin "$public_url" 'Site URL'; installer_origin "$aaid_url" 'AA ID URL'
[[ ${public_url%/} != "${aaid_url%/}" ]] || ui_die "Site and AA ID must have different origins."
installer_port 'Site' "$site_port"; installer_port 'AA ID' "$aaid_port"; installer_port 'Panel' "$panel_port"
((10#$site_port != 10#$aaid_port && 10#$site_port != 10#$panel_port && 10#$aaid_port != 10#$panel_port)) || ui_die "The three local ports must be different."
if ((EUID != 0 && install_deps)) && command -v sudo >/dev/null; then
  if [[ -t 0 ]]; then sudo -v; else sudo -n true || ui_die "Run sudo bash migrate-go.sh with the same options."; fi
fi
ui_init "$((8 + build))" 'Migrating data to Go / PostgreSQL'
ui_run 'Checking and preparing dependencies' installer_dependencies
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  privileged docker info >/dev/null 2>&1 || ui_die "Start Docker and retry with sudo."
  DOCKER=(sudo -n docker)
fi
snapshot=$(realpath -- "$snapshot"); master=$(realpath -- "$master"); signing=$(realpath -- "$signing")
compose=("${DOCKER[@]}" compose --env-file "$env_file" --project-name "$project" -f compose.go.yaml)
ui_run 'Checking the Compose configuration' "${compose[@]}" config --quiet
ui_run 'Checking resources, ports and DNS' installer_preflight
if ((build)); then ui_run 'Building Go services and Chromium' installer_build; fi
ui_run 'Starting PostgreSQL' "${compose[@]}" up -d --wait --wait-timeout "$timeout" postgres
ui_run 'Checking and preserving original keys' "${compose[@]}" run --rm --no-deps -v "$master:/source/master.key:ro" -v "$signing:/source/signing.key:ro" initialize aa-init -directory /keys -uid 10001 -schema -split -roles -master-source /source/master.key -signing-source /source/signing.key
ui_run 'Validating and atomically importing the snapshot' "${compose[@]}" run --rm --no-deps -e MASTER_KEY_FILE=/keys/sessions/master.key -v "$snapshot:/migration/snapshot:ro" initialize aa-import -input /migration/snapshot
ui_run 'Starting services and waiting for readiness' "${compose[@]}" up -d --no-build --wait --wait-timeout "$timeout"
ui_run 'Checking HTTP responses after migration' installer_health
ui_finish 'Data migrated and services verified'
printf '\nMigration details\n  Site: %s\n  AA ID: %s\n  Private panel: http://127.0.0.1:%s/panel\n  Panel password: original, preserved from the source database\n  Log: %s\n\nHost nginx was not switched. After verification, change upstreams to local ports %s and %s.\n' \
  "$public_url" "$aaid_url" "$panel_port" "$UI_LOG_FILE" "$site_port" "$aaid_port"
