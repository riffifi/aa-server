"""Read-only deployment checks. Run with Python 3 from the Compose directory."""
import concurrent.futures
import json
import os
from pathlib import Path
import shutil
import subprocess


PROBE = '''import json, time, urllib.request
samples = []
for _ in range(5):
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(URL, timeout=5) as response:
            response.read(1024)
            status = response.status
    except Exception as error:
        status = type(error).__name__
    samples.append({'status': status, 'ms': round((time.perf_counter()-started)*1000, 1)})
print(json.dumps(samples))
'''


def run(command):
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=40)
        return {'exit_code': result.returncode, 'output': result.stdout.strip(),
                'error': result.stderr.strip()}
    except (OSError, subprocess.TimeoutExpired) as error:
        return {'error': type(error).__name__}


def main():
    disk = shutil.disk_usage('.')
    report = {'host': {'cpu_count': os.cpu_count(),
                       'load_average': os.getloadavg() if hasattr(os, 'getloadavg') else None,
                       'disk_free_gb': round(disk.free / 2**30, 2)}}
    memory = Path('/proc/meminfo')
    if memory.exists():
        wanted = {'MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree', 'Dirty'}
        report['host']['memory'] = {line.split(':')[0]: line.split(':')[1].strip()
                                    for line in memory.read_text().splitlines()
                                    if line.split(':')[0] in wanted}
    base = ['docker', 'compose']
    jobs = {
        'containers': base + ['ps', '--format', 'json'],
        'resources': base + ['stats', '--no-stream', '--format', 'json'],
    }
    for service, port in (('sessions', 8080), ('aaid', 8090)):
        # Sample directly and through container networking, without credentials.
        for label, url in ((service + '_local', f'http://127.0.0.1:{port}/health'),
                           (service + '_network', f'http://{service}:{port}/health')):
            source = 'URL = ' + repr(url) + '\n' + PROBE
            jobs[label] = base + ['exec', '-T', 'sessions', 'python', '-c', source]
        source = 'URL = ' + repr(f'http://127.0.0.1:{port}/health') + '\n' + PROBE
        jobs[service + '_local'] = base + ['exec', '-T', service, 'python', '-c', source]
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as executor:
        pending = {name: executor.submit(run, command) for name, command in jobs.items()}
        report.update({name: future.result() for name, future in pending.items()})
    print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
