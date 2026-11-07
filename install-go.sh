#!/usr/bin/env bash
if [ -z "${BASH_VERSION:-}" ]; then
  printf '%s\n' 'Bash is required: bash install.sh --help' >&2; exit 2
fi
set +x
set -euo pipefail
umask 077
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
source scripts/installer-ui.sh
source scripts/installer-system.sh
source scripts/installer-env.sh

mode=auto env_file=.env.go project=antiattendance-go timeout=240
public_url=https://aa.ytc-srv.org aaid_url=https://aaid.ytc-srv.org
site_port=18080 aaid_port=18091 panel_port=18092
configure_nginx=0 tls=0 tls_email= nginx_config=
interactive=auto start=1 build=1 pull=1 install_deps=1 deps_explicit=0
show_password=0
python_dir=$PWD python_project=antiattendance-sessions python_requested=0 stop_python=0
original_args=("$@")
declare -A updates=()
usage() {
  cat <<'HELP'
AntiAttendance · Go / PostgreSQL installation and updates

  bash install.sh [install|update|check|migrate] [options]

Without a command: install/update Go, or migrate Python if a legacy .env exists.
The first interactive run opens the setup wizard.

URLs and ports:
  --site-url URL         Site URL (also --public-url, --url)
  --aaid-url URL         AA ID
  --site-port PORT       Local site port (also --port)
  --aaid-port PORT       Local AA ID port
  --panel-port PORT      Local private panel port
  -e, --env-file FILE    Settings file, default: .env.go
  -p, --project NAME     Compose project name, default: antiattendance-go

Nginx and HTTPS:
  --configure-nginx     Configure separate site and AA ID server blocks
  --no-host-nginx       Manage host nginx manually
  --nginx-config FILE   Main nginx.conf to configure
  --tls-email EMAIL     Issue/check a Let's Encrypt certificate with Certbot
  --no-tls              Skip Certbot (for example, with an external TLS proxy)

Operation:
  migrate               Migrate a legacy Python installation to Go
  --from-python DIR     Legacy .env directory; enables Python migration
  --python-project NAME Source Compose project (antiattendance-sessions)
  --stop-python         Allow Python downtime for a consistent snapshot/migration
  --interactive         Prompt for settings in the terminal
  --non-interactive     Run without prompts; for automation
  --no-start            Prepare settings without starting services/nginx
  --check               Only check the existing configuration
  --no-build            Use an existing Go image
  --skip-pull           Skip pulling PostgreSQL/nginx images
  --timeout SECONDS     Service readiness timeout, default: 240
  --no-install-deps     Check dependencies without installing them
  --install-deps        Install missing dependencies (default)

Output:
  --plain               Plain text without animation or ANSI codes
  --no-color            Animation without colors; also supports NO_COLOR
  --log-level LEVEL     error, warn, info (default), debug
  -q, --quiet           Only errors and the result (log-level error)
  -v, --verbose         Stream command output without animation (log-level debug)
  --debug               Same as --log-level debug; no shell xtrace
  --log-file FILE       New private log file
  --show-password       Show the new password even without a terminal
  -h, --help            Show this help

Supports --site-port=18080. Explicit options update only the corresponding
settings; existing passwords and keys are preserved.
Domains: aa.ytc-srv.org and aaid.ytc-srv.org; *.ytc-srv.org and loopback are allowed.
The legacy Python installer is available as install-python.sh.
HELP
}
if (($#)) && [[ $1 != -* ]]; then mode=$1; shift; fi
while (($#)); do
  if [[ $1 == --*=* ]]; then set -- "${1%%=*}" "${1#*=}" "${@:2}"; fi
  case "$1" in
    --site-url|--public-url|--url) ui_value "$@"; public_url=$2; updates[GO_PUBLIC_URL]=${2%/}; shift 2 ;;
    --aaid-url) ui_value "$@"; aaid_url=$2; updates[GO_AAID_PUBLIC_URL]=${2%/}; shift 2 ;;
    --site-port|--port) ui_value "$@"; site_port=$2; updates[GO_PORT]=$2; shift 2 ;;
    --aaid-port) ui_value "$@"; aaid_port=$2; updates[GO_AAID_PORT]=$2; shift 2 ;;
    --panel-port) ui_value "$@"; panel_port=$2; updates[GO_PANEL_PORT]=$2; shift 2 ;;
    --env-file|-e) ui_value "$@"; env_file=$2; shift 2 ;;
    --project|-p) ui_value "$@"; project=$2; updates[GO_INSTALL_PROJECT]=$2; shift 2 ;;
    --timeout) ui_value "$@"; timeout=$2; shift 2 ;;
    --nginx-config) ui_value "$@"; nginx_config=$2; configure_nginx=1; updates[GO_INSTALL_NGINX]=1; updates[GO_NGINX_CONFIG]=$2; shift 2 ;;
    --configure-nginx) configure_nginx=1; updates[GO_INSTALL_NGINX]=1; shift ;;
    --no-host-nginx) configure_nginx=0; tls=0; updates[GO_INSTALL_NGINX]=0; updates[GO_INSTALL_TLS]=0; shift ;;
    --tls-email) ui_value "$@"; tls_email=$2; tls=1; configure_nginx=1; updates[GO_INSTALL_NGINX]=1; updates[GO_INSTALL_TLS]=1; updates[GO_TLS_EMAIL]=$2; shift 2 ;;
    --no-tls) tls=0; updates[GO_INSTALL_TLS]=0; shift ;;
    --interactive) interactive=1; shift ;;
    --non-interactive) interactive=0; shift ;;
    --no-start) start=0; shift ;;
    --check) mode=check; shift ;;
    --no-build) build=0; shift ;;
    --skip-pull) pull=0; shift ;;
    --no-install-deps) install_deps=0; deps_explicit=1; shift ;;
    --install-deps) install_deps=1; deps_explicit=1; shift ;;
    --plain) UI_PLAIN=1; shift ;;
    --no-color) UI_NO_COLOR=1; shift ;;
    --log-level) ui_value "$@"; ui_set_log_level "$2"; shift 2 ;;
    --quiet|-q) ui_set_log_level error; shift ;;
    --verbose|-v|--debug) ui_set_log_level debug; shift ;;
    --log-file) ui_value "$@"; UI_LOG_FILE=$2; shift 2 ;;
    --show-password) show_password=1; shift ;;
    --from-python) ui_value "$@"; python_dir=$2; python_requested=1; shift 2 ;;
    --python-project) ui_value "$@"; python_project=$2; python_requested=1; shift 2 ;;
    --stop-python) stop_python=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) ui_die "Unknown option: $1. Help: bash install.sh --help" ;;
  esac
