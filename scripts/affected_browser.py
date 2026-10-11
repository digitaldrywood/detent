import fnmatch
from pathlib import Path
import sys


SMOKE = ("board-navigation", "hub-login", "hub-titles")
GLOBAL_DEPENDENCIES = (
    "playwright.config.*",
    "package.json",
    "package-lock.json",
    "web/conversation/package.json",
    "web/conversation/package-lock.json",
    "web/conversation/*.config.*",
    "web/conversation/tsconfig*.json",
    "web/conversation/index.html",
    "web/conversation/src/app/App.tsx",
    "web/conversation/src/app/main.tsx",
    "web/conversation/src/app/router.tsx",
    "web/conversation/src/components/ui/*",
    "web/conversation/src/*.css",
    "static/css/*",
    "static/js/templui/*",
    "internal/hubserver/hosted_browser*_test.go",
    "internal/cli/board_snapshot_browser_test.go",
    "internal/cloudentry/pilot_client_preview_test.go",
    "scripts/check-browser.sh",
)
MAPPING = (
    ("web/conversation/src/app/fleet/*", ("providers-runners",)),
    ("web/conversation/src/app/account/*", ("account", "providers-runners")),
    ("web/conversation/src/contracts/account*", ("account", "providers-runners")),
    ("web/conversation/src/contracts/fixtures/account*", ("account", "providers-runners")),
    ("web/conversation/src/app/components/InlineActionCard.tsx", ("conversation-actions", "providers-runners")),
    ("web/conversation/src/app/settings/*APIKeys*", ("api-mcp",)),
    ("web/conversation/src/app/settings/MCPSettings.tsx", ("api-mcp",)),
    ("web/conversation/src/app/settings/SlackSettings.tsx", ("slack-settings",)),
    ("web/conversation/src/app/settings/*Credit*", ("ai-credits",)),
    ("web/conversation/src/app/settings/*plan*", ("account",)),
    ("web/conversation/src/app/reports/*", ("cloud-reports",)),
    ("web/conversation/src/contracts/reports.ts", ("cloud-reports",)),
    ("web/conversation/src/contracts/fixtures/reports.json", ("cloud-reports",)),
    ("web/conversation/src/app/usage/*", ("cloud-reports",)),
    ("web/conversation/src/app/work/IssuePage.tsx", ("issue-layout",)),
    ("web/conversation/src/app/work/components/Issue*", ("issue-layout",)),
    ("web/conversation/src/app/work/ChangesPage.tsx", ("change-review",)),
    ("web/conversation/src/app/work/*", ("work", "work-views")),
    ("web/conversation/src/app/entry/PlatformAudit.tsx", ("platform-audit",)),
    ("web/conversation/src/app/entry/PlatformUsers.tsx", ("platform-users",)),
    ("web/conversation/src/app/entry/PlatformStaff.tsx", ("platform-staff",)),
    ("web/conversation/src/app/entry/PlatformTenants.tsx", ("platform-tenants",)),
    ("web/conversation/src/app/entry/Platform*.tsx", ("platform-shell",)),
    ("web/conversation/src/app/entry/*", ("hub-login",)),
    ("internal/web/templates/reports*", ("reports-cost-outcomes", "reports-digest")),
    ("internal/web/templates/runner*", ("runner-settings", "runner-problems")),
    ("internal/web/templates/work.*", ("work",)),
    ("internal/web/templates/work_templ.go", ("work",)),
    ("internal/web/templates/diagnostics*", ("diagnostics",)),
    ("internal/web/templates/artifacts*", ("artifacts",)),
    ("internal/web/templates/hosted_billing*", ("account",)),
    ("internal/hubserver/hosted_billing_ui.go", ("account",)),
    ("internal/hubserver/hosted_plan_ui.go", ("account",)),
    ("internal/hubserver/hosted_login.go", ("hub-login", "magic-link-auth", "oidc-auth")),
    ("internal/hubserver/native_*page*.go", ("work", "issue-layout")),
    ("internal/hubserver/hosted_ui.go", ("hub-login",)),
    ("tests/visual/providers-runners-*", ("providers-runners",)),
    ("tests/visual/platform-*", ("platform-shell", "platform-audit", "platform-users", "platform-staff", "platform-tenants", "platform-model-choice")),
    ("tests/visual/slack-settings-*", ("slack-settings",)),
    ("tests/visual/entry-preview.js", ("hub-login", "entry-fallback", "invitations")),
    ("tests/visual/issue-diagnostics-fixture.js", ("issue-diagnostics",)),
    ("tests/visual/onboarding-help-server.ts", ("onboarding-help",)),
)


def is_ui(path):
    if path.startswith("internal/hubserver/") and path.endswith("_test.go"):
        return False
    return (
        path.startswith(("web/conversation/src/", "internal/web/templates/", "internal/web/static/", "static/", "tests/visual/"))
        or fnmatch.fnmatchcase(path, "internal/web/*.templ")
        or fnmatch.fnmatchcase(path, "internal/hubserver/*_ui.go")
        or fnmatch.fnmatchcase(path, "internal/hubserver/*page*.go")
        or path == "internal/hubserver/hosted_login.go"
    )


def select_specs(paths, root):
    selected = set()
    for path in paths:
        if path.startswith("tests/visual/") and path.endswith(".spec.js"):
            if (root / path).is_file():
                selected.add(path)
            continue
        names = {name for pattern, specs in MAPPING if fnmatch.fnmatchcase(path, pattern) for name in specs}
        shared_fixture = not names and path.startswith("tests/visual/") and Path(path).suffix in {".js", ".ts", ".tsx", ".json"}
        if shared_fixture or any(fnmatch.fnmatchcase(path, pattern) for pattern in GLOBAL_DEPENDENCIES):
            selected.update(spec.relative_to(root).as_posix() for spec in (root / "tests/visual").rglob("*.spec.js"))
        elif is_ui(path):
            selected.update(f"tests/visual/{name}.spec.js" for name in names or SMOKE)
    return sorted(selected)


if __name__ == "__main__":
    for spec in select_specs(sys.stdin.read().splitlines(), Path.cwd()):
        print(spec)
