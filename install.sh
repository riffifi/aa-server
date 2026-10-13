#!/usr/bin/env bash
set -euo pipefail
umask 077
server_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
server_url='http://127.0.0.1:8080'
bind_address='127.0.0.1'
server_port='8080'
server_domain=''
check_only=false
usage() {
  cat <<'HELP'
Usage: ./install.sh [--url http://PRIVATE-IP:8080] [--bind IP] [--port PORT]
                    [--domain sessions.example.org] [--check]
HTTP defaults to loopback. Use --bind 0.0.0.0 for a private LAN/VPN listener.
--domain enables Caddy automatic HTTPS (DNS must point to this server).
--check validates configuration without installing or starting anything.
Existing credentials and the Docker data volume are preserved on reruns.
HELP
}
while (($#)); do
  case "$1" in
    --url|--bind|--port|--domain)
      option=$1
      [[ $# -ge 2 ]] || { usage; exit 2; }
      case "$option" in
        --url) server_url=$2;; --bind) bind_address=$2;;
        --port) server_port=$2;; --domain) server_domain=$2;;
      esac
      shift 2;;
    --check) check_only=true; shift;;
    --help|-h) usage; exit 0;;
    *) usage; exit 2;;
  esac
done
[[ "$server_port" =~ ^[0-9]{1,5}$ ]] && ((10#$server_port >= 1 && 10#$server_port <= 65535)) || { echo 'Invalid port' >&2; exit 2; }
[[ "$bind_address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Use an IPv4 bind address' >&2; exit 2; }
[[ "$server_url" =~ ^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?/?$ ]] || { echo 'Use an HTTP(S) URL without paths or credentials' >&2; exit 2; }
if [[ -n "$server_domain" ]]; then
  [[ "$server_domain" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ && "$server_domain" == *.* ]] || { echo 'Invalid domain' >&2; exit 2; }
  server_url="https://$server_domain"
  bind_address='127.0.0.1'
fi
if $check_only; then
  echo "Configuration valid: $server_url (bind $bind_address:$server_port)"
  exit 0
fi
if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
  [[ $EUID -eq 0 ]] || { echo 'Run with sudo to install Docker, or install Docker Compose first.' >&2; exit 1; }
  [[ -r /etc/os-release ]] || { echo 'Install Docker manually on this OS.' >&2; exit 1; }
  . /etc/os-release
  [[ "$ID" == ubuntu || "$ID" == debian ]] || { echo 'Automatic Docker installation supports Ubuntu/Debian. Install Docker manually on this OS.' >&2; exit 1; }
  apt-get update
  apt-get install -y ca-certificates curl
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  docker_codename=${UBUNTU_CODENAME:-${VERSION_CODENAME}}
  docker_arch=$(dpkg --print-architecture)
  cat > /etc/apt/sources.list.d/docker.sources <<SOURCES
Types: deb
URIs: https://download.docker.com/linux/$ID
Suites: $docker_codename
Components: stable
Architectures: $docker_arch
Signed-By: /etc/apt/keyrings/docker.asc
SOURCES
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker
fi
cd -- "$server_dir"
if [[ ! -e .env ]]; then
  if command -v openssl >/dev/null; then
    admin_password=$(openssl rand -hex 24)
  elif command -v python3 >/dev/null; then
    admin_password=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
  else
    echo 'Install openssl or python3 to generate the panel password.' >&2
    exit 1
  fi
  cat > .env <<CONFIG
PUBLIC_URL=$server_url
BIND_ADDRESS=$bind_address
PORT=$server_port
DOMAIN=$server_domain
ADMIN_PASSWORD=$admin_password
REFRESH_SECONDS=300
CONFIG
  chmod 600 .env
  echo 'Generated panel credentials in server/.env (mode 600).'
else
  echo 'Preserving existing server/.env. Edit it to change URL or binding.'
fi
# Do not source .env as shell code. Docker Compose parses it as configuration.
compose_args=()
if [[ -n "$server_domain" ]] || awk -F= '$1 == "DOMAIN" && length($2) > 0 { found=1 } END { exit !found }' .env; then
  compose_args+=(--profile https)
fi
docker compose "${compose_args[@]}" up -d --build --wait --wait-timeout 180
printf 'Server started. Open the PUBLIC_URL from %s/.env.\n' "$server_dir"
echo 'Use ADMIN_PASSWORD from that file to open the panel. Create a separate device key for each phone.'
