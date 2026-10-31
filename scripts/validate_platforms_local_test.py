import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class PlatformValidationTest(unittest.TestCase):
    def program(self, directory, name, body):
        path = directory / name
        path.write_text('#!/usr/bin/env python3\n' + body)
        path.chmod(0o755)

    def test_windows_exact_toolchain(self):
        for actual in ('go1.26.10', 'go1.26.9'):
            with self.subTest(actual=actual), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                source = root / 'source'
                source.mkdir()
                (source / 'go.mod').write_text('module example.org/project\n\ngo 1.26.0\ntoolchain go1.26.10\n')
                state = root / 'cache/windows'
                state.mkdir(parents=True)
                (state / 'id_ed25519').touch()
                (state / 'id_ed25519.pub').touch()
                binaries = root / 'bin'
                binaries.mkdir()
                self.program(binaries, 'colima', '')
                self.program(binaries, 'docker', 'print("true")\n')
                self.program(binaries, 'ssh', '''import os, sys
command = sys.argv[-1]
with open(os.environ['CALLS'], 'a') as f: f.write(command + '\\n')
if command.startswith('Test-Path'): print('True')
elif 'env GOVERSION' in command:
    print(os.environ['ACTUAL'] + '\\r')
elif command.startswith('tar '): sys.stdin.buffer.read()
''')
                env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ['PATH'],
                           DETENT_PLATFORM_CACHE=str(root / 'cache'), ACTUAL=actual, CALLS=str(root / 'calls'))
                result = subprocess.run(['bash', str(ROOT / 'scripts/validate-windows-local.sh'), str(source)],
                                        env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode == 0, actual == 'go1.26.10', result.stdout + result.stderr)
                calls = (root / 'calls').read_text()
                self.assertIn('env GOVERSION', calls)
                self.assertEqual('go test -short' in calls, actual == 'go1.26.10')
                if actual != 'go1.26.10':
                    self.assertIn('does not match go.mod', result.stderr)

    def test_validated_sources_and_linux_amd64(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / 'repo'
            repo.mkdir()
            env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)

            def git(*args):
                result = subprocess.run(['git', *args], cwd=repo, env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                return result.stdout.strip()

            git('init', '-q', '-b', 'develop')
            git('config', 'user.name', 'Example')
            git('config', 'user.email', 'example@example.com')
            scripts = repo / 'scripts'
            scripts.mkdir()
            shutil.copyfile(ROOT / 'scripts/validate-platforms-local.sh', scripts / 'validate-platforms-local.sh')
            (scripts / 'linux-validate.Dockerfile').write_text('validated Dockerfile\n')
            (scripts / 'validate-windows-local.sh').write_text('echo validated > "$WINDOWS_CALL"\n')
            (repo / 'go.mod').write_text('toolchain go1.26.10\n')
            (repo / 'Makefile').write_text('app:\n\t@true\n')
            git('add', '.')
            git('commit', '-qm', 'validated source')
            head = git('rev-parse', 'HEAD')
            git('remote', 'add', 'origin', str(repo))
            (scripts / 'linux-validate.Dockerfile').write_text('caller Dockerfile\n')
            (scripts / 'validate-windows-local.sh').write_text('echo caller > "$WINDOWS_CALL"\nexit 1\n')
            binaries = root / 'bin'
            binaries.mkdir()
            self.program(binaries, 'gh', '''import json, os, sys
args = sys.argv[1:]
with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')
if args[:2] == ['repo', 'view']: print('example-org/example-project')
''')
            self.program(binaries, 'docker', '''import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with open(os.environ['DOCKER_CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')
if 'build' in args:
    dockerfile = Path(args[args.index('-f') + 1])
    assert dockerfile.read_text() == 'validated Dockerfile\\n'
    assert dockerfile.parent == Path(args[-1])
    assert dockerfile.parent.parent.name == 'source'
if 'run' in args: sys.exit(int(os.environ.get('DOCKER_RUN_EXIT', '0')))
''')
            env.update(PATH=str(binaries) + os.pathsep + env['PATH'], CALLS=str(root / 'calls'),
                       DOCKER_CALLS=str(root / 'docker-calls'), WINDOWS_CALL=str(root / 'windows-call'),
                       DETENT_PLATFORM_CACHE=str(root / 'cache'))
            result = subprocess.run(['bash', 'scripts/validate-platforms-local.sh', 'develop'], cwd=repo,
                                    env=env, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual((root / 'windows-call').read_text(), 'validated\n')
            docker_calls = [json.loads(line) for line in (root / 'docker-calls').read_text().splitlines()]
            self.assertEqual(len(docker_calls), 2)
            for args in docker_calls:
                self.assertEqual(args[args.index('--platform') + 1], 'linux/amd64')
            posts = [json.loads(line) for line in (root / 'calls').read_text().splitlines() if 'POST' in line]
            self.assertIn('state=success', posts[-1])
            self.assertIn('repos/example-org/example-project/statuses/' + head, posts[-1])
            for windows, linux_ok, state in (('unprovisioned', True, 'success'), ('unprovisioned', False, 'failure')):
                with self.subTest(windows=windows, linux_ok=linux_ok):
                    (root / 'windows-call').unlink(missing_ok=True)
                    (root / 'calls').unlink()
                    run_env = dict(env, DETENT_PLATFORM_WINDOWS=windows, DOCKER_RUN_EXIT='0' if linux_ok else '1')
                    result = subprocess.run(['bash', 'scripts/validate-platforms-local.sh', 'develop'], cwd=repo,
                                            env=run_env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode == 0, state == 'success', result.stdout + result.stderr)
                    self.assertFalse((root / 'windows-call').exists())
                    posts = [json.loads(line) for line in (root / 'calls').read_text().splitlines() if 'POST' in line]
                    self.assertIn(f'state={state}', posts[-1])
                    self.assertTrue(any('Windows unprovisioned' in arg for arg in posts[-1]))


if __name__ == '__main__':
    unittest.main()
