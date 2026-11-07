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
        print(f'Disk {root}: {free / (1 << 30):.1f} GiB free')
        if free < 4 << 30:
            raise ValueError('Not enough space to build Go/Chromium. Solution: free at least 4 GiB on the project and Docker disks.')
    memory = Path('/proc/meminfo')
    if memory.exists():
        fields = dict(line.split(':', 1) for line in memory.read_text().splitlines())
        available = int(fields.get('MemAvailable', fields['MemTotal']).split()[0]) / (1 << 20)
        print(f'Available memory: {available:.1f} GiB; CPU: {os.cpu_count()}')
        if available < 2:
            print('Recommendation: Chromium should have at least 2 GiB of available RAM; reduce LOGIN_SLOTS/RENEW_SLOTS or free memory.')
    for name, port in [('Site', args.site_port), ('AA ID', args.aaid_port), ('Panel', args.panel_port)]:
        with socket.socket() as probe:
            try:
                probe.bind(('127.0.0.1', port))
            except OSError:
                if f'127.0.0.1:{port}->' not in owned_ports:
                    raise ValueError(f'{name}: port {port} is used by another process. Solution: choose another --site-port/--aaid-port/--panel-port or free the port.')
                print(f'{name}: port {port} belongs to the current Compose project')
            else:
                print(f'{name}: port {port} is available')
    for name, origin in [('Site', args.site_url), ('AA ID', args.aaid_url)]:
        host = urlsplit(origin).hostname
        try:
            addresses = sorted({entry[4][0] for entry in socket.getaddrinfo(host, None)})
            print(f'DNS {name}: {host} = {", ".join(addresses)}')
        except socket.gaierror:
            message = f'DNS {host} does not resolve. Solution: create an A/AAAA record pointing to the server and wait for DNS propagation.'
            if args.require_dns:
                raise ValueError(message)
            print('Recommendation: ' + message)
    if args.require_dns:
        print('Let’s Encrypt requires port 80 to be accessible externally. Local DNS checks cannot confirm access through NAT/firewall.')


def check_health(args):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for name, port, field, expected in [('Site', args.site_port, 'protocol', 2),
                                        ('AA ID', args.aaid_port, 'service', 'aa-id'),
                                        ('Panel', args.panel_port, 'protocol', 2)]:
        try:
            with opener.open(f'http://127.0.0.1:{port}/health', timeout=10) as response:
                data = json.loads(response.read(65536))
                if response.status != 200 or data.get('ok') is not True or data.get(field) != expected:
                    raise ValueError('unexpected health response')
        except Exception as error:
            raise ValueError(f'{name}: health check failed. Solution: check docker compose ps/logs and the corresponding local port {port}.') from error
        print(f'{name}: HTTP 200, expected protocol confirmed')


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
        print(f'Check error: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
