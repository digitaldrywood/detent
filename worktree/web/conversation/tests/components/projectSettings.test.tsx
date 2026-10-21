// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createBrowserHistory, createMemoryHistory, createRootRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import "../../src/app/routes.account.screens.tsx";
import { accountRoutes } from "../../src/app/routes.account.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import { AccountError } from "../../src/app/account/api.ts";
import { projectSettingsSearchItems, searchSettings } from "../../src/components/settings/settingsSearch.ts";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import integration from "../../src/contracts/fixtures/account-integration.json";
import onboarding from "../../src/contracts/fixtures/account-onboarding.json";
import policy from "../../src/contracts/fixtures/account-policy.json";

let destroyHistory: (() => void) | undefined;

const api = vi.hoisted(() => ({ integration: vi.fn(), policy: vi.fn(), onboarding: vi.fn(), slug: vi.fn(), saveSlug: vi.fn(), saveIntegration: vi.fn(), modelSelection: vi.fn(), spritesSecret: vi.fn() }));
const account = { ...accountFixture, projects: [
  { ...accountFixture.projects[0]!, slug: "example-project" },
  { ...accountFixture.projects[0]!, id: "proj_second", slug: "second-project", name: "Second project" },
] };
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => account }));

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("PointerEvent", MouseEvent);
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) { return selector === ":fullscreen" ? false : matches.call(this, selector); });
  api.integration.mockResolvedValue(integration);
  api.policy.mockResolvedValue(policy);
  api.onboarding.mockResolvedValue({ ...onboarding, ready: true });
  api.slug.mockResolvedValue({ slug: "example-project" });
  api.modelSelection.mockResolvedValue({ revision: "1", selection: null, effective: { levels: {}, stages: {} } });
  api.spritesSecret.mockResolvedValue({ present: false });
  api.saveIntegration.mockImplementation(async () => integration);
});
afterEach(() => { cleanup(); destroyHistory?.(); destroyHistory = undefined; vi.restoreAllMocks(); vi.unstubAllGlobals(); });

async function mount(path: string, entries = [path]) {
  if (entries.length > 1) window.history.replaceState(null, "", entries[0]);
  const history = entries.length > 1 ? createBrowserHistory() : createMemoryHistory({ initialEntries: entries });
  if (entries.length > 1) {
    history.push(entries[1]!);
    history.flush();
    destroyHistory = history.destroy;
  }
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({ routeTree: root.addChildren(accountRoutes(root)), history });
  await router.load();
  await act(async () => { render(<ClientContext.Provider value={{ account, bootstrap: { organization: account.organization, preferences: { models: [] } } } as unknown as ConversationClient}><RouterProvider router={router} /></ClientContext.Provider>); });
  return { router, history, user: userEvent.setup() };
}

async function dirtyGeneral() {
  const toggle = await screen.findByRole("switch", { name: "Check for duplicate issues" });
  fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-checked")).toBe("false");
  return toggle;
}

