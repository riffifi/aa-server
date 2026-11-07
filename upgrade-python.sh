#!/usr/bin/env bash
if [ -z "${BASH_VERSION:-}" ]; then printf '%s\n' 'Bash is required: bash install.sh migrate --help' >&2; exit 2; fi
set +x
set -euo pipefail
umask 077
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
source scripts/installer-ui.sh
source scripts/installer-system.sh
source scripts/installer-env.sh
python_dir=$PWD python_project=antiattendance-sessions
env_file=.env.go project=antiattendance-go project_explicit=0 timeout=600
stop_python=0 interactive=auto build=1 pull=1 prepare_only=0 install_deps=0
configure_nginx=0 tls=0 mode=migration
metrics=auto
setup_args=()
if (($#)) && [[ $1 != -* ]]; then shift; fi
while (($#)); do
  if [[ $1 == --*=* ]]; then set -- "${1%%=*}" "${1#*=}" "${@:2}"; fi
  case "$1" in
    --from-python) ui_value "$@"; python_dir=$2; shift 2 ;;
    --python-project) ui_value "$@"; python_project=$2; shift 2 ;;
    --env-file|-e) ui_value "$@"; env_file=$2; shift 2 ;;
    --project|-p) ui_value "$@"; project=$2; project_explicit=1; shift 2 ;;
    --timeout) ui_value "$@"; timeout=$2; shift 2 ;;
    --stop-python) stop_python=1; shift ;;
    --interactive) interactive=1; shift ;;
    --non-interactive) interactive=0; shift ;;
    --no-start) prepare_only=1; shift ;;
    --no-install-deps) install_deps=0; shift ;;
    --install-deps) install_deps=1; shift ;;
    --no-build) build=0; shift ;;
    --skip-pull) pull=0; shift ;;
    --metrics) metrics=1; setup_args+=(--metrics); shift ;;
    --no-metrics) metrics=0; setup_args+=(--no-metrics); shift ;;
    --no-host-nginx|--no-tls) shift ;;
    --site-url|--public-url|--url|--aaid-url|--site-port|--port|--aaid-port|--panel-port)
      ui_value "$@"; setup_args+=("$1" "$2"); shift 2 ;;
    --log-level) ui_value "$@"; ui_set_log_level "$2"; shift 2 ;;
    --quiet|-q) ui_set_log_level error; shift ;;
    --verbose|-v|--debug) ui_set_log_level debug; shift ;;
    --plain) UI_PLAIN=1; shift ;;
    --no-color) UI_NO_COLOR=1; shift ;;
    --log-file) ui_value "$@"; UI_LOG_FILE=$2; shift 2 ;;
    --show-password) shift ;;
    --help|-h)
      printf '%s\n' 'Migrate Python to Go, preserving SQLite data and original keys.' \
        'sudo bash install.sh migrate --from-python /home/aa/aa-server' \
        'For unattended operation, add --non-interactive --stop-python.' \
        'Source project: --python-project antiattendance-sessions.' \
        'Target: --project antiattendance-go --env-file .env.go.' \
        'Also: --no-start, --no-build, --no-install-deps, --timeout, --log-level, --plain.' \
        'New ports: 18080 / 18091 / 18092. Switch host nginx manually after import.' \
        'Python is stopped for the snapshot and restored on failure.'
      exit 0 ;;
    *) ui_die "This option is not supported during Python migration: $1. Help: bash upgrade-python.sh --help" ;;
  esac
done
[[ -d $python_dir && -f $python_dir/.env ]] || ui_die "Source .env not found. Use --from-python DIRECTORY."
if ((!project_explicit)) && [[ -f $env_file ]]; then
  saved_project=$(installer_env_value "$env_file" GO_INSTALL_PROJECT)
  project=${saved_project:-$project}
fi
installer_project "$python_project"; installer_project "$project"
[[ $python_project != "$project" ]] || ui_die "Source and target Compose projects must be different."
[[ $timeout =~ ^[0-9]{1,4}$ ]] && ((10#$timeout >= 1 && 10#$timeout <= 3600)) || ui_die "--timeout: between 1 and 3600."
[[ ! -L $env_file ]] || ui_die "The Go settings file must not be a symbolic link."
if [[ $interactive == auto ]]; then interactive=0; if [[ -t 0 && -t 1 ]]; then interactive=1; fi; fi
printf '\nPython to Go / PostgreSQL migration detected.\nSource databases and keys will be saved in a private backup.\nGo uses new ports 18080 / 18091 / 18092. Switch host nginx upstreams after import.\nImages are built before Python is stopped.\n'
if ((!prepare_only && !stop_python)); then
  if ((!interactive)); then ui_die "A consistent snapshot requires stopping Python. Add --stop-python or run the wizard in a terminal."; fi
  printf 'Allow stopping Python after the build and migrating data? [yes/NO]: ' >&2
  IFS= read -r answer || exit 2
  answer=${answer%$'\r'}
  case "${answer,,}" in yes|y) stop_python=1 ;; *) printf 'Migration cancelled; server unchanged.\n'; exit 0 ;; esac
