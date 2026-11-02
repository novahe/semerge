#!/usr/bin/env python3
"""Cold boot stage measurements, raw evidence, and before/after comparison.

All durations are milliseconds. See cold_start/README.md for boundaries.
Live runs import the existing bench environment; offline analysis needs stdlib only.
"""
import argparse
import csv
import hashlib
import json
import os
import re
import statistics
import subprocess
import sys
import time
from datetime import datetime, timedelta
from pathlib import Path

SCHEMA = 'coldstart-stages-v1'
REQUIRED = ('create_ms', 'node_ctl_ms', 'sandbox_ctl_ms', 'kernel_init_ms',
            'sandbox_init_ms', 'envd_ms', 'exec_ms')
DESCRIPTIONS = {
    'e2e_ms': '客户端 create 开始 → 首命令返回',
    'create_ms': '异步 create 调用 → SDK 收到响应；与 admission 重叠',
    'launch_wait_ms': 'create 返回 → 观测 running 并准备发首命令',
    'exec_ms': 'running 后调用 commands.run → 首命令返回；含 SDK/RPC/guest/流结束',
    'node_ctl_ms': 'admission + prepare + runner commit；服务端 elapsed，非 CPU 时间',
    'sandbox_ctl_ms': 'sandbox-ctl 首条可见日志 → CH started；仅可见启动准备段',
    'kernel_init_ms': 'CH started → Run /sbin/init；含 VM 早期引导，非纯内核执行时间',
    'sandbox_init_ms': 'Run /sbin/init → host 收到 app_started；含 guest/host 握手',
    'envd_ms': 'mandatory /init RPC 阶段；含连接/等待监听/重试，非 envd 进程启动时间',
    'console_tail_ms': '首条可见 console → Run /sbin/init；是 kernel_init 的子区间',
    'before_first_console_ms': 'CH started → 首条可见 console；不能等同于内核解压',
    'runtime_ready_ms': 'node-ctl 等待 runtime readiness；包含 ctl/VM/guest init',
    'server_total_ms': 'conductor starting_total_duration；独立服务端总窗口',
    'runtime_uncovered_ms': 'runtime_ready 减去 ctl/VM/guest 三个可见段；日志之外的区间',
    'client_uncovered_ms': 'e2e 减去 create/launch_wait/exec；客户端计时闭合差',
    'node_admission_ms': 'node-ctl 创建请求校验/受理，与客户端 create 重叠',
    'node_prepare_ms': 'node-ctl 启动资源/网络准备，排除 runtime readiness 等待',
    'node_runner_wait_ms': '等待可用 runner 分配',
    'node_runner_commit_ms': '绑定 runner、持久化、发布路由等接管提交',
    'node_runner_handoff_ms': 'runner 提交后交接收尾',
    'node_envd_init_ms': 'envd_ms 的别名，不能重复相加',
    'node_runtime_ready_ms': 'runtime_ready_ms 的别名，不能重复相加',
    'ctl_to_admit_ms': '首条 ctl 日志 → 资源控制器接纳日志，含此前准备',
    'ctl_admit_to_budget_ms': '接纳日志 → 初始 cold Budget 已预留日志',
    'ctl_budget_to_tap_ms': 'Budget 已预留 → 收到 TAP fd',
    'ctl_tap_to_args_ms': '收到 TAP fd → 打印 CH 参数',
    'ctl_args_to_ch_ms': '打印 CH 参数 → CH started',
    'ch_to_uffd0_ms': 'CH started → 首个 UFFD region 注册，仅里程碑，与 boot 并行',
    'ch_to_uffd1_ms': 'CH started → 追加 UFFD region 注册，仅里程碑，与 boot 并行',
    'init_to_guest_start_ms': '内核 Run /sbin/init → guest 首条 sandbox-init 日志',
    'guest_start_to_spec_ms': 'guest 首条 init 日志 → 收到 launch spec',
    'guest_spec_to_base_ms': '收到 launch spec → base mounts done',
    'guest_wait_disks_ms': 'base mounts done → 确认 vda/vdb 存在',
    'guest_disks_to_vda_ms': 'vda/vdb 存在 → vda EROFS mounted',
    'guest_vda_to_vdb_ms': 'vda mounted → vdb EXT4 mounted；不是独立 mount 函数计时',
    'guest_vdb_to_overlay_ms': 'vdb mounted → overlay assembled',
    'guest_overlay_to_sysroot_ms': 'overlay assembled → sysroot ready（payload 已绑定）',
    'guest_phase1b_ms': 'sysroot ready → switch-root/devpts/cgroup/run 完成',
    'guest_network_stdio_ms': 'phase1b 完成 → network/stdio 完成',
    'guest_app_fork_ms': 'network/stdio 完成 → app forked',
    'guest_app_notify_ms': 'guest app forked → host 收到 app_started',
    'host_hello_to_spec_ms': 'host 收到 guest hello → host 发 launch spec，包含在 guest 初始化内',
    'spec_delivery_ms': 'host 发 launch spec → guest 收到，包含在 guest 初始化内',
    'guest_partition_error_ms': 'sandbox_init 减去其连续子区间之和；闭合校验',
}
MARKERS = {
    'admit': ('sandbox-ctl', 'controller admit:'),
    'budget': ('sandbox-ctl', 'initial cold Budget reserved='),
    'tap': ('sandbox-ctl', 'tapfd: received tap fd'),
    'args': ('sandbox-ctl', 'CH args:'),
    'ch': ('sandbox-ctl', 'CH started pid='),
    'uffd0': ('sandbox-ctl', 'uffd handler: adopted CH uffd region #0'),
    'uffd1': ('sandbox-ctl', 'uffd handler: attached additional region'),
    'hello': ('sandbox-ctl', 'launch: hello received'),
    'spec_sent': ('sandbox-ctl', 'launch: sending launch spec'),
    'app_started': ('sandbox-ctl', 'launch: app_started'),
    'init': ('console', 'Run /sbin/init as init process'),
    'guest_start': ('console', 'sandbox-init starting'),
    'spec_received': ('console', 'runtime ready: launch spec received'),
    'base_done': ('console', 'phase1a: base mounts done'),
    'disks': ('console', 'phase1a: vda+vdb present'),
    'vda': ('console', 'erofs: (device vda): mounted'),
    'vdb': ('console', 'EXT4-fs (vdb): mounted'),
    'overlay': ('console', 'phase1a: overlay assembled'),
    'sysroot': ('console', 'phase1a: /sysroot ready'),
    'phase1b': ('console', 'phase1b: switch-root'),
    'phase2': ('console', 'phase2: network + stdio done'),
    'app_fork': ('console', 'phase2: app forked'),
}
# These are elapsed intervals between visible markers, not function CPU costs.
INTERVALS = [
    ('sandbox_ctl_ms', 'ctl_first', 'ch'),
    ('ctl_to_admit_ms', 'ctl_first', 'admit'),
    ('ctl_admit_to_budget_ms', 'admit', 'budget'),
    ('ctl_budget_to_tap_ms', 'budget', 'tap'),
    ('ctl_tap_to_args_ms', 'tap', 'args'),
    ('ctl_args_to_ch_ms', 'args', 'ch'),
    ('kernel_init_ms', 'ch', 'init'),
    ('before_first_console_ms', 'ch', 'console_first'),
    ('console_tail_ms', 'console_first', 'init'),
    ('ch_to_uffd0_ms', 'ch', 'uffd0'),
    ('ch_to_uffd1_ms', 'ch', 'uffd1'),
    ('sandbox_init_ms', 'init', 'app_started'),
    ('init_to_guest_start_ms', 'init', 'guest_start'),
    ('guest_start_to_spec_ms', 'guest_start', 'spec_received'),
    ('guest_spec_to_base_ms', 'spec_received', 'base_done'),
    ('guest_wait_disks_ms', 'base_done', 'disks'),
    ('guest_disks_to_vda_ms', 'disks', 'vda'),
    ('guest_vda_to_vdb_ms', 'vda', 'vdb'),
    ('guest_vdb_to_overlay_ms', 'vdb', 'overlay'),
    ('guest_overlay_to_sysroot_ms', 'overlay', 'sysroot'),
    ('guest_phase1b_ms', 'sysroot', 'phase1b'),
    ('guest_network_stdio_ms', 'phase1b', 'phase2'),
    ('guest_app_fork_ms', 'phase2', 'app_fork'),
    ('guest_app_notify_ms', 'app_fork', 'app_started'),
    ('host_hello_to_spec_ms', 'hello', 'spec_sent'),
    ('spec_delivery_ms', 'spec_sent', 'spec_received'),
]
for key, begin, end in INTERVALS:
    DESCRIPTIONS.setdefault(key, f'可见日志 {begin} → {end}；与上层区间包含/重叠')
