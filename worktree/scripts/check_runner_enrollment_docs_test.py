import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location(
    "enrollment_docs", Path(__file__).with_name("check-runner-enrollment-docs.py"),
)
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


class RunnerEnrollmentDocsCheckTest(unittest.TestCase):
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
