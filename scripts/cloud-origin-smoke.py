#!/usr/bin/env python3
"""Read-only hosted-origin checks; these do not prove authenticated journeys."""

import argparse
import json
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--environment", choices=["production", "staging"], required=True)
    parser.add_argument("--tls-only", action="store_true", help="check DNS/TLS only; not a migration pass")
    parser.add_argument("--expected-version")
    parser.add_argument("--expected-commit")
    args = parser.parse_args()
    if bool(args.expected_version) != bool(args.expected_commit) or args.tls_only and args.expected_version:
        parser.error("release identity checks require both version and commit and a full smoke")
    prefix = "staging." if args.environment == "staging" else ""
    canonical = "https://" + prefix + "cloud.detent.build"
    legacy = "https://" + prefix + "hub.detent.build"
    mode = "test" if prefix else "live"
    opener = urllib.request.build_opener(NoRedirect())

    def request(url, data=None):
        req = urllib.request.Request(url, data=data)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            response = opener.open(req, timeout=20)
        except urllib.error.HTTPError as err:
            response = err
        with response:
            return response.code, response.headers, response.read()

    for origin in (canonical, legacy):
        host = urllib.parse.urlsplit(origin).hostname
        socket.getaddrinfo(host, 443, type=socket.SOCK_STREAM)
        request(origin + "/")  # default trust store and hostname verification
        print("PASS DNS and TLS:", host)
    if args.tls_only:
        print("SKIP canonical routing and authenticated journeys (--tls-only)")
        return

    def check(condition, description):
        if not condition:
            raise RuntimeError(description)
        print("PASS", description)

    if args.expected_version:
        status, _, body = request(canonical + "/health")
        identity = json.loads(body)
        check(status == 200 and identity.get("version", "").removeprefix("v") == args.expected_version.removeprefix("v") and identity.get("commit") == args.expected_commit, "deployed release identity")
    status, _, _ = request(canonical + "/")
    check(status == 200, "canonical sign-in page")
    for path in ("/organizations?return=%2Fwork&view=all", "/invite?invitation_token=domain_smoke"):
        status, headers, _ = request(legacy + path)
        check(status == 307 and headers.get("Location") == canonical + path, "legacy navigation preserves path/query")
    status, headers, _ = request(canonical + "/auth/oidc/start")
    target = urllib.parse.urlsplit(headers.get("Location", ""))
    query = urllib.parse.parse_qs(target.query)
    check(status == 303 and target.hostname == "api.workos.com" and
          query.get("redirect_uri") == [canonical + "/auth/oidc/callback"], "sign-in uses canonical callback")
    for origin in (canonical, legacy):
        status, headers, _ = request(origin + "/auth/oidc/callback?code=domain_smoke&state=domain_smoke")
        check(status == 401 and not headers.get("Location"), "invalid callback reaches auth handler without redirect")
        status, headers, _ = request(origin + "/webhooks/stripe/" + mode, b"{}")
        check(status == 400 and not headers.get("Location"), "unsigned webhook reaches signature verification without redirect")
        other = "live" if mode == "test" else "test"
        status, _, _ = request(origin + "/webhooks/stripe/" + other, b"{}")
        check(status == 404, "other billing environment stays closed")
        path = "/organizations/org_domain_smoke/api/v1/work-items/1?cursor=2"
        status, headers, _ = request(origin + path)
        check(status == 404 and not headers.get("Location"), "runner API retains environment and does not redirect")
    print("SKIP authenticated sign-in, invitations, billing delivery and runner enrollment: separate live acceptance evidence required")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError, urllib.error.URLError) as err:
        # URLs from auth responses can contain opaque transaction values; avoid
        # printing exception representations or response headers/bodies.
        detail = str(err) if isinstance(err, RuntimeError) else type(err).__name__
        print("FAIL hosted-origin smoke:", detail, file=sys.stderr)
        sys.exit(1)
