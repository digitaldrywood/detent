from pathlib import Path
import unittest

from scripts.affected_browser import select_specs


ROOT = Path(__file__).resolve().parent.parent


class AffectedBrowserTest(unittest.TestCase):
    def test_selection(self):
        all_specs = sorted(path.relative_to(ROOT).as_posix() for path in (ROOT / "tests/visual").rglob("*.spec.js"))
        cases = (
            ("non UI", ["internal/hubserver/runner_routing.go", "internal/hubserver/native_issue_pages_test.go", "scripts/check-land.sh", "README.md"], []),
            ("mapped", ["web/conversation/src/app/reports/ReportsPage.tsx"], ["cloud-reports"]),
            ("changed spec union", ["web/conversation/src/app/fleet/RunnerDetailSheet.tsx", "tests/visual/cloud-reports.spec.js", "tests/visual/providers-runners.spec.js"], ["cloud-reports", "providers-runners"]),
            ("unmapped", ["web/conversation/src/app/NewPage.tsx"], ["board-navigation", "hub-login", "hub-titles"]),
            ("mixed", ["web/conversation/src/app/fleet/format.ts", "static/js/new.js"], ["board-navigation", "hub-login", "hub-titles", "providers-runners"]),
            ("template", ["internal/web/templates/reportsv2_templ.go"], ["reports-cost-outcomes", "reports-digest"]),
            ("Hub page", ["internal/hubserver/native_issue_pages.go"], ["issue-layout", "work"]),
            ("Hub unmapped", ["internal/hubserver/new_ui.go"], ["board-navigation", "hub-login", "hub-titles"]),
            ("assets", ["internal/web/static/new.js"], ["board-navigation", "hub-login", "hub-titles"]),
            ("preview", ["tests/visual/providers-runners-preview.tsx"], ["providers-runners"]),
            ("entry fixture consumers", ["tests/visual/entry-preview.js"], ["entry-fallback", "hub-login", "invitations"]),
            ("shared inline action", ["web/conversation/src/app/components/InlineActionCard.tsx"], ["conversation-actions", "providers-runners"]),
            ("specific fixture", ["tests/visual/issue-diagnostics-fixture.js"], ["issue-diagnostics"]),
            ("overlapping work maps", ["web/conversation/src/app/work/IssuePage.tsx"], ["issue-layout", "work-views", "work"]),
            ("overlapping platform maps", ["web/conversation/src/app/entry/PlatformAudit.tsx"], ["hub-login", "platform-audit", "platform-shell"]),
            ("deleted spec", ["tests/visual/deleted.spec.js"], []),
            ("mobile spec", ["tests/visual/mobile.spec.js"], ["mobile"]),
        )
        for name, paths, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(select_specs(paths, ROOT), [f"tests/visual/{spec}.spec.js" for spec in expected])
        for path in (
            "playwright.config.js",
            "package.json",
            "package-lock.json",
            "web/conversation/package.json",
            "web/conversation/package-lock.json",
            "web/conversation/vite.config.ts",
            "web/conversation/vite.gallery.config.ts",
            "web/conversation/tsconfig.json",
            "web/conversation/index.html",
            "web/conversation/src/app/App.tsx",
            "web/conversation/src/app/main.tsx",
            "web/conversation/src/app/router.tsx",
            "web/conversation/src/components/ui/button.tsx",
            "web/conversation/src/app/global.css",
            "static/css/input.css",
            "static/js/templui/dialog.js",
            "tests/visual/hosted-hub.js",
            "tests/visual/detent-runtime.js",
            "tests/visual/fake-oidc.js",
            "tests/visual/new-shared-helper.ts",
            "internal/hubserver/hosted_browser_test.go",
            "internal/cli/board_snapshot_browser_test.go",
            "internal/cloudentry/pilot_client_preview_test.go",
            "scripts/check-browser.sh",
        ):
            with self.subTest(global_dependency=path):
                self.assertEqual(select_specs([path], ROOT), all_specs)


if __name__ == "__main__":
    unittest.main()
