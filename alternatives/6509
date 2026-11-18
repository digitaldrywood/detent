import React from "react";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { ClientContext } from "../../src/app/client.ts";
import { accountApiFor } from "../../src/app/account/context.ts";
import { decodeAccountBootstrap } from "../../src/contracts/account.ts";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { NativeIssue } from "../../src/contracts/work.ts";
import type { AccessKey, OAuthGrant } from "../../src/contracts/accessKeys.ts";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import {
  ReporterOutlet,
  RoleSidebar,
  ReporterLanding,
} from "../../src/app/reporter/ReporterShell.tsx";
import {
  ReporterIssueGroups,
  readMyIssues,
  MyIssues,
} from "../../src/app/reporter/MyIssues.tsx";
import {
  ReporterEditForm,
  ReporterIssue,
  reportSections,
} from "../../src/app/reporter/ReporterIssue.tsx";
import { ConnectAI } from "../../src/app/reporter/ConnectAI.tsx";
import {
  keyInWorkspace,
  readConnections,
} from "../../src/app/reporter/connections.ts";
import { workHttpFor } from "../../src/app/work/lib/useWork.ts";
import { resetHubPaths, applyHubPaths } from "../../src/runtime/basePath.ts";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  resetHubPaths();
});
beforeEach(() => {
  window.localStorage.clear();
});

it("uses the readable workspace route base while API paths use the organization ID", async () => {
  applyHubPaths({ base_path: "/organizations/org_example" });
  const meta = document.createElement("meta");
  meta.name = "detent-router-base";
  meta.content = "/example-support";
  document.head.append(meta);
  try {
    const root = createRootRoute({ component: ReporterOutlet });
    const issues = createRoute({
      getParentRoute: () => root,
      path: "/my-issues",
      component: () => <p>Readable reports</p>,
    });
    const router = createRouter({
      routeTree: root.addChildren([issues]),
      basepath: "/example-support",
      history: createMemoryHistory({
        initialEntries: ["/example-support/my-issues"],
      }),
    });
    render(<RouterProvider router={router as never} />);
    expect(await screen.findByText("Readable reports")).toBeTruthy();
    expect(router.history.location.pathname).toBe("/example-support/my-issues");
  } finally {
    meta.remove();
  }
});

it("keeps quoted template headings and attachment examples inside code blocks", () => {
  const parsed = reportSections(
    "## What happened\n\nExample:\n~~~md\n## What you expected\n![Example](attachment:att_code)\n~~~\n\n## What you expected\n\nA download.\n\n![Screenshot](attachment:att_image)",
    ["What happened", "What you expected"],
  );
  expect(parsed.sections.get("What happened")).toBe(
    "Example:\n~~~md\n## What you expected\n![Example](attachment:att_code)\n~~~",
  );
  expect(parsed.sections.get("What you expected")).toBe("A download.");
  expect(parsed.remaining).toBe("![Screenshot](attachment:att_image)");
});

it("filters My issues and sends Cancel and verification with the loaded revision", async () => {
  const current = client();
  const http = workHttpFor(current);
  const projectId = current.account!.projects[0]!.id;
  const submitted = { ...issue, project_id: projectId };
  const done = {
    ...issue,
    project_id: projectId,
    work_item_id: "wi_done",
    title: "Search accents",
    reporter_status: "done",
  } as NativeIssue;
  vi.spyOn(http, "listWorkItems").mockImplementation(
    async ({ projectId: requested }) => ({
      items: requested === projectId ? [submitted, done] : [],
    }),
  );
  vi.spyOn(http, "transition").mockResolvedValue({
    ...submitted,
    reporter_status: "cancelled",
  });
  vi.spyOn(http, "verifyFix").mockResolvedValue({
    ...done,
    verification: { state: "verified" },
  });
  mount(<MyIssues />, current);
  await screen.findByRole("link", { name: "Export does nothing" });
  const user = userEvent.setup();
  await user.type(
    screen.getByRole("searchbox", { name: "Search my issues" }),
    "Export",
  );
  expect(screen.queryByRole("link", { name: "Search accents" })).toBeNull();
  await user.clear(screen.getByRole("searchbox", { name: "Search my issues" }));
  await user.click(
    screen.getByRole("button", { name: "More for Export does nothing" }),
  );
  await user.click(await screen.findByRole("menuitem", { name: "Cancel" }));
  await waitFor(() =>
    expect(http.transition).toHaveBeenCalledWith(
      expect.objectContaining({
        itemId: submitted.work_item_id,
        projectId,
        expectedRevision: "1",
        state: "Cancelled",
      }),
    ),
  );
  await user.click(screen.getByRole("button", { name: "Confirm it's fixed" }));
  await waitFor(() =>
    expect(http.verifyFix).toHaveBeenCalledWith(
      expect.objectContaining({
        itemId: "wi_done",
        projectId,
        expectedRevision: "1",
        result: "verified",
      }),
    ),
  );
});