GUEST_PARTS = [key for key, _, _ in INTERVALS
               if key.startswith('guest_') and key != 'guest_start_to_spec_ms']
GUEST_PARTS = ['init_to_guest_start_ms', 'guest_start_to_spec_ms'] + GUEST_PARTS
UNITS = {'ns': 1e-6, 'µs': .001, 'us': .001, 'ms': 1, 's': 1000}
DURATION = r'([\d.]+)(ns|µs|us|ms|s)(?=\s|$)'


def duration_ms(value, unit):
    return float(value) * UNITS[unit]


def read_jsonl(path):
    with open(path) as stream:
        return [json.loads(line) for line in stream if line.strip()]


def journal_for_sid(rows, sid):
    # A PID is scoped to a journal boot ID. Filter evidence before writing it.
    owners = {(row.get('_BOOT_ID'), str(row.get('_PID'))) for row in rows
              if row.get('SYSLOG_IDENTIFIER') == 'sandbox-ctl'
              and sid in str(row.get('MESSAGE', ''))}
    return sorted((row for row in rows
                   if (row.get('_BOOT_ID'), str(row.get('_PID'))) in owners
                   and row.get('SYSLOG_IDENTIFIER') in ('sandbox-ctl', 'console')
                   and row.get('__REALTIME_TIMESTAMP')),
                  key=lambda row: int(row['__REALTIME_TIMESTAMP']))


