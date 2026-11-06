#!/usr/bin/env bash
if [ -z "${BASH_VERSION:-}" ]; then
  printf '%s\n' 'Запустите установщик через Bash: bash install.sh --help' >&2
  exit 2
fi
exec bash "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/install-go.sh" "$@"
