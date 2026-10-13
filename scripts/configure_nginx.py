#!/usr/bin/env python3
"""Install a managed host site without replacing existing Nginx configuration."""
import argparse
import datetime
import pathlib
import re
import shutil
import subprocess


def add_include(text, site):
    directive = f'include "{site}"; # antiattendance managed site'
    if directive in text:
        return text
    tokens = list(re.finditer(r'''#[^\n]*|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s{};]+|[{};]''', text))
    depth = 0
    for index, token in enumerate(tokens):
        value = token.group()
        if value.startswith('#'):
            continue
        if value == 'http' and depth == 0:
            following = next(t for t in tokens[index + 1:] if not t.group().startswith('#'))
            if following.group() == '{':
                position = following.end()
                return text[:position] + '\n    ' + directive + '\n' + text[position:]
        if value == '{':
            depth += 1
        elif value == '}':
            depth -= 1
    raise ValueError('No top-level http block found; use --nginx-conf for the global configuration.')


def configure(domain, port, config=None):
    args = ['nginx']
    if config:
        args += ['-c', str(pathlib.Path(config).resolve())]
    dump = subprocess.run(args + ['-T'], check=True, capture_output=True, text=True).stdout
    if not config:
        match = re.search(r'^# configuration file (.+):$', dump, re.M)
        if not match:
            raise ValueError('Cannot discover the global Nginx configuration; use --nginx-conf.')
        config = match.group(1)
        args += ['-c', config]
    main = pathlib.Path(config).resolve()
    # An exact include avoids relying on distribution-specific sites-enabled layouts.
    site = main.parent / 'antiattendance-managed' / 'sessions.conf'
    original = main.read_text()
    managed = f'include "{site}"; # antiattendance managed site' in original
    names = re.findall(r'\bserver_name\s+([^;]+);', re.sub(r'#[^\n]*', '', dump))
    if not managed and any(domain in names_list.split() for names_list in names):
        raise ValueError(f'{domain} already has a server block; refusing to create a conflicting site.')
    updated = add_include(original, site)
    old_site = site.read_bytes() if site.exists() else None
    stamp = datetime.datetime.now().strftime('%Y%m%d%H%M%S%f')
    backup = main.with_name(main.name + '.antiattendance-backup-' + stamp)
    shutil.copy2(main, backup)
    site.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    template = (pathlib.Path(__file__).resolve().parents[1] / 'nginx/host.conf.example').read_text()
    rendered = template.replace('aa.yellowteacrew.ru', domain).replace('127.0.0.1:8080', f'127.0.0.1:{port}')
    try:
        site.write_text(rendered)
        site.chmod(0o644)
        main.write_text(updated)
        subprocess.run(args + ['-t'], check=True)
        subprocess.run(args + ['-s', 'reload'], check=True)
    except BaseException:
        shutil.copy2(backup, main)
        if old_site is None:
            site.unlink(missing_ok=True)
        else:
            site.write_bytes(old_site)
        raise
    print(f'Host Nginx configured for {domain}; backup: {backup}')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--domain', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--config')
    options = parser.parse_args()
    configure(options.domain, options.port, options.config)
