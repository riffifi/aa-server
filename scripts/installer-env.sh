#!/usr/bin/env bash
# Read literal installer-generated dotenv values; never execute the file.
installer_env_value() {
  local path=$1 key=$2 line value=
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    if [[ $line == "$key="* ]]; then value=${line#*=}; fi
  done < "$path"
  if [[ $value == \"*\" || $value == \'*\' ]]; then value=${value:1:${#value}-2}; fi
  printf '%s' "$value"
}
