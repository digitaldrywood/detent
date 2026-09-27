// @vitest-environment jsdom
//
// The account mutations, against the mock hub.
//
// These are the calls that change something — an invitation, a role, a grant,
// a project, a checkout, the wizard's saves — and what matters about each is
// not that the request was formed, but what the client does with the answer:
// the last-owner refusal that must not be re-implemented locally, the revision
// conflict that must not be retried with a guess, and the wizard's keys, which
// have to survive a request whose outcome the browser never learned.
//
// jsdom is here for `sessionStorage`, which the wizard's idempotency keys live
// in, not for a DOM: nothing below renders.
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { startMockHub, type MockHub } from "../dev/mock-hub.ts";
import { makeAccountApi, AccountError, type AccountApi } from "../src/app/account/api.ts";
import { enrollCommand } from "../src/app/fleet/EnrollRunner.tsx";
import { clearSetupKeys, setupKey, retireKey, SETUP_KEY_PREFIX } from "../src/app/account/idempotency.ts";
import { lastAccountBootstrap, loadBootstrap } from "../src/runtime/bootstrap.ts";
import type { AccountBootstrap } from "../src/contracts/account.ts";

let hub: MockHub;
let api: AccountApi;
let bootstrap: AccountBootstrap;

async function connect(mode: "write" | "read_only" = "write"): Promise<AccountApi> {
  const payload = await loadBootstrap(hub.url);
  const account = lastAccountBootstrap();
  expect(account, "the mock hub serves the extended bootstrap of §12").not.toBeNull();
  bootstrap = account!;
  expect(mode === "read_only" ? !account!.actor.can_manage : account!.actor.can_manage).toBe(true);
  return makeAccountApi({
    origin: hub.url,
    apiBase: payload.api_base,
    csrfToken: payload.csrf_token,
  });
}

async function reset(mode: "write" | "read_only" = "write") {
  await fetch(`${hub.url}/__mock/reset`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  await fetch(`${hub.url}/__mock/account`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode }),
  });
  api = await connect(mode);
}

beforeAll(async () => {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000 });
  await reset();
});

afterAll(async () => {
  await hub.close();
});

afterEach(() => {
  clearSetupKeys();
});

/** The seeded member with that role, which the tests address by role not id. */
async function memberWithRole(role: string) {
  const { members } = await api.members();
  const member = members.find((candidate) => candidate.role === role);
  expect(member, `the mock hub seeds a ${role}`).toBeDefined();
  return member!;
}

describe("the extended bootstrap", () => {
  it("decodes through both schemas from one payload", async () => {
    const payload = await loadBootstrap(hub.url);
    const account = lastAccountBootstrap();
    // The narrow schema keeps working: it ignores what it does not know.
    expect(payload.organization.id).toBe(account?.organization.id);
    expect(account?.organizations.some((organization) => organization.current)).toBe(true);
    expect(account?.projects[0]?.states.length).toBeGreaterThan(0);
    expect(account?.support).toBeNull();
  });
});

describe("invitations", () => {
  it("creates one and lists it", async () => {
    await reset();
    const before = await api.members();
    await api.invite({ email: "rae@example.test", role: "member", key: "inv-key-1" });
    const after = await api.members();
    expect(after.invitations.length).toBe(before.invitations.length + 1);
    expect(after.invitations.some((invitation) => invitation.email === "rae@example.test")).toBe(
      true,
    );
  });

  it("replays the same key rather than inviting twice", async () => {
    await reset();
    await api.invite({ email: "twice@example.test", role: "member", key: "inv-key-2" });
    await api.invite({ email: "twice@example.test", role: "member", key: "inv-key-2" });
    const { invitations } = await api.members();
    expect(
      invitations.filter((invitation) => invitation.email === "twice@example.test"),
    ).toHaveLength(1);
  });

  it("refuses the same key with a different payload", async () => {
    await reset();
    await api.invite({ email: "first@example.test", role: "member", key: "inv-key-3" });
    const failure = await api
      .invite({ email: "second@example.test", role: "member", key: "inv-key-3" })
      .catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).status).toBe(409);
    expect((failure as AccountError).code).toBe("idempotency_conflict");
  });

  it("refuses an address that is not one", async () => {
    await reset();
    const failure = await api
      .invite({ email: "not-an-address", role: "member", key: "inv-key-4" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(422);
  });
});

