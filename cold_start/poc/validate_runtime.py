#!/usr/bin/env python3
"""Verify deployed binaries, balloon convergence, and live snapshot continuity."""
from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shlex
import socket
import sys
import time

RESOURCE = {
    'capacity': {'memory': '8GiB'},
    'allocatable': {'memory': '256MiB'},
    'startup': {'memory': '1GiB'},
}


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=5)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def vm_info(run_root, sid):
    connection = UnixHTTP(run_root / 'sandboxes' / sid / 'ch.sock')
    try:
        connection.request('GET', '/api/v1/vm.info')
        response = connection.getresponse()
        body = response.read()
        if response.status != 200:
            raise RuntimeError(f'vm.info HTTP {response.status}')
        value = json.loads(body)
        return {
            'state': value.get('state'),
            'memory_actual_size': value.get('memory_actual_size'),
            'balloon_target': (value.get('config', {}).get('balloon') or {}).get('size'),
        }
    finally:
        connection.close()


def identity(sid, expected):
    table = {}
    for proc in Path('/proc').iterdir():
        if not proc.name.isdigit():
            continue
        try:
            table[int(proc.name)] = {
                'ppid': int((proc / 'stat').read_text().rsplit(')', 1)[1].split()[1]),
                'exe': os.readlink(proc / 'exe'),
                'cmd': (proc / 'cmdline').read_bytes().replace(b'\0', b' ').decode(errors='replace'),
            }
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
    descendants = {
        pid for pid, item in table.items()
        if sid in item['cmd'] and 'sandbox-ctl' in item['exe']
    }
    for _ in range(len(table)):
        added = {pid for pid, item in table.items() if item['ppid'] in descendants} - descendants
        if not added:
            break
        descendants.update(added)
    result = []
    for pid in sorted(descendants):
        try:
            digest = hashlib.sha256(Path('/proc', str(pid), 'exe').read_bytes()).hexdigest()
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
        item = {'pid': pid, 'exe': table[pid]['exe'], 'sha256': digest}
        try:
            item['smaps_rollup_kib'] = {
                parts[0].rstrip(':'): int(parts[1])
                for line in Path('/proc', str(pid), 'smaps_rollup').read_text().splitlines()[1:]
                if len(parts := line.split()) == 3 and parts[2] == 'kB'
            }
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            pass
        result.append(item)
    if not result:
        raise RuntimeError('could not find this sandbox runtime in local /proc')
    for role, path in expected.items():
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        if not any(item['sha256'] == digest for item in result):
            raise RuntimeError(f'running {role} does not match {path}')
    return result


def command(sb, cmd, timeout=60):
    result = sb.commands.run(cmd, user='root', cwd='/', timeout=timeout)
    if result.exit_code:
        raise RuntimeError(f'guest command exit {result.exit_code}: {result.stderr[:200]}')
    return result.stdout.strip()


def memory_server(sb):
    code = """const fs=require('fs'),crypto=require('crypto'),http=require('http');
const data=Buffer.alloc(32*1024*1024,0x5a),disk=Buffer.alloc(8*1024*1024,0x33);
fs.writeFileSync('/tmp/balloon-poc-data',disk);let fd=fs.openSync('/tmp/balloon-poc-data','r+');fs.fsyncSync(fd);fs.closeSync(fd);
const token=crypto.randomBytes(16).toString('hex');let ticks=0;setInterval(()=>ticks++,100);
http.createServer((req,res)=>{res.end(JSON.stringify({pid:process.pid,token,ticks,sha256:crypto.createHash('sha256').update(data).digest('hex')}));}).listen(49123,'127.0.0.1');
"""
    handle = sb.commands.run('node -e ' + shlex.quote(code), background=True, user='root', cwd='/', timeout=0)
    client = "const http=require('http');http.get('http://127.0.0.1:49123',r=>{let s='';r.on('data',x=>s+=x);r.on('end',()=>console.log(s));}).on('error',e=>{console.error(e.message);process.exit(1)});"
    query = 'node -e ' + shlex.quote(client)
    for _ in range(40):
        try:
            return handle, json.loads(command(sb, query)), query
        except Exception:
            time.sleep(.1)
    raise RuntimeError('memory server did not become ready; template requires node')


