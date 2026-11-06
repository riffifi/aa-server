#!/usr/bin/env bash
# Shared terminal presentation. Percentages reflect completed named stages,
# never an invented estimate of Docker bytes or build time.
UI_PLAIN=0 UI_NO_COLOR=0 UI_VERBOSE=0 UI_LOG_FILE=
UI_LOG_LEVEL=info UI_LEVEL_NUM=2
UI_CHILD= UI_DONE=0 UI_TOTAL=1 UI_ACTIVE=0 UI_ANIMATE=0
UI_CYAN= UI_GREEN= UI_RED= UI_DIM= UI_RESET=

ui_die() { printf 'Ошибка: %s\n' "$*" >&2; exit 2; }
ui_set_log_level() {
  UI_LOG_LEVEL=$1 UI_VERBOSE=0
  case "$1" in
    error) UI_LEVEL_NUM=0 ;;
    warn) UI_LEVEL_NUM=1 ;;
    info) UI_LEVEL_NUM=2 ;;
    debug) UI_LEVEL_NUM=3; UI_VERBOSE=1 ;;
    *) ui_die "--log-level: выберите error, warn, info или debug." ;;
  esac
}
ui_value() {
  [[ $# -ge 2 && -n $2 && $2 != --* ]] || ui_die "Для $1 требуется значение. Справка: --help"
}
ui_duration() {
  local seconds=$1
  printf '%02d:%02d:%02d' "$((seconds / 3600))" "$((seconds / 60 % 60))" "$((seconds % 60))"
}
ui_cleanup() {
  if [[ -n $UI_CHILD ]]; then
    # Stop only the active installer command and its immediate helper.
    if command -v pkill >/dev/null 2>&1; then pkill -TERM -P "$UI_CHILD" 2>/dev/null || true; fi
    kill -TERM "$UI_CHILD" 2>/dev/null || true
    wait "$UI_CHILD" 2>/dev/null || true
    UI_CHILD=
  fi
  if ((UI_ANIMATE)); then printf '\r\033[K\033[?25h'; fi
}
ui_cancel() {
  local status=$1
  ui_cleanup
  printf '\n[ОТМЕНЕНО] Операция прервана. Журнал: %s\n' "$UI_LOG_FILE" >&2
  exit "$status"
}
ui_init() {
  UI_TOTAL=$1 UI_DONE=0 UI_START=$SECONDS UI_ACTIVE=1
  if [[ -t 1 && ${TERM:-dumb} != dumb ]] && ((!UI_PLAIN && UI_LEVEL_NUM >= 2)); then
    if ((!UI_VERBOSE)); then UI_ANIMATE=1; fi
    if ((!UI_NO_COLOR)) && [[ -z ${NO_COLOR+x} ]]; then
      UI_CYAN=$'\033[36m' UI_GREEN=$'\033[32m' UI_RED=$'\033[31m'
      UI_DIM=$'\033[2m' UI_RESET=$'\033[0m'
    fi
  fi
  local locale_name=${LC_ALL:-${LC_CTYPE:-${LANG:-C}}}
  UI_FRAMES=('|' '/' '-' '\')
  if [[ ${locale_name^^} == *UTF* ]]; then UI_FRAMES=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏'); fi
  if [[ -z $UI_LOG_FILE ]]; then
    mkdir -p -- .go-data
    UI_LOG_FILE=$(mktemp ".go-data/install-$(date +%Y%m%d-%H%M%S)-XXXXXX")
  else
    [[ -d $(dirname -- "$UI_LOG_FILE") ]] || ui_die "Нет каталога для журнала: $UI_LOG_FILE"
    (set -o noclobber; : > "$UI_LOG_FILE") 2>/dev/null || ui_die "Журнал уже существует или недоступен: $UI_LOG_FILE"
  fi
  chmod 600 -- "$UI_LOG_FILE"
  trap ui_cleanup EXIT
  trap 'ui_cancel 130' INT
  trap 'ui_cancel 143' TERM
  if ((UI_LEVEL_NUM >= 2)); then
    printf '\n%sAntiAttendance%s\n%s\n\n' "$UI_CYAN" "$UI_RESET" "$2"
    printf 'Этапов: %d\nЖурнал: %s\n' "$UI_TOTAL" "$UI_LOG_FILE"
    printf 'Проценты показывают завершённые этапы; длительность обновляется во время работы.\n\n'
  fi
  if ((UI_ANIMATE)); then printf '\033[?25l'; fi
}
ui_render() {
  local frame=$1 step_start=$2 width=18 filled bar= index percent
  percent=$((UI_DONE * 100 / UI_TOTAL))
  filled=$((UI_DONE * width / UI_TOTAL))
  for ((index=0; index<width; index++)); do
    if ((index < filled)); then bar+='#'; else bar+='.'; fi
  done
  printf '\r\033[K%s %s[%s] %3d%%%s | этап %s | всего %s' \
    "${UI_FRAMES[frame % ${#UI_FRAMES[@]}]}" "$UI_CYAN" "$bar" "$percent" "$UI_RESET" \
    "$(ui_duration "$((SECONDS - step_start))")" "$(ui_duration "$((SECONDS - UI_START))")"
}
ui_run() {
  local name=$1 step_start=$SECONDS frame=0 status first_line
  shift
  first_line=$(wc -l < "$UI_LOG_FILE")
  printf '[INFO] Этап %d/%d: %s\n' "$((UI_DONE + 1))" "$UI_TOTAL" "$name" >> "$UI_LOG_FILE"
  if ((UI_LEVEL_NUM >= 2)); then printf '[%02d/%02d] %s\n' "$((UI_DONE + 1))" "$UI_TOTAL" "$name"; fi
  if ((UI_LEVEL_NUM >= 3)); then printf '[DEBUG] Исполнитель: %s; аргументов: %d\n' "$1" "$(($# - 1))"; fi
  if ((UI_VERBOSE)); then
    ("$@" 2>&1 | tee -a "$UI_LOG_FILE" | sed 's/=>/[Docker]/g') &
  else
    ("$@" >>"$UI_LOG_FILE" 2>&1) &
  fi
  UI_CHILD=$!
  while kill -0 "$UI_CHILD" 2>/dev/null; do
    if ((UI_ANIMATE)); then ui_render "$frame" "$step_start"; fi
    frame=$((frame + 1))
    sleep 0.1
  done
  if wait "$UI_CHILD"; then status=0; else status=$?; fi
  UI_CHILD=
  if ((UI_ANIMATE)); then printf '\r\033[K'; fi
  if ((status != 0)); then
    printf '%s[ОШИБКА]%s %s | время %s | код %d\n' "$UI_RED" "$UI_RESET" "$name" "$(ui_duration "$((SECONDS - step_start))")" "$status" >&2
    printf '[ERROR] %s: код %d\n' "$name" "$status" >> "$UI_LOG_FILE"
    printf 'Полный журнал: %s\n' "$UI_LOG_FILE" >&2
    if ((UI_LEVEL_NUM >= 2)); then tail -n 20 -- "$UI_LOG_FILE" | sed 's/=>/[Docker]/g' >&2; fi
    case "$name" in
      *зависимост*) printf 'Решение: проверьте доступ к apt-репозиториям и права sudo; повторите --install-deps.\n' >&2 ;;
      *Compose*|*настроек*) printf 'Решение: проверьте путь --env-file, обязательные GO_* поля и повторите check.\n' >&2 ;;
      *Сборка*|*Загрузка*) printf 'Решение: проверьте свободное место, доступ к Docker registry/модулям Go и повторите update --verbose.\n' >&2 ;;
      *nginx*|*HTTPS*) printf 'Решение: проверьте nginx -t, DNS A/AAAA и доступность порта 80; при внешнем TLS используйте --no-tls.\n' >&2 ;;
      *готовности*|*PostgreSQL*|*HTTP*) printf 'Решение: проверьте docker compose ps/logs, пароли БД и сохранённые ключи; при долгом старте увеличьте --timeout.\n' >&2 ;;
    esac
    return "$status"
  fi
  UI_DONE=$((UI_DONE + 1))
  printf '[INFO] Завершено: %s; время %s\n' "$name" "$(ui_duration "$((SECONDS - step_start))")" >> "$UI_LOG_FILE"
  if ((UI_LEVEL_NUM >= 2)); then
    printf '%s[ГОТОВО]%s %s | %3d%% | этап %s | всего %s\n\n' \
      "$UI_GREEN" "$UI_RESET" "$name" "$((UI_DONE * 100 / UI_TOTAL))" \
      "$(ui_duration "$((SECONDS - step_start))")" "$(ui_duration "$((SECONDS - UI_START))")"
  fi
  if ((!UI_VERBOSE && UI_LEVEL_NUM >= 1)); then
    awk -v start="$first_line" -v level="$UI_LEVEL_NUM" 'NR > start && (/^Рекомендация:/ || (level >= 2 && /^(Диск |Доступная память:|DNS |Nginx )/))' "$UI_LOG_FILE"
  fi
}
ui_finish() {
  ui_cleanup
  printf '%s[ЗАВЕРШЕНО]%s %s | [##################] 100%% | время %s\n' "$UI_GREEN" "$UI_RESET" "$1" "$(ui_duration "$((SECONDS - UI_START))")"
  UI_ACTIVE=0
}