describe("the last owner", () => {
  it("cannot be removed", async () => {
    await reset();
    const owner = await memberWithRole("owner");
    const failure = await api
      .removeMember({ member: owner.id, key: "remove-owner" })
      .catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).status).toBe(409);
    expect((failure as AccountError).code).toBe("last_owner");
    // And the refusal is real: the owner is still there.
    expect((await api.members()).members.some((member) => member.id === owner.id)).toBe(true);
  });

  it("cannot be demoted either", async () => {
    await reset();
    const owner = await memberWithRole("owner");
    const failure = await api
      .setMemberRole({ member: owner.id, role: "admin", key: "demote-owner" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).code).toBe("last_owner");
  });

  it("stops being the last one once somebody else is an owner", async () => {
    await reset();
    const owner = await memberWithRole("owner");
    const admin = await memberWithRole("admin");
    await api.setMemberRole({ member: admin.id, role: "owner", key: "promote-admin" });
    const removed = await api
      .removeMember({ member: owner.id, key: "remove-owner-2" })
      .catch((cause: unknown) => cause);
    expect(removed).not.toBeInstanceOf(AccountError);
    expect((await api.members()).members.some((member) => member.id === owner.id)).toBe(false);
  });
});

describe("roles and grants", () => {
  it("changes a role and reports the member back", async () => {
    await reset();
    const viewer = await memberWithRole("viewer");
    const updated = await api.setMemberRole({ member: viewer.id, role: "member", key: "role-1" });
    expect(updated.role).toBe("member");
    expect((await api.members()).members.find((member) => member.id === viewer.id)?.role).toBe(
      "member",
    );
  });

  it("upserts a project grant", async () => {
    await reset();
    const viewer = await memberWithRole("viewer");
    const project = bootstrap.projects[0]!;
    const updated = await api.setMemberGrant({
      member: viewer.id,
      projectId: project.id,
      write: true,
      runner: true,
      key: "grant-1",
    });
    const grant = updated.grants.find((entry) => entry.project_id === project.id);
    expect(grant).toEqual({ project_id: project.id, write: true, runner: true });
  });

  it("revokes a grant rather than leaving an empty one behind", async () => {
    await reset();
    const admin = await memberWithRole("admin");
    const project = bootstrap.projects[0]!;
    await api.setMemberGrant({
      member: admin.id,
      projectId: project.id,
      write: true,
      runner: false,
      key: "grant-2",
    });
    const updated = await api.setMemberGrant({
      member: admin.id,
      projectId: project.id,
      write: false,
      runner: false,
      revoke: true,
      key: "grant-3",
    });
    expect(updated.grants.some((entry) => entry.project_id === project.id)).toBe(false);
  });

  it("is 404 for a member who is not in this organization", async () => {
    await reset();
    const failure = await api
      .setMemberRole({ member: "mem_nobody", role: "admin", key: "role-2" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(404);
  });
});

describe("projects", () => {
  it("creates one and it appears in the list", async () => {
    await reset();
    const before = await api.projects();
    await api.createProject({ name: "newproject", grantAccess: true, key: "project-1" });
    const after = await api.projects();
    expect(after.length).toBe(before.length + 1);
    expect(after.some((project) => project.name === "newproject")).toBe(true);
  });

  it("carries the readiness summary the settings list reads", async () => {
    await reset();
    const projects = await api.projects();
    for (const project of projects) {
      expect(project.onboarding.steps.length).toBe(4);
      expect(project.onboarding.ready).toBe(
        project.onboarding.steps.every((step) => step.state === "ready"),
      );
    }
  });
});

describe("billing", () => {
  it("answers checkout with the URL the browser is sent to", async () => {
    await reset();
    const report = await api.billing();
    const price = report.prices[0]!;
    const { url } = await api.checkout({ price: price.id, key: "checkout-1" });
    expect(url).toMatch(/^https:\/\//);
    expect(url).toContain(price.id);
  });

  it("answers the portal with a URL too", async () => {
    await reset();
    const { url } = await api.portal({ key: "portal-1" });
    expect(url).toMatch(/^https:\/\//);
  });

  it("is 404 for a price the hub is not configured to sell", async () => {
    await reset();
    const failure = await api
      .checkout({ price: "price_not_sold", key: "checkout-2" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(404);
  });
});

describe("a viewer", () => {
  it("reads the members and is refused every mutation (§10.11)", async () => {
    await reset("read_only");
    const { members } = await api.members();
    expect(members.length).toBeGreaterThan(0);
    const failure = await api
      .invite({ email: "nope@example.test", role: "member", key: "viewer-invite" })
      .catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).status).toBe(403);
    expect((failure as AccountError).isAccessError).toBe(true);
    await reset();
  });

  it("cannot read the plan or billing at all", async () => {
    await reset("read_only");
    for (const read of [() => api.plan(), () => api.billing()]) {
      const failure = await read().catch((cause: unknown) => cause);
      expect((failure as AccountError).status).toBe(403);
    }
    await reset();
  });
});

describe("the project settings save", () => {
  it("writes the integration and hands back the next revision", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const before = await api.integration(project.id);
    const after = await api.saveIntegration({
      projectId: project.id,
      key: "integration-1",
      revision: before.revision,
      intake: before.intake,
      projection: before.projection,
      repositoryEnabled: before.repository_enabled,
    });
    expect(after.revision).not.toBe(before.revision);
  });

  it("refuses a stale revision with a conflict that carries nothing to guess from", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const stale = await api.integration(project.id);
    // Somebody else saves first.
    await fetch(`${hub.url}/__mock/revision-drift`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ resource: "integration" }),
    });
    const failure = await api
      .saveIntegration({
        projectId: project.id,
        key: "integration-2",
        revision: stale.revision,
        intake: stale.intake,
        projection: stale.projection,
        repositoryEnabled: !stale.repository_enabled,
      })
      .catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).isConflict).toBe(true);
    // The hosted hub redacts the current revision, so there is nothing here to
    // retry with: the screen re-reads instead.
    expect((failure as AccountError).details).toBeNull();
    const fresh = await api.integration(project.id);
    expect(fresh.revision).not.toBe(stale.revision);
  });
});

