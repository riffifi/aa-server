#!/usr/bin/env python3
"""Install the local-only resource collector on a Linux/systemd host."""
import argparse
from pathlib import Path
import re
import shutil
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', required=True)
    parser.add_argument('--disable', action='store_true')
    args = parser.parse_args()
    if not re.fullmatch(r'[a-z0-9][a-z0-9_-]{0,62}', args.project):
        parser.error('invalid Compose project name')
    unit_name = 'antiattendance-metrics-' + args.project + '.service'
    if args.disable:
        subprocess.run(['systemctl', 'disable', '--now', unit_name], check=False)
        return
    install(args.project)


def install(project, root=Path('/')):
    if not re.fullmatch(r'[a-z0-9][a-z0-9_-]{0,62}', project):
        raise ValueError('invalid Compose project name')
    unit_name = 'antiattendance-metrics-' + project + '.service'
    target = root / 'usr/local/lib/antiattendance-metrics/collect_metrics.py'
    previous_code = target.read_bytes() if target.exists() else None
    running = subprocess.run(['systemctl', 'is-active', '--quiet', unit_name], check=False).returncode == 0
    if running:
        # --once uses the same exclusive state lock as the daemon. Updating a
        # live collector used to fail here before the service could be restarted.
        subprocess.run(['systemctl', 'stop', unit_name], check=True)
    try:
        _install(project, root)
    except Exception:
        if previous_code is not None:
            target.write_bytes(previous_code)
            target.chmod(0o755)
        if running:
            subprocess.run(['systemctl', 'start', unit_name], check=False)
        raise


def _install(project, root):
    unit_name = 'antiattendance-metrics-' + project + '.service'
    destination = root / 'usr/local/lib/antiattendance-metrics'
    destination.mkdir(parents=True, exist_ok=True, mode=0o755)
    target = destination / 'collect_metrics.py'
    shutil.copyfile(Path(__file__).with_name('collect_metrics.py'), target)
    target.chmod(0o755)
    parent = root / 'var/lib/antiattendance-metrics'
    parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    parent.chmod(0o755)
    directory = parent / project
    directory.mkdir(parents=True, exist_ok=True, mode=0o755)
    directory.chmod(0o755)
    python = shutil.which('python3')
    if not python or any(char in python for char in '"%\n\r\\'):
        raise RuntimeError('Use a system Python 3 installation in a standard path')
    command = [python, '-I', str(target), '--project', project, '--directory', str(directory)]
    # Verify Docker access before reporting that monitoring works.
    subprocess.run(command + ['--once'], check=True, timeout=120)
    unit = f'''[Unit]
Description=AntiAttendance application resource counters ({project})
After=docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart="{python}" -I {target} --project {project} --directory {directory}
Restart=on-failure
RestartSec=10
UMask=0022
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX
ReadWritePaths={directory}
MemoryMax=128M
CPUQuota=10%

[Install]
WantedBy=multi-user.target
'''
    units = root / 'etc/systemd/system'
    units.mkdir(parents=True, exist_ok=True)
    (units / unit_name).write_text(unit, encoding='utf-8')
    subprocess.run(['systemctl', 'daemon-reload'], check=True)
    subprocess.run(['systemctl', 'enable', unit_name], check=True)
    subprocess.run(['systemctl', 'restart', unit_name], check=True)
    subprocess.run(['systemctl', 'is-active', '--quiet', unit_name], check=True)
    print('Resource collector is running. Counters are preserved in ' + str(directory))


if __name__ == '__main__':
    main()