done
[[ $mode == auto || $mode == install || $mode == update || $mode == check || $mode == migrate ]] || ui_die "Command must be install, update, check or migrate."
[[ ! -L $env_file ]] || ui_die "The settings file must not be a symbolic link."
fresh=1
if [[ -e $env_file ]]; then
  [[ -f $env_file ]] || ui_die "Settings must be a regular file: $env_file"
  fresh=0
fi
pending_python=
if [[ -f $env_file ]]; then pending_python=$(installer_env_value "$env_file" GO_PYTHON_MIGRATION_PENDING); fi
if ((python_requested)) || [[ $mode == migrate ]] || { [[ $mode == auto || $mode == update ]] && { [[ $pending_python == 1 ]] || { ((fresh)) && [[ -f $python_dir/.env ]]; }; }; }; then
  exec bash "$PWD/upgrade-python.sh" "${original_args[@]}"
fi
[[ $mode != update || $fresh == 0 ]] || ui_die "Missing $env_file. Use install for a new deployment, or migrate --from-python DIRECTORY for Python."
[[ $mode != check || $fresh == 0 ]] || ui_die "Missing $env_file. Run install --no-start first."
if [[ $mode == auto ]]; then if ((fresh)); then mode=install; else mode=update; fi; fi
env_value() {
  [[ -f $env_file ]] || return 0
  installer_env_value "$env_file" "$1"
}
load_setting() {
  local key=$1 fallback=$2
  if [[ -n ${updates[$key]+x} ]]; then printf '%s' "${updates[$key]}"; else
    local value; value=$(env_value "$key"); printf '%s' "${value:-$fallback}"
  fi
}
public_url=$(load_setting GO_PUBLIC_URL "$public_url")
aaid_url=$(load_setting GO_AAID_PUBLIC_URL "$aaid_url")
site_port=$(load_setting GO_PORT "$site_port")
aaid_port=$(load_setting GO_AAID_PORT "$aaid_port")
panel_port=$(load_setting GO_PANEL_PORT "$panel_port")
configure_nginx=$(load_setting GO_INSTALL_NGINX "$configure_nginx")
tls=$(load_setting GO_INSTALL_TLS "$tls")
tls_email=$(load_setting GO_TLS_EMAIL "$tls_email")
nginx_config=$(load_setting GO_NGINX_CONFIG "$nginx_config")
project=$(load_setting GO_INSTALL_PROJECT "$project")
if [[ $interactive == auto ]]; then
  interactive=0
  if ((fresh)) && [[ -t 0 && -t 1 ]]; then interactive=1; fi
