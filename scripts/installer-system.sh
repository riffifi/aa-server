#!/usr/bin/env bash
privileged() {
  if ((EUID == 0)); then "$@"; else
    command -v sudo >/dev/null || { printf 'Solution: run the installer as root or install sudo.\n' >&2; return 1; }
    sudo -n -- "$@"
  fi
}
installer_apt() {
  [[ $(uname -s) == Linux && -r /etc/os-release ]] || {
    printf 'Automatic installation supports Ubuntu/Debian. Solution: install Docker Compose, Python 3, OpenSSL and curl manually.\n' >&2; return 1;
  }
  local ID VERSION_CODENAME UBUNTU_CODENAME
  source /etc/os-release
  [[ $ID == ubuntu || $ID == debian ]] || { printf 'Solution: install dependencies manually for your OS and retry with --no-install-deps.\n' >&2; return 1; }
  if ((!APT_UPDATED)); then privileged apt-get update || return; APT_UPDATED=1; fi
  privileged env DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
}
installer_dependencies() {
  local package tool missing=() APT_UPDATED=0
  for tool in python3 openssl curl; do
    if ! command -v "$tool" >/dev/null; then missing+=("$tool"); fi
  done
  if ((configure_nginx)); then
    if ! command -v nginx >/dev/null; then missing+=(nginx); fi
  fi
  if ((tls)); then
    if ! command -v certbot >/dev/null || ! certbot plugins --text 2>/dev/null | grep -q nginx; then missing+=(certbot python3-certbot-nginx); fi
  fi
  if (("${#missing[@]}" > 0)); then
    if ((!install_deps)); then
      printf 'Missing dependencies: %s\n' "${missing[*]}" >&2
      if [[ $mode == migration ]]; then
        printf 'Solution: rerun the migration command with the same options and --install-deps.\n' >&2
      else
        printf 'Solution: sudo bash install.sh %s --install-deps\n' "$mode" >&2
      fi
      return 1
    fi
    installer_apt "${missing[@]}" || return
  fi
  if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
    if ((!install_deps)); then
      printf 'Docker Compose is missing. Solution: retry with --install-deps or install Docker Engine and the Compose plugin.\n' >&2; return 1
    fi
    installer_apt ca-certificates curl || return
    local ID VERSION_CODENAME UBUNTU_CODENAME architecture codename source_file
    source /etc/os-release
    architecture=$(dpkg --print-architecture) || return
    codename=${UBUNTU_CODENAME:-${VERSION_CODENAME}}
    privileged install -m 0755 -d /etc/apt/keyrings || return
    local key_temp; key_temp=$(mktemp) || return
    if ! curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o "$key_temp"; then rm -f -- "$key_temp"; return 1; fi
    privileged install -m 0644 "$key_temp" /etc/apt/keyrings/docker.asc || return
    rm -f -- "$key_temp"
    source_file=$(mktemp) || return
    printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$ID" "$codename" "$architecture" > "$source_file"
    privileged install -m 0644 "$source_file" /etc/apt/sources.list.d/docker.sources || return
    rm -f -- "$source_file"
    APT_UPDATED=0
    installer_apt docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || return
    privileged systemctl enable --now docker || return
  fi
  if ! docker info >/dev/null 2>&1 && ! privileged docker info >/dev/null 2>&1; then
    if ((install_deps)) && command -v systemctl >/dev/null; then privileged systemctl start docker || return; fi
    if ! docker info >/dev/null 2>&1 && ! privileged docker info >/dev/null 2>&1; then
      printf 'Docker daemon is unavailable. Solution: sudo systemctl start docker; run the installer with sudo or check Docker socket permissions.\n' >&2; return 1
    fi
  fi
  docker compose version || return
  python3 --version || return
  openssl version || return
  if ((configure_nginx)); then privileged nginx -t || return; fi
}
installer_preflight() {
  local args=(--site-url "$public_url" --aaid-url "$aaid_url" --site-port "$site_port" --aaid-port "$aaid_port" --panel-port "$panel_port" --project "$project")
  if [[ ${DOCKER[0]} == sudo ]]; then args+=(--docker-sudo); fi
  if ((tls)); then args+=(--require-dns); fi
  python3 scripts/installer-preflight.py "${args[@]}" || return
  if ((configure_nginx)); then
    local nginx_args=(--site-url "$public_url" --aaid-url "$aaid_url" --site-port "$site_port" --aaid-port "$aaid_port" --dry-run)
    if [[ -n $nginx_config ]]; then nginx_args+=(--config "$nginx_config"); fi
    privileged python3 "$PWD/scripts/configure_go_nginx.py" "${nginx_args[@]}"
  fi
}
installer_nginx() {
  local args=(--site-url "$public_url" --aaid-url "$aaid_url" --site-port "$site_port" --aaid-port "$aaid_port")
  if [[ -n $nginx_config ]]; then args+=(--config "$nginx_config"); fi
  privileged python3 "$PWD/scripts/configure_go_nginx.py" "${args[@]}"
}
installer_tls() {
  local site_host=${public_url#*://} identity_host=${aaid_url#*://}
  site_host=${site_host%/}; site_host=${site_host%%:*}
  identity_host=${identity_host%/}; identity_host=${identity_host%%:*}
  local args=(--nginx --non-interactive --agree-tos --keep-until-expiring --email "$tls_email" --redirect -d "$site_host" -d "$identity_host")
  if [[ -n $nginx_config ]]; then
    args+=(--nginx-server-root "$(dirname -- "$nginx_config")")
    [[ $(basename -- "$nginx_config") == nginx.conf ]] || { printf 'Solution: for a custom nginx.conf filename, configure Certbot manually and use --no-tls.\n' >&2; return 1; }
  fi
  privileged certbot "${args[@]}" || return
  curl --noproxy '*' --fail --silent --show-error --max-time 15 --resolve "$site_host:443:127.0.0.1" "${public_url%/}/health" || return
  curl --noproxy '*' --fail --silent --show-error --max-time 15 --resolve "$identity_host:443:127.0.0.1" "${aaid_url%/}/health"
}
installer_health() {
  python3 scripts/installer-preflight.py --health --site-port "$site_port" --aaid-port "$aaid_port" --panel-port "$panel_port"
}

# Embed the source revision in both services without copying .git into the image.
installer_build() {
  local version=${AA_BUILD_VERSION:-} commit=${AA_BUILD_COMMIT:-} dirty=${AA_BUILD_DIRTY:-} changes=
  if [[ -z $version && -f internal/buildinfo/VERSION ]]; then
    IFS= read -r version < internal/buildinfo/VERSION || [[ -n $version ]] || return 1
    version=${version%$'\r'}
  fi
  if command -v git >/dev/null 2>&1; then
    if [[ -z $commit ]]; then commit=$(git rev-parse HEAD 2>/dev/null) || commit=unknown; fi
    if [[ -z $dirty ]]; then
      changes=$(git status --porcelain 2>/dev/null) || changes=
      dirty=false; if [[ -n $changes ]]; then dirty=true; fi
    fi
  fi
  version=${version:-dev}; commit=${commit:-unknown}; dirty=${dirty:-false}
  [[ $version =~ ^[a-zA-Z0-9._+-]{1,128}$ ]] || ui_die 'AA_BUILD_VERSION must contain only letters, digits, dots, underscores, + or - (up to 128 characters).'
  [[ $commit == unknown || $commit =~ ^[a-fA-F0-9]{7,64}$ ]] || ui_die 'AA_BUILD_COMMIT must be a Git commit hash or unknown.'
  [[ $dirty == true || $dirty == false ]] || ui_die 'AA_BUILD_DIRTY must be true or false.'
  export AA_BUILD_VERSION=$version AA_BUILD_COMMIT=$commit AA_BUILD_DIRTY=$dirty
  printf 'Build version: %s; commit: %s\n' "$version" "$commit"
  "${compose[@]}" build initialize
}