def parse_phases(lines, sid):
    phases = {}
    for line in lines:
        if not re.search(r'\bsid=' + re.escape(sid) + r'(?:\s|$)', line):
            continue
        match = re.search(r'phase=(\w+) duration=' + DURATION, line)
        if match:
            name, value, unit = match.groups()
            phases[name] = duration_ms(value, unit)
        match = re.search(r'starting_total_duration=' + DURATION, line)
        if match:
            phases['terminal_total'] = duration_ms(*match.groups())
    return phases


def extract(sample, rows, phases):
    result = {key: sample[key] for key in ('create_ms', 'launch_wait_ms', 'exec_ms', 'e2e_ms')
              if sample.get(key) is not None}
    if all(name in phases for name in ('admission_duration', 'prepare_duration', 'runner_commit_duration')):
        result['node_ctl_ms'] = sum(phases[name] for name in
                                   ('admission_duration', 'prepare_duration', 'runner_commit_duration'))
    for source, target in [('envd_init_duration', 'envd_ms'),
                           ('runtime_ready_duration', 'runtime_ready_ms'),
                           ('terminal_total', 'server_total_ms')]:
        if source in phases:
            result[target] = phases[source]
    for name, value in phases.items():
        if name.endswith('_duration'):
            result['node_' + name.removesuffix('_duration') + '_ms'] = value
    points = {}
    for ident, key in [('sandbox-ctl', 'ctl_first'), ('console', 'console_first')]:
        found = next((row for row in rows if row.get('SYSLOG_IDENTIFIER') == ident), None)
        if found:
            points[key] = int(found['__REALTIME_TIMESTAMP'])
    for key, (ident, marker) in MARKERS.items():
        found = next((row for row in rows if row.get('SYSLOG_IDENTIFIER') == ident
                      and marker in str(row.get('MESSAGE', ''))), None)
        if found:
            points[key] = int(found['__REALTIME_TIMESTAMP'])
    warnings = []
    for key, begin, end in INTERVALS:
        if begin in points and end in points:
            value = (points[end] - points[begin]) / 1000
            if value < 0:
                warnings.append(f'{key}: 日志时间顺序逆转 {value}ms，未计入统计')
            else:
                result[key] = value
    if all(name in result for name in ('runtime_ready_ms', 'sandbox_ctl_ms', 'kernel_init_ms', 'sandbox_init_ms')):
        result['runtime_uncovered_ms'] = result['runtime_ready_ms'] - sum(
            result[name] for name in ('sandbox_ctl_ms', 'kernel_init_ms', 'sandbox_init_ms'))
    if all(name in result for name in ('e2e_ms', 'create_ms', 'launch_wait_ms', 'exec_ms')):
        result['client_uncovered_ms'] = result['e2e_ms'] - sum(
            result[name] for name in ('create_ms', 'launch_wait_ms', 'exec_ms'))
    if all(name in result for name in GUEST_PARTS + ['sandbox_init_ms']):
        result['guest_partition_error_ms'] = result['sandbox_init_ms'] - sum(result[name] for name in GUEST_PARTS)
    counters = {}
    balloon = None
    for row in rows:
        if row.get('SYSLOG_IDENTIFIER') != 'sandbox-ctl':
            continue
        msg = str(row.get('MESSAGE', ''))
        match = re.fullmatch(r'\s+(\w+)\s+(\d+)\s*', msg)
        if match:
            counters[match.group(1)] = int(match.group(2))
        if 'CH args:' in msg:
            match = re.search(r'--balloon ([^ ]+)', msg)
            balloon = match.group(1) if match else 'absent'
    # Normalize the ambiguous percentile names explicitly; keep raw counters too.
    uffd_ms = {name + '_ms': value / 1e6 for name, value in counters.items()
               if name.endswith('_ns') or name in ('fault_queue_wait_p50', 'fault_queue_wait_p95', 'fault_queue_wait_p99')}
    missing = [key for key in REQUIRED if key not in result]
    timing_valid = abs(result.get('client_uncovered_ms', 0)) <= 1
    if not timing_valid:
        warnings.append('客户端分段不闭合；检查旧数据 exec_ms 是否包含 launch_wait')
    return {**sample, 'schema': SCHEMA, 'timing_valid': timing_valid,
            'stages_ms': result, 'points_realtime_us': points,
            'phases_ms': phases, 'uffd_raw': counters, 'uffd_time_ms': uffd_ms,
            'balloon_argument': balloon, 'missing_stages': missing, 'warnings': warnings}


