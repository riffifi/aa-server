"""Read-only host and local HTTP checks used by the Go installer."""
import argparse
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import urllib.request
from urllib.parse import urlsplit


def check_host(args):
    docker = ['sudo', '-n', 'docker'] if args.docker_sudo else ['docker']
    result = subprocess.run(docker + ['ps', '--filter', f'label=com.docker.compose.project={args.project}',
                                    '--format', '{{.Ports}}'], check=True, text=True, capture_output=True, timeout=20)
    owned_ports = result.stdout
    roots = [Path.cwd()]
    info = subprocess.run(docker + ['info', '--format', '{{.DockerRootDir}}'],
                          check=True, text=True, capture_output=True, timeout=20)
    if info.stdout.strip() and Path(info.stdout.strip()).is_dir():
        roots.append(Path(info.stdout.strip()))
    for root in roots:
        free = shutil.disk_usage(root).free
        print(f'Диск {root}: свободно {free / (1 << 30):.1f} GiB')
        if free < 4 << 30:
            raise ValueError('Мало места для сборки Go/Chromium. Решение: освободите не менее 4 GiB на диске проекта и Docker.')
    memory = Path('/proc/meminfo')
    if memory.exists():
        fields = dict(line.split(':', 1) for line in memory.read_text().splitlines())
        available = int(fields.get('MemAvailable', fields['MemTotal']).split()[0]) / (1 << 20)
        print(f'Доступная память: {available:.1f} GiB; CPU: {os.cpu_count()}')
        if available < 2:
            print('Рекомендация: для Chromium желательно от 2 GiB свободной RAM; уменьшите LOGIN_SLOTS/RENEW_SLOTS или освободите память.')
    for name, port in [('Сайт', args.site_port), ('AA ID', args.aaid_port), ('Панель', args.panel_port)]:
        with socket.socket() as probe:
            try:
                probe.bind(('127.0.0.1', port))
            except OSError:
                if f'127.0.0.1:{port}->' not in owned_ports:
                    raise ValueError(f'{name}: порт {port} занят другим процессом. Решение: выберите другой --site-port/--aaid-port/--panel-port либо освободите порт.')
                print(f'{name}: порт {port} принадлежит текущему Compose-проекту')
            else:
                print(f'{name}: порт {port} свободен')
    for name, origin in [('Сайт', args.site_url), ('AA ID', args.aaid_url)]:
        host = urlsplit(origin).hostname
        try:
            addresses = sorted({entry[4][0] for entry in socket.getaddrinfo(host, None)})
            print(f'DNS {name}: {host} = {", ".join(addresses)}')
        except socket.gaierror:
            message = f'DNS {host} не разрешается. Решение: создайте A/AAAA-запись домена на адрес сервера и дождитесь обновления DNS.'
            if args.require_dns:
                raise ValueError(message)
            print('Рекомендация: ' + message)
    if args.require_dns:
        print('Для Let’s Encrypt порт 80 должен быть доступен извне. Локальная DNS-проверка не подтверждает доступность через NAT/firewall.')


def check_health(args):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for name, port, field, expected in [('Сайт', args.site_port, 'protocol', 2),
                                        ('AA ID', args.aaid_port, 'service', 'aa-id'),
                                        ('Панель', args.panel_port, 'protocol', 2)]:
        try:
            with opener.open(f'http://127.0.0.1:{port}/health', timeout=10) as response:
                data = json.loads(response.read(65536))
                if response.status != 200 or data.get('ok') is not True or data.get(field) != expected:
                    raise ValueError('unexpected health response')
        except Exception as error:
            raise ValueError(f'{name}: health-проверка не прошла. Решение: проверьте docker compose ps/logs и соответствующий локальный порт {port}.') from error
        print(f'{name}: HTTP 200, ожидаемый протокол подтверждён')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--site-url', default='https://aa.ytc-srv.org')
    parser.add_argument('--aaid-url', default='https://aaid.ytc-srv.org')
    parser.add_argument('--site-port', type=int, default=18080)
    parser.add_argument('--aaid-port', type=int, default=18091)
    parser.add_argument('--panel-port', type=int, default=18092)
    parser.add_argument('--project', default='antiattendance-go')
    parser.add_argument('--docker-sudo', action='store_true')
    parser.add_argument('--require-dns', action='store_true')
    parser.add_argument('--health', action='store_true')
    args = parser.parse_args()
    try:
        if args.health:
            check_health(args)
        else:
            check_host(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f'Ошибка проверки: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
