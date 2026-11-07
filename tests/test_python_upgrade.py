import base64
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('python_upgrade', ROOT / 'scripts/python-upgrade.py')
upgrade = importlib.util.module_from_spec(spec)
spec.loader.exec_module(upgrade)
BASH = shutil.which('bash')
if os.name == 'nt':
    candidate = Path('C:/Program Files/Git/bin/bash.exe')
    BASH = str(candidate) if candidate.exists() else None


class DiscoveryTests(unittest.TestCase):
    def fixture(self, foreign=False, occupied=False, public=False):
        deployment = upgrade.LegacyDeployment('legacy', 'go')

        def command(*args):
            if args[0] == 'ps':
                if 'label=com.docker.compose.project=go' in args:
                    return 'target-id' if occupied else ''
                for name in ('sessions', 'aaid', 'nginx'):
                    if 'label=com.docker.compose.service=' + name in args:
                        return 'old-' + name
            raise AssertionError(args)

        def inspect(identifier, expression):
            if expression == '.Config.Cmd':
                return ['aa-server'] if foreign else ['uvicorn', 'app.run:app']
            if expression == '.State.Running':
                return True
            host = '0.0.0.0' if public else '127.0.0.1'
            return {'80/tcp': [{'HostIp': host, 'HostPort': '6767'}],
                    '8081/tcp': [{'HostIp': host, 'HostPort': '8092'}],
                    '8090/tcp': [{'HostIp': host, 'HostPort': '8091'}]}
        deployment.command = command
        deployment.inspect = inspect
        return deployment

    def test_detects_actual_published_ports(self):
        result = self.fixture().discover()
        self.assertEqual(result['ports'], {'site': 6767, 'panel': 8092, 'aaid': 8091})

    def test_refuses_non_python_source_and_existing_target(self):
        for options in ({'foreign': True}, {'occupied': True}, {'public': True}):
            with self.subTest(options=options), self.assertRaises(ValueError):
                self.fixture(**options).discover()

    def test_backup_refuses_running_writer(self):
        deployment = self.fixture()
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, 'остановлены'):
                deployment.backup(deployment.discover(), directory)


MOCK_DOCKER = r'''
import json, os, shutil, sys
from pathlib import Path
root=Path(os.environ['MOCK_ROOT'])
state_path=root/'docker-state.json'
state=json.loads(state_path.read_text())
args=sys.argv[1:]
with (root/'docker-calls.jsonl').open('a') as log: log.write(json.dumps(args)+'\n')
def save(): state_path.write_text(json.dumps(state))
if args[0]=='info': print('/nonexistent-docker-root' if '--format' in args else 'ready')
elif args[0]=='ps':
    if 'label=com.docker.compose.project=antiattendance-go' in args:
        if state['target']: print('target-pg')
    else:
        for service in ('sessions','aaid','nginx'):
            if 'label=com.docker.compose.service='+service in args: print('old-'+service)
elif args[0]=='inspect':
    expression=args[args.index('--format')+1]
    identifier=args[-1]
    service=identifier.removeprefix('old-')
    if '.Config.Cmd' in expression: print(json.dumps(['uvicorn','app.run:app']))
    elif '.State.Running' in expression: print(json.dumps(state['running'][service]))
    else:
        data={'80/tcp':[{'HostIp':'127.0.0.1','HostPort':'6767'}],
              '8081/tcp':[{'HostIp':'127.0.0.1','HostPort':'8092'}],
              '8090/tcp':[{'HostIp':'127.0.0.1','HostPort':'8091'}]}
        print(json.dumps(data))
elif args[0] in ('stop','start'):
    for value in args[1:]:
        if value.startswith('old-'): state['running'][value[4:]]=args[0]=='start'
    save()
elif args[0]=='cp':
    service=args[1].split(':')[0].removeprefix('old-')
    shutil.copytree(root/'legacy-data'/service,Path(args[2]),dirs_exist_ok=True)
elif args[0]=='compose':
    if 'build' in args and os.environ.get('FAIL_BUILD')=='1': raise SystemExit(17)
    if 'up' in args:
        state['target']=True
        if 'postgres' not in args: state['go_running']=True
        save()
    if 'aa-import' in args:
        if os.environ.get('FAIL_IMPORT')=='1': raise SystemExit(19)
        state['imported']=True
        save()
    if 'stop' in args:
        state['go_running']=False
        save()
    print('mock Compose ready')
elif args[0]=='image': print('image ready')
else: raise AssertionError(args)
'''


