import contextlib
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest import mock
import urllib.error


spec = importlib.util.spec_from_file_location("cloud_origin_smoke", Path(__file__).with_name("cloud-origin-smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class OriginSmokeTest(unittest.TestCase):
    def test_identity_readiness(self):
        correct = (200, {}, json.dumps({"version": "1.2.3", "commit": "expected"}))
        old = (200, {}, json.dumps({"version": "1.2.2", "commit": "old"}))
        cases = (
            ("correct immediately", [correct], True, 0),
            ("correct after thirty seconds", [old] * 15 + [correct], True, 30),
            ("restart unavailable", [urllib.error.URLError("restarting"), (502, {}, "unavailable"), correct], True, 4),
            ("deadline exceeded", [old], False, 120),
            ("wrong commit", [(200, {}, json.dumps({"version": "1.2.3", "commit": "wrong"}))], False, 120),
            ("malformed identity", [(200, {}, "null")], False, 120),
            ("HTTP error with correct body", [(502, {}, correct[2])], False, 120),
        )
        for name, responses, passes, elapsed in cases:
            with self.subTest(name=name), contextlib.redirect_stdout(io.StringIO()) as output:
                clock = [0]
                attempts = []

                def request(url, timeout):
                    self.assertEqual(url, "https://example.test/health")
                    self.assertGreater(timeout, 0)
                    response = responses[min(len(attempts), len(responses) - 1)]
                    attempts.append(timeout)
                    if isinstance(response, Exception):
                        raise response
                    return response

                def sleep(duration):
                    clock[0] += duration

                if passes:
                    smoke.wait_for_release(request, "https://example.test/health", "v1.2.3", "expected",
                                           monotonic=lambda: clock[0], sleep=sleep)
                    self.assertIn("PASS deployed release identity", output.getvalue())
                else:
                    with self.assertRaisesRegex(RuntimeError, "identity.*deadline"):
                        smoke.wait_for_release(request, "https://example.test/health", "v1.2.3", "expected",
                                               monotonic=lambda: clock[0], sleep=sleep)
                    self.assertNotIn("PASS", output.getvalue())
                self.assertEqual(clock[0], elapsed)
                self.assertEqual(len(attempts), elapsed // 2 + 1 if passes else 60)

    def test_remaining_checks(self):
        class Response(io.BytesIO):
            def __init__(self, code, headers=None, body=b""):
                super().__init__(body)
                self.code = code
                self.headers = headers or {}

        for failure in ("", "canonical", "auth", "webhook"):
            with self.subTest(failure=failure), contextlib.redirect_stdout(io.StringIO()):
                calls = []

                def request(req, timeout):
                    calls.append(req.full_url)
                    url = req.full_url
                    if url.endswith("/health"):
                        return Response(200, body=b'{"version":"1.2.3","commit":"expected"}')
                    if "/auth/oidc/start" in url:
                        return Response(500 if failure == "auth" else 303, {"Location": "https://api.workos.com/user_management/authorize?redirect_uri=https%3A%2F%2Fcloud.detent.build%2Fauth%2Foidc%2Fcallback"})
                    if "/auth/oidc/callback" in url:
                        return Response(401)
                    if "/webhooks/stripe/" in url:
                        return Response(500 if failure == "webhook" else 400 if url.endswith("live") else 404)
                    if "/api/v1/work-items/" in url:
                        return Response(404)
                    if "hub.detent.build" in url and not url.endswith("/"):
                        return Response(307, {"Location": url.replace("hub.detent.build", "cloud.detent.build")})
                    return Response(500 if failure == "canonical" and len(calls) > 3 else 200)

                with mock.patch.object(smoke.sys, "argv", ["smoke", "--environment", "production", "--expected-version", "v1.2.3", "--expected-commit", "expected"]), \
                        mock.patch.object(smoke.socket, "getaddrinfo"), \
                        mock.patch.object(smoke.urllib.request, "build_opener") as opener:
                    opener.return_value.open.side_effect = request
                    if failure:
                        with self.assertRaises(RuntimeError):
                            smoke.main()
                    else:
                        smoke.main()
                        self.assertTrue(any("webhooks/stripe/live" in url for url in calls))
                        self.assertTrue(any("api/v1/work-items" in url for url in calls))


if __name__ == "__main__":
    unittest.main()
