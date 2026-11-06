import os
from pathlib import Path
import subprocess
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / 'deploy/hub/deploy-from-ci.sh'


class DeployHealthTest(unittest.TestCase):
    def shell(self, code):
        result = subprocess.run(
            ['bash', '-euo', 'pipefail', '-c', 'NAME=detent-hub STATE=/var/lib/detent-hub\nsource "$1"\nrelease=${release:-candidate}\nunit=detent-cloud\nsystemctl() { echo 1; }\nsudo() { [ "$2" = readlink ] && echo "$release"; }\n' + code, 'test', str(SCRIPT)],
            text=True, capture_output=True, timeout=20,
            env={'PATH': os.environ['PATH'], 'TMPDIR': os.environ['TMPDIR']})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout.strip()

    def test_rollback_requires_known_compatible_schema(self):
        for supported, candidate, applied, allowed in [
            (100, 200, 200, False),
            (100, 100, 100, True),
            (200, 200, 200, True),
            (300, 200, 200, False),
            (200, 200, 300, False),
            ('unknown', 200, 200, False),
        ]:
            with self.subTest(supported=supported, candidate=candidate, applied=applied):
                actual = self.shell(f'''
previous=previous release=candidate applied={applied}
applied_known=true applied_final=true applied_registry={applied} applied_auth={applied} applied_platform={applied}
applied_registry_applicable=true applied_auth_applicable=true applied_platform_applicable=true
rollback_schema_known=true rollback_schema_version={applied} rollback_registry_schema_version={applied} rollback_auth_schema_version={applied} rollback_platform_schema_version={applied}
rollback_hub_applicable=true rollback_registry_applicable=true rollback_auth_applicable=true rollback_platform_applicable=true
rollback_schema_fingerprint=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa rollback_registry_schema_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb rollback_auth_schema_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc rollback_platform_schema_fingerprint=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
applied_hub_fingerprint=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa applied_registry_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb applied_auth_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc applied_platform_fingerprint=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
binary_schema() {{
  local version={supported}
  [ "$1" != candidate ] || version={candidate}
  echo "$version $version $version $version aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
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
    echo '{"status":"unavailable","commit":"abcdef123","schema_version":100,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":true}'
  else
    echo '{"status":"ok","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
  fi
}
sleep() { probe=$((probe + 1)); }
wait_healthy
[ "$probe" = 65 ] && [ "$applied" = 200 ]
''')

    def test_predeploy_health_allows_same_schema_candidate_crash_before_health(self):
        self.shell('''
previous=previous release=previous short=123456789 unit=detent-hub
read_health() {
  if [ "$release" = previous ]; then
    echo '{"status":"ok","commit":"123456789","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
  else
    return 1
  fi
}
capture_previous_schema
release=candidate short=abcdef123
probe=0
systemctl() { if (( probe < 1 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied_final" = false ] && [ "$rollback_schema_known" = true ]
binary_schema() { echo '200 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
rollback_compatible
''')

    def test_predeploy_health_requires_complete_previous_schema_facts(self):
        self.shell('''
previous=previous release=previous short=123456789 unit=detent-hub
read_health() { echo '{"status":"ok","commit":"123456789","schema_version":200,"migrations_running":false}'; }
if capture_previous_schema; then exit 1; fi
[ "$rollback_schema_known" = false ]
''')

    def test_empty_registry_refuses_rollback_with_changed_hub_source_set(self):
        self.shell('''
previous=previous release=previous short=123456789 unit=detent-cloud
read_health() {
  echo '{"status":"ok","commit":"123456789","schema_version":0,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":0,"schema_fingerprint":"","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"","hub_schema_applicable":false,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":false,"schema_facts_complete":true,"migrations_running":false}'
}
capture_previous_schema
release=candidate short=abcdef123
systemctl() { echo 0; }
if wait_healthy; then exit 1; fi
binary_schema() {
  if [ "$1" = candidate ]; then echo '201 10 20 30 eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd';
  else echo '200 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; fi
}
if rollback_compatible; then exit 1; fi
''')

    def test_empty_tenant_registry_is_complete_without_hub_schema(self):
        self.shell('''
short=abcdef123 probe=0
read_health() {
  echo '{"status":"ok","commit":"abcdef123","schema_version":0,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":0,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":false,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":false,"schema_facts_complete":true,"migrations_running":false}'
}
wait_healthy
[ "$applied_known" = true ] && [ "$applied_final" = true ] && [ "$applied" = 0 ]
''')

    def test_rollback_skips_non_applicable_hub_schema(self):
        self.assertEqual(self.shell('''
previous=previous release=candidate applied=0
applied_known=true applied_final=true applied_hub_applicable=false
applied_registry=10 applied_auth=20 applied_platform=0
applied_registry_applicable=true applied_auth_applicable=true applied_platform_applicable=false
rollback_schema_known=true rollback_hub_applicable=false rollback_registry_applicable=true rollback_auth_applicable=true rollback_platform_applicable=false
rollback_registry_schema_version=10 rollback_auth_schema_version=20
rollback_registry_schema_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb rollback_auth_schema_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
applied_registry_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb applied_auth_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
binary_schema() { echo '1 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
if rollback_compatible; then echo allowed; else echo refused; fi
'''), 'allowed')

    def test_failed_health_keeps_schema_for_rollback_decision(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  echo '{"status":"unavailable","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
}
systemctl() { if (( probe < 2 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied" = 200 ]
applied_known=true applied_final=true applied_registry=10 applied_auth=20 applied_platform=30
applied_registry_applicable=true applied_auth_applicable=true applied_platform_applicable=true
applied_hub_fingerprint=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa applied_registry_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb applied_auth_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc applied_platform_fingerprint=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
binary_schema() { echo '100 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
if rollback_compatible; then exit 1; fi
''')

    def test_failed_binary_rollback_preserves_database(self):
        self.shell('''
database="$TMPDIR/hub.db"
mkdir -p "$TMPDIR"
printf 'writes-after-predeploy-backup' > "$database"
previous=/releases/detent-abcdef123 release=candidate OPT="$TMPDIR/opt"
link_updates=0
sudo() {
  if [ "$1" = -n ] && [ "$2" = ln ] && [ "$3" = -sfn ] && [ "$4" = "$previous" ] && [ "$5" = "$OPT/detent" ]; then
    link_updates=$((link_updates + 1))
    return 0
  fi
  echo "unexpected privileged command: $2" >&2
  return 99
}
restart_unit() { :; }
wait_healthy() { return 1; }
rollback_previous
[ "$link_updates" = 1 ] && [ "$(cat "$database")" = writes-after-predeploy-backup ]
''')

    def test_rollback_rejects_same_max_with_different_migration_set(self):
        self.assertEqual(self.shell('''
previous=previous release=candidate applied=17
applied_known=true applied_final=true applied_registry=10 applied_auth=20 applied_platform=30
applied_registry_applicable=true applied_auth_applicable=true applied_platform_applicable=true
rollback_schema_known=true rollback_schema_version=17 rollback_registry_schema_version=10 rollback_auth_schema_version=20 rollback_platform_schema_version=30
rollback_hub_applicable=true rollback_registry_applicable=true rollback_auth_applicable=true rollback_platform_applicable=true
rollback_schema_fingerprint=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee rollback_registry_schema_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb rollback_auth_schema_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc rollback_platform_schema_fingerprint=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
applied_hub_fingerprint=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
applied_registry_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
applied_auth_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
applied_platform_fingerprint=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
binary_schema() {
  if [ "$1" = candidate ]; then echo '17 10 20 30 eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd';
  else echo '17 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; fi
}
if rollback_compatible; then echo allowed; else echo refused; fi
'''), 'refused')

    def test_missing_applied_schema_facts_refuse_rollback(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  echo '{"status":"unavailable","commit":"abcdef123","schema_version":200,"migrations_running":false}'
}
systemctl() { if (( probe < 1 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied_known" = false ]
binary_schema() { echo '500 500 500 500 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
if rollback_compatible; then exit 1; fi
''')

    def test_process_dies_after_progress_but_before_postmigration_receipt(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  echo '{"status":"unavailable","commit":"abcdef123","schema_version":100,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"migrations_running":true}'
}
systemctl() { if (( probe < 1 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied" = 100 ] && [ "$applied_final" = false ]
binary_schema() { echo '500 500 500 500 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
if rollback_compatible; then exit 1; fi
''')

    def test_wait_failure_allows_rollback_when_migration_sets_match(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  echo '{"status":"degraded","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
}
systemctl() { if (( probe < 1 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied_final" = false ] && [ "$rollback_schema_known" = true ]
binary_schema() { echo '200 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
rollback_compatible
''')

    def test_wait_failure_rejects_rollback_when_same_max_set_differs(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  echo '{"status":"degraded","commit":"abcdef123","schema_version":17,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
}
systemctl() { if (( probe < 1 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied_final" = false ] && [ "$rollback_schema_known" = true ]
binary_schema() {
  if [ "$1" = candidate ]; then echo '17 10 20 30 eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd';
  else echo '17 10 20 30 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; fi
}
if rollback_compatible; then exit 1; fi
''')

    def test_later_incomplete_sample_invalidates_rollback_evidence(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  if (( probe == 0 )); then
    echo '{"status":"degraded","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}'
  else
    echo '{"status":"unavailable","commit":"abcdef123","schema_version":100,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":false,"migrations_running":true}'
  fi
}
systemctl() { if (( probe < 2 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$rollback_schema_known" = false ]
''')

    def test_shallow_health_or_other_commit_cannot_pass(self):
        for response in ['{}', '{"status":"ok","commit":"other"}',
                         '{"status":"ok","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":30,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":false,"migrations_running":false}',
                         '{"status":"unavailable","commit":"other","migrations_running":true}']:
            with self.subTest(response=response):
                self.shell(f'''
short=abcdef123 probe=0
read_health() {{ echo '{response}'; }}
systemctl() {{ if (( probe < 1 )); then echo 1; else echo 0; fi; }}
sleep() {{ probe=$((probe + 1)); }}
if wait_healthy; then exit 1; fi
''')

    def test_invalid_current_sample_does_not_reuse_prior_complete_facts(self):
        self.shell('''
short=abcdef123 previous=previous release=candidate probe=0
read_health() {
  if (( probe == 0 )); then
    echo '{"status":"unavailable","commit":"abcdef123","schema_version":200,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":0,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":false,"schema_facts_complete":true,"migrations_running":false}'
  else
    echo '{"status":"ok","commit":"abcdef123","schema_version":0,"registry_schema_version":10,"auth_schema_version":20,"platform_schema_version":0,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":false,"schema_facts_complete":true,"migrations_running":false}'
  fi
}
systemctl() { if (( probe < 2 )); then echo 1; else echo 0; fi; }
sleep() { probe=$((probe + 1)); }
if wait_healthy; then exit 1; fi
[ "$applied" = 200 ] && [ "$applied_final" = false ]
binary_schema() { echo '500 500 500 500 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; }
if rollback_compatible; then exit 1; fi
''')


if __name__ == '__main__':
    unittest.main()
