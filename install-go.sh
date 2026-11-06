#!/usr/bin/env bash
if [ -z "${BASH_VERSION:-}" ]; then
  printf '%s\n' 'Нужен Bash: bash install.sh --help' >&2; exit 2
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
declare -A updates=()
usage() {
  cat <<'HELP'
AntiAttendance · установка и обновление Go / PostgreSQL

  bash install.sh [install|update|check] [параметры]

Без команды: новая установка или обновление по наличию .env.go.
При первом запуске в терминале открывается мастер настройки.

Адреса и порты:
  --site-url URL         Сайт (также --public-url, --url)
  --aaid-url URL         AA ID
  --site-port PORT       Локальный порт сайта (также --port)
  --aaid-port PORT       Локальный порт AA ID
  --panel-port PORT      Локальный порт приватной панели
  -e, --env-file FILE    Файл настроек, по умолчанию .env.go
  -p, --project NAME     Имя Compose-проекта, по умолчанию antiattendance-go

Nginx и HTTPS:
  --configure-nginx     Настроить отдельные блоки сайта и AA ID
  --no-host-nginx       Оставить host nginx под ручным управлением
  --nginx-config FILE   Глобальный nginx.conf для настройки
  --tls-email EMAIL     Выпустить/проверить Let's Encrypt через Certbot
  --no-tls              Не запускать Certbot (например, внешний TLS-прокси)

Режим работы:
  --interactive         Запросить настройки в консоли
  --non-interactive     Без вопросов; для автоматизации
  --no-start            Подготовить настройки, не запускать сервер/nginx
  --check               Только проверить существующую конфигурацию
  --no-build            Использовать уже собранный Go-образ
  --skip-pull           Не загружать заново образы PostgreSQL/nginx
  --timeout SECONDS     Ожидание готовности сервисов, по умолчанию 240
  --no-install-deps     Только проверять зависимости, не устанавливать
  --install-deps        Устанавливать недостающие зависимости (по умолчанию)

Вывод:
  --plain               Обычный текст без анимации и ANSI-кодов
  --no-color            Анимация без цвета; также поддерживается NO_COLOR
  --log-level LEVEL     error, warn, info (по умолчанию), debug
  -q, --quiet           Только ошибки и итог (log-level error)
  -v, --verbose         Вывод команд без анимации (log-level debug)
  --debug               То же, что --log-level debug; без shell xtrace
  --log-file FILE       Новый приватный файл журнала
  --show-password       Показать новый пароль также без терминала
  -h, --help            Эта справка

Можно писать --site-port=18080. Явные параметры обновляют только соответствующие
настройки; существующие пароли и ключи сохраняются.
Домены: aa.ytc-srv.org и aaid.ytc-srv.org; разрешены *.ytc-srv.org и loopback.
Старый Python-установщик сохранён как install-python.sh.
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
    --help|-h) usage; exit 0 ;;
    *) ui_die "Неизвестный параметр: $1. Справка: bash install.sh --help" ;;
  esac
done
[[ $mode == auto || $mode == install || $mode == update || $mode == check ]] || ui_die "Команда должна быть install, update или check."
[[ ! -L $env_file ]] || ui_die "Файл настроек не должен быть символической ссылкой."
fresh=1
if [[ -e $env_file ]]; then
  [[ -f $env_file ]] || ui_die "Настройки должны быть обычным файлом: $env_file"
  fresh=0
