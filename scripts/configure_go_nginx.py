"""Manage only the Go services' nginx include; preserve other sites and TLS."""
import argparse
import datetime
from pathlib import Path
import re
import shutil
import subprocess
import sys
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))
from configure_nginx import add_include


def domain(origin):
    parsed = urlsplit(origin)
    host = (parsed.hostname or '').lower()
    if (parsed.scheme not in ('http', 'https') or parsed.username or parsed.password
            or parsed.path not in ('', '/') or parsed.query or parsed.fragment
            or not re.fullmatch(r'(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+ytc-srv\.org', host)):
        raise ValueError('Host nginx requires a domain under *.ytc-srv.org. Use --no-host-nginx for localhost.')
    if parsed.port not in (None, 443 if parsed.scheme == 'https' else 80):
        raise ValueError('Automatic nginx configuration uses ports 80/443. Solution: use --no-host-nginx for a different public port.')
    return host


def server_spans(text):
    """Locate actual server blocks without counting braces in quotes/comments."""
    tokens = list(re.finditer(r'''#[^\n]*|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s{};]+|[{};]''', text))
    index = 0
    while index < len(tokens):
        token = tokens[index]
        if token.group() == 'server' and index + 1 < len(tokens) and tokens[index + 1].group() == '{':
            depth = 1
            end = index + 2
            while end < len(tokens) and depth:
                if tokens[end].group() == '{':
                    depth += 1
                elif tokens[end].group() == '}':
                    depth -= 1
                end += 1
            if depth:
                raise ValueError('Unclosed nginx server block.')
            yield token.start(), tokens[end - 1].end()
            index = end
        else:
            index += 1


def render_existing(text, targets):
    names = set()
    output = text
    spans = list(server_spans(text))
    changed = set()
    for start, end in reversed(spans):
        block = text[start:end]
        clean = re.sub(r'#[^\n]*', '', block)
        found = re.findall(r'\bserver_name\s+([^;]+);', clean)
        declared = {name for row in found for name in row.split()}
        names.update(declared)
        selected = declared.intersection(targets)
        if len(selected) > 1 or declared - set(targets):
            raise ValueError('Unexpected server_name in the managed include. Solution: check the file manually; unrelated sites are not replaced.')
        if selected:
            name = next(iter(selected))
            block, count = re.subn(r'\bproxy_pass\s+http://127\.0\.0\.1:[0-9]+;', f'proxy_pass http://127.0.0.1:{targets[name]};', block)
            if count:
                changed.add(name)
            output = output[:start] + block + output[end:]
    if names != set(targets) or changed != set(targets):
        raise ValueError('Managed include domains have changed. Solution: migrate domains/certificates manually; existing TLS settings are preserved.')
    return output


def configure(site_url, aaid_url, site_port, aaid_port, config=None, dry_run=False):
    targets = {domain(site_url): site_port, domain(aaid_url): aaid_port}
    if len(targets) != 2:
        raise ValueError('Site and AA ID must have different domains.')
    if any(not 1 <= port <= 65535 for port in targets.values()):
        raise ValueError('Invalid upstream port.')
    args = ['nginx']
    if config:
        args += ['-c', str(Path(config).resolve())]
    dump = subprocess.run(args + ['-T'], check=True, capture_output=True, text=True).stdout
    if not config:
        first = re.search(r'^# configuration file (.+):$', dump, re.M)
        if not first:
            raise ValueError('nginx.conf not found. Solution: use --nginx-config /path/nginx.conf.')
        config = first.group(1)
        args += ['-c', config]
    main = Path(config).resolve()
    managed = main.parent / 'antiattendance-go-managed' / 'services.conf'
    if managed.is_symlink() or managed.parent.is_symlink():
        raise ValueError('The managed include must not be a symbolic link.')
    for section in re.finditer(r'^# configuration file ([^\r\n]+):\n(.*?)(?=^# configuration file |\Z)', dump, re.M | re.S):
        if Path(section.group(1)).resolve() == managed:
            continue
        clean = re.sub(r'#[^\n]*', '', section.group(2))
        for declaration in re.findall(r'\bserver_name\s+([^;]+);', clean):
            conflict = set(declaration.split()).intersection(targets)
            if conflict:
                raise ValueError(f'Domain {", ".join(sorted(conflict))} is already configured in {section.group(1)}. Solution: update its upstream manually or use --no-host-nginx; conflicting blocks are not created.')
    original = main.read_bytes()
    old_site = managed.read_bytes() if managed.exists() else None
    updated = add_include(original.decode(), managed)
    if old_site is None:
        template = (Path(__file__).resolve().parents[1] / 'nginx/go-hosts.conf.example').read_text(encoding='utf-8')
        rendered = (template.replace('aa.ytc-srv.org', domain(site_url))
                    .replace('aaid.ytc-srv.org', domain(aaid_url))
                    .replace('127.0.0.1:18080', f'127.0.0.1:{site_port}')
                    .replace('127.0.0.1:18091', f'127.0.0.1:{aaid_port}'))
    else:
        rendered = render_existing(old_site.decode(), targets)
    if dry_run:
        print(f'Nginx: domains and include {managed} checked; files unchanged.')
        return
    # Each write has a backup; validate before reload, restore both files on failure.
    stamp = datetime.datetime.now().strftime('%Y%m%d%H%M%S%f')
    backup = main.with_name(main.name + '.aa-go-backup-' + stamp)
    shutil.copy2(main, backup)
    if old_site is not None:
        shutil.copy2(managed, managed.with_name(managed.name + '.backup-' + stamp))
    managed.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    try:
        managed.write_bytes(rendered.encode('utf-8'))
        managed.chmod(0o644)
        main.write_bytes(updated.encode('utf-8'))
        subprocess.run(args + ['-t'], check=True)
        subprocess.run(args + ['-s', 'reload'], check=True)
    except BaseException:
        main.write_bytes(original)
        if old_site is None:
            managed.unlink(missing_ok=True)
        else:
            managed.write_bytes(old_site)
        # A reload failure may have reached nginx; re-validate/reload the restored config.
        try:
            subprocess.run(args + ['-t'], check=True, capture_output=True)
            subprocess.run(args + ['-s', 'reload'], check=True, capture_output=True)
        except (OSError, subprocess.SubprocessError):
            pass
        raise
    print(f'Nginx configured: {", ".join(targets)}. Backup: {backup}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--site-url', required=True)
    parser.add_argument('--aaid-url', required=True)
    parser.add_argument('--site-port', type=int, required=True)
    parser.add_argument('--aaid-port', type=int, required=True)
    parser.add_argument('--config')
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args()
    try:
        configure(args.site_url, args.aaid_url, args.site_port, args.aaid_port, args.config, args.dry_run)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f'Nginx error: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
