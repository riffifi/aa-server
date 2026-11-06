"""Check that the API reference names every in-scope route and has valid JSON examples."""
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]


def canonical(path):
    return re.sub(r'\{[^}]+\}', '{}', path)


def main():
    document = (ROOT / 'docs/API.md').read_text(encoding='utf-8')
    inventory = json.loads((ROOT / 'docs/go-route-coverage.json').read_text(encoding='utf-8'))
    mentions = set()
    for method, origin, path in re.findall(r'\b(GET|POST|PUT|DELETE|PATCH)\s+(?:(BASE|ID|PANEL))?(/[A-Za-z0-9_./{}-]*)', document):
        service = 'aaid' if origin == 'ID' or path.startswith(('/v1/', '/web/', '/.well-known/')) else 'sessions'
        mentions.add((service, method, canonical(path)))
    missing = []
    count = 0
    for route in inventory['routes']:
        if route.get('excluded_reason'):
            continue
        count += 1
        key = (route['service'], route['method'], canonical(route['path']))
        if key not in mentions:
            missing.append(' '.join((route['service'], route['method'], route['path'])))
    for path in ('/panel/api/debug/browser', '/panel/api/pass-snapshot'):
        if ('sessions', 'GET', path) not in mentions:
            missing.append('sessions GET ' + path)
    if missing:
        raise SystemExit('Undocumented API routes:\n' + '\n'.join(missing))
    examples = re.findall(r'~~~json\s*\n(.*?)\n~~~', document, re.S)
    for index, example in enumerate(examples, 1):
        try:
            json.loads(example)
        except ValueError as error:
            raise SystemExit(f'Invalid JSON example {index}: {error}') from error
    print(f'API reference: {count} legacy in-scope routes, 2 additional native routes and {len(examples)} JSON examples checked.')


if __name__ == '__main__':
    main()
