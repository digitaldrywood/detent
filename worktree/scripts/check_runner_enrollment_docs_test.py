import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location(
    "enrollment_docs", Path(__file__).with_name("check-runner-enrollment-docs.py"),
)
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


class RunnerEnrollmentDocsCheckTest(unittest.TestCase):
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