describe("project settings URLs and sections", () => {
  it.each([
    ["general", "General", "Archive completed issues"],
    ["repository", "Repository & GitHub", "Pull requests"],
    ["workflow", "Workflow", "Project states"],
    ["models", "Models", "Override organization model selection"],
    ["runners", "Runners & Sprites", "Sprites project override"],
    ["danger", "Danger zone", "Delete project"],
  ])("renders %s at its own URL", async (tab, label, content) => {
    const { router } = await mount(`/settings/projects/example-project/${tab}`);
    expect((await screen.findByRole("tab", { name: label })).getAttribute("aria-selected")).toBe("true");
    if (tab === "models") await screen.findByRole("switch", { name: content });
    else if (tab === "danger") await screen.findByRole("button", { name: content });
    else await screen.findByText(content, { exact: true });
    expect(router.state.location.pathname).toBe(`/settings/projects/example-project/${tab}`);
    if (tab !== "general") expect(screen.queryByRole("combobox", { name: "Archive completed issues" })).toBeNull();
    if (tab !== "repository") expect(screen.queryByRole("switch", { name: "Pull requests" })).toBeNull();
  });

  it.each([
    ["/settings/projects/proj_example_project/models", "models"],
    ["/settings/projects/example-project", "general"],
    ["/settings/projects/example-project/unknown", "general"],
    ["/projects/proj_example_project/settings", "general"],
    ["/projects/example-project/settings", "general"],
    ["/settings/integrations?project=proj_example_project", "general"],
    ["/settings/integrations?project=example-project", "general"],
  ])("replaces %s with the canonical URL", async (path, tab) => {
    const { router, history } = await mount(path);
    await waitFor(() => expect(router.state.location.pathname).toBe(`/settings/projects/example-project/${tab}`));
    expect(history.length).toBe(1);
  });

  it("keeps the tab strip while only the panel loads", async () => {
    let release!: (value: typeof integration) => void;
    api.integration.mockImplementation(() => new Promise((resolve) => { release = resolve; }));
    await mount("/settings/projects/example-project/repository");
    expect(await screen.findByRole("tab", { name: "Repository & GitHub" })).toBeTruthy();
    expect(await screen.findByRole("status", { name: "Loading project settings" })).toBeTruthy();
    await act(async () => release(integration));
    expect(await screen.findByRole("switch", { name: "Pull requests" })).toBeTruthy();
  });

  it.each(["unknown-project", "example-project"])("shows no access for %s", async (project) => {
    api.integration.mockRejectedValue(new AccountError({ status: 404, code: "not_found", message: "Not found" }));
    await mount(`/settings/projects/${project}/general`);
    expect(await screen.findByText("You do not have access to this project")).toBeTruthy();
    expect(screen.queryByRole("switch", { name: "Check for duplicate issues" })).toBeNull();
  });

  it("puts unfinished setup above the strip", async () => {
    api.onboarding.mockResolvedValue({ ...onboarding, ready: false });
    await mount("/settings/projects/example-project/general");
    const finish = await screen.findByRole("button", { name: "Finish setup" });
    expect(finish.compareDocumentPosition(screen.getByRole("tablist")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps Integrations organization-only", async () => {
    const { router } = await mount("/settings/integrations");
    await waitFor(() => expect(router.state.status).toBe("idle"));
    expect(router.state.location.pathname).toBe("/settings/integrations");
    expect(screen.queryByRole("combobox", { name: "Project" })).toBeNull();
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(api.integration).not.toHaveBeenCalled();
  });
});

describe("unsaved project settings navigation", () => {
  it("keeps edits on cancel, discards on navigation, and switches clean tabs freely", async () => {
    const { router, user } = await mount("/settings/projects/example-project/general");
    const toggle = await dirtyGeneral();
    await user.click(screen.getByRole("tab", { name: "Repository & GitHub" }));
    let dialog = await screen.findByRole("alertdialog", { name: "Discard unsaved changes?" });
    expect(dialog.textContent).toContain("Your edits on General are not saved.");
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe("/settings/projects/example-project/general");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    await user.click(screen.getByRole("tab", { name: "Repository & GitHub" }));
    dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: "Discard" }));
    await screen.findByRole("switch", { name: "Pull requests" });
    expect(router.state.location.pathname).toBe("/settings/projects/example-project/repository");
    await user.click(screen.getByRole("tab", { name: "General" }));
    expect((await screen.findByRole("switch", { name: "Check for duplicate issues" })).getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it.each(["back", "forward"] as const)("guards browser %s", async (direction) => {
    const current = "/settings/projects/example-project/general";
    const other = "/settings/projects/example-project/repository";
    const { router, history, user } = await mount(current, direction === "back" ? [other, current] : [current, other]);
    if (direction === "forward") { act(() => history.back()); await waitFor(() => expect(router.state.location.pathname).toBe(current)); }
    await dirtyGeneral();
    act(() => history[direction]());
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe(current);
    act(() => history[direction]());
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(other));
  });

  it("guards a project switch and preserves its tab", async () => {
    const { router, user } = await mount("/settings/projects/example-project/general");
    await dirtyGeneral();
    await user.click(screen.getByRole("combobox", { name: "Project" }));
    await user.click(await screen.findByRole("option", { name: "Second project" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/projects/second-project/general"));
    await user.click(screen.getByRole("tab", { name: "Runners & Sprites" }));
    await user.click(screen.getByRole("combobox", { name: "Project" }));
    await user.click(await screen.findByRole("option", { name: "example-project" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/projects/example-project/runners"));
  });

  it.each([
    ["general", "Project slug", "textbox"],
    ["workflow", "Workflow definition", "textbox"],
    ["runners", "Sprites organization token", "textbox"],
    ["models", "Override organization model selection", "switch"],
  ])("registers unsaved edits from the %s editor", async (tab, label, role) => {
    const { user } = await mount(`/settings/projects/example-project/${tab}`);
    const control = role === "textbox" ? await screen.findByLabelText(label) : await screen.findByRole(role, { name: label });
    if (role === "textbox") await waitFor(() => expect((control as HTMLInputElement).disabled).toBe(false));
    if (role === "textbox") fireEvent.change(control, { target: { value: "unsaved-edit" } });
    else await user.click(control);
    await user.click(screen.getByRole("tab", { name: "Repository & GitHub" }));
    expect(await screen.findByRole("alertdialog", { name: "Discard unsaved changes?" })).toBeTruthy();
  });

  it("clears the guard after an integration save", async () => {
    api.saveIntegration.mockResolvedValue({ ...integration, duplicate_challenge: false });
    const { user } = await mount("/settings/projects/example-project/general");
    await dirtyGeneral();
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.saveIntegration).toHaveBeenCalledWith(expect.objectContaining({ projectId: "proj_example_project", duplicateChallenge: false, archiveCompletedAfterDays: 30 })));
    await waitFor(() => expect((screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled).toBe(true));
    await user.click(screen.getByRole("tab", { name: "Workflow" }));
    expect(await screen.findByText("Project states")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});

it("finds each project's settings on its matching tab", () => {
  const items = projectSettingsSearchItems(account.projects);
  for (const [query, tab] of [["repository", "repository"], ["routing", "runners"], ["workflow", "workflow"], ["models", "models"], ["archive", "general"], ["delete", "danger"]]) {
    expect(searchSettings(query!, items).map((item) => item.to)).toContain(`/settings/projects/second-project/${tab}`);
  }
});