fi
[[ $mode != update || $fresh == 0 ]] || ui_die "Нет $env_file. Для новой установки используйте install."
[[ $mode != check || $fresh == 0 ]] || ui_die "Нет $env_file. Сначала выполните install --no-start."
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
  IFS= read -r reply || ui_die "Ввод завершён. Для автоматизации используйте --non-interactive."
  reply=${reply%$'\r'}
  REPLY=${reply:-$default}
}
if ((interactive)) && [[ $mode != check ]]; then
  if ((fresh)) && [[ -z ${updates[GO_INSTALL_NGINX]+x} ]]; then configure_nginx=1; fi
  if ((fresh)) && [[ -z ${updates[GO_INSTALL_TLS]+x} ]]; then tls=1; fi
  printf '\nAntiAttendance · мастер настройки\nПароли и ключи существующей установки сохраняются.\n\n'
  ask 'Адрес сайта' "$public_url"; public_url=$REPLY; updates[GO_PUBLIC_URL]=${REPLY%/}
  ask 'Адрес AA ID' "$aaid_url"; aaid_url=$REPLY; updates[GO_AAID_PUBLIC_URL]=${REPLY%/}
  ask 'Локальный порт сайта' "$site_port"; site_port=$REPLY; updates[GO_PORT]=$REPLY
  ask 'Локальный порт AA ID' "$aaid_port"; aaid_port=$REPLY; updates[GO_AAID_PORT]=$REPLY
  ask 'Локальный порт приватной панели' "$panel_port"; panel_port=$REPLY; updates[GO_PANEL_PORT]=$REPLY
  ask 'Настроить host nginx: да/нет' "$([[ $configure_nginx == 1 ]] && printf да || printf нет)"
  case "${REPLY,,}" in да|д|yes|y) configure_nginx=1 ;; нет|н|no|n) configure_nginx=0; tls=0 ;; *) ui_die "Ожидалось да или нет." ;; esac
  updates[GO_INSTALL_NGINX]=$configure_nginx
  if ((configure_nginx)); then
    ask 'Глобальный nginx.conf (auto — определить автоматически)' "${nginx_config:-auto}"
    nginx_config=$REPLY; [[ $nginx_config != auto ]] || nginx_config=
    updates[GO_NGINX_CONFIG]=$nginx_config
    ask 'Настроить HTTPS через Let’s Encrypt: да/нет' "$([[ $tls == 1 ]] && printf да || printf нет)"
    case "${REPLY,,}" in да|д|yes|y) tls=1 ;; нет|н|no|n) tls=0 ;; *) ui_die "Ожидалось да или нет." ;; esac
    if ((tls)); then ask 'Email для Let’s Encrypt' "$tls_email"; tls_email=$REPLY; updates[GO_TLS_EMAIL]=$tls_email; fi
  fi
  updates[GO_INSTALL_TLS]=$tls