it("manages returning connections and clears the shown-once key after Done", async () => {
  const current = client();
  const api = accountApiFor(current);
  vi.spyOn(api, "personalKeys").mockResolvedValue({ keys: [key] });
  vi.spyOn(api, "oauthGrants").mockResolvedValue({ grants: [grant] });
  vi.spyOn(api, "revokePersonalKey").mockResolvedValue(undefined);
  vi.spyOn(api, "revokeOAuthGrant").mockResolvedValue(undefined);
  vi.spyOn(api, "createPersonalKey").mockResolvedValue({
    ...key,
    token: "detent_test_new_key",
  });
  mount(<ConnectAI />, current, "/connect-ai");
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Disconnect" }));
  expect(api.revokeOAuthGrant).toHaveBeenCalledWith("grant_example");
  await user.click(screen.getByRole("button", { name: "Revoke" }));
  expect(api.revokePersonalKey).toHaveBeenCalledWith("key_example");
  await user.click(screen.getByRole("button", { name: "Create key" }));
  await user.type(
    screen.getByRole("textbox", { name: "Key name" }),
    "Work agent",
  );
  await user.click(screen.getByRole("button", { name: "Create key" }));
  await screen.findByText(/DETENT_API_KEY: detent_test_new_key/);
  expect(api.createPersonalKey).toHaveBeenCalledWith(
    expect.objectContaining({ name: "Work agent" }),
  );
  await user.click(screen.getByRole("button", { name: "Done" }));
  expect(screen.queryByText(/detent_test_new_key/)).toBeNull();
  await user.click(screen.getByRole("button", { name: "Connect another" }));
  expect(
    (await screen.findByRole("tab", { name: "ChatGPT" })).getAttribute(
      "aria-selected",
    ),
  ).toBe("true");
  await user.click(screen.getByRole("tab", { name: "Claude" }));
  expect(
    await screen.findByText(/Click Connect and sign in to Detent/),
  ).toBeTruthy();
  expect(api.createPersonalKey).toHaveBeenCalledTimes(1);
});

function client(role = "reporter") {
  const account = decodeAccountBootstrap({
    ...accountFixture,
    actor: { ...accountFixture.actor, role },
  });
  return {
    account,
    bootstrap: account,
    http: { origin: "", apiBase: account.api_base, csrfToken: "csrf" },
  } as unknown as ConversationClient;
}