describe("the first-run wizard", () => {
  it("walks the four steps and the hub recomputes readiness from what was saved", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const opened = await api.onboarding(project.id);
    expect(opened.steps.map((step) => step.name)).toEqual([
      "Repository configuration",
      "Local validation",
      "Execution runner",
      "Artifact history",
    ]);

    // Step two: the reader reports what is true on their own machine.
    const progress = await api.saveProgress({
      projectId: project.id,
      key: "progress-1",
      revision: opened.progress.revision,
      repository: "existing",
      doctor: true,
      provider: true,
      artifacts: "",
    });
    expect(progress.revision).not.toBe(opened.progress.revision);
    const afterValidation = await api.onboarding(project.id);
    expect(
      afterValidation.steps.find((step) => step.name === "Local validation")?.state,
    ).toBe("ready");

    // Step four: local history needs no service.
    await api.saveProgress({
      projectId: project.id,
      key: "progress-2",
      revision: progress.revision,
      repository: "existing",
      doctor: true,
      provider: true,
      artifacts: "local",
    });
    const afterArtifacts = await api.onboarding(project.id);
    expect(
      afterArtifacts.steps.find((step) => step.name === "Artifact history")?.state,
    ).toBe("ready");
    expect(afterArtifacts.ready).toBe(
      afterArtifacts.steps.every((step) => step.state === "ready"),
    );
  });

  it("refuses a progress save with a stale revision", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const opened = await api.onboarding(project.id);
    await api.saveProgress({
      projectId: project.id,
      key: "progress-3",
      revision: opened.progress.revision,
      repository: "",
      doctor: true,
      provider: false,
      artifacts: "",
    });
    const failure = await api
      .saveProgress({
        projectId: project.id,
        key: "progress-4",
        revision: opened.progress.revision,
        repository: "",
        doctor: true,
        provider: true,
        artifacts: "",
      })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).isConflict).toBe(true);
  });

  it("refuses a repository choice the hub does not have", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const opened = await api.onboarding(project.id);
    const failure = await api
      .saveProgress({
        projectId: project.id,
        key: "progress-5",
        revision: opened.progress.revision,
        repository: "invented",
        doctor: false,
        provider: false,
        artifacts: "",
      })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(422);
  });

  it("hands back a one-time enrollment token and refuses the same identity twice", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const enrollment = await api.enrollRunner({
      projectIds: [project.id],
      runnerId: "rnr_fresh",
      machineId: "mac_fresh",
    });
    expect(enrollment.token.length).toBeGreaterThan(0);
    expect(Date.parse(enrollment.expires_at)).not.toBeNaN();
    const failure = await api
      .enrollRunner({ projectIds: [project.id], runnerId: "rnr_fresh", machineId: "mac_fresh" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(409);
    expect((failure as AccountError).code).toBe("identity_collision");
  });

  // Settings → Providers & runners enrolls a host for every project the actor
  // can read, not just the one a wizard happens to be standing in. The mock
  // records what it was sent so the widening is asserted on the request, not
  // only on the 201 that comes back.
  it("enrolls one host across every chosen project", async () => {
    await reset();
    const ids = bootstrap.projects.map((project) => project.id);
    expect(ids.length).toBeGreaterThan(1);
    const enrollment = await api.enrollRunner({
      projectIds: ids,
      runnerId: "runner_" + "a".repeat(32),
      machineId: "machine_" + "b".repeat(32),
      operations: ["read", "collaborate", "claim", "heartbeat", "events"],
      ttlSeconds: 900,
    });
    expect(enrollment.id.length).toBeGreaterThan(0);
    expect(enrollment.token.length).toBeGreaterThan(0);
    const sent = hub.lastEnrollment();
    expect(sent?.project_ids).toEqual(ids);
    expect(sent?.operations).toEqual(["read", "collaborate", "claim", "heartbeat", "events"]);
    expect(sent?.ttl_seconds).toBe(900);
    // The one-time token is what the host needs, and the command the dialog
    // shows is the command the CLI actually parses.
    expect(enrollCommand(enrollment.token, bootstrap.organization.id)).toBe(
      `DETENT_RUNNER_ENROLLMENT_TOKEN=${enrollment.token} detent hub runner enroll --organization ${bootstrap.organization.id}`,
    );
  });

  it("creates the first issue once, however often the same body is sent", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const state = project.states[0]!.name;
    const first = await api.createFirstIssue({
      projectId: project.id,
      title: "Set the lock renewal right",
      body: "Renewal returns before the handoff completes.",
      state,
      key: "issue-1",
    });
    const again = await api.createFirstIssue({
      projectId: project.id,
      title: "Set the lock renewal right",
      body: "Renewal returns before the handoff completes.",
      state,
      key: "issue-1",
    });
    expect(again).toEqual(first);
  });

  it("refuses a lane the project does not have", async () => {
    await reset();
    const project = bootstrap.projects[0]!;
    const failure = await api
      .createFirstIssue({
        projectId: project.id,
        title: "Wrong lane",
        body: "",
        state: "Nowhere",
        key: "issue-2",
      })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(422);
  });
});

