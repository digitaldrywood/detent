import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location(
    "enrollment_docs", Path(__file__).with_name("check-runner-enrollment-docs.py"),
)
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


class RunnerEnrollmentDocsCheckTest(unittest.TestCase):
    def test_migration_commands(self):
        help_text = {
            (): "Examples:\ndetent --config example.yaml --headless\n\nAvailable Commands:\n  hub         Hub service\n  pause       Pause project\n\nFlags:\n      --config string   Configuration\n      --headless        Run without UI\n  -h, --help            Help\n",
            ("hub",): "\nAvailable Commands:\n  issue       Native issues\n  serve       Start Hub\n\nFlags:\n  -h, --help   Help\n",
            ("hub", "issue"): "\nAvailable Commands:\n  import-local Import a store\n\nFlags:\n  -h, --help   Help\n",
            ("hub", "issue", "import-local"): "\nFlags:\n      --dry-run           Preview\n      --project string    Project\n",
            ("hub", "serve"): "\nFlags:\n      --github-disabled   Disable GitHub\n      --database string   Database\n",
            ("pause",): "\nFlags:\n      --reason string   Reason\n",
        }
        cases = (
            ("nested import", '`detent hub issue import-local board.sqlite --dry-run`', []),
            ("root flag before command", '`detent --config "private config.yaml" pause example-project --reason migration`', []),
            ("root invocation", '`detent --config local.yaml --headless`', []),
            ("boolean before value", '```sh\ndetent hub serve --github-disabled --database hub.db\n```', []),
            ("absolute executable", '`sudo -u example /usr/bin/detent hub serve`', []),
            ("plist path", '```sh\nplutil -lint "$HOME/com.example.detent.hub.plist"\n```', []),
            ("backup filename", '```sh\ncurl --data \'{"output":"/var/lib/detent-hub/snapshot.db"}\'\n```', []),
            ("unknown command", '`detent hub issue import-store board.sqlite`', ["unknown documented command: detent hub issue import-store"]),
            ("unknown flag", '`detent hub serve --missing`', ["unknown documented flag: detent hub serve --missing"]),
            ("unknown after root flag", '`detent --config local.yaml missing`', ["unknown documented command: detent missing"]),
        )
        for name, document, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(CHECKER.check_commands(document, help_text.__getitem__), expected)

    def test_subcommand_contract(self):
        help_text = '''Examples:
detent hub runner init --hub-url https://hub.example.com

Available Commands:
  enroll      Enroll a runner
  init        Initialize a runner
  register    Register a runner
  renew       Renew credentials
  rotate      Rotate credentials

Flags:
  -h, --help   help for runner
'''
        cases = (
            ("inline", '`detent hub runner renew`', []),
            ("inline reflow", '`detent hub runner\nrenew`', []),
            ("shell", '```sh\ndetent hub runner enroll\n```', []),
            ("continued shell", '```bash\ndetent hub runner \\\n  rotate\n```', []),
            ("prose", 'detent hub runner is the identity command.', []),
            ("prose reflow", 'detent hub runner\nis the identity command.', []),
            ("parent command", '`detent hub runner` is the identity command.', []),
            ("help flag", '`detent hub runner --help`', []),
            ("inline unknown", '`detent hub runner revoke`', ["unknown runner subcommand in docs: revoke"]),
            ("shell unknown", '```shell\ndetent hub runner routing\n```', ["unknown runner subcommand in docs: routing"]),
            ("path prefix", '`tmp/detent hub runner revoke`', ["unknown runner subcommand in docs: revoke"]),
        )
        for name, document, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(CHECKER.check_subcommands(document, help_text), expected)
        self.assertEqual(
            CHECKER.check_subcommands('`detent hub runner renew`', help_text.replace('  renew       Renew credentials\n', '')),
            ["unknown runner subcommand in docs: renew"],
        )
        self.assertEqual(CHECKER.check_subcommands('`detent hub runner init`', ''), ["no runner subcommands found in CLI help"])

    def test_flag_contract(self):
        source = '''
            cmd.Flags().StringVar(&url, "url", "", "Hub URL")
            cmd.Flags().BoolVar(&service, "service", false, "Service")
            cmd.Flags().StringVar(&token, "token", "", "Token")
        '''
        cases = (
            ("one line", '```sh\ndetent hub runner register --url "https://hub.example.com"\n```', None),
            ("continued", '```sh\ndetent hub runner register \\\n  --url "https://hub.example.com"\n```', None),
            ("renamed heading", '# Another heading\n`detent hub runner register --service`', None),
            ("unrelated snippet", '`operator\'s config`\n`detent hub runner register --service`', None),
            ("inline unknown", '`detent hub runner register --missing`', "unknown runner registration flag"),
            ("shell unknown", '```sh\ndetent hub runner register --missing=value\n```', "unknown runner registration flag"),
            ("token argument", '`detent hub runner register --token=value`', "never argv"),
            ("missing command", "No registration example.", "no runner registration command"),
        )
        for name, document, expected in cases:
            with self.subTest(name=name):
                errors = CHECKER.check(document, source)
                if expected is None:
                    self.assertEqual(errors, [])
                else:
                    self.assertTrue(any(expected in error for error in errors), errors)
        errors = CHECKER.check('`detent hub runner register --service`', source.replace('"service"', '"background"'))
        self.assertEqual(errors, ["unknown runner registration flag in self-hosting docs: --service"])
