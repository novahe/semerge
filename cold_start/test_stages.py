import tempfile
import unittest
import contextlib
import io
import json
from pathlib import Path
from unittest import mock

import stages
import split_logs


class StageContractTests(unittest.TestCase):
    def test_ten_sample_percentiles_and_average(self):
        self.assertEqual(stages.stats(list(range(1,11))),
                         {'n':10,'min':1,'p50':5.5,'p90':9.1,'p95':9.55,'max':10,'avg':5.5})
        self.assertEqual(stages.stats([7]),
                         {'n':1,'min':7,'p50':7,'p90':7,'p95':7,'max':7,'avg':7})
        self.assertIsNone(stages.stats([]))

    def test_duration_units_and_sid_boundary(self):
        lines = [
            'sid=abc phase=runner_wait_duration duration=500ns',
            'sid=abc phase=prepare_duration duration=12045µs',
            'sid=abc phase=admission_duration duration=10us',
            'sid=abc phase=envd_init_duration duration=1.5s',
            'sid=abc starting_total_duration=2000ms err=<nil>',
            'sid=abcd phase=prepare_duration duration=999s',
        ]
        result = stages.parse_phases(lines, 'abc')
        self.assertEqual(result['runner_wait_duration'], .0005)
        self.assertEqual(result['prepare_duration'], 12.045)
        self.assertEqual(result['admission_duration'], .01)
        self.assertEqual(result['envd_init_duration'], 1500)
        self.assertEqual(result['terminal_total'], 2000)

    def test_missing_evidence_is_not_zero(self):
        result = stages.extract({'i': 0, 'ok': True, 'create_ms': 5, 'exec_ms': 20}, [], {})
        self.assertNotIn('node_ctl_ms', result['stages_ms'])
        self.assertNotIn('kernel_init_ms', result['stages_ms'])
        self.assertIn('node_ctl_ms', result['missing_stages'])
        self.assertIn('envd_ms', result['missing_stages'])

    def test_boot_id_scopes_pid_and_foreign_logs_are_excluded(self):
        def row(boot, pid, msg):
            return {'_BOOT_ID': boot, '_PID': pid, 'MESSAGE': msg,
                    'SYSLOG_IDENTIFIER': 'sandbox-ctl', '__REALTIME_TIMESTAMP': '1000'}
        own = [row('A', '10', '/sandboxes/my-sid/ch.sock'), row('A', '10', 'CH started')]
        foreign = [row('B', '10', 'other boot'), row('A', '11', 'other process')]
        self.assertEqual(stages.journal_for_sid(own + foreign, 'my-sid'), own)

    def test_negative_log_interval_is_not_a_valid_duration(self):
        rows = [
            {'SYSLOG_IDENTIFIER': 'sandbox-ctl', '__REALTIME_TIMESTAMP': '2000', 'MESSAGE': 'CH started pid=1'},
            {'SYSLOG_IDENTIFIER': 'console', '__REALTIME_TIMESTAMP': '1000', 'MESSAGE': 'Run /sbin/init as init process'},
        ]
        result = stages.extract({'i': 0, 'ok': True}, rows, {})
        self.assertNotIn('kernel_init_ms', result['stages_ms'])
        self.assertTrue(result['warnings'])

    def test_uffd_percentile_is_nanoseconds_and_not_a_stage(self):
        rows = [{'SYSLOG_IDENTIFIER': 'sandbox-ctl', '__REALTIME_TIMESTAMP': '1000',
                 'MESSAGE': '  fault_queue_wait_p50 12045'}]
        result = stages.extract({'i': 0, 'ok': True}, rows, {})
        self.assertAlmostEqual(result['uffd_time_ms']['fault_queue_wait_p50_ms'], .012045)
        self.assertNotIn('fault_queue_wait_p50_ms', result['stages_ms'])

    def test_old_inclusive_exec_is_detected_by_client_closure(self):
        old = {'i':0,'ok':True,'create_ms':5,'launch_wait_ms':80,'exec_ms':100,'e2e_ms':105}
        self.assertFalse(stages.extract(old,[],{})['timing_valid'])
        corrected = {**old,'exec_ms':20}
        self.assertTrue(stages.extract(corrected,[],{})['timing_valid'])

    def test_incompatible_comparison_is_rejected(self):
        with tempfile.TemporaryDirectory() as path:
            with self.assertRaisesRegex(ValueError, 'template'):
                stages.report(Path(path), [], {'label': 'candidate', 'template': 'different', 'cmd': 'true'},
                              {'schema': stages.SCHEMA, 'template': 'baseline', 'cmd': 'true'})

    def test_offline_directory_replay_preserves_resource_without_host_logs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root/'source'
            source.mkdir()
            sample = {'schema':stages.SCHEMA,'i':0,'sid':'own-sid','ok':True,
                      'create_ms':5,'launch_wait_ms':10,'exec_ms':20,'e2e_ms':35,
                      'phases_ms':{'admission_duration':1,'prepare_duration':2,
                                   'runner_commit_duration':3,'envd_init_duration':4}}
            resource = {'startup':{'memory':'8GiB'}}
            (source/'samples.jsonl').write_text(json.dumps(sample)+'\n')
            (source/'result.json').write_text(json.dumps({'template':'e2b-img-test','cmd':'true',
                                                        'resource_override':resource}))
            rows = []
            for ident, timestamp, message in [('sandbox-ctl',1000,'/sandboxes/own-sid/ch.sock'),
                                               ('sandbox-ctl',2000,'CH started pid=2'),
                                               ('console',3000,'Run /sbin/init as init process'),
                                               ('sandbox-ctl',5000,'launch: app_started pid=1')]:
                rows.append({'_BOOT_ID':'boot','_PID':'10','__REALTIME_TIMESTAMP':str(timestamp),
                             'SYSLOG_IDENTIFIER':ident,'MESSAGE':message})
            (source/'journal-0.jsonl').write_text(''.join(json.dumps(row)+'\n' for row in rows))
            with mock.patch.object(stages,'collect_journal',side_effect=AssertionError('offline journal access')), \
                 mock.patch.object(stages,'parse_phases',side_effect=AssertionError('offline conductor access')), \
                 contextlib.redirect_stdout(io.StringIO()):
                status = split_logs.main(['--input',str(source),'--out',str(root/'replayed')])
            self.assertEqual(status,0)
            result = json.loads((root/'replayed/result.json').read_text())
            self.assertEqual(result['resource_override'],resource)
            self.assertEqual(result['stages_ms']['exec_ms']['p50'],20)
            self.assertFalse(result['incomplete_samples'])


if __name__ == '__main__':
    unittest.main()
