#!/usr/bin/env python3
"""Host-only Docker cgroup collector. No listening socket, no third-party packages.

Only sanitized counters are published; Docker credentials/configuration never leave
this process. Run as a systemd service, never mount docker.sock in the web app.
"""
import argparse
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import sys
import time
from urllib.parse import quote

SERVICES = frozenset(('sessions', 'aaid', 'postgres', 'nginx', 'initialize'))
COUNTERS = ('rx_bytes', 'tx_bytes', 'disk_read_bytes', 'disk_write_bytes')
MAX_JSON = 16 * 1024 * 1024


def atomic_json(path, data):
    temporary = path.with_suffix('.tmp')
    with temporary.open('w', encoding='utf-8') as stream:
        os.fchmod(stream.fileno(), 0o644)
        json.dump(data, stream, separators=(',', ':'), allow_nan=False)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    if os.name == 'posix':
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


def read_json(path, default):
    try:
        with path.open('rb') as stream:
            data = stream.read(MAX_JSON + 1)
        if len(data) > MAX_JSON:
            raise ValueError('metrics state exceeds size limit')
        return json.loads(data)
    except FileNotFoundError:
        return default


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=5)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Docker:
    def __init__(self, path):
        self.path = path

    def get(self, path):
        connection = UnixHTTP(self.path)
        try:
            connection.request('GET', path)
            response = connection.getresponse()
            body = response.read(MAX_JSON + 1)
            if response.status != 200 or len(body) > MAX_JSON:
                raise RuntimeError('Docker metrics request failed')
            return json.loads(body)
        finally:
            connection.close()

    def containers(self, project):
        filters = quote(json.dumps({'label': ['com.docker.compose.project=' + project]}))
        return self.get('/containers/json?all=true&filters=' + filters)

    def stats(self, container):
        return self.get('/containers/' + quote(container, safe='') + '/stats?stream=false&one-shot=true')


def counters(stats):
    networks = stats.get('networks', {}).values()
    result = {key: sum(max(0, int(net.get(key, 0))) for net in networks)
              for key in ('rx_bytes', 'tx_bytes')}
    for direction in ('read', 'write'):
        result['disk_' + direction + '_bytes'] = sum(
            max(0, int(row.get('value', 0)))
            for row in stats.get('blkio_stats', {}).get('io_service_bytes_recursive') or []
            if row.get('op', '').lower() == direction)
    return result


