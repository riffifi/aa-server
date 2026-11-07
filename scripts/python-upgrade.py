"""Discover and back up a legacy Docker deployment without reading its env."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


class LegacyDeployment:
    def __init__(self, project, target_project, docker=None):
        self.project = project
        self.target_project = target_project
        self.docker = docker or ['docker']

    def command(self, *args):
        return subprocess.run(self.docker + list(args), check=True, capture_output=True,
                              text=True, timeout=60).stdout.strip()

    def inspect(self, identifier, expression):
        return json.loads(self.command('inspect', '--format', '{{json ' + expression + '}}', identifier))

    def discover(self):
        if self.project == self.target_project:
            raise ValueError('Python and Go must use different Compose projects.')
        existing = self.command('ps', '-a', '--filter',
                                'label=com.docker.compose.project=' + self.target_project,
                                '--format', '{{.ID}}')
        if existing:
            raise ValueError('The target Go project already contains containers. Preserve its data: choose a new --project and --env-file, or recover the previous migration using its log.')
        containers = {}
        for service in ('sessions', 'aaid', 'nginx'):
            ids = self.command('ps', '-a', '--filter',
                               'label=com.docker.compose.project=' + self.project,
                               '--filter', 'label=com.docker.compose.service=' + service,
                               '--format', '{{.ID}}').splitlines()
            if len(ids) != 1:
                raise ValueError(f'Expected one Python container for {service}, found {len(ids)}. Solution: check --python-project.')
            identifier = ids[0]
            command = self.inspect(identifier, '.Config.Cmd')
            if service != 'nginx' and 'uvicorn' not in json.dumps(command):
                raise ValueError(f'{service} does not appear to be the source Python version; automatic stop refused.')
            containers[service] = {'id': identifier, 'running': bool(self.inspect(identifier, '.State.Running'))}
        nginx_ports = self.inspect(containers['nginx']['id'], '.HostConfig.PortBindings') or {}
        identity_ports = self.inspect(containers['aaid']['id'], '.HostConfig.PortBindings') or {}

        def local_port(bindings, key):
            values = bindings.get(key) or []
            if len(values) != 1 or values[0].get('HostIp') != '127.0.0.1':
                raise ValueError(f'{key}: expected one published port on 127.0.0.1. Use manual migration for other configurations.')
            port = int(values[0]['HostPort'])
            if not 1 <= port <= 65535:
                raise ValueError('Invalid published Python port.')
            return port

        ports = {'site': local_port(nginx_ports, '80/tcp'),
                 'panel': local_port(nginx_ports, '8081/tcp'),
                 'aaid': local_port(identity_ports, '8090/tcp')}
        if len(set(ports.values())) != 3:
            raise ValueError('Source deployment ports must be different.')
        return {'legacy_project': self.project, 'target_project': self.target_project,
                'containers': containers, 'ports': ports}

    def backup(self, manifest, destination):
        destination = Path(destination)
        for service in ('sessions', 'aaid'):
            identifier = manifest['containers'][service]['id']
            if self.inspect(identifier, '.State.Running'):
                raise ValueError('Both Python services must be stopped before backup.')
            folder = destination / service
            folder.mkdir(mode=0o700)
            self.command('cp', identifier + ':/data/.', str(folder.resolve()))
        required = [destination / 'sessions/sessions.sqlite3',
                    destination / 'sessions/master.key',
                    destination / 'aaid/aaid.sqlite3',
                    destination / 'aaid/signing.key']
        if any(not file.is_file() or file.is_symlink() for file in required):
            raise ValueError('Regular database/key files are missing from /data. Source containers will be restored; check the backup directory.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('discover', 'backup', 'ids', 'port', 'mark-pending'))
    parser.add_argument('--python-project', default='antiattendance-sessions')
    parser.add_argument('--project', default='antiattendance-go')
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--directory')
    parser.add_argument('--name', choices=('site', 'aaid', 'panel'))
    parser.add_argument('--running-only', action='store_true')
    parser.add_argument('--docker-sudo', action='store_true')
    parser.add_argument('--env-file')
    parser.add_argument('--pending', choices=('0', '1'))
    args = parser.parse_args()
    deployment = LegacyDeployment(args.python_project, args.project,
                                  ['sudo', '-n', 'docker'] if args.docker_sudo else None)
    try:
        path = Path(args.manifest)
        if args.action == 'discover':
            data = deployment.discover()
            descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
                json.dump(data, stream)
            print('Python deployment found; ports and containers checked.')
        elif args.action == 'mark-pending':
            if not args.env_file or args.pending is None:
                parser.error('--env-file and --pending are required')
            settings = Path(args.env_file)
            if settings.is_symlink():
                raise ValueError('The settings file must not be a symlink.')
            lines = [line for line in settings.read_text(encoding='utf-8').splitlines()
                     if not line.startswith('GO_PYTHON_MIGRATION_PENDING=')]
            lines.append('GO_PYTHON_MIGRATION_PENDING=' + args.pending)
            descriptor, temporary = tempfile.mkstemp(prefix=settings.name + '.', dir=settings.parent)
            try:
                with os.fdopen(descriptor, 'w', encoding='utf-8', newline='\n') as stream:
                    stream.write('\n'.join(lines) + '\n')
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, settings)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        else:
            data = json.loads(path.read_text(encoding='utf-8'))
            if args.action == 'backup':
                if not args.directory:
                    parser.error('--directory is required')
                deployment.backup(data, args.directory)
            elif args.action == 'port':
                if not args.name:
                    parser.error('--name is required')
                print(data['ports'][args.name])
            else:
                for value in data['containers'].values():
                    if not args.running_only or value['running']:
                        print(value['id'])
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(f'Python migration error: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
