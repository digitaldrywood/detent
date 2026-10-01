const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-review-dock");
});
test.afterAll(async () => {
  await hub?.stop();
});

function changeFixture(id, path, number = "2", status = "ready") {
  const actor = { kind: "human", principal_id: "owner" };
  const head = `${id}abcdef1234567`;
  const change = {
    change_id: id,
    organization_id: "org",
    project_id: hub.fixture.project_id,
    work_item_id: hub.fixture.work_item,
    linked_issues: [],
    title: `Change ${id}`,
    body: "",
    current_version_id: `${id}-v2`,
    revision: "1",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  };
  const version = {
    version_id: change.current_version_id,
    change_id: id,
    number,
    base_sha: "base123",
    head_sha: head,
    merge_base_sha: "base123",
    repository: "repo",
    attempt_id: `${id}-attempt`,
    code: { kind: "code", uri: "artifact:code", sha256: "digest", availability: "available" },
    artifacts: [],
    policy_id: "policy",
    policy: {
      schema: 1,
      policy_id: "policy",
      source_revision: "1",
      source_digest: "digest",
      config_digest: "digest",
      requirements: {},
      gates: {},
    },
    review_policy: {
      review_policy_id: "review",
      policy_id: "policy",
      require_review: false,
      required_checks: [],
    },
    checks: [],
    actor,
    created_at: change.created_at,
  };
  return {
    change,
    detail: {
      change,
      versions: [
        {
          ...version,
          version_id: `${id}-v1`,
          number: "1",
          head_sha: "oldhead",
          attempt_id: "old-attempt",
        },
        version,
      ],
      reviews: [],
      checks: [],
      discussion: [],
      summary: {
        native_review: "approved",
        external_review: "none",
        checks: "success",
        status,
        messages: [],
      },
      ...(status === "merged" || status === "landed"
        ? { external_snapshot: { merge_commit_sha: "landed123456" } }
        : {}),
    },
    diff: {
      id: `${id}-diff`,
      attempt_id: version.attempt_id,
      producer: { kind: "runner", lease_id: "lease", fencing_token: 1 },
      generation: { source: "attempt", seq: 1 },
      base_sha: version.base_sha,
      head_sha: head,
      file_count: 1,
      patch_bytes: 100,
      truncated: false,
      created_at: change.created_at,
      files: [
        {
          path,
          status: "modified",
          additions: 1,
          deletions: 1,
          binary: false,
          denied: false,
          truncated: false,
          patch: `diff --git a/${path} b/${path}\n--- a/${path}\n+++ b/${path}\n@@ -1 +1 @@\n-old value\n+new ${id} value\n`,
        },
      ],
    },
  };
}

async function openIssue(page, fixtures, unavailable = [], diffs = {}) {
  await page.route("**/work-items/*/changes", (route) =>
    route.fulfill({ json: fixtures.map((fixture) => fixture.change) }),
  );
  for (const fixture of fixtures) {
    await page.route(`**/work-items/*/changes/${fixture.change.change_id}`, (route) =>
      unavailable.includes(fixture.change.change_id)
        ? route.fulfill({ status: 503, json: { message: "Unavailable" } })
        : route.fulfill({ json: fixture.detail }),
    );
    await page.route(`**/attempts/${fixture.diff.attempt_id}/diff`, (route) =>
      diffs.attemptUnavailable
        ? route.fulfill({ status: 404, json: { message: "No stored diff" } })
        : route.fulfill({ json: diffs.attempt ?? fixture.diff }),
    );
  }
  if (Object.hasOwn(diffs, "latest")) {
    await page.route("**/work-items/*/diff", (route) =>
      route.fulfill({ json: { diff: diffs.latest } }),
    );
  }
  await page.goto(hub.fixture.accounts.owner);
  await page.goto(new URL(`/work/i/${hub.fixture.work_item}`, hub.fixture.url).toString());
  if (page.viewportSize().width < 1280)
    await page.getByRole("button", { name: "Open review", exact: true }).click();
  await expect(page.getByTestId("issue-review-dock")).toBeVisible();
}