class Collector:
    def __init__(self, directory, project, docker):
        self.directory = Path(directory)
        self.project = project
        self.docker = docker
        self.state = read_json(self.directory / 'state.json', {
            'version': 1, 'project': project, 'since': time.time(),
            'containers': {}, 'totals': {}})
        if self.state.get('version') != 1 or self.state.get('project') != project:
            raise ValueError('metrics state belongs to another project/version; use a separate directory')
        self.history = read_json(self.directory / 'history.json', [])

    def sample(self, now=None):
        now = time.time() if now is None else now
        components = []
        errors = 0
        # Docker stats account for every process in a container, including browser
        # children, rather than just the Go parent process.
        seen = set()
        for container in self.docker.containers(self.project):
            labels = container.get('Labels') or {}
            service = labels.get('com.docker.compose.service')
            if labels.get('com.docker.compose.project') != self.project or service not in SERVICES:
                continue
            cid = container['Id']
            seen.add(cid)
            item = {'service': service, 'running': container.get('State') == 'running'}
            totals = self.state['totals'].setdefault(service, {key: 0 for key in COUNTERS})
            if item['running']:
                try:
                    stats = self.docker.stats(cid)
                except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                    errors += 1
                    item['unavailable'] = True
                    item.update(totals)
                    components.append(item)
                    continue
                current = counters(stats)
                previous = self.state['containers'].get(cid)
                elapsed = now - previous['time'] if previous else 0
                delta = {key: max(0, current[key] - previous[key])
                         if previous and current[key] >= previous[key] else current[key]
                         for key in COUNTERS}
                for key in COUNTERS:
                    totals[key] += delta[key]
                    item[key + '_per_second'] = delta[key] / elapsed if elapsed > 0 else None
                cpu = stats.get('cpu_stats', {}).get('cpu_usage', {}).get('total_usage', 0)
                item['cpu_percent'] = max(0, cpu - previous.get('cpu', cpu)) / (elapsed * 1e7) if elapsed > 0 else None
                memory = stats.get('memory_stats', {})
                cache = memory.get('stats', {}).get('inactive_file', memory.get('stats', {}).get('total_inactive_file', 0))
                item['memory_bytes'] = max(0, memory.get('usage', 0) - cache)
                item['memory_limit_bytes'] = memory.get('limit', 0)
                item['processes'] = stats.get('pids_stats', {}).get('current', 0)
                self.state['containers'][cid] = dict(current, time=now, cpu=cpu)
            item.update(totals)
            components.append(item)
        # Retain seen IDs for a week: stopped containers can restart without
        # double-counting their counters. Bound state even under repeated deploys.
        known = self.state['containers']
        self.state['containers'] = dict(sorted(
            ((cid, row) for cid, row in known.items() if cid in seen or now - row['time'] < 7 * 86400),
            key=lambda pair: pair[1]['time'], reverse=True)[:2048])
        grouped = {}
        gauges = ('cpu_percent', 'memory_bytes', 'memory_limit_bytes', 'processes') + tuple(key + '_per_second' for key in COUNTERS)
        for item in components:
            row = grouped.setdefault(item['service'], {'service': item['service'], 'running': False, 'containers': 0})
            row['containers'] += 1
            row['running'] = row['running'] or item['running']
            if item.get('unavailable'):
                row['unavailable'] = True
            for key in gauges:
                if item.get(key) is not None:
                    row[key] = row.get(key, 0) + item[key]
        components = sorted(grouped.values(), key=lambda row: row['service'])
        for row in components:
            row.update(self.state['totals'][row['service']])
        total = {key: sum(row.get(key, 0) for row in self.state['totals'].values()) for key in COUNTERS}
        for key in ('cpu_percent', 'memory_bytes', 'processes') + tuple(key + '_per_second' for key in COUNTERS):
            values = [row[key] for row in components if row.get(key) is not None]
            total[key] = sum(values) if values else None
        disk = os.statvfs(self.directory)
        filesystem = {'total_bytes': disk.f_blocks * disk.f_frsize, 'available_bytes': disk.f_bavail * disk.f_frsize}
        # Minute history is written separately, not rewritten twice every 10s.
        # This keeps monitoring from generating gigabytes of disk writes daily.
        if not self.history or now - self.history[-1]['time'] >= 60:
            point = {'time': now}
            for key in ('cpu_percent', 'memory_bytes') + tuple(key + '_per_second' for key in COUNTERS):
                value = total.get(key)
                point[key] = round(value, 2) if value is not None else None
            self.history.append(point)
            self.history = [row for row in self.history[-1440:] if now - row['time'] <= 86400]
            atomic_json(self.directory / 'history.json', self.history)
        snapshot = {'version': 1, 'project': self.project, 'collected_at': now,
                    'since': self.state['since'], 'errors': errors, 'components': components,
                    'total': total, 'filesystem': filesystem,
                    'network_scope': 'container_interfaces_including_internal_traffic'}
        # Persist baselines before publishing totals. Atomic files prevent partial
        # reads; the next sample recovers after a publication failure.
        atomic_json(self.directory / 'state.json', self.state)
        atomic_json(self.directory / 'snapshot.json', snapshot)
        return dict(snapshot, history=self.history)


def main():
    import fcntl  # Linux host utility; helpers can also be tested on Windows.
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', default='antiattendance-go')
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--socket', default='/var/run/docker.sock')
    parser.add_argument('--interval', type=int, default=10)
    parser.add_argument('--once', action='store_true')
    args = parser.parse_args()
    if not 5 <= args.interval <= 300:
        parser.error('--interval must be between 5 and 300 seconds')
    args.directory.mkdir(parents=True, exist_ok=True, mode=0o755)
    # Prevent two collectors from corrupting durable totals.
    with (args.directory / '.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        collector = Collector(args.directory, args.project, Docker(args.socket))
        stopping = False

        def stop(*_):
            nonlocal stopping
            stopping = True

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        while not stopping:
            start = time.monotonic()
            try:
                collector.sample()
            except (OSError, ValueError, RuntimeError, http.client.HTTPException) as error:
                # Do not log Docker responses or container configuration.
                print('Metrics unavailable (' + type(error).__name__ + '); check Docker and output directory permissions.', file=sys.stderr, flush=True)
                if args.once:
                    return 1
            if args.once:
                break
            while not stopping and time.monotonic() - start < args.interval:
                time.sleep(max(0, min(1, args.interval - (time.monotonic() - start))))
    return 0


if __name__ == '__main__':
    sys.exit(main())