describe("the wizard's persisted keys", () => {
  it("mints one key per path and body, and reuses it for an identical retry", async () => {
    clearSetupKeys();
    const body = { progress: { revision: "3", doctor: true } };
    const first = await setupKey("/api/v2/organizations/org/projects/p/onboarding", body);
    const again = await setupKey("/api/v2/organizations/org/projects/p/onboarding", body);
    expect(again.key).toBe(first.key);
    expect(again.storageKey).toBe(first.storageKey);
    expect(first.storageKey.startsWith(SETUP_KEY_PREFIX)).toBe(true);
    expect(globalThis.sessionStorage.getItem(first.storageKey)).toBe(first.key);
  });

  it("mints a different key once a field changes, because that is a different command", async () => {
    clearSetupKeys();
    const path = "/api/v2/organizations/org/projects/p/onboarding";
    const first = await setupKey(path, { progress: { doctor: true } });
    const edited = await setupKey(path, { progress: { doctor: false } });
    expect(edited.key).not.toBe(first.key);
    expect(edited.storageKey).not.toBe(first.storageKey);
  });

  it("keeps the key until the hub answers, so an unknown outcome is retried under it", async () => {
    clearSetupKeys();
    const path = "/api/v2/organizations/org/projects/p/work-items";
    const body = { title: "one" };
    const { key, storageKey } = await setupKey(path, body);
    // The request failed and nothing retired the key.
    expect(globalThis.sessionStorage.getItem(storageKey)).toBe(key);
    const retry = await setupKey(path, body);
    expect(retry.key).toBe(key);
    // Now the hub answered.
    retireKey(storageKey);
    expect(globalThis.sessionStorage.getItem(storageKey)).toBeNull();
    const afterward = await setupKey(path, body);
    expect(afterward.key).not.toBe(key);
  });

  it("scopes a key to its path", async () => {
    clearSetupKeys();
    const body = { title: "same" };
    const one = await setupKey("/a/work-items", body);
    const two = await setupKey("/b/work-items", body);
    expect(two.key).not.toBe(one.key);
  });
});

describe("support and the organization switch", () => {
  it("opens a support session and the bootstrap says so", async () => {
    await reset();
    const { support } = await api.startSupport({ key: "support-1" });
    expect(support.actor.length).toBeGreaterThan(0);
    await loadBootstrap(hub.url);
    expect(lastAccountBootstrap()?.support?.actor).toBe(support.actor);
    // Billing is closed while somebody is impersonating.
    const failure = await api.billing().catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(403);
    await reset();
  });

  it("names where the browser goes to switch organization", async () => {
    await reset();
    const other = bootstrap.organizations.find((organization) => !organization.current)!;
    const { next } = await api.switchOrganization({
      organization: other.id,
      key: "switch-1",
    });
    expect(next).toContain("/auth/oidc/start");
  });

  it("is 404 for an organization the actor does not belong to", async () => {
    await reset();
    const failure = await api
      .switchOrganization({ organization: "org_elsewhere", key: "switch-2" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(404);
  });

  it("accepts a valid invitation token and refuses an invented one", async () => {
    await reset();
    const failure = await api
      .acceptInvitation({ token: "invented", key: "accept-1" })
      .catch((cause: unknown) => cause);
    expect((failure as AccountError).status).toBe(404);
  });
});