fi
if ((EUID != 0 && install_deps)) && command -v sudo >/dev/null; then
  if [[ -t 0 ]]; then sudo -v; else sudo -n true || ui_die "Rerun the command with sudo."; fi
fi
if [[ $metrics == auto && -f $env_file ]]; then metrics=$(installer_env_value "$env_file" GO_INSTALL_METRICS); metrics=${metrics:-auto}; fi
if [[ $metrics == auto ]]; then
  metrics=0
  if [[ $(uname -s) == Linux && -d /run/systemd/system ]]; then metrics=1; fi
fi
[[ $metrics == 0 || $metrics == 1 ]] || ui_die "GO_INSTALL_METRICS must be 0 or 1."
if ((metrics && !prepare_only)); then
  [[ $(uname -s) == Linux && -d /run/systemd/system ]] || ui_die "Metrics require Linux/systemd. Solution: add --no-metrics."
  if ((EUID != 0)); then
    if [[ -t 0 ]]; then sudo -v; else sudo -n true || ui_die "Metrics require root. Solution: rerun migration with sudo or --no-metrics."; fi
  fi
fi
total=$((8 + metrics))
if ((prepare_only)); then total=3; fi
ui_init "$total" 'Upgrading Python to Go'
python_stopped=0
running_ids=()
rollback() {
  local status=$?
  trap - EXIT
  if ((status != 0)) && [[ -f $env_file && -n ${manifest:-} && -f ${manifest:-} ]]; then
    "${helper[@]}" mark-pending "${helper_options[@]}" --env-file "$env_file" --pending 1 >> "$UI_LOG_FILE" 2>&1 || true
  fi
  if ((status != 0 && python_stopped)); then
    printf '\n[RECOVERY] Stopping target Go services and restoring original Python containers.\n' >&2
    if "${compose[@]}" stop sessions aaid nginx >> "$UI_LOG_FILE" 2>&1; then
      if (("${#running_ids[@]}" > 0)); then
        if ! "${DOCKER[@]}" start "${running_ids[@]}" >> "$UI_LOG_FILE" 2>&1; then
          printf '[ERROR] Automatic Python recovery failed. Container IDs and backup: %s\n' "$backup_dir" >&2
        else
          printf '[RESTORED] Previously running Python containers restarted. Go data and backup were preserved.\n' >&2
        fi
      fi
    else
      printf '[ERROR] Could not stop Go. Check the log and free the original ports before starting Python.\n' >&2
    fi
  fi
  ui_cleanup
  exit "$status"
}
trap rollback EXIT
ui_run 'Checking and preparing dependencies' installer_dependencies
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  privileged docker info >/dev/null 2>&1 || ui_die "Start Docker and retry with sudo."
  DOCKER=(sudo -n docker)
