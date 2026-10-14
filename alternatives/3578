const { test, expect } = require("@playwright/test");
const { startHostedHub, STARTUP_TIMEOUT_MS } = require("./hosted-hub");

let hub;
test.beforeAll(async () => {
  test.setTimeout(STARTUP_TIMEOUT_MS + 30_000);
  hub = await startHostedHub("issue-layout");
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

async function openIssue(page, fixtures, diffs = {}) {
  await page.route("**/work-items/*/changes", (route) =>
    route.fulfill({ json: fixtures.map((fixture) => fixture.change) }),
  );
  for (const fixture of fixtures) {
    await page.route(`**/work-items/*/changes/${fixture.change.change_id}`, (route) =>
      route.fulfill({ json: fixture.detail }),
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
  await expect(page.getByTestId("issue-body")).toBeVisible();
}

for (const count of [0, 1, 3]) {
  test(`issue retains the Properties aside with ${count} change requests`, async ({ page }) => {
    await page.setViewportSize({ width: 1100, height: 900 });
    const fixtures = Array.from({ length: count }, (_, index) =>
      changeFixture(`change${index}`, `src/file${index}.ts`),
    );
    await openIssue(page, fixtures);
    const aside = page.getByRole("complementary", { name: "Properties", exact: true });
    await expect(aside).toBeVisible();
    expect((await aside.boundingBox()).width).toBe(300);
    await expect(aside.getByTestId("issue-properties")).toBeVisible();
    await expect(page.getByTestId("issue-review-dock")).toHaveCount(0);
    await expect(page.getByRole("tablist", { name: "Issue review" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Open review", exact: true })).toHaveCount(0);
    await expect(page.getByTestId("issue-activity")).toBeVisible();
    await page.setViewportSize({ width: 1440, height: 1100 });
    await expect(aside).toBeVisible();
    expect((await aside.boundingBox()).width).toBe(300);
  });
}

test("mobile issue retains its original layout without a review dialog", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openIssue(page, [changeFixture("narrow", "src/narrow.ts")]);
  await expect(page.getByRole("complementary", { name: "Properties", exact: true })).toBeHidden();
  await expect(page.getByRole("button", { name: "Open review", exact: true })).toHaveCount(0);
  await expect(page.getByRole("dialog", { name: "Issue review" })).toHaveCount(0);
  await expect(page.getByTestId("issue-review-dock")).toHaveCount(0);
  await expect(page.getByTestId("issue-body")).toBeVisible();
  await expect(page.getByTestId("issue-activity")).toBeVisible();
});

test("selected Surface Diff yields the aside and keeps Properties reachable", async ({ page }) => {
  const fixture = changeFixture("surface", "src/surface.ts");
  await openIssue(page, [fixture], { latest: fixture.diff });
  await page.keyboard.press("ControlOrMeta+k");
  await page.getByTestId("command-palette").getByText("Open Diff panel", { exact: true }).click();
  await expect(page.getByTestId("diff-file-section")).toHaveAttribute("data-diff-path", "src/surface.ts");
  await expect(page.getByTestId("diff-round")).toContainText(fixture.diff.head_sha.slice(0, 7));
  await expect(page.getByRole("complementary", { name: "Properties", exact: true })).toHaveCount(0);
  await page.getByTestId("properties-disclosure").click();
  await expect(page.getByTestId("issue-properties")).toBeVisible();
  await page.getByRole("button", { name: /^Toggle right panel/ }).click();
  await expect(page.getByRole("complementary", { name: "Properties", exact: true })).toBeVisible();
});

for (const source of ["available", "unavailable", "wrong head", "empty files"]) {
  test(`Change Request reads the round when the attempt diff is ${source}`, async ({
    page,
  }) => {
    const fixture = changeFixture("fallback", "src/fallback.ts");
    const attempt =
      source === "available"
        ? fixture.diff
        : source === "wrong head"
          ? { ...fixture.diff, head_sha: "another-head" }
          : { ...fixture.diff, files: [] };
    await openIssue(page, [fixture], {
      attemptUnavailable: source === "unavailable",
      attempt,
      latest: fixture.diff,
    });
    await page.getByTestId("issue-resources").getByRole("button", { name: /Change fallback/ }).click();
    await expect.poll(() => new URL(page.url()).pathname).toBe(`/work/i/${hub.fixture.work_item}/changes/${fixture.change.change_id}`);
    await expect(page.getByTestId("diff-file-section")).toHaveAttribute(
      "data-diff-path",
      "src/fallback.ts",
    );
    await expect(page.getByTestId("change-rounds")).toContainText("Round 1");
    await expect(page.getByTestId("change-rounds")).toContainText("Round 2");
    await expect(page.getByTestId("change-review-actions")).toBeVisible();
  });
}

for (const latest of [null, "wrong head"]) {
  test(`Change Request withholds an unrelated or missing fallback (${latest})`, async ({
    page,
  }) => {
    const fixture = changeFixture("missing", "src/missing.ts");
    await openIssue(page, [fixture], {
      attemptUnavailable: true,
      latest: latest === null ? null : { ...fixture.diff, head_sha: "another-head" },
    });
    await page.goto(new URL(`/work/i/${hub.fixture.work_item}/changes/${fixture.change.change_id}`, hub.fixture.url).toString());
    await expect(page.getByTestId("change-diff-empty")).toContainText(
      "No stored diff is available for this round",
    );
    await expect(page.getByTestId("diff-file-section")).toHaveCount(0);
  });
}
