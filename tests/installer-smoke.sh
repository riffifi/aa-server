#!/usr/bin/env bash
# Run inside a disposable Linux container mounted at /repo, with /tmp executable.
# Every Docker/package operation below is a stub; no server or network is used.
set -euo pipefail
sandbox=$(mktemp -d /tmp/aa-installer-smoke.XXXXXX)
cp /repo/install.sh /repo/install-go.sh /repo/compose.go.yaml "$sandbox/"
mkdir "$sandbox/scripts" "$sandbox/bin"
cp /repo/scripts/installer-ui.sh /repo/scripts/installer-system.sh /repo/scripts/installer-env.sh "$sandbox/scripts/"
cat > "$sandbox/bin/mock-tool" <<'MOCK'
#!/usr/bin/env bash
case "${0##*/}" in
  openssl) printf '0123456789abcdef0123456789abcdef0123456789abcdef\n' ;;
  docker)
    if [[ " $* " == *" up "* ]]; then sleep 0.25; fi
    printf 'mock Docker ready\n' ;;
  *) printf 'mock checks passed\n' ;;
esac
MOCK
chmod +x "$sandbox/bin/mock-tool"
for name in docker python3 openssl curl; do ln -s mock-tool "$sandbox/bin/$name"; done
export PATH="$sandbox/bin:$PATH" TERM=xterm LANG=C.UTF-8
cd "$sandbox"
command -v docker
docker compose version
bash install.sh install --non-interactive --no-install-deps --no-start --plain > preparation.txt
grep -q 'GO_ADMIN_PASSWORD=' .env.go
grep 'PASSWORD=' .env.go > initial-passwords
bash install.sh update --non-interactive --no-install-deps --skip-pull --no-build --site-port=19180
grep 'PASSWORD=' .env.go > updated-passwords
cmp initial-passwords updated-passwords
grep -q 'GO_PORT=19180' .env.go
bash install.sh check --non-interactive --log-level=warn --plain > check.txt
if grep -q '\[OK\]' check.txt; then exit 1; fi
bash install.sh check --non-interactive --debug --plain > debug.txt
grep -q '\[DEBUG\]' debug.txt
(
  source scripts/installer-system.sh
  configure_nginx=0 tls=0 install_deps=1 mode=update docker_available=0
  docker() {
    if [[ "${1:-} ${2:-}" == 'compose version' && $docker_available == 0 ]]; then return 1; fi
    printf 'mock Docker version\n'
  }
  privileged() { printf 'privileged:%s\n' "$*"; }
  installer_apt() {
    printf 'packages:%s\n' "$*"
    if [[ " $* " == *' docker-ce '* ]]; then docker_available=1; fi
  }
  installer_dependencies
  ((docker_available == 1))
) > dependencies.txt
grep -q 'docker-compose-plugin' dependencies.txt
bash -c 'set -euo pipefail; source scripts/installer-ui.sh; UI_LOG_FILE=cancel.log; ui_init 1 cancel; UI_ANIMATE=1; ui_run cancel sleep 30' > cancel.txt 2>&1 &
cancel_pid=$!
sleep 0.3
kill -TERM "$cancel_pid"
if wait "$cancel_pid"; then exit 1; else cancel_status=$?; fi
[[ $cancel_status == 143 ]]
grep -q '\[CANCELLED\]' cancel.txt
printf '\n[SMOKE] Linux: preparation, updates, animation, logging levels and password preservation checked.\n'