fi
mkdir -p .go-data
backup_dir=$(mktemp -d ".go-data/python-backup-$(date +%Y%m%d-%H%M%S)-XXXXXX")
manifest="$backup_dir/manifest.json"
helper=(python3 scripts/python-upgrade.py)
helper_options=(--manifest "$manifest" --python-project "$python_project" --project "$project")
if [[ ${DOCKER[0]} == sudo ]]; then helper_options+=(--docker-sudo); fi
ui_run 'Discovering source Python containers and checking ports' "${helper[@]}" discover "${helper_options[@]}"
site_port=18080 aaid_port=18091 panel_port=18092
public_url=$(installer_env_value "$python_dir/.env" PUBLIC_URL); public_url=${public_url:-https://aa.ytc-srv.org}
aaid_url=$(installer_env_value "$python_dir/.env" AAID_PUBLIC_URL); aaid_url=${aaid_url:-https://aaid.ytc-srv.org}
child_args=(install --no-start --non-interactive --no-install-deps --no-host-nginx --no-tls
  --env-file "$env_file" --project "$project"
  --site-url "$public_url" --aaid-url "$aaid_url"
  --site-port "$site_port" --aaid-port "$aaid_port" --panel-port "$panel_port"
  --log-level "$UI_LOG_LEVEL" --plain "${setup_args[@]}")
ui_run 'Preparing Go settings on new ports' bash install-go.sh "${child_args[@]}"
"${helper[@]}" mark-pending "${helper_options[@]}" --env-file "$env_file" --pending 1
public_url=$(installer_env_value "$env_file" GO_PUBLIC_URL)
aaid_url=$(installer_env_value "$env_file" GO_AAID_PUBLIC_URL)
site_port=$(installer_env_value "$env_file" GO_PORT)
aaid_port=$(installer_env_value "$env_file" GO_AAID_PORT)
panel_port=$(installer_env_value "$env_file" GO_PANEL_PORT)
legacy_ports=()
for name in site aaid panel; do
  legacy_ports+=("$("${helper[@]}" port "${helper_options[@]}" --name "$name")")
done
for target_port in "$site_port" "$aaid_port" "$panel_port"; do
  for legacy_port in "${legacy_ports[@]}"; do
    [[ $target_port != "$legacy_port" ]] || ui_die "Go must use new ports; port $target_port belongs to the source deployment."
  done
done
compose=("${DOCKER[@]}" compose --env-file "$env_file" --project-name "$project" -f compose.go.yaml)
if ((prepare_only)); then
  ui_finish 'Go settings prepared; Python was not stopped'
  printf 'Continue: sudo bash install.sh migrate --from-python %q --python-project %q --env-file %q --project %q\n' "$python_dir" "$python_project" "$env_file" "$project"
  exit 0
fi
prepare_images() {
  if ((pull)); then "${compose[@]}" pull postgres nginx || return; fi
  if ((build)); then installer_build; else "${DOCKER[@]}" image inspect antiattendance-go-native:local >/dev/null; fi
}
ui_run 'Building and preparing images before stopping Python' prepare_images
ids=$("${helper[@]}" ids "${helper_options[@]}" --running-only)
ids=${ids//$'\r'/}
if [[ -n $ids ]]; then mapfile -t running_ids <<< "$ids"; fi
python_stopped=1
if (("${#running_ids[@]}" > 0)); then ui_run 'Stopping Python for a consistent snapshot' "${DOCKER[@]}" stop -t 30 "${running_ids[@]}"; else ui_run 'Python is already stopped' true; fi
backup_sources() {
  cp -p -- "$python_dir/.env" "$backup_dir/python.env"
  if [[ -f $python_dir/compose.yaml ]]; then cp -p -- "$python_dir/compose.yaml" "$backup_dir/compose.python.yaml"; fi
  "${helper[@]}" backup "${helper_options[@]}" --directory "$backup_dir"
}
ui_run 'Backing up SQLite, WAL and original keys' backup_sources
ui_run 'Validating and exporting a consistent SQLite snapshot' python3 scripts/export_sqlite_for_go.py \
  --sessions "$backup_dir/sessions/sessions.sqlite3" --aaid "$backup_dir/aaid/aaid.sqlite3" --output "$backup_dir/snapshot.jsonl"
ui_run 'Importing and checking Go services on new ports' bash migrate-go.sh \
  --snapshot "$backup_dir/snapshot.jsonl" --master-key "$backup_dir/sessions/master.key" --signing-key "$backup_dir/aaid/signing.key" \
  --env-file "$env_file" --project "$project" --timeout "$timeout" --no-build --no-install-deps --plain --log-level "$UI_LOG_LEVEL"
if ((metrics)); then ui_run 'Enabling resource and durable traffic counters' privileged python3 scripts/install_metrics.py --project "$project"; fi
"${helper[@]}" mark-pending "${helper_options[@]}" --env-file "$env_file" --pending 0
python_stopped=0
ui_finish 'Python upgraded to Go; data, keys and panel password preserved'
printf '\nBackup: %s\nSource containers and volumes preserved. Host nginx unchanged.\nSite: %s\nAA ID: %s\nPanel: http://127.0.0.1:%s/panel\nPanel password: original, from the Python database.\n' "$backup_dir" "$public_url" "$aaid_url" "$panel_port"
printf '\n[NGINX SWITCH REQUIRED]\nUpdate proxy_pass in the existing server blocks:\n  %s: http://127.0.0.1:%s;\n  %s: http://127.0.0.1:%s;\nThen: sudo nginx -t && sudo systemctl reload nginx\nPublic URLs will work after nginx is switched.\n' "$public_url" "$site_port" "$aaid_url" "$aaid_port"
