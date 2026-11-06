"""Read-only Docker traffic sampling; no login, restart, configuration or packet capture."""
import argparse
import json
import re
import subprocess
import time

def size(text):
    match = re.fullmatch(r'\s*([0-9.]+)\s*([kMGTPE]?i?B)\s*', text)
    if not match:
        raise ValueError('Unknown Docker traffic unit')
    number, unit = match.groups()
    exponent = {'B': 0, 'kB': 1, 'MB': 2, 'GB': 3, 'TB': 4, 'PB': 5, 'EB': 6,
                'KiB': 1, 'MiB': 2, 'GiB': 3, 'TiB': 4, 'PiB': 5, 'EiB': 6}[unit]
    return round(float(number) * (1024 if 'i' in unit else 1000) ** exponent)

def sample(containers):
    command = ['docker', 'stats', '--no-stream', '--format', '{{json .}}', *containers]
    result = subprocess.run(command, capture_output=True, text=True, timeout=20, check=True)
    records = {}
    for line in result.stdout.splitlines():
        item = json.loads(line)
        received, sent = item['NetIO'].split('/')
        records[item['ID']] = {'name': item['Name'], 'received': size(received), 'sent': size(sent),
                               'memory': item['MemUsage'], 'cpu': item['CPUPerc']}
    return records

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--seconds', type=int, default=30)
    parser.add_argument('containers', nargs='*')
    args = parser.parse_args()
    if not 1 <= args.seconds <= 300:
        parser.error('--seconds must be between 1 and 300')
    before = sample(args.containers)
    started = time.monotonic()
    time.sleep(args.seconds)
    after = sample(args.containers)
    elapsed = time.monotonic() - started
    output = []
    for identifier, item in after.items():
        old = before.get(identifier)
        received = item['received'] - old['received'] if old else None
        sent = item['sent'] - old['sent'] if old else None
        output.append({**item, 'received_delta': received, 'sent_delta': sent,
                       'received_bytes_per_second': round(received / elapsed, 1) if received is not None and received >= 0 else None,
                       'sent_bytes_per_second': round(sent / elapsed, 1) if sent is not None and sent >= 0 else None})
    print(json.dumps({'seconds': round(elapsed, 1), 'containers': output,
                      'note': 'Docker totals are rounded and cumulative; short samples may miss small transfers. Negative deltas or changed IDs indicate counter resets. No historical 200 GB attribution can be recovered from these counters alone.'}, ensure_ascii=False, indent=2))

if __name__ == '__main__':
    main()