fi
ask() {
  local reply default=$2
  printf '%s [%s]: ' "$1" "$default" >&2
  IFS= read -r reply || ui_die "Input ended. Use --non-interactive for automation."
  reply=${reply%$'\r'}
  REPLY=${reply:-$default}
}
if ((interactive)) && [[ $mode != check ]]; then
  if ((fresh)) && [[ -z ${updates[GO_INSTALL_NGINX]+x} ]]; then configure_nginx=1; fi
  if ((fresh)) && [[ -z ${updates[GO_INSTALL_TLS]+x} ]]; then tls=1; fi
  printf '\nAntiAttendance · setup wizard\nExisting installation passwords and keys are preserved.\n\n'
  ask 'Site URL' "$public_url"; public_url=$REPLY; updates[GO_PUBLIC_URL]=${REPLY%/}
  ask 'AA ID URL' "$aaid_url"; aaid_url=$REPLY; updates[GO_AAID_PUBLIC_URL]=${REPLY%/}
  ask 'Local site port' "$site_port"; site_port=$REPLY; updates[GO_PORT]=$REPLY
  ask 'Local AA ID port' "$aaid_port"; aaid_port=$REPLY; updates[GO_AAID_PORT]=$REPLY
  ask 'Local private panel port' "$panel_port"; panel_port=$REPLY; updates[GO_PANEL_PORT]=$REPLY
  ask 'Configure host nginx: yes/no' "$([[ $configure_nginx == 1 ]] && printf yes || printf no)"
  case "${REPLY,,}" in yes|y) configure_nginx=1 ;; no|n) configure_nginx=0; tls=0 ;; *) ui_die "Expected yes or no." ;; esac
  updates[GO_INSTALL_NGINX]=$configure_nginx
  if ((configure_nginx)); then
    ask 'Main nginx.conf (auto to detect automatically)' "${nginx_config:-auto}"
    nginx_config=$REPLY; [[ $nginx_config != auto ]] || nginx_config=
    updates[GO_NGINX_CONFIG]=$nginx_config
    ask 'Configure HTTPS with Let’s Encrypt: yes/no' "$([[ $tls == 1 ]] && printf yes || printf no)"
    case "${REPLY,,}" in yes|y) tls=1 ;; no|n) tls=0 ;; *) ui_die "Expected yes or no." ;; esac
    if ((tls)); then ask 'Email for Let’s Encrypt' "$tls_email"; tls_email=$REPLY; updates[GO_TLS_EMAIL]=$tls_email; fi
  fi
  updates[GO_INSTALL_TLS]=$tls