for (const count of [1, 3]) {
  test(`issue review dock shows all files from ${count} change requests`, async ({ page }) => {
    const fixtures = Array.from({ length: count }, (_, index) =>
      changeFixture(
        `change${index}`,
        index === 2 ? "src/file0.ts" : `src/file${index}.ts`,
        "2",
        index === 1 ? "landed" : index === 2 ? "merged" : "ready",
      ),
    );
    await openIssue(page, fixtures);
    const dock = page.getByTestId("issue-review-dock");
    await expect(dock.getByRole("tab", { name: "Diff", exact: true })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await expect(dock.getByTestId("diff-file-section")).toHaveCount(count);
    for (const fixture of fixtures) {
      const section = dock.locator(`[data-change-id="${fixture.change.change_id}"]`);
      await expect(section).toContainText(`Round 2 · head ${fixture.diff.head_sha.slice(0, 7)}`);
      await expect(section.getByTestId("diff-files")).toContainText(
        fixture.diff.files[0].path.split("/").at(-1),
      );
      await expect(section.getByTestId("diff-file-section")).toHaveAttribute(
        "data-diff-path",
        fixture.diff.files[0].path,
      );
    }
    if (count > 1) await expect(dock).toContainText("Landed landed1");
    await dock.getByRole("tab", { name: "State", exact: true }).click();
    await expect(dock.getByTestId("issue-properties")).toBeVisible();
    await dock.getByRole("tab", { name: "Receipt", exact: true }).click();
    await expect(dock).toContainText("Native review: approved");
    await dock.getByRole("tab", { name: "Activity", exact: true }).click();
    await expect(dock.getByTestId("issue-activity")).toBeVisible();
    await dock.getByRole("tab", { name: "Diff", exact: true }).click();
    await dock
      .getByRole("button", { name: "Round history and review", exact: true })
      .first()
      .click();
    await expect(page).toHaveURL(new RegExp(`/changes/${fixtures[0].change.change_id}`));
  });
}

test("issue review dock retains unavailable changes beside readable diffs", async ({ page }) => {
  const fixtures = [
    changeFixture("readable", "src/shared.ts"),
    changeFixture("unavailable", "src/other.ts"),
  ];
  await openIssue(page, fixtures, ["unavailable"]);
  await expect(page.getByTestId("issue-review-change")).toHaveCount(2);
  await expect(page.getByTestId("issue-review-dock")).toContainText(
    "This change’s details are unavailable",
  );
  await expect(page.getByTestId("diff-file-section")).toHaveCount(1);
  await expect(page.getByRole("button", { name: /Change unavailable/ }).first()).toBeVisible();
});

test("issue review dock shows its empty state and properties without changes", async ({ page }) => {
  await openIssue(page, []);
  await expect(page.getByTestId("issue-review-empty")).toContainText("No change requests yet");
  await expect(page.getByTestId("issue-properties")).toBeVisible();
});

test("issue review dock opens and closes on narrow screens", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openIssue(page, [changeFixture("narrow", "src/narrow.ts")]);
  await expect(page.getByTestId("diff-file-section")).toHaveCount(1);
  await expect(page.getByTestId("issue-review-dock")).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Issue review" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Close review", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("issue-review-dock")).toBeHidden();
  await expect(page.getByRole("button", { name: "Open review", exact: true })).toBeFocused();
  await page.getByRole("button", { name: "Open review", exact: true }).click();
  await page.getByRole("button", { name: "Close review", exact: true }).click();
  await expect(page.getByTestId("issue-review-dock")).toBeHidden();
  await expect(page.getByRole("button", { name: "Open review", exact: true })).toBeVisible();
});

for (const source of ["unavailable", "wrong head", "empty files"]) {
  test(`issue review dock uses a matching fallback when the attempt diff has ${source}`, async ({
    page,
  }) => {
    const fixture = changeFixture("fallback", "src/fallback.ts");
    const attempt =
      source === "wrong head"
        ? { ...fixture.diff, head_sha: "another-head" }
        : { ...fixture.diff, files: [] };
    await openIssue(page, [fixture], [], {
      attemptUnavailable: source === "unavailable",
      attempt,
      latest: fixture.diff,
    });
    await expect(page.getByTestId("diff-file-section")).toHaveAttribute(
      "data-diff-path",
      "src/fallback.ts",
    );
  });
}

for (const latest of [null, "wrong head"]) {
  test(`issue review dock withholds an unrelated or missing fallback (${latest})`, async ({
    page,
  }) => {
    const fixture = changeFixture("missing", "src/missing.ts");
    await openIssue(page, [fixture], [], {
      attemptUnavailable: true,
      latest: latest === null ? null : { ...fixture.diff, head_sha: "another-head" },
    });
    await expect(page.getByTestId("issue-review-diff-empty")).toContainText(
      "No stored diff is available for this round",
    );
    await expect(page.getByTestId("diff-file-section")).toHaveCount(0);
  });
}
