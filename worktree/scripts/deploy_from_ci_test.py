import os
from pathlib import Path
import subprocess
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / 'deploy/hub/deploy-from-ci.sh'


class DeployHealthTest(unittest.TestCase):
    def shell(self, code):
        result = subprocess.run(
            ['bash', '-euo', 'pipefail', '-c', 'source "$1"\n' + code, 'test', str(SCRIPT)],
            text=True, capture_output=True, timeout=20,
            env={'PATH': os.environ['PATH'], 'TMPDIR': os.environ['TMPDIR']})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout.strip()

    def test_rollback_requires_known_compatible_schema(self):
        for supported, candidate, applied, allowed in [
            (100, 200, 200, False),
            (100, 200, 100, False),
            (200, 200, 200, True),
            (300, 200, 200, True),
            (200, 200, 300, False),
            ('unknown', 200, 200, False),
            (200, 'unknown', 200, False),
        ]:
            with self.subTest(supported=supported, candidate=candidate, applied=applied):
                actual = self.shell(f'''
previous=previous release=candidate applied={applied}
binary_schema() {{
  if [ "$1" = previous ]; then echo {supported}; else echo {candidate}; fi
}}
if rollback_compatible; then echo allowed; else echo refused; fi
''')
                self.assertEqual(actual, 'allowed' if allowed else 'refused')

    def test_release_commits(self):
        for name, expected in [('detent-ci-abcdef123-20261031T185256Z', 'abcdef123'),
                               ('detent-abcdef123', 'abcdef123'), ('bad-release', '')]:
            with self.subTest(name=name):
                self.assertEqual(self.shell(f'release_commit /releases/{name}'), expected)

    def test_migration_progress_outlasts_old_timeout(self):
        self.shell('''
short=abcdef123
probe=0
read_health() {
  if (( probe < 65 )); then
    echo '{"status":"unavailable","commit":"abcdef123","schema_version":100,"migrations_running":true}'
  else
    echo '{"status":"ok","commit":"abcdef123","schema_version":200,"migrations_running":false}'
  fi
}
sleep() { probe=$((probe + 1)); }
wait_healthy
[ "$probe" = 65 ] && [ "$applied" = 200 ]
''')

    def test_failed_health_keeps_schema_for_rollback_decision(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate
read_health() {
  echo '{"status":"unavailable","commit":"abcdef123","schema_version":200,"migrations_running":false}'
}
sleep() { :; }
if wait_healthy; then exit 1; fi
[ "$applied" = 200 ]
binary_schema() { echo 100; }
if rollback_compatible; then exit 1; fi
''')

    def test_shallow_health_or_other_commit_cannot_pass(self):
        for response in ['{}', '{"status":"ok","commit":"other"}',
                         '{"status":"unavailable","commit":"other","migrations_running":true}']:
            with self.subTest(response=response):
                self.shell(f'''
short=abcdef123
read_health() {{ echo '{response}'; }}
sleep() {{ :; }}
if wait_healthy; then exit 1; fi
''')


if __name__ == '__main__':
    unittest.main()