fi
installer_origin "$public_url" 'Site URL'; installer_origin "$aaid_url" 'AA ID URL'
[[ ${public_url%/} != "${aaid_url%/}" ]] || ui_die "Site and AA ID must have different origins."
installer_port 'Site' "$site_port"; installer_port 'AA ID' "$aaid_port"; installer_port 'Panel' "$panel_port"
((10#$site_port != 10#$aaid_port && 10#$site_port != 10#$panel_port && 10#$aaid_port != 10#$panel_port)) || ui_die "The three local ports must be different."
installer_project "$project"
[[ $timeout =~ ^[0-9]{1,4}$ ]] && ((10#$timeout >= 1 && 10#$timeout <= 3600)) || ui_die "--timeout: between 1 and 3600 seconds."
[[ $configure_nginx == 0 || $configure_nginx == 1 ]] || ui_die "GO_INSTALL_NGINX must be 0 or 1."
[[ $tls == 0 || $tls == 1 ]] || ui_die "GO_INSTALL_TLS must be 0 or 1."
if ((tls)); then
  ((configure_nginx)) || ui_die "TLS requires --configure-nginx."
  [[ $tls_email =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || ui_die "Provide an email: --tls-email admin@ytc-srv.org"
  [[ $public_url == https://* && $aaid_url == https://* ]] || ui_die "Certbot requires URLs starting with https://."
fi
if [[ $mode == check ]] || ((!start && !deps_explicit)); then install_deps=0; fi
case "$mode" in install) operation_title='Installation' ;; update) operation_title='Update' ;; check) operation_title='Check' ;; esac
if ((UI_LEVEL_NUM >= 2 || interactive)); then
  nginx_title='manual configuration'
  if ((configure_nginx)); then nginx_title='configured by the installer'; fi
  tls_title='external TLS proxy or manual configuration'
  if ((tls)); then tls_title='Let’s Encrypt / Certbot'; fi
  printf '\nServer settings\n  Mode: %s\n  Site: %s\n  AA ID: %s\n  Panel: http://127.0.0.1:%s/panel\n  Settings file: %s\n  Compose project: %s\n  Nginx: %s\n  HTTPS: %s\n' \
    "$operation_title" "$public_url" "$aaid_url" "$panel_port" "$env_file" "$project" "$nginx_title" "$tls_title"
fi
if ((fresh)) && [[ -f .env ]]; then
  printf '\nRecommendation: a legacy .env was found. This explicit install does not import SQLite; use migrate to preserve accounts (docs/go-postgres-migration.md).\n'
fi
if ((interactive)); then
  ask 'Apply these settings: yes/no' yes
  case "${REPLY,,}" in yes|y) ;; *) printf 'Settings were not changed.\n'; exit 0 ;; esac
fi
# Authorize sudo visibly before any background stage can need it.
need_privilege=0
if ((configure_nginx && start)); then need_privilege=1; fi
if ((install_deps)); then
  for required_tool in docker python3 openssl curl; do
    if ! command -v "$required_tool" >/dev/null; then need_privilege=1; fi
  done
  if ! docker compose version >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then need_privilege=1; fi
fi
if ((EUID != 0 && need_privilege)) && [[ $mode != check ]]; then
  if command -v sudo >/dev/null 2>&1; then
    if ((interactive)) || [[ -t 0 ]]; then sudo -v; else sudo -n true || ui_die "Administrator privileges required. Run sudo bash install.sh $mode --non-interactive."; fi
  fi
fi
total=4
if [[ $mode != check ]] && ((start)); then
  total=$((total + 3 + pull + build + configure_nginx + tls))
fi
ui_init "$total" "Go / PostgreSQL · $operation_title"
ui_run 'Checking and preparing dependencies' installer_dependencies
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  if privileged docker info >/dev/null 2>&1; then DOCKER=(sudo -n docker); else
    printf 'Solution: start Docker and retry with sudo; check systemctl status docker.\n' >&2
    exit 1
  fi
fi
compose=("${DOCKER[@]}" compose --env-file "$env_file" --project-name "$project" -f compose.go.yaml)
write_settings() {
  local key line temporary database_password sessions_password identity_password admin_password
  mkdir -p -- "$(dirname -- "$env_file")" || return
  if ((fresh)); then
    database_password=$(openssl rand -hex 24) || return
    sessions_password=$(openssl rand -hex 24) || return
    identity_password=$(openssl rand -hex 24) || return
    admin_password=$(openssl rand -hex 24) || return
    (set -o noclobber
      {
        printf 'GO_POSTGRES_PASSWORD=%s\n' "$database_password"
        printf 'GO_SESSIONS_DB_PASSWORD=%s\n' "$sessions_password"
        printf 'GO_IDENTITY_DB_PASSWORD=%s\n' "$identity_password"
        printf 'GO_ADMIN_PASSWORD=%s\n' "$admin_password"
        printf 'GO_PUBLIC_URL=%s\nGO_AAID_PUBLIC_URL=%s\n' "${public_url%/}" "${aaid_url%/}"
        printf 'GO_PORT=%s\nGO_AAID_PORT=%s\nGO_PANEL_PORT=%s\n' "$site_port" "$aaid_port" "$panel_port"
        printf 'GO_INSTALL_NGINX=%s\nGO_INSTALL_TLS=%s\nGO_TLS_EMAIL=%s\nGO_NGINX_CONFIG=%s\n' "$configure_nginx" "$tls" "$tls_email" "$nginx_config"
        printf 'GO_INSTALL_PROJECT=%s\n' "$project"
      } > "$env_file"
    ) || return
    chmod 600 -- "$env_file"
  elif (("${#updates[@]}" > 0)); then
    temporary=$(mktemp "$env_file.tmp.XXXXXX") || return
    while IFS= read -r line || [[ -n $line ]]; do
      key=${line%%=*}
      if [[ -z $key ]] || [[ -z ${updates[$key]+x} ]]; then printf '%s\n' "$line"; fi
    done < "$env_file" > "$temporary"
    for key in "${!updates[@]}"; do printf '%s=%s\n' "$key" "${updates[$key]}" >> "$temporary"; done
    chmod 600 -- "$temporary"; mv -f -- "$temporary" "$env_file"
  fi
  printf 'Settings are ready; existing passwords and keys were preserved.\n'
}
if [[ $mode == check ]]; then
  ui_run 'Checking the settings file' test -r "$env_file"
else
  ui_run 'Preparing settings and passwords' write_settings
fi
ui_run 'Checking the Compose configuration' "${compose[@]}" config --quiet
ui_run 'Checking resources, ports, DNS and nginx' installer_preflight
if [[ $mode == check ]] || ((!start)); then
  ui_finish 'Checks complete; services were not started'
else
  if ((pull)); then ui_run 'Pulling PostgreSQL and nginx images' "${compose[@]}" pull postgres nginx; fi
  if ((build)); then ui_run 'Building Go services and Chromium' installer_build; fi
  ui_run 'Starting PostgreSQL' "${compose[@]}" up -d --wait --wait-timeout "$timeout" postgres
  ui_run 'Starting services and waiting for readiness' "${compose[@]}" up -d --no-build --wait --wait-timeout "$timeout"
  if ((configure_nginx)); then ui_run 'Configuring host nginx server blocks' installer_nginx; fi
  if ((tls)); then ui_run 'Checking and configuring HTTPS' installer_tls; fi
  ui_run 'Checking site and AA ID HTTP responses' installer_health
  ui_finish 'Server installed and verified'
fi
printf '\nInstallation details\n  Site: %s\n  AA ID: %s\n  Panel: http://127.0.0.1:%s/panel\n  Local upstreams: site %s, AA ID %s\n  Settings and passwords: %s\n  Log: %s\n' \
  "$public_url" "$aaid_url" "$panel_port" "$site_port" "$aaid_port" "$env_file" "$UI_LOG_FILE"
printf '  Volumes to back up: %s_keys, %s_postgres_data\n' "$project" "$project"
if ((fresh)) && { [[ -t 1 ]] || ((show_password)); }; then
  printf '  Panel password for a new empty database: %s\n' "$(env_value GO_ADMIN_PASSWORD)"
else
  printf '  Panel password preserved; after import, use the original password.\n'
fi
if ((!configure_nginx)); then printf '  Nginx: use nginx/go-hosts.conf.example with the configured upstream ports.\n'; fi
