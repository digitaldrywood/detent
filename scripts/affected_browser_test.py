from pathlib import Path
import unittest

from scripts.affected_browser import select_specs


ROOT = Path(__file__).resolve().parent.parent


class AffectedBrowserTest(unittest.TestCase):
    def test_selection(self):
        cases = (
            ("non UI", ["internal/hubserver/runner_routing.go", "internal/hubserver/native_issue_pages_test.go", "scripts/check-land.sh", "README.md"], []),
            ("mapped", ["web/conversation/src/app/reports/ReportsPage.tsx"], ["cloud-reports"]),
            ("changed spec union", ["web/conversation/src/app/fleet/RunnerDetailSheet.tsx", "tests/visual/cloud-reports.spec.js", "tests/visual/providers-runners.spec.js"], ["cloud-reports", "providers-runners"]),
            ("unmapped", ["web/conversation/src/app/NewPage.tsx"], ["cloud-reports", "hub-login", "layout"]),
            ("mixed", ["web/conversation/src/app/fleet/format.ts", "static/css/input.css"], ["cloud-reports", "hub-login", "layout", "providers-runners"]),
            ("template", ["internal/web/templates/reportsv2_templ.go"], ["reports-cost-outcomes", "reports-digest"]),
            ("Hub page", ["internal/hubserver/native_issue_pages.go"], ["issue-layout", "work"]),
            ("Hub unmapped", ["internal/hubserver/new_ui.go"], ["cloud-reports", "hub-login", "layout"]),
            ("assets", ["internal/web/static/new.js"], ["cloud-reports", "hub-login", "layout"]),
            ("preview", ["tests/visual/providers-runners-preview.tsx"], ["providers-runners"]),
            ("shared fixture", ["tests/visual/hosted-hub.js"], ["cloud-reports", "hub-login", "layout"]),
            ("deleted spec", ["tests/visual/deleted.spec.js"], []),
            ("mobile spec", ["tests/visual/mobile.spec.js"], ["mobile"]),
        )
        for name, paths, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(select_specs(paths, ROOT), [f"tests/visual/{spec}.spec.js" for spec in expected])


if __name__ == "__main__":
    unittest.main()