def snapshot(c, sb, cycles, row):
    handle, before, query = memory_server(sb)
    expected = hashlib.sha256(bytes([0x5a]) * (32 * 1024 * 1024)).hexdigest()
    disk = command(sb, 'sha256sum /tmp/balloon-poc-data').split()[0]
    if before['sha256'] != expected or disk != hashlib.sha256(bytes([0x33]) * (8 * 1024 * 1024)).hexdigest():
        raise RuntimeError('initial payload hash mismatch')
    row.update(before=before, disk_sha256=disk, cycles=[])
    for cycle in range(cycles):
        item = {'i': cycle}
        row['cycles'].append(item)
        response = c.e2b_env.api_request('POST', f'/sandboxes/{sb.sandbox_id}/pause', timeout=1800)
        item['pause_http'] = response.status_code
        if response.status_code != 204:
            raise RuntimeError(f'pause HTTP {response.status_code}')
        state = c.e2b_env.api_request('GET', f'/sandboxes/{sb.sandbox_id}', timeout=10).json()['state']
        item['paused_state'] = state
        if state != 'paused':
            raise RuntimeError(f'pause state: {state}')
        response = c.e2b_env.api_request('POST', f'/sandboxes/{sb.sandbox_id}/connect', json={'timeout': 600}, timeout=1800)
        item['resume_http'] = response.status_code
        if response.status_code not in (200, 201, 204):
            raise RuntimeError(f'resume HTTP {response.status_code}')
        _, state, _ = c.wait_state_running(sb.sandbox_id)
        if state != 'running':
            raise RuntimeError(f'resume state: {state}')
        after = json.loads(command(sb, query))
        item['after'] = after
        if after['pid'] != before['pid'] or after['token'] != before['token']:
            raise RuntimeError('snapshot did not preserve the same process')
        if after['sha256'] != expected or after['ticks'] <= before['ticks']:
            raise RuntimeError('snapshot memory or liveness check failed')
        item['disk_sha256'] = command(sb, 'sha256sum /tmp/balloon-poc-data').split()[0]
        if item['disk_sha256'] != disk:
            raise RuntimeError('snapshot disk hash mismatch')
        before = after
    return row


def cleanup(c, sb):
    accepted, errors = c.bench._delete_sandbox(sb.sandbox_id, sb)
    row = {'accepted': accepted, 'errors': errors}
    for _ in range(100):
        status = c.e2b_env.api_request('GET', f'/sandboxes/{sb.sandbox_id}', timeout=5).status_code
        if status == 404:
            break
        time.sleep(.1)
    row['http_status'] = status
    if status == 404:
        row['directories'] = c.bench.cleanup_sandbox_directories(sb.sandbox_id)
    row['ok'] = status == 404 and not errors
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('identity', 'convergence', 'snapshot'))
    parser.add_argument('--out', required=True, type=Path, help='new JSON evidence file')
    parser.add_argument('--label', default='candidate')
    parser.add_argument('--run-root', type=Path, default=Path('/run/sandbox-' + os.environ.get('E2B_NAMESPACE', 'nova')))
    parser.add_argument('--template', help='img template; default follows cold_start/bench.py')
    parser.add_argument('--expect-sandbox-ctl', type=Path)
    parser.add_argument('--expect-ch', type=Path)
    parser.add_argument('--cycles', type=int, default=3)
    parser.add_argument('--timeout', type=float, default=90)
    args = parser.parse_args()
    if args.cycles < 1 or args.timeout <= 0:
        parser.error('cycles and timeout must be positive')
    if args.out.exists():
        parser.error('output already exists; choose a new file')
    args.out.parent.mkdir(parents=True, exist_ok=True)
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
    import bench_coldstart_envd as c

    expected = {role: path for role, path in [('sandbox-ctl', args.expect_sandbox_ctl), ('CH', args.expect_ch)] if path}
    row = {'label': args.label, 'mode': args.mode, 'resource': RESOURCE, 'ok': False}
    sb = None
    try:
        template = c.resolve_template(args.template or c.DEFAULT_MANIFEST_KEY)
        row['template'] = template
        start = time.perf_counter()
        sb = c.Sandbox.create(template=template, timeout=600, metadata={'kuasar-sandbox.resource': json.dumps(RESOURCE)})
        row['sid'] = sb.sandbox_id
        if args.mode == 'convergence':
            timeline = row['timeline'] = []
            running = None
            while time.perf_counter() < start + args.timeout:
                try:
                    value = vm_info(args.run_root, sb.sandbox_id)
                except (OSError, RuntimeError, json.JSONDecodeError):
                    time.sleep(.02)
                    continue
                now = time.perf_counter()
                value['from_create_ms'] = round((now - start) * 1000, 3)
                timeline.append(value)
                if value['state'] == 'Running' and running is None:
                    running = now
                if value['state'] == 'Running' and value['memory_actual_size'] <= 1 << 30:
                    if value['balloon_target'] != 7 << 30:
                        raise RuntimeError('convergence used a different balloon target than 7GiB')
                    row['converged_ms'] = round((now - start) * 1000, 3)
                    row['running_to_convergence_ms'] = round((now - running) * 1000, 3)
                    row['final'] = value
                    break
                time.sleep(.02)
            else:
                raise RuntimeError('actual memory did not converge within timeout')
        else:
            _, state, _ = c.wait_state_running(sb.sandbox_id)
            if state != 'running':
                raise RuntimeError(f'launch state: {state}')
        row['processes'] = identity(sb.sandbox_id, expected)
        command(sb, 'true')
        if args.mode == 'snapshot':
            row['snapshot'] = {}
            snapshot(c, sb, args.cycles, row['snapshot'])
        row['ok'] = True
    except Exception as exc:
        row['error'] = repr(exc)
    finally:
        if sb:
            try:
                row['cleanup'] = cleanup(c, sb)
                row['ok'] = row['ok'] and row['cleanup']['ok']
            except Exception as exc:
                row['cleanup'] = {'ok': False, 'error': repr(exc)}
                row['ok'] = False
        args.out.write_text(json.dumps(row, indent=2) + '\n')
        print(json.dumps({key: value for key, value in row.items() if key != 'timeline'}, ensure_ascii=False))
    return 0 if row['ok'] else 1


if __name__ == '__main__':
    sys.exit(main())
