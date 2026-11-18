import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class DeployReleaseTest(unittest.TestCase):
    def test_postgres_cutover_uploads_exact_frozen_receiver_and_binary(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / 'repository'
            (repository / 'scripts').mkdir(parents=True)
            (repository / 'deploy/hub').mkdir(parents=True)
            scratch = root / 'scratch'
            (scratch / 'release-hub').mkdir(parents=True)
            (scratch / 'bin').mkdir()
            receiver = b'#!/bin/bash\nexec true\n'
            (repository / 'scripts/deploy-release.sh').write_bytes(
                (ROOT / 'scripts/deploy-release.sh').read_bytes()
            )
            (repository / 'README.md').write_text('frozen release source\n')
            binary = scratch / 'release-hub/detent'
            binary.write_text('#!/bin/sh\nprintf \'%s\\n\' "$RELEASE_IDENTITY"\n')
            binary.chmod(0o755)
            subprocess.run(['git', 'init', '-q', str(repository)], check=True)
            subprocess.run(
                ['git', '-C', str(repository), 'config', 'user.name', 'Example Operator'],
                check=True,
            )
            subprocess.run(
                ['git', '-C', str(repository), 'config', 'user.email', 'operator@example.test'],
                check=True,
            )
            subprocess.run(['git', '-C', str(repository), 'add', '.'], check=True)
            subprocess.run(['git', '-C', str(repository), 'commit', '-qm', 'frozen source base'], check=True)
            (repository / 'deploy/hub/cutover-postgres.sh').write_bytes(receiver)
            subprocess.run(['git', '-C', str(repository), 'add', 'deploy/hub/cutover-postgres.sh'], check=True)
            subprocess.run(['git', '-C', str(repository), 'commit', '-qm', 'test frozen release'], check=True)
            commit = subprocess.run(
                ['git', '-C', str(repository), 'rev-parse', 'HEAD'],
                check=True,
                capture_output=True,
                text=True,
            ).stdout.strip()
            (scratch / 'release-hub/detent').write_text(
                '#!/bin/sh\nprintf \'%s\\n\' \'{"version":"1.2.3","commit":"'
                + commit
                + '"}\'\n'
            )
            binary.chmod(0o755)
            ssh = scratch / 'bin/ssh'
            ssh.write_text(
                '#!/usr/bin/env python3\n'
                'import json, os, shutil, sys\n'
                'open(os.environ["SSH_ARGUMENTS"], "w").write(json.dumps(sys.argv[1:]))\n'
                'with open(os.environ["SSH_PAYLOAD"], "wb") as output: shutil.copyfileobj(sys.stdin.buffer, output)\n'
            )
            ssh.chmod(0o755)
            jq = scratch / 'bin/jq'
            jq.write_text('#!/bin/sh\ncat >/dev/null\n')
            jq.chmod(0o755)
            payload = scratch / 'payload.tar'
            arguments = scratch / 'ssh-arguments.json'
            environment = {
                'PATH': str(scratch / 'bin') + os.pathsep + os.environ['PATH'],
                'TMPDIR': str(scratch),
                'SSH_KEY': 'example-private-key',
                'KNOWN_HOSTS': 'example-host-key',
                'RELEASE_IDENTITY': json.dumps({'version': '1.2.3', 'commit': commit}),
                'SSH_ARGUMENTS': str(arguments),
                'SSH_PAYLOAD': str(payload),
            }
            subprocess.run(
                [
                    'bash',
                    str(repository / 'scripts/deploy-release.sh'),
                    'production',
                    commit,
                    'v1.2.3',
                    'postgres-cutover',
                ],
                cwd=repository,
                env=environment,
                check=True,
                capture_output=True,
                text=True,
                timeout=30,
            )
            ssh_arguments = json.loads(arguments.read_text())
            self.assertEqual(ssh_arguments[-2:], ['apprunner@cloud.detent.build', f'cutover-postgres {commit}'])
            with tarfile.open(payload) as archive:
                self.assertEqual(set(archive.getnames()), {'detent', 'cutover-postgres.sh', 'source.bundle'})
                self.assertEqual(archive.extractfile('cutover-postgres.sh').read(), receiver)
                self.assertEqual(archive.extractfile('detent').read(), binary.read_bytes())
                bundle = root / 'source.bundle'
                bundle.write_bytes(archive.extractfile('source.bundle').read())
            bundle_heads = subprocess.run(
                ['git', 'bundle', 'list-heads', str(bundle)],
                check=True,
                capture_output=True,
                text=True,
            ).stdout.strip()
            self.assertEqual(bundle_heads, f'{commit} HEAD')

    def test_postgres_cutover_refuses_staging(self):
        result = subprocess.run(
            ['bash', str(ROOT / 'scripts/deploy-release.sh'), 'staging', 'a' * 40, 'v1.2.3', 'postgres-cutover'],
            env={'PATH': os.environ['PATH'], 'TMPDIR': tempfile.gettempdir()},
            capture_output=True,
            text=True,
            timeout=10,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn('production-only', result.stderr)


if __name__ == '__main__':
    unittest.main()