def stats(values):
    values = sorted(values)
    if not values:
        return None
    def percentile(p):
        k = (len(values) - 1) * p
        lo = int(k)
        return values[lo] + (values[min(lo + 1, len(values) - 1)] - values[lo]) * (k - lo)
    return {'n': len(values), 'min': round(values[0], 3), 'p50': round(percentile(.5), 3),
            'p90': round(percentile(.9), 3), 'p95': round(percentile(.95), 3),
            'max': round(values[-1], 3), 'avg': round(statistics.mean(values), 3)}


def collect_journal(since):
    until = (datetime.now() + timedelta(seconds=1)).strftime('%Y-%m-%d %H:%M:%S.%f')
    response = subprocess.run(['journalctl', '--since', since, '--until', until, '--no-pager', '-o', 'json',
                               'SYSLOG_IDENTIFIER=sandbox-ctl', 'SYSLOG_IDENTIFIER=console'],
                              check=True, capture_output=True, text=True, timeout=30)
    return [json.loads(line) for line in response.stdout.splitlines() if line.strip()]


def cleanup(cold, sb):
    sid = sb.sandbox_id
    deleted, errors = cold.bench._delete_sandbox(sid, sb)
    if not deleted:
        return {'ok': False, 'errors': errors, 'directories_retained': True}
    deadline = time.monotonic() + 15
    last_status = None
    while time.monotonic() < deadline:
        try:
            last_status = cold.e2b_env.api_request('GET', f'/sandboxes/{sid}', timeout=3).status_code
            if last_status == 404:
                errors = cold.bench.cleanup_sandbox_directories(sid)
                return {'ok': not errors, 'http_status': 404, 'errors': errors}
        except Exception as exc:
            errors = [str(exc)]
        time.sleep(.1)
    return {'ok': False, 'http_status': last_status, 'errors': errors + ['delete 未收敛到 404，保留目录']}