@unittest.skipUnless(BASH, 'Bash is required')
class PythonUpgradeFlowTests(unittest.TestCase):
    def setUp(self):
        (ROOT / '.go-data').mkdir(exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=ROOT / '.go-data', prefix='upgrade-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for name in ('install.sh', 'install-go.sh', 'upgrade-python.sh', 'migrate-go.sh', 'compose.go.yaml', 'compose.yaml'):
            shutil.copy2(ROOT / name, self.root / name)
        shutil.copytree(ROOT / 'scripts', self.root / 'scripts', ignore=shutil.ignore_patterns('__pycache__'))
        (self.root / '.env').write_text('PUBLIC_URL=https://aa.ytc-srv.org\nAAID_PUBLIC_URL=https://aaid.ytc-srv.org\nADMIN_PASSWORD=preserved-not-printed\n')
        for service in ('sessions', 'aaid'):
            folder = self.root / 'legacy-data' / service
            folder.mkdir(parents=True)
            with sqlite3.connect(folder / ('sessions.sqlite3' if service == 'sessions' else 'aaid.sqlite3')) as db:
                if service == 'sessions':
                    db.executescript('CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT); CREATE TABLE accounts(id TEXT PRIMARY KEY,secret TEXT); CREATE TABLE events(id INTEGER PRIMARY KEY);')
                    db.execute('INSERT INTO config VALUES(?,?)', ('admin_password', 'preserved-hash'))
                else:
                    db.executescript('CREATE TABLE users(id TEXT); CREATE TABLE mirea_links(user_id TEXT); CREATE TABLE sessions(id TEXT); CREATE TABLE refresh_tokens(hash TEXT);')
            db.close()
        (self.root / 'legacy-data/sessions/master.key').write_bytes(base64.urlsafe_b64encode(bytes(32)))
        (self.root / 'legacy-data/aaid/signing.key').write_bytes(bytes(32))
        self.state_path = self.root / 'docker-state.json'
        self.state_path.write_text(json.dumps({'running': dict.fromkeys(('sessions','aaid','nginx'), True),
                                               'target':False,'go_running':False,'imported':False}))
        (self.root / 'mock-docker.py').write_text(MOCK_DOCKER)
        (self.root / 'mock-python-runner.py').write_text('''
import os, runpy, subprocess, sys
original_run=subprocess.run
def run(args, **kwargs):
    if isinstance(args,list) and args and args[0]=='docker':
        args=[os.environ['MOCK_BASH'],os.environ['MOCK_ROOT']+'/fake-bin/docker',*args[1:]]
    return original_run(args,**kwargs)
subprocess.run=run
sys.argv=sys.argv[1:]
runpy.run_path(sys.argv[0],run_name='__main__')
''')
        self.bin = self.root / 'fake-bin'
        self.bin.mkdir()
        self.executable('docker', '#!/usr/bin/env bash\nexec "$MOCK_PYTHON" "$MOCK_ROOT/mock-docker.py" "$@"\n')
        self.executable('python3', '''#!/usr/bin/env bash
case "${1:-}" in
  --version) printf 'Python mock\\n' ;;
  *installer-preflight.py) printf 'mock host checks passed\\n' ;;
  *) exec "$MOCK_PYTHON" "$MOCK_ROOT/mock-python-runner.py" "$@" ;;
esac
''')
        self.executable('openssl', '#!/usr/bin/env bash\nif [[ "$1" == rand ]]; then printf "0123456789abcdef0123456789abcdef0123456789abcdef\\n"; else printf "OpenSSL mock\\n"; fi\n')
        self.executable('curl', '#!/usr/bin/env bash\nexit 0\n')
        self.env = dict(os.environ, MOCK_ROOT=self.root.as_posix(), MOCK_PYTHON=Path(sys.executable).as_posix(),
                        MOCK_BASH=Path(BASH).as_posix(), PATH=str(self.bin)+os.pathsep+os.environ['PATH'], TERM='dumb',
                        PYTHONUTF8='1', PYTHONIOENCODING='utf-8', MSYS2_ARG_CONV_EXCL='*', MSYS_NO_PATHCONV='1')
        self.env.pop('BASH_ENV', None)

    def executable(self, name, content):
        target = self.bin / name
        target.write_text(content, encoding='utf-8', newline='\n')
        target.chmod(0o755)

    def invoke(self, *options):
        return subprocess.run([BASH, 'install.sh', 'update', '--non-interactive', '--no-install-deps',
                               '--plain', '--skip-pull', *options], cwd=self.root, env=self.env,
                              capture_output=True, text=True, encoding='utf-8', timeout=60)

    def state(self):
        return json.loads(self.state_path.read_text())

    def test_missing_go_env_routes_update_to_preserving_migration(self):
        old_env = (self.root / '.env').read_bytes()
        result = self.invoke('--stop-python')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        state = self.state()
        self.assertTrue(state['imported'])
        self.assertTrue(state['go_running'])
        self.assertFalse(any(state['running'].values()))
        settings = (self.root / '.env.go').read_text()
        self.assertIn('GO_PORT=18080', settings)
        self.assertIn('GO_AAID_PORT=18091', settings)
        self.assertIn('GO_PANEL_PORT=18092', settings)
        self.assertIn('[ТРЕБУЕТСЯ ПЕРЕКЛЮЧЕНИЕ NGINX]', result.stdout)
        self.assertIn('GO_PYTHON_MIGRATION_PENDING=0', settings)
        self.assertEqual((self.root / '.env').read_bytes(), old_env)
        backups = list((self.root / '.go-data').glob('python-backup-*'))
        self.assertEqual(len(backups), 1)
        self.assertTrue((backups[0] / 'snapshot.jsonl').exists())
        self.assertEqual((backups[0] / 'sessions/master.key').read_bytes(), base64.urlsafe_b64encode(bytes(32)))
        self.assertEqual((backups[0] / 'aaid/signing.key').read_bytes(), bytes(32))
        self.assertNotIn('preserved-not-printed', result.stdout + result.stderr)
        calls = [json.loads(line) for line in (self.root / 'docker-calls.jsonl').read_text().splitlines()]
        build_index = next(i for i,a in enumerate(calls) if 'build' in a)
        stop_index = next(i for i,a in enumerate(calls) if a[0]=='stop')
        self.assertLess(build_index, stop_index)

    def test_failed_build_does_not_stop_python(self):
        self.env['FAIL_BUILD'] = '1'
        result = self.invoke('--stop-python')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(all(self.state()['running'].values()))
        self.assertFalse(self.state()['imported'])
        self.assertIn('GO_PYTHON_MIGRATION_PENDING=1', (self.root / '.env.go').read_text())
        self.env.pop('FAIL_BUILD')
        retry = self.invoke('--stop-python')
        self.assertEqual(retry.returncode, 0, retry.stdout + retry.stderr)
        self.assertTrue(self.state()['imported'])

    def test_legacy_port_override_is_rejected_before_stopping_python(self):
        result = self.invoke('--stop-python', '--site-port', '6767')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(all(self.state()['running'].values()))
        self.assertFalse(self.state()['target'])

    def test_failed_import_restores_original_containers_and_keeps_backup(self):
        self.env['FAIL_IMPORT'] = '1'
        result = self.invoke('--stop-python')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(all(self.state()['running'].values()))
        self.assertFalse(self.state()['go_running'])
        self.assertIn('[ВОССТАНОВЛЕНО]', result.stderr)
        self.assertTrue(list((self.root / '.go-data').glob('python-backup-*/snapshot.jsonl')), result.stdout + result.stderr)

    def test_noninteractive_stop_requires_explicit_permission(self):
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--stop-python', result.stderr)
        self.assertTrue(all(self.state()['running'].values()))
        self.assertFalse((self.root / '.env.go').exists())

    def test_prepare_only_creates_go_settings_without_stopping_python(self):
        result = self.invoke('--no-start')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / '.env.go').exists())
        self.assertTrue(all(self.state()['running'].values()))
        self.assertFalse(self.state()['target'])


if __name__ == '__main__':
    unittest.main()
