#!/usr/bin/env python3
"""Recompute stages from an existing bench directory, without creating sandboxes.

Usage: python3 split_logs.py --input results/baseline --out results/recomputed
Raw legacy inputs: --samples FILE --journal-json FILE --conductor-log FILE
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

from stages import main as stage_main


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument('--input', type=Path, help='bench 输出目录，含 samples 和原始 journal JSONL')
    source.add_argument('--samples', type=Path, help='旧 samples.jsonl；同时传 --journal-json')
    parser.add_argument('--journal-json', type=Path)
    parser.add_argument('--conductor-log', type=Path)
    parser.add_argument('--phase-json', type=Path)
    parser.add_argument('--legacy-exec-includes-launch-wait', action='store_true')
    parser.add_argument('--out', type=Path)
    parser.add_argument('--compare', type=Path)
    parser.add_argument('--label', default='recomputed')
    args = parser.parse_args(argv)
    samples = args.input/'samples.jsonl' if args.input else args.samples
    if not samples.is_file():
        parser.error(f'找不到样本文件：{samples}')
    if args.samples and not args.journal_json:
        parser.error('--samples 必须同时提供 --journal-json；不从截断的文本猜测')
    with tempfile.TemporaryDirectory(prefix='coldstart-split-') as temporary:
        journal = args.journal_json
        if journal is None:
            paths = sorted(args.input.glob('journal-*.jsonl'))
            if not paths:
                parser.error(f'{args.input} 中没有 journal-*.jsonl')
            journal = Path(temporary)/'journal.jsonl'
            with journal.open('w') as stream:
                for path in paths:
                    with path.open() as raw:
                        for line in raw:
                            if line.strip():
                                # Fail before processing on corrupt evidence.
                                json.loads(line)
                                stream.write(line.rstrip('\n')+'\n')
        forwarded = ['--from-samples', str(samples), '--journal-json', str(journal), '--label', args.label]
        for name in ('out', 'compare', 'conductor_log', 'phase_json'):
            value = getattr(args, name)
            if value is not None:
                forwarded += ['--'+name.replace('_','-'), str(value)]
        if args.legacy_exec_includes_launch_wait:
            forwarded.append('--legacy-exec-includes-launch-wait')
        return stage_main(forwarded)


if __name__ == '__main__':
    sys.exit(main())