installer_port() {
  [[ $2 =~ ^[0-9]{1,5}$ ]] && ((10#$2 >= 1 && 10#$2 <= 65535)) || ui_die "$1: порт должен быть от 1 до 65535."
}
installer_project() {
  [[ $1 =~ ^[a-z0-9][a-z0-9_-]*$ ]] || ui_die "Имя проекта: строчные латинские буквы, цифры, _ и -."
}
installer_origin() {
  local value=$1 host port= suffix octet
  [[ $value =~ ^https?://[^/[:space:]\#\?]+/?$ && $value != *'@'* ]] || ui_die "$2: нужен HTTP(S) origin без пути, логина, query и fragment."
  host=${value#*://}; host=${host%/}
  if [[ $host == \[* ]]; then
    suffix=${host#*]}
    [[ -z $suffix || $suffix == :* ]] || ui_die "$2: некорректный адрес после IPv6."
    if [[ -n $suffix ]]; then port=${suffix#:}; [[ -n $port ]] || ui_die "$2: пустой порт."; fi
    host=${host#\[}; host=${host%%]*}
  else
    if [[ $host == *:* ]]; then port=${host#*:}; [[ -n $port ]] || ui_die "$2: пустой порт."; fi
    host=${host%%:*}
  fi
  if [[ -n $port ]]; then installer_port "$2" "$port"; fi
  host=${host,,}
  [[ $host == *.ytc-srv.org || $host == localhost || $host == ::1 || $host =~ ^127(\.[0-9]{1,3}){3}$ ]] || ui_die "$2: разрешены *.ytc-srv.org и локальный loopback."
  if [[ $host == 127.* ]]; then
    local parts; IFS=. read -r -a parts <<< "$host"
    for octet in "${parts[@]}"; do ((10#$octet <= 255)) || ui_die "$2: некорректный loopback IP."; done
  fi
}