def run_sample(cold, index, template, resource, cmd):
    sample = {'schema': SCHEMA, 'i': index, 'ok': False,
              'wall_start': datetime.now().strftime('%Y-%m-%d %H:%M:%S.%f')}
    sb = None
    try:
        kwargs = {'metadata': {'kuasar-sandbox.resource': json.dumps(resource)}} if resource else {}
        start = time.perf_counter()
        sb = cold.Sandbox.create(template=template, timeout=300, **kwargs)
        created = time.perf_counter()
        sample['sid'] = sb.sandbox_id
        _, state, polls = cold.wait_state_running(sb.sandbox_id)
        if state != 'running':
            raise RuntimeError(f'launch state={state}')
        exec_start = time.perf_counter()
        result = sb.commands.run(cmd, user='root', cwd='/', timeout=60)
        finished = time.perf_counter()
        sample.update(create_ms=(created-start)*1000, launch_wait_ms=(exec_start-created)*1000,
                      exec_ms=(finished-exec_start)*1000, e2e_ms=(finished-start)*1000,
                      launch_polls=polls, exit_code=result.exit_code)
        if result.exit_code != 0:
            raise RuntimeError(f'command exit {result.exit_code}: {result.stderr[:200]}')
        sample['ok'] = True
    except Exception as exc:
        sample['error'] = f'{type(exc).__name__}: {exc}'
    finally:
        if sb:
            sample['cleanup'] = cleanup(cold, sb)
            if not sample['cleanup']['ok']:
                sample['ok'] = False
    sample['wall_end'] = datetime.now().strftime('%Y-%m-%d %H:%M:%S.%f')
    return sample