fi
installer_origin "$public_url" 'Адрес сайта'; installer_origin "$aaid_url" 'Адрес AA ID'
[[ ${public_url%/} != "${aaid_url%/}" ]] || ui_die "Сайт и AA ID должны иметь разные origin."
installer_port 'Сайт' "$site_port"; installer_port 'AA ID' "$aaid_port"; installer_port 'Панель' "$panel_port"
((10#$site_port != 10#$aaid_port && 10#$site_port != 10#$panel_port && 10#$aaid_port != 10#$panel_port)) || ui_die "Три локальных порта должны различаться."
installer_project "$project"
[[ $timeout =~ ^[0-9]{1,4}$ ]] && ((10#$timeout >= 1 && 10#$timeout <= 3600)) || ui_die "--timeout: от 1 до 3600 секунд."
[[ $configure_nginx == 0 || $configure_nginx == 1 ]] || ui_die "GO_INSTALL_NGINX должен быть 0 или 1."
[[ $tls == 0 || $tls == 1 ]] || ui_die "GO_INSTALL_TLS должен быть 0 или 1."
if ((tls)); then
  ((configure_nginx)) || ui_die "TLS требует --configure-nginx."
  [[ $tls_email =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || ui_die "Укажите email: --tls-email admin@ytc-srv.org"
  [[ $public_url == https://* && $aaid_url == https://* ]] || ui_die "Для Certbot адреса должны начинаться с https://."
fi
if [[ $mode == check ]] || ((!start && !deps_explicit)); then install_deps=0; fi
case "$mode" in install) operation_title='Установка' ;; update) operation_title='Обновление' ;; check) operation_title='Проверка' ;; esac
if ((UI_LEVEL_NUM >= 2 || interactive)); then
  nginx_title='ручная настройка'
  if ((configure_nginx)); then nginx_title='настройка установщиком'; fi
  tls_title='внешний TLS-прокси или ручная настройка'
  if ((tls)); then tls_title='Let’s Encrypt / Certbot'; fi
  printf '\nПараметры сервера\n  Режим: %s\n  Сайт: %s\n  AA ID: %s\n  Панель: http://127.0.0.1:%s/panel\n  Файл настроек: %s\n  Compose-проект: %s\n  Nginx: %s\n  HTTPS: %s\n' \
    "$operation_title" "$public_url" "$aaid_url" "$panel_port" "$env_file" "$project" "$nginx_title" "$tls_title"
fi
if ((fresh)) && [[ -f .env ]]; then
  printf '\nРекомендация: обнаружена .env старой установки. SQLite автоматически не импортируется; для сохранения аккаунтов используйте migrate-go.sh по инструкции docs/go-postgres-migration.md.\n'
fi
if ((interactive)); then
  ask 'Применить эти настройки: да/нет' да
  case "${REPLY,,}" in да|д|yes|y) ;; *) printf 'Настройки не изменены.\n'; exit 0 ;; esac
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
    if ((interactive)) || [[ -t 0 ]]; then sudo -v; else sudo -n true || ui_die "Нужны права администратора. Запустите sudo bash install.sh $mode --non-interactive."; fi
  fi
fi
total=4
if [[ $mode != check ]] && ((start)); then
  total=$((total + 3 + pull + build + configure_nginx + tls))
fi
ui_init "$total" "Go / PostgreSQL · $operation_title"
ui_run 'Проверка и подготовка зависимостей' installer_dependencies
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  if privileged docker info >/dev/null 2>&1; then DOCKER=(sudo -n docker); else
    printf 'Решение: запустите Docker и повторите с sudo; проверьте systemctl status docker.\n' >&2
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
  printf 'Настройки готовы; существующие пароли и ключи не заменялись.\n'
}
if [[ $mode == check ]]; then
  ui_run 'Проверка файла настроек' test -r "$env_file"
else
  ui_run 'Подготовка настроек и паролей' write_settings
fi
ui_run 'Проверка Compose-конфигурации' "${compose[@]}" config --quiet
ui_run 'Проверка ресурсов, портов, DNS и nginx' installer_preflight
if [[ $mode == check ]] || ((!start)); then
  ui_finish 'Проверки завершены; сервисы не запускались'
else
  if ((pull)); then ui_run 'Загрузка PostgreSQL и nginx' "${compose[@]}" pull postgres nginx; fi
  if ((build)); then ui_run 'Сборка Go-сервисов и Chromium' "${compose[@]}" build initialize; fi
  ui_run 'Запуск PostgreSQL' "${compose[@]}" up -d --wait --wait-timeout "$timeout" postgres
  ui_run 'Запуск и ожидание готовности сервисов' "${compose[@]}" up -d --no-build --wait --wait-timeout "$timeout"
  if ((configure_nginx)); then ui_run 'Настройка блоков host nginx' installer_nginx; fi
  if ((tls)); then ui_run 'Проверка и настройка HTTPS' installer_tls; fi
  ui_run 'Проверка HTTP-ответов сервера и AA ID' installer_health
  ui_finish 'Сервер установлен и проверен'
fi
printf '\nДанные установки\n  Сайт: %s\n  AA ID: %s\n  Панель: http://127.0.0.1:%s/panel\n  Локальные upstream: сайт %s, AA ID %s\n  Настройки и пароли: %s\n  Журнал: %s\n' \
  "$public_url" "$aaid_url" "$panel_port" "$site_port" "$aaid_port" "$env_file" "$UI_LOG_FILE"
printf '  Тома для резервного копирования: %s_keys, %s_postgres_data\n' "$project" "$project"
if ((fresh)) && { [[ -t 1 ]] || ((show_password)); }; then
  printf '  Пароль панели для новой пустой БД: %s\n' "$(env_value GO_ADMIN_PASSWORD)"
else
  printf '  Пароль панели сохранён; после импорта действует прежний пароль.\n'
fi
if ((!configure_nginx)); then printf '  Nginx: используйте nginx/go-hosts.conf.example с указанными upstream-портами.\n'; fi
