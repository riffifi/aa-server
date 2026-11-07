"""Exercise installer flows against fake commands; never install host packages."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
BASH = shutil.which('bash')
if os.name == 'nt':
    candidate = Path('C:/Program Files/Git/bin/bash.exe')
    BASH = str(candidate) if candidate.exists() else None


@unittest.skipUnless(BASH, 'Bash is required')
class InstallerTests(unittest.TestCase):
    def setUp(self):
        (ROOT / '.go-data').mkdir(exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix='installer-test-', dir=ROOT / '.go-data')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ('install.sh', 'install-go.sh', 'migrate-go.sh', 'compose.go.yaml'):
            shutil.copy2(ROOT / name, self.root / name)
        shutil.copytree(ROOT / 'scripts', self.root / 'scripts', ignore=shutil.ignore_patterns('__pycache__'))
        self.bin = self.root / 'fake-bin'
        self.bin.mkdir()
        self.executable('docker', '''#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$MOCK_CALLS"
if [[ " $* " == *" build initialize "* ]]; then
  printf 'build-metadata:%s:%s\\n' "${AA_BUILD_VERSION:-}" "${AA_BUILD_COMMIT:-}" >> "$MOCK_CALLS"
fi
if [[ " $* " == *" build initialize "* && ${MOCK_FAIL_BUILD:-0} == 1 ]]; then
  printf '=> registry unavailable\\n' >&2
  exit 17
fi
printf 'mock Docker ready\\n'
''')
        self.executable('python3', '#!/usr/bin/env bash\nprintf "mock host checks passed\\n"\n')
        self.executable('openssl', '#!/usr/bin/env bash\nprintf "0123456789abcdef0123456789abcdef0123456789abcdef\\n"\n')
        self.executable('curl', '#!/usr/bin/env bash\nexit 0\n')
        self.env = os.environ.copy()
        self.env['PATH'] = str(self.bin) + os.pathsep + self.env['PATH']
        self.env['MOCK_CALLS'] = str(self.root / 'calls')
        self.env['TERM'] = 'dumb'
        self.env.pop('BASH_ENV', None)

    def executable(self, name, content):
        path = self.bin / name
        path.write_text(content, encoding='utf-8', newline='\n')
        path.chmod(0o755)

    def run_installer(self, *args, input=None):
        return subprocess.run([BASH, 'install.sh', *args], cwd=self.root, env=self.env,
                              input=input, capture_output=True, text=True, encoding='utf-8', timeout=30)

    def prepare(self, *extra):
        command = []
        if extra and extra[0] in ('install', 'update', 'check'):
            command, extra = [extra[0]], extra[1:]
        result = self.run_installer(*command, '--non-interactive', '--no-start', '--no-install-deps',
                                    '--plain', *extra)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def test_fresh_configuration_has_named_stages_and_never_prints_password_by_default(self):
        result = self.prepare('--site-port=19180', '--project', 'test-project')
        settings = (self.root / '.env.go').read_text()
        self.assertIn('GO_PORT=19180', settings)
        self.assertIn('GO_INSTALL_PROJECT=test-project', settings)
        self.assertIn('[OK]', result.stdout)
        self.assertIn('100%', result.stdout)
        self.assertIn('total 00:', result.stdout)
        self.assertNotIn('\x1b', result.stdout)
        self.assertNotIn('=>', result.stdout)
        self.assertNotRegex(result.stdout + result.stderr, '[А-Яа-яЁё]')
        self.assertNotIn('0123456789abcdef0123456789abcdef0123456789abcdef', result.stdout)
        calls = (self.root / 'calls').read_text()
        self.assertNotIn(' up ', calls)
        self.assertNotIn(' build ', calls)

    def test_update_changes_only_explicit_settings_and_retains_passwords_project_and_comments(self):
        self.prepare('--project', 'retained-project')
        path = self.root / '.env.go'
        initial = path.read_text() + '\n# retained comment\nCUSTOM_SETTING=keep\n'
        path.write_text(initial)
        result = self.prepare('update', '--site-port', '19181')
        updated = path.read_text()
        self.assertIn('GO_PORT=19181', updated)
        self.assertIn('GO_INSTALL_PROJECT=retained-project', updated)
        self.assertIn('# retained comment', updated)
        self.assertIn('CUSTOM_SETTING=keep', updated)
        self.assertEqual([s for s in initial.splitlines() if 'PASSWORD=' in s],
                         [s for s in updated.splitlines() if 'PASSWORD=' in s])

    def test_check_is_read_only_and_does_not_start_services(self):
        self.prepare()
        path = self.root / '.env.go'
        before = path.read_bytes()
        result = self.run_installer('check', '--plain', '--non-interactive')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(path.read_bytes(), before)
        self.assertNotIn(' up ', (self.root / 'calls').read_text())

    def test_missing_values_bad_ports_and_foreign_domains_fail_before_creating_settings(self):
        for args in [('--site-port',), ('--site-port=0',), ('--site-url=https://outside.invalid',),
                     ('--site-port=18091',), ('--unknown',), ('--site-url=https://aa.ytc-srv.org:banana',),
                     ('--site-url=http://127.999.0.1',), ('--site-url=https://aaid.ytc-srv.org',)]:
            with self.subTest(args=args):
                result = self.run_installer(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Error:', result.stderr)
                self.assertFalse((self.root / '.env.go').exists())

    def test_failed_build_stops_sequence_retains_settings_and_suggests_solution(self):
        self.prepare()
        self.env['MOCK_FAIL_BUILD'] = '1'
        result = self.run_installer('update', '--non-interactive', '--no-install-deps',
                                    '--plain', '--skip-pull')
        self.assertEqual(result.returncode, 17, result.stdout + result.stderr)
        self.assertIn('[ERROR]', result.stderr)
        self.assertIn('Solution:', result.stderr)
        self.assertNotIn('=>', result.stderr)
        self.assertNotIn('[DONE]', result.stdout)
        self.assertNotIn(' up ', (self.root / 'calls').read_text())
        self.assertTrue((self.root / '.env.go').exists())

    def test_successful_start_and_no_build_flags_are_forwarded(self):
        self.prepare()
        result = self.run_installer('update', '--non-interactive', '--no-install-deps',
                                    '--plain', '--skip-pull', '--no-build', '--timeout=321')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('100%', result.stdout)
        calls = (self.root / 'calls').read_text()
        self.assertIn('--wait-timeout 321 postgres', calls)
        self.assertIn('up -d --no-build --wait --wait-timeout 321', calls)
        self.assertNotIn('build initialize', calls)

    def test_release_metadata_is_passed_to_image_build(self):
        self.prepare()
        self.env['AA_BUILD_VERSION'] = 'v1.2.3-test'
        self.env['AA_BUILD_COMMIT'] = '1234567890abcdef1234567890abcdef12345678'
        result = self.run_installer('update', '--non-interactive', '--no-install-deps', '--plain', '--skip-pull')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('build-metadata:v1.2.3-test:' + self.env['AA_BUILD_COMMIT'], (self.root / 'calls').read_text())

    def test_first_run_wizard_uses_console_answers(self):
        answers = 'https://portal.ytc-srv.org\n\n19180\n19191\n19192\nno\nyes\n'
        result = self.run_installer('--interactive', '--no-start', '--no-install-deps',
                                    '--no-host-nginx', '--plain', input=answers)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('setup wizard', result.stdout)
        settings = (self.root / '.env.go').read_text()
        self.assertIn('GO_PUBLIC_URL=https://portal.ytc-srv.org', settings)
        self.assertIn('GO_PORT=19180', settings)
        self.assertIn('GO_INSTALL_NGINX=0', settings)

    def test_wizard_cancel_changes_nothing(self):
        answers = '\n\n\n\n\nno\nno\n'
        result = self.run_installer('--interactive', '--no-start', '--no-install-deps',
                                    '--no-host-nginx', '--plain', input=answers)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.root / '.env.go').exists())
        self.assertFalse((self.root / 'calls').exists())

    def test_spinner_bar_and_cursor_cleanup(self):
        script = '''set -euo pipefail
source scripts/installer-ui.sh
UI_LOG_FILE=animation-test.log
ui_init 1 demo
UI_ANIMATE=1
ui_run 'Loading test' sleep 0.3
ui_finish done
'''
        result = subprocess.run([BASH, '-c', script], cwd=self.root, env=self.env,
                                capture_output=True, text=True, encoding='utf-8', timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('[..................]', result.stdout)
        self.assertIn('\x1b[?25h', result.stdout)
        self.assertIn('100%', result.stdout)

    def test_update_installs_missing_dependency_using_package_hook(self):
        script = '''set -euo pipefail
source scripts/installer-system.sh
configure_nginx=0 tls=0 install_deps=1 mode=update
command() {
  if [[ "$1" == -v && "$2" == openssl ]]; then return 1; fi
  builtin command "$@"
}
installer_apt() { printf 'installed:%s\\n' "$*"; }
installer_dependencies
'''
        result = subprocess.run([BASH, '-c', script], cwd=self.root, env=self.env,
                                capture_output=True, text=True, encoding='utf-8', timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('installed:openssl', result.stdout)

    def test_console_logging_levels_and_private_log_file(self):
        self.prepare()
        for level in ('error', 'warn', 'info', 'debug'):
            with self.subTest(level=level):
                log = self.root / f'{level}.log'
                result = self.run_installer('check', '--non-interactive', '--plain',
                                            '--log-level=' + level, '--log-file', log.name)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn('[DONE]', result.stdout)
                self.assertTrue(log.exists())
                self.assertIn('[INFO] Stage', log.read_text(encoding='utf-8'))
                if level in ('error', 'warn'):
                    self.assertNotIn('[OK]', result.stdout)
                    self.assertNotIn('mock Docker ready', result.stdout)
                elif level == 'debug':
                    self.assertIn('[DEBUG] Command:', result.stdout)
                    self.assertIn('mock Docker ready', result.stdout)
                else:
                    self.assertIn('[OK]', result.stdout)
                self.assertNotIn('0123456789abcdef0123456789abcdef0123456789abcdef', result.stdout)
                self.assertNotIn('\x1b', result.stdout)

    def test_invalid_log_level_and_existing_log_file_are_refused(self):
        result = self.run_installer('--log-level=trace')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / '.env.go').exists())
        log = self.root / 'existing.log'
        log.write_text('keep original')
        result = self.run_installer('--non-interactive', '--no-start', '--no-install-deps',
                                    '--log-file', log.name)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(log.read_text(), 'keep original')

    def test_migration_has_named_stages_and_preserves_source_files(self):
        self.prepare('--project', 'migration-project')
        sources = []
        for name in ('snapshot.jsonl', 'original-master.key', 'original-signing.key'):
            path = self.root / name
            path.write_bytes(b'synthetic-preserved-source')
            sources.append(path)
        result = subprocess.run([BASH, 'migrate-go.sh', '--snapshot', sources[0].name,
                                 '--master-key', sources[1].name, '--signing-key', sources[2].name,
                                 '--no-install-deps', '--no-build', '--plain', '--log-level=info'],
                                cwd=self.root, env=self.env, capture_output=True,
                                text=True, encoding='utf-8', timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('atomically importing', result.stdout)
        self.assertIn('100%', result.stdout)
        calls = (self.root / 'calls').read_text()
        self.assertIn('--project-name migration-project', calls)
        self.assertIn('aa-init -directory /keys', calls)
        self.assertIn('aa-import -input /migration/snapshot', calls)
        for path in sources:
            self.assertEqual(path.read_bytes(), b'synthetic-preserved-source')


if __name__ == '__main__':
    unittest.main()