def report(out, samples, metadata, previous=None):
    keys = list(dict.fromkeys(['e2e_ms', 'create_ms', 'launch_wait_ms', 'node_ctl_ms', 'sandbox_ctl_ms',
                             'kernel_init_ms', 'sandbox_init_ms', 'envd_ms', 'exec_ms'] +
                            sorted({key for sample in samples for key in sample['stages_ms']})))
    good = [sample for sample in samples if sample['ok'] and sample.get('timing_valid',True)]
    summary = {key: stats([sample['stages_ms'][key] for sample in good if key in sample['stages_ms']]) for key in keys}
    incomplete = [sample['i'] for sample in samples if sample['missing_stages']]
    invalid = [sample['i'] for sample in samples if not sample.get('timing_valid',True)]
    uffd_keys = sorted({key for sample in good for key in sample['uffd_time_ms']})
    result = {'schema': SCHEMA, 'units': 'ms', **metadata,
              'ok': sum(sample['ok'] for sample in samples),
              'fail': sum(not sample['ok'] for sample in samples),
              'incomplete_samples': incomplete, 'invalid_timing_samples': invalid,
              'stages_ms': summary, 'definitions': DESCRIPTIONS,
              'uffd_lifetime_time_ms': {key: stats([sample['uffd_time_ms'][key] for sample in good
                                                   if key in sample['uffd_time_ms']]) for key in uffd_keys},
              'unmeasured': ['纯内核初始化/解压', 'envd 进程自身启动', 'REMOVE/锁等待/guest 文件页等待的完整独占分摊'],
              'notes': ['各段 p50 不可相加；create/admission 重叠，runtime_ready 包含 ctl/VM/guest',
                        '日志阶段为可见事件间隔，非函数 CPU；console_tail 包含在 kernel_init 内',
                        'UFFD 时间为 ns 归一到 ms，且是累计/分位数，不是独占阶段']}
    comparison = {}
    if previous:
        if previous.get('schema') != SCHEMA:
            raise ValueError('--compare 只接受 coldstart-stages-v1 result.json')
        for field in ('template', 'cmd'):
            if previous.get(field) != metadata.get(field):
                raise ValueError(f'对照 {field} 不同，拒绝生成可误导的性能比较')
        for key, current in summary.items():
            old = previous.get('stages_ms', {}).get(key)
            if current and old:
                delta = current['p50'] - old['p50']
                comparison[key] = {'before_p50': old['p50'], 'after_p50': current['p50'],
                                   'delta_ms': round(delta, 3),
                                   'delta_percent': round(delta/old['p50']*100, 2) if old['p50'] else None,
                                   'before_n': old['n'], 'after_n': current['n']}
        result['comparison'] = comparison
    (out/'samples.jsonl').write_text(''.join(json.dumps(sample, ensure_ascii=False)+'\n' for sample in samples))
    (out/'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
    with (out/'stages.csv').open('w') as stream:
        writer = csv.writer(stream)
        writer.writerow(['stage', 'n', 'min_ms', 'p50_ms', 'p90_ms', 'p95_ms', 'max_ms', 'avg_ms', 'definition'])
        for key, value in summary.items():
            writer.writerow([key] + ([value[k] for k in ('n','min','p50','p90','p95','max','avg')] if value else [0,'','','','','','']) + [DESCRIPTIONS.get(key,'服务端埋点')])
    lines = [f'# Coldstart stages: {metadata["label"]}', '', '全部时长单位 ms；缺测为 —，不当作零。', '',
             '| 阶段 | n | min | p50 | p90 | p95 | max | avg |', '|---|---:|---:|---:|---:|---:|---:|---:|']
    for key, value in summary.items():
        lines.append(f'| {key} | {value["n"]} | {value["min"]} | {value["p50"]} | {value["p90"]} | {value["p95"]} | {value["max"]} | {value["avg"]} |' if value else f'| {key} | 0 | — | — | — | — | — | — |')
    if comparison:
        lines += ['', '变化率负数表示更快；比较样本数见 JSON。', '', '| 阶段 | before p50 | after p50 | Δms | Δ% |', '|---|---:|---:|---:|---:|']
        lines += [f'| {key} | {v["before_p50"]} | {v["after_p50"]} | {v["delta_ms"]} | {v["delta_percent"]} |' for key,v in comparison.items()]
    lines += ['', '## 口径', ''] + [f'- `{key}`：{value}' for key,value in DESCRIPTIONS.items()]
    lines += ['', '## 未测到的区间', ''] + ['- '+value for value in result['unmeasured'] + result['notes']]
    if uffd_keys:
        lines += ['', '## UFFD 诊断量', '', '以下为生命周期累计计时或每个沙箱的分位数再汇总，不能作为互斥阶段相加。', '',
                  '| 计数器（已转 ms） | n | p50 | 平均 |', '|---|---:|---:|---:|']
        for key, value in result['uffd_lifetime_time_ms'].items():
            lines.append(f'| {key} | {value["n"]} | {value["p50"]} | {value["avg"]} |')
    (out/'report.md').write_text('\n'.join(lines)+'\n')
    print('\n'.join(lines[:15]))
    print(f'结果：{out}/report.md；失败={result["fail"]}，缺分段={incomplete}，计时不闭合={invalid}')
    return 1 if result['fail'] else (2 if incomplete or invalid else 0)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('-n', '--samples', type=int, default=10)
    parser.add_argument('--template')
    parser.add_argument('--cmd', help='首命令，live 默认 true，offline 默认沿用源 result.json')
    parser.add_argument('--label', default='baseline')
    parser.add_argument('--startup-memory')
    parser.add_argument('--allocatable-memory')
    parser.add_argument('--capacity-memory')
    parser.add_argument('--out', type=Path)
    parser.add_argument('--compare', type=Path)
    parser.add_argument('--conductor-log', type=Path, help='默认 Nova conductor.log；offline 优先使用样本归档阶段')
    parser.add_argument('--from-samples', type=Path, help='离线重算已有 samples.jsonl')
    parser.add_argument('--journal-json', type=Path, help='离线原始 journal JSONL')
    parser.add_argument('--phase-json', type=Path, help='可选归档 [{sid,phase,ms}]；否则读取 conductor.log')
    parser.add_argument('--legacy-exec-includes-launch-wait', action='store_true', help='旧 bench 的 exec_ms 包含 launch_wait，显式减去')
    args = parser.parse_args(argv)
    if args.samples < 1:
        parser.error('-n 必须大于零')
    if args.from_samples and not args.journal_json:
        parser.error('离线重算必须提供 --journal-json；不从截断的文本日志猜测')
    if args.from_samples and any((args.startup_memory,args.allocatable_memory,args.capacity_memory)):
        parser.error('离线重算不能修改已发生实验的资源配置')
    out = args.out or Path(__file__).resolve().parent/'results'/datetime.now().strftime('coldstart-stages-%Y%m%d-%H%M%S-%f')
    if args.from_samples and args.from_samples.resolve().parent == out.resolve():
        parser.error('--out 不得覆盖源样本目录')
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    resource = {key: {'memory': value} for key, value in
                [('startup', args.startup_memory), ('allocatable', args.allocatable_memory), ('capacity', args.capacity_memory)] if value}
    phase_archive = json.loads(args.phase_json.read_text()) if args.phase_json else None
    previous = json.loads(args.compare.read_text()) if args.compare else None
    template = args.template
    source_rows = read_jsonl(args.journal_json) if args.journal_json else None
    samples = []
    if args.from_samples:
        source_samples = read_jsonl(args.from_samples)
        for sample in source_samples:
            if args.legacy_exec_includes_launch_wait and 'exec_ms' in sample:
                if sample.get('schema') == SCHEMA:
                    parser.error('本脚本的 samples 已是正确 exec 口径，不能再次减 launch_wait')
                sample['exec_ms'] -= sample.get('launch_wait_ms', 0)
        result_path = args.from_samples.parent/'result.json'
        if result_path.exists():
            source_result = json.loads(result_path.read_text())
            template = template or source_result.get('template')
            args.cmd = args.cmd or source_result.get('cmd')
            resource = source_result.get('resource_override', {})
    else:
        # The environment/SDK helpers remain in the semerge repository root.
        sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
        import bench_coldstart_envd as cold
        # requests prefers lowercase no_proxy. Bypass only this tenant's domain.
        domain = os.environ['E2B_DOMAIN']
        for key in ('no_proxy', 'NO_PROXY'):
            os.environ[key] = ','.join(filter(None, [os.environ.get(key,''), domain, '.'+domain]))
        template = cold.resolve_template(template or cold.DEFAULT_MANIFEST_KEY)
        if '-img-' not in template:
            parser.error('当前分段口径用于 img 冷启动；不把 snp 恢复冒充内核初始化')
        source_samples = None
    args.cmd = args.cmd or 'true'
    if previous:
        if previous.get('schema') != SCHEMA:
            parser.error('--compare 需要同 schema 的 result.json')
        for field,current in [('template',template),('cmd',args.cmd)]:
            if previous.get(field) != current:
                parser.error(f'对照 {field} 不同，拒绝生成比较')
    count = len(source_samples) if source_samples is not None else args.samples
    for index in range(count):
        sample = source_samples[index] if source_samples is not None else run_sample(cold,index,template,resource,args.cmd)
        with (out/'attempts.jsonl').open('a') as stream:
            stream.write(json.dumps(sample,ensure_ascii=False)+'\n')
        rows, phases = [], {}
        if sample.get('sid'):
            for attempt in range(4 if source_samples is None else 1):
                try:
                    rows = journal_for_sid(source_rows if source_rows is not None else collect_journal(sample['wall_start']),sample['sid'])
                except (OSError, subprocess.SubprocessError) as exc:
                    sample.setdefault('evidence_errors',[]).append(f'journal: {type(exc).__name__}: {exc}')
                try:
                    if phase_archive is not None:
                        phases = {entry['phase']: entry['ms'] for entry in phase_archive if entry['sid']==sample['sid']}
                        old_total = (sample.get('server') or {}).get('phases',{}).get('terminal_total')
                        if old_total is not None:
                            phases.setdefault('terminal_total',old_total)
                    elif source_samples is not None and 'phases_ms' in sample and args.conductor_log is None:
                        phases = dict(sample['phases_ms'])
                    else:
                        log_path = args.conductor_log or Path('/root/sandbox/nova-work/var/lib/logs/conductor.log')
                        phases = parse_phases(log_path.read_text().splitlines(),sample['sid'])
                except OSError as exc:
                    sample.setdefault('evidence_errors',[]).append(f'conductor: {type(exc).__name__}: {exc}')
                measured = extract(sample,rows,phases)
                if not measured['missing_stages'] or source_samples is not None:
                    break
                time.sleep(.5)
            (out/f'journal-{index}.jsonl').write_text(''.join(json.dumps(row,ensure_ascii=False)+'\n' for row in rows))
        measured = extract(sample,rows,phases)
        samples.append(measured)
        # Persist progress before the next potentially failing create.
        (out/'samples.jsonl').write_text(''.join(json.dumps(item,ensure_ascii=False)+'\n' for item in samples))
        print(f'[{index+1}/{count}] ok={sample["ok"]} exec={measured["stages_ms"].get("exec_ms")}ms missing={measured["missing_stages"]}',flush=True)
    metadata = {'label': args.label, 'template': template, 'cmd': args.cmd, 'resource_override': resource,
                'sample_count': count, 'collected_at': datetime.now().astimezone().isoformat(),
                'script_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                'source_samples': str(args.from_samples) if args.from_samples else None}
    return report(out,samples,metadata,previous)


if __name__ == '__main__':
    sys.exit(main())
