#!/usr/bin/env bash
set -euo pipefail
umask 077
server_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
server_url='http://aa.yellowteacrew.ru'
bind_address='127.0.0.1'
server_port='8080'
server_domain='aa.yellowteacrew.ru'
host_nginx=true
nginx_config=''
check_only=false
usage() {
  cat <<'HELP'
Usage: ./install.sh [--url http://PRIVATE-IP:8080] [--bind IP] [--port PORT]
                    [--domain DOMAIN] [--nginx-conf PATH] [--no-host-nginx] [--check]
Defaults to http://aa.yellowteacrew.ru through the host Nginx on port 80.
Automatically adds a managed site include to the discovered global Nginx config.
--no-host-nginx keeps only the Docker proxy; use --url and --bind for direct access.
--check validates configuration without installing or starting anything.
Existing credentials and the Docker data volume are preserved on reruns.
HELP
}
while (($#)); do
  case "$1" in
    --url|--bind|--port|--domain|--nginx-conf)
      option=$1
      [[ $# -ge 2 ]] || { usage; exit 2; }
      case "$option" in
        --url) server_url=$2;; --bind) bind_address=$2;;
        --port) server_port=$2;; --domain) server_domain=$2;;
        --nginx-conf) nginx_config=$2;;
      esac
      shift 2;;
    --check) check_only=true; shift;;
    --no-host-nginx) host_nginx=false; server_domain=''; shift;;
    --help|-h) usage; exit 0;;
    *) usage; exit 2;;
  esac
done
[[ "$server_port" =~ ^[0-9]{1,5}$ ]] && ((10#$server_port >= 1 && 10#$server_port <= 65535)) || { echo 'Invalid port' >&2; exit 2; }
[[ "$bind_address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Use an IPv4 bind address' >&2; exit 2; }
[[ "$server_url" =~ ^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?/?$ ]] || { echo 'Use an HTTP(S) URL without paths or credentials' >&2; exit 2; }
if [[ -n "$server_domain" ]]; then
  [[ "$server_domain" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ && "$server_domain" == *.* ]] || { echo 'Invalid domain' >&2; exit 2; }
  if $host_nginx; then
    server_url="http://$server_domain"
    bind_address='127.0.0.1'
    [[ "$server_port" != 80 ]] || { echo 'Docker upstream port must differ from host Nginx port 80.' >&2; exit 2; }
  else
    server_url="http://$server_domain:$server_port"
  fi
fi
if $check_only; then
  echo "Configuration valid: $server_url (bind $bind_address:$server_port)"
  exit 0
fi
if $host_nginx; then
  [[ $EUID -eq 0 ]] || { echo 'Run with sudo to configure host Nginx.' >&2; exit 1; }
  if ! command -v nginx >/dev/null || ! command -v python3 >/dev/null; then
    [[ -r /etc/os-release ]] || { echo 'Install Nginx and Python 3 first.' >&2; exit 1; }
    . /etc/os-release
    [[ "$ID" == ubuntu || "$ID" == debian ]] || { echo 'Install Nginx and Python 3 first on this OS.' >&2; exit 1; }
    apt-get update
    apt-get install -y nginx python3
  fi
  nginx_args=()
  [[ -z "$nginx_config" ]] || nginx_args=(-c "$nginx_config")
  nginx "${nginx_args[@]}" -t
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
  echo 'Preserving existing server credentials and data.'
fi
if $host_nginx; then
  # Update deployment fields only; never evaluate .env or replace panel credentials.
  python3 - "$server_dir/.env" "$server_url" "$server_port" "$server_domain" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
updates = dict(PUBLIC_URL=sys.argv[2], BIND_ADDRESS='127.0.0.1', PORT=sys.argv[3], DOMAIN=sys.argv[4])
lines = []
for line in path.read_text().splitlines():
    key = line.split('=', 1)[0]
    if key in updates:
        continue
    lines.append(line)
lines.extend(f'{key}={value}' for key, value in updates.items())
path.write_text('\n'.join(lines) + '\n')
PY
fi
# Do not source .env as shell code. Docker Compose parses it as configuration.
docker compose up -d --build --remove-orphans --wait --wait-timeout 180
if $host_nginx; then
  host_config_args=()
  [[ -z "$nginx_config" ]] || host_config_args=(--config "$nginx_config")
  python3 "$server_dir/scripts/configure_nginx.py" --domain "$server_domain" --port "$server_port" "${host_config_args[@]}"
fi
printf 'Server started. Open the PUBLIC_URL from %s/.env.\n' "$server_dir"
echo 'Use ADMIN_PASSWORD from that file to open the panel. Phones only need PUBLIC_URL; synchronization is automatic.'
