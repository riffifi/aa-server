import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('configure_go_nginx', ROOT / 'scripts/configure_go_nginx.py')
nginx = importlib.util.module_from_spec(spec)
spec.loader.exec_module(nginx)


class GoNginxTests(unittest.TestCase):
    def setUp(self):
        (ROOT / '.go-data').mkdir(exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(dir=ROOT / '.go-data')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / 'nginx.conf'
        self.config.write_text('events {}\nhttp { server { server_name other.ytc-srv.org; } }\n')
        self.managed = self.root / 'antiattendance-go-managed/services.conf'
        self.calls = []

    def command(self, args, **kwargs):
        self.calls.append(args)
        dump = f'# configuration file {self.config}:\n' + self.config.read_text()
        if self.managed.exists():
            dump += f'\n# configuration file {self.managed}:\n' + self.managed.read_text()
        return subprocess.CompletedProcess(args, 0, dump)

    def configure(self, **kwargs):
        return nginx.configure('https://aa.ytc-srv.org', 'https://aaid.ytc-srv.org',
                               kwargs.pop('site_port', 18080), kwargs.pop('aaid_port', 18091),
                               str(self.config), **kwargs)

    def test_install_and_repeat_preserve_other_sites(self):
        with patch.object(nginx.subprocess, 'run', side_effect=self.command):
            self.configure()
            first = self.config.read_bytes()
            self.configure()
        self.assertEqual(self.config.read_bytes(), first)
        self.assertIn('server_name other.ytc-srv.org;', self.config.read_text())
        self.assertEqual(self.config.read_text().count('antiattendance managed site'), 1)
        self.assertIn('server_name aa.ytc-srv.org;', self.managed.read_text())
        self.assertIn('server_name aaid.ytc-srv.org;', self.managed.read_text())

    def test_update_preserves_certbot_tls_and_redirect_blocks(self):
        with patch.object(nginx.subprocess, 'run', side_effect=self.command):
            self.configure()
            text = self.managed.read_text().replace('listen 80;', 'listen 443 ssl;\n    ssl_certificate /etc/test/fullchain.pem;')
            text += '\nserver { listen 80; server_name aa.ytc-srv.org; return 301 https://$host$request_uri; }\n'
            self.managed.write_text(text)
            self.configure(site_port=19180, aaid_port=19191)
        after = self.managed.read_text()
        self.assertEqual(after.count('ssl_certificate /etc/test/fullchain.pem;'), 2)
        self.assertIn('return 301 https://$host$request_uri;', after)
        self.assertIn('proxy_pass http://127.0.0.1:19180;', after)
        self.assertIn('proxy_pass http://127.0.0.1:19191;', after)

    def test_existing_external_server_block_is_not_overwritten(self):
        self.config.write_text('events {}\nhttp { server { server_name aa.ytc-srv.org; } }\n')
        before = self.config.read_bytes()
        with patch.object(nginx.subprocess, 'run', side_effect=self.command):
            with self.assertRaisesRegex(ValueError, 'Solution:'):
                self.configure()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertFalse(self.managed.exists())

    def test_validation_failure_rolls_back_both_files(self):
        with patch.object(nginx.subprocess, 'run', side_effect=self.command):
            self.configure()
        before_main, before_site = self.config.read_bytes(), self.managed.read_bytes()

        def fail_check(args, **kwargs):
            if '-t' in args:
                raise subprocess.CalledProcessError(1, args)
            return self.command(args, **kwargs)

        with patch.object(nginx.subprocess, 'run', side_effect=fail_check):
            with self.assertRaises(subprocess.CalledProcessError):
                self.configure(site_port=19180)
        self.assertEqual(self.config.read_bytes(), before_main)
        self.assertEqual(self.managed.read_bytes(), before_site)

    def test_dry_run_does_not_create_files_or_reload(self):
        before = self.config.read_bytes()
        with patch.object(nginx.subprocess, 'run', side_effect=self.command):
            self.configure(dry_run=True)
        self.assertEqual(self.config.read_bytes(), before)
        self.assertFalse(self.managed.parent.exists())
        self.assertTrue(all('-T' in call for call in self.calls))

    def test_foreign_domain_and_credentials_rejected_before_nginx(self):
        with patch.object(nginx.subprocess, 'run') as command:
            for origin in ('https://outside.invalid', 'https://user:pass@aa.ytc-srv.org', 'https://aa.ytc-srv.org/path', 'https://aa.ytc-srv.org:8443'):
                with self.subTest(origin=origin), self.assertRaises(ValueError):
                    nginx.configure(origin, 'https://aaid.ytc-srv.org', 18080, 18091, str(self.config))
            command.assert_not_called()


if __name__ == '__main__':
    unittest.main()