function mount(
  element: React.ReactElement,
  current = client(),
  path = "/my-issues",
) {
  const root = createRootRoute({ component: () => element });
  const routes = [
    "/my-issues",
    "/connect-ai",
    "/reporter",
    "/work",
    "/work/i/$workItemId",
    "/settings/keys",
    "/chat",
    "/work/changes",
    "/activity",
    "/reports",
    "/diagnostics",
    "/usage",
  ].map((route) =>
    createRoute({
      getParentRoute: () => root,
      path: route,
      component: () => (
        <p>
          {route === "/my-issues"
            ? "Your reports"
            : route === "/connect-ai"
              ? "Assistant setup"
              : "Hidden content"}
        </p>
      ),
    }),
  );
  const router = createRouter({
    routeTree: root.addChildren(routes),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <ClientContext.Provider value={current}>
      <SidebarProvider>
        <RouterProvider router={router as never} />
      </SidebarProvider>
    </ClientContext.Provider>,
  );
  return { router, api: accountApiFor(current), http: workHttpFor(current) };
}

const key = {
  id: "key_example",
  name: "Work laptop",
  kind: "personal",
  owner: "person_example",
  owner_email: "reporter@example.test",
  permission: "write",
  access_context: "selected",
  organizations: [
    { organization_id: "org_example", project_access: "all", project_ids: [] },
  ],
  created_at: "2026-10-01T00:00:00Z",
} satisfies AccessKey;
const grant = {
  id: "grant_example",
  client_id: "client_example",
  client_name: "ChatGPT",
  organization_id: "org_example",
  connected_at: "2026-10-01T00:00:00Z",
  revoked_at: null,
  last_used_at: null,
} satisfies OAuthGrant;
const issue = {
  organization_id: "org_example",
  project_id: "proj_example",
  work_item_id: "wi_example",
  number: 12,
  revision: "1",
  profile: "native",
  web_url: "/work/i/wi_example",
  terminal: false,
  labels: [],
  assignees: [],
  dependencies: [],
  blockers: [],
  external_references: [],
  actor: { kind: "human", principal_id: "person_example" },
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
  reporter_status: "submitted",
  state: "Triage",
  type: "bug",
  title: "Export does nothing",
  body: "## What happened\n\nNothing downloaded.\n\n## What you expected\n\nA CSV download.",
} satisfies NativeIssue;

it.each(["owner", "admin", "member", "viewer", "reporter"])(
  "selects sidebar contents from the current workspace role: %s",
  async (role) => {
    mount(
      <RoleSidebar>
        <nav>Work Chat Browse Settings Pull Requests Usage</nav>
      </RoleSidebar>,
      client(role),
    );
    if (role === "reporter") {
      expect(
        await screen.findByRole("link", { name: "My issues" }),
      ).toBeTruthy();
      expect(screen.getByRole("link", { name: "Connect AI" })).toBeTruthy();
      expect(screen.getByRole("combobox", { name: "Project" })).toBeTruthy();
      expect(
        screen.queryByText("Work Chat Browse Settings Pull Requests Usage"),
      ).toBeNull();
      expect(
        screen.queryByRole("searchbox", { name: "Search threads" }),
      ).toBeNull();
      expect(screen.queryByRole("button", { name: "Add project" })).toBeNull();
      const user = userEvent.setup();
      await user.click(
        screen.getByRole("button", { name: /Switch workspace/ }),
      );
      expect(
        await screen.findByRole("menuitem", { name: "Sign out" }),
      ).toBeTruthy();
      expect(
        screen.getByRole("menuitem", { name: "Add a workspace" }),
      ).toBeTruthy();
    } else
      expect(
        await screen.findByText(
          "Work Chat Browse Settings Pull Requests Usage",
        ),
      ).toBeTruthy();
  },
);

it.each([
  "/work",
  "/chat",
  "/settings/keys",
  "/activity",
  "/reports",
  "/diagnostics",
  "/usage",
  "/work/changes",
])(
  "redirects the hidden route %s before displaying its screen",
  async (path) => {
    const { router } = mount(<ReporterOutlet />, client(), path);
    expect(await screen.findByText("Your reports")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/my-issues");
    expect(screen.queryByText("Hidden content")).toBeNull();
  },
);

it.each([
  { keys: [], grants: [], destination: "/connect-ai" },
  { keys: [key], grants: [], destination: "/my-issues" },
  { keys: [], grants: [grant], destination: "/my-issues" },
])(
  "lands a reporter at $destination using Hub connection state",
  async ({ keys, grants, destination }) => {
    const current = client();
    const api = accountApiFor(current);
    vi.spyOn(api, "personalKeys").mockResolvedValue({ keys });
    vi.spyOn(api, "oauthGrants").mockResolvedValue({ grants });
    const root = createRootRoute({ component: ReporterOutlet });
    const home = createRoute({
      getParentRoute: () => root,
      path: "/reporter",
      component: ReporterLanding,
    });
    const issues = createRoute({
      getParentRoute: () => root,
      path: "/my-issues",
      component: () => <p>Reports destination</p>,
    });
    const connect = createRoute({
      getParentRoute: () => root,
      path: "/connect-ai",
      component: () => <p>Connect destination</p>,
    });
    const router = createRouter({
      routeTree: root.addChildren([home, issues, connect]),
      history: createMemoryHistory({ initialEntries: ["/reporter"] }),
    });
    render(
      <ClientContext.Provider value={current}>
        <RouterProvider router={router as never} />
      </ClientContext.Provider>,
    );
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(destination),
    );
  },
);

it("reads every connection page and excludes another workspace, expired and revoked keys and grants", async () => {
  const api = accountApiFor(client());
  vi.spyOn(api, "personalKeys")
    .mockResolvedValueOnce({
      keys: [
        {
          ...key,
          organizations: [
            { ...key.organizations[0]!, organization_id: "org_other" },
          ],
        },
      ],
      next_cursor: "key_page",
    })
    .mockResolvedValueOnce({
      keys: [
        key,
        { ...key, revoked_at: "2026-01-01T00:00:00Z" },
        { ...key, expires_at: "2000-01-01T00:00:00Z" },
      ],
    });
  vi.spyOn(api, "oauthGrants")
    .mockResolvedValueOnce({
      grants: [{ ...grant, organization_id: "org_other" }],
      next_cursor: "grant_page",
    })
    .mockResolvedValueOnce({
      grants: [grant, { ...grant, revoked_at: "2026-01-01T00:00:00Z" }],
    });
  expect(await readConnections(api, "org_example")).toEqual({
    keys: [key],
    grants: [grant],
  });
  expect(api.personalKeys).toHaveBeenLastCalledWith("key_page");
  expect(api.oauthGrants).toHaveBeenLastCalledWith("grant_page");
  expect(
    keyInWorkspace({ ...key, access_context: "global" }, "org_other"),
  ).toBe(true);
});

it("groups Hub statuses, collapses Cancelled and exposes Submitted editing and Done verification", async () => {
  const items = [
    "submitted",
    "planned",
    "in_progress",
    "done",
    "cancelled",
  ].map(
    (status, index) =>
      ({
        ...issue,
        reporter_status: status,
        work_item_id: `wi_${index}`,
        title: `Report ${status}`,
      }) as NativeIssue,
  );
  items.push({
    ...issue,
    reporter_status: "done",
    work_item_id: "wi_verified",
    title: "Verified report",
    verification: { state: "verified" },
  });
  const verify = vi.fn();
  mount(
    <ReporterIssueGroups
      items={items}
      projectName={() => "Example project"}
      pending={false}
      onCancel={vi.fn()}
      onVerify={verify}
    />,
  );
  const submitted = await screen.findByRole("region", { name: "Submitted" });
  expect(
    within(submitted).getByRole("link", { name: "Edit Report submitted" }),
  ).toBeTruthy();
  expect(
    within(screen.getByRole("region", { name: "Planned" })).queryByRole(
      "link",
      { name: /Edit/ },
    ),
  ).toBeNull();
  expect(screen.queryByText("Report cancelled")).toBeNull();
  expect(screen.getByText("You confirmed")).toBeTruthy();
  await userEvent
    .setup()
    .click(screen.getByRole("button", { name: "Confirm it's fixed" }));
  expect(verify).toHaveBeenCalledWith(items[3]);
  await userEvent
    .setup()
    .click(screen.getByRole("button", { name: "Cancelled · 1" }));
  expect(await screen.findByText("Report cancelled")).toBeTruthy();
});

it("saves template sections and preserves attachments, with Discard restoring the loaded issue", async () => {
  const save = vi.fn(async (_title: string, _body: string) => {});
  mount(
    <ReporterEditForm
      issue={{
        ...issue,
        body: issue.body + "\n\n![Screenshot](attachment:att_example)",
      }}
      headings={["What happened", "What you expected"]}
      onSave={save}
    />,
  );
  const title = await screen.findByRole("textbox", { name: "Title" });
  fireEvent.change(title, { target: { value: "Changed title" } });
  fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  expect((title as HTMLInputElement).value).toBe("Export does nothing");
  fireEvent.change(screen.getByRole("textbox", { name: "What happened" }), {
    target: { value: "Export stopped" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() =>
    expect(save).toHaveBeenCalledWith(
      "Export does nothing",
      expect.stringContaining("## What happened\n\nExport stopped"),
    ),
  );
  expect(save.mock.calls[0]?.[1]).toContain("attachment:att_example");
});

it.each(["submitted", "planned", "in_progress", "done", "cancelled"])(
  "offers the issue text form only in Submitted (%s)",
  async (status) => {
    const current = client();
    const http = workHttpFor(current);
    const api = accountApiFor(current);
    vi.spyOn(http, "getWorkItemById").mockResolvedValue({
      ...issue,
      reporter_status: status,
    } as NativeIssue);
    vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "reporterTemplates").mockResolvedValue({
      reporter_templates: {
        bug: ["What happened", "What you expected"],
        change_request: ["What you want"],
      },
    });
    mount(<ReporterIssue />, current, "/work/i/wi_example");
    await screen.findByText(
      /^(Submitted|Planned|In progress|Done|Cancelled) ·/,
    );
    if (status === "submitted")
      expect(
        await screen.findByRole("textbox", { name: "Title" }),
      ).toBeTruthy();
    else expect(screen.queryByRole("textbox", { name: "Title" })).toBeNull();
  },
);

it("creates a workspace write key and copies the actual token only in reporter agent instructions", async () => {
  const current = client();
  const api = accountApiFor(current);
  vi.spyOn(api, "personalKeys").mockResolvedValue({ keys: [] });
  vi.spyOn(api, "oauthGrants").mockResolvedValue({ grants: [] });
  vi.spyOn(api, "createPersonalKey").mockResolvedValue({
    ...key,
    token: "detent_test_secret",
  });
  mount(<ConnectAI />, current, "/connect-ai");
  const user = userEvent.setup();
  const clipboard = vi
    .spyOn(navigator.clipboard, "writeText")
    .mockResolvedValue();
  expect(
    (await screen.findByRole("tab", { name: "ChatGPT" })).getAttribute(
      "aria-selected",
    ),
  ).toBe("true");
  await user.click(
    screen.getByRole("tab", { name: "Claude Code, Codex, other agents" }),
  );
  await user.click(screen.getByRole("button", { name: "Create my key" }));
  await screen.findByText(/DETENT_API_KEY: detent_test_secret/);
  expect(api.createPersonalKey).toHaveBeenCalledWith(
    expect.objectContaining({
      permission: "write",
      access_context: "selected",
      organizations: [
        {
          organization_id: "org_example",
          project_access: "all",
          project_ids: [],
        },
      ],
    }),
  );
  await user.click(screen.getByRole("button", { name: "Copy instructions" }));
  expect(clipboard).toHaveBeenCalledOnce();
  const instructions = clipboard.mock.calls[0]?.[0];
  expect(instructions).toContain("DETENT_API_KEY: detent_test_secret");
  expect(instructions).toContain("https://cloud.detent.build/mcp");
  expect(instructions).toContain(
    "Authorization: Bearer $DETENT_API_KEY",
  );
});

it("loads only the reporter's issues across granted projects and all result pages", async () => {
  const http = workHttpFor(client());
  vi.spyOn(http, "listWorkItems").mockImplementation(
    async ({ projectId, cursor }) => ({
      items: [
        {
          ...issue,
          project_id: projectId,
          work_item_id: `wi_${projectId}_${cursor ?? "first"}`,
        },
      ],
      ...(!cursor ? { next_cursor: "next" } : {}),
    }),
  );
  const items = await readMyIssues(http, [
    { id: "project_a" },
    { id: "project_b" },
  ]);
  expect(items.map((item) => item.work_item_id).sort()).toEqual([
    "wi_project_a_first",
    "wi_project_a_next",
    "wi_project_b_first",
    "wi_project_b_next",
  ]);
  expect(http.listWorkItems).toHaveBeenCalledWith(
    expect.objectContaining({ projectId: "project_a", createdBy: "me" }),
  );
  expect(http.listWorkItems).toHaveBeenCalledWith(
    expect.objectContaining({
      projectId: "project_b",
      createdBy: "me",
      cursor: "next",
    }),
  );
});
