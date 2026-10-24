// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createBrowserHistory, createMemoryHistory, createRootRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import "../../src/app/routes.account.screens.tsx";
import { ProjectIconPickerRow } from "../../src/app/account/ProjectIconSettings.tsx";
import { ProjectFavicon } from "../../src/components/ProjectFavicon.tsx";
import { ProjectGlyph } from "../../src/app/components/ProjectGlyph.tsx";
import { toEnvironmentProject } from "../../src/app/adapters/shell.ts";
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

const api = vi.hoisted(() => ({ integration: vi.fn(), policy: vi.fn(), onboarding: vi.fn(), slug: vi.fn(), saveSlug: vi.fn(), saveIntegration: vi.fn(), modelSelection: vi.fn(), spritesSecret: vi.fn(), renameProject: vi.fn() }));
const account = { ...accountFixture, projects: [
  { ...accountFixture.projects[0]!, slug: "example-project" },
  { ...accountFixture.projects[0]!, id: "proj_second", slug: "second-project", name: "Second project" },
] };
vi.mock("../../src/app/client.ts", async (importOriginal) => ({ ...await importOriginal<typeof import("../../src/app/client.ts")>(), useRefreshBootstrap: () => async () => undefined }));
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => account }));

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("PointerEvent", MouseEvent);
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) { return selector === ":fullscreen" ? false : matches.call(this, selector); });
  account.actor = accountFixture.actor;
  api.integration.mockResolvedValue(integration);
  api.renameProject.mockResolvedValue({ ...integration, name: "Renamed project", revision: "2" });
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
    ["workflow", "Workflow", "Lanes"],
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
    if (tab === "runners") await user.click(await screen.findByRole("button", { name: "Set override" }));
    const control = role === "textbox" ? await screen.findByLabelText(label) : await screen.findByRole(role, { name: label });
    if (role === "textbox") await waitFor(() => expect((control as HTMLInputElement).disabled).toBe(false));
    if (role === "textbox") fireEvent.change(control, { target: { value: "unsaved-edit" } });
    else await user.click(control);
    await user.click(screen.getByRole("tab", { name: "Repository & GitHub" }));
    expect(await screen.findByRole("alertdialog", { name: "Discard unsaved changes?" })).toBeTruthy();
    if (tab === "workflow" || tab === "runners") {
      await user.click(screen.getByRole("button", { name: "Keep editing" }));
      await user.click(screen.getByRole("button", { name: "Cancel" }));
      await user.click(screen.getByRole("tab", { name: "Repository & GitHub" }));
      await screen.findByRole("switch", { name: "Pull requests" });
      expect(screen.queryByRole("alertdialog")).toBeNull();
    }
  });

  it("clears the guard after an integration save", async () => {
    api.saveIntegration.mockResolvedValue({ ...integration, duplicate_challenge: false });
    const { user } = await mount("/settings/projects/example-project/general");
    await dirtyGeneral();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveIntegration).toHaveBeenCalledWith(expect.objectContaining({ projectId: "proj_example_project", duplicateChallenge: false, archiveCompletedAfterDays: 30 })));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Save" })).toBeNull());
    await user.click(screen.getByRole("tab", { name: "Workflow" }));
    expect(await screen.findByText("Lanes")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});

it("finds each project's settings on its matching tab", () => {
  const items = projectSettingsSearchItems(account.projects);
  for (const [query, tab] of [["repository", "repository"], ["routing", "runners"], ["workflow", "workflow"], ["models", "models"], ["archive", "general"], ["delete", "danger"]]) {
    expect(searchSettings(query!, items).map((item) => item.to)).toContain(`/settings/projects/second-project/${tab}`);
  }
});


describe("project General and Repository section saves", () => {
  it("enables Save name only for a changed nonempty name and preserves pending Issues edits", async () => {
    const { user } = await mount("/settings/projects/example-project/general");
    const name = await screen.findByRole("textbox", { name: "Project name" });
    const save = screen.getByRole("button", { name: "Save name" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(name, { target: { value: "   " } });
    expect(save.disabled).toBe(true);
    await dirtyGeneral();
    fireEvent.change(name, { target: { value: "Renamed project" } });
    expect(save.disabled).toBe(false);
    await user.click(save);
    await waitFor(() => expect(api.renameProject).toHaveBeenCalledWith(expect.objectContaining({ name: "Renamed project", revision: integration.revision })));
    await waitFor(() => expect(save.disabled).toBe(true));
    expect(screen.getByRole("switch", { name: "Check for duplicate issues" }).getAttribute("aria-checked")).toBe("false");
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
    expect(api.saveIntegration).not.toHaveBeenCalled();
  });

  it.each(["Saved elsewhere", account.projects[0]!.name])("reloads the name and settings after a conflict (%s)", async (currentName) => {
    api.renameProject.mockRejectedValue(new AccountError({ status: 409, code: "revision_conflict", message: "Conflict" }));
    const { user } = await mount("/settings/projects/example-project/general");
    const field = await screen.findByRole("textbox", { name: "Project name" }) as HTMLInputElement;
    await dirtyGeneral();
    fireEvent.change(field, { target: { value: "My edit" } });
    const concurrent = { ...integration, name: currentName, revision: "9", archive_completed_after_days: 60, archive_cancelled_after_days: 90, projection: "disabled", repository_enabled: false };
    api.integration.mockResolvedValue(concurrent);
    await user.click(screen.getByRole("button", { name: "Save name" }));
    expect(await screen.findByText("Changed by someone else. Reloaded; save again.")).toBeTruthy();
    expect(field.value).toBe(currentName);
    expect(screen.getByRole("combobox", { name: "Archive completed issues" }).textContent).toContain("After 60 days");
    expect(screen.getByRole("combobox", { name: "Archive cancelled issues" }).textContent).toContain("After 90 days");
    expect(screen.getByRole("switch", { name: "Check for duplicate issues" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    await dirtyGeneral();
    api.saveIntegration.mockResolvedValue({ ...concurrent, duplicate_challenge: false, revision: "10" });
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveIntegration).toHaveBeenLastCalledWith(expect.objectContaining({ revision: "9", duplicateChallenge: false, archiveCompletedAfterDays: 60, archiveCancelledAfterDays: 90, projection: "disabled", repositoryEnabled: false })));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Save" })).toBeNull());
    fireEvent.change(field, { target: { value: "My edit again" } });
    api.renameProject.mockResolvedValue({ ...concurrent, name: "My edit again", revision: "11" });
    await user.click(screen.getByRole("button", { name: "Save name" }));
    await waitFor(() => expect(api.renameProject).toHaveBeenLastCalledWith(expect.objectContaining({ revision: "10" })));
  });

  it("shows Issues actions only while dirty and discards the section", async () => {
    const { user } = await mount("/settings/projects/example-project/general");
    await screen.findByRole("switch", { name: "Check for duplicate issues" });
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
    await dirtyGeneral();
    await user.click(screen.getByRole("button", { name: "Discard" }));
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.getByRole("switch", { name: "Check for duplicate issues" }).getAttribute("aria-checked")).toBe("true");
    expect(api.saveIntegration).not.toHaveBeenCalled();
  });

  it("re-reads Repository settings on a section conflict", async () => {
    const { user } = await mount("/settings/projects/example-project/repository");
    const toggle = await screen.findByRole("switch", { name: "Work summaries" });
    await user.click(toggle);
    api.saveIntegration.mockRejectedValue(new AccountError({ status: 409, code: "revision_conflict", message: "Conflict" }));
    api.integration.mockResolvedValue({ ...integration, projection: "summary", revision: "8" });
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Not saved. Somebody else changed these settings. Values re-read; check and save again.")).toBeTruthy();
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it.each(["general", "repository"])("shows read-only %s values with no Save actions", async (tab) => {
    account.actor = { ...accountFixture.actor, can_manage: false };
    await mount(`/settings/projects/example-project/${tab}`);
    await screen.findByRole("heading", { name: tab === "general" ? "Identity" : "Repository & GitHub" });
    expect(screen.queryByRole("button", { name: /^Save/ })).toBeNull();
    expect(screen.queryByRole("textbox", { name: "Project name" })).toBeNull();
    for (const toggle of screen.getAllByRole("switch")) expect(toggle.getAttribute("aria-disabled")).toBe("true");
    if (tab === "general") expect(screen.queryByRole("combobox", { name: "Archive completed issues" })).toBeNull();
  });
});


describe("project icon picker", () => {
  function picker(icon: import("../../src/contracts/projectIcon.ts").ProjectIcon | null = null, canManage = true) {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<ProjectIconPickerRow projectId="proj_alpha" projectName="Example project" icon={icon} canManage={canManage} onSave={onSave} />);
    return { onSave, user: userEvent.setup() };
  }

  it("selects a searched Lucide icon and color, and saves only on Save", async () => {
    const { user, onSave } = picker();
    await user.click(screen.getByRole("button", { name: "Choose project icon" }));
    await user.type(screen.getByRole("textbox", { name: "Search icons" }), "coffee");
    await user.click(await screen.findByRole("button", { name: "coffee" }));
    await user.click(screen.getByRole("button", { name: "Violet" }));
    expect(onSave).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith({ kind: "lucide", name: "coffee", color: "violet" }, undefined);
    await waitFor(() => expect(screen.queryByRole("textbox", { name: "Search icons" })).toBeNull());
  });

  it("searches native emoji and refuses multiple graphemes", async () => {
    const { user, onSave } = picker();
    await user.click(screen.getByRole("button", { name: "Choose project icon" }));
    await user.click(screen.getByRole("tab", { name: "Emoji" }));
    await user.type(screen.getByRole("textbox", { name: "Search emoji" }), "rocket");
    await user.click(screen.getByRole("button", { name: "rocket launch space" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Emoji" }), { target: { value: "🚀🌍" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Emoji" }), { target: { value: "👨‍👩‍👧‍👦" } });
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith({ kind: "emoji", emoji: "👨‍👩‍👧‍👦" }, undefined);
  });

  it("previews a byte-validated image and disables Save for a refused replacement", async () => {
    vi.stubGlobal("createImageBitmap", vi.fn().mockResolvedValue({ close: vi.fn() }));
    const { user, onSave } = picker();
    await user.click(screen.getByRole("button", { name: "Choose project icon" }));
    await user.click(screen.getByRole("tab", { name: "Upload" }));
    const bytes = new Uint8Array(33);
    bytes.set([137,80,78,71,13,10,26,10]);
    bytes.set([73,72,68,82],12);
    new DataView(bytes.buffer).setUint32(16,64);
    new DataView(bytes.buffer).setUint32(20,64);
    const file = new File([bytes], "example.jpg", { type: "image/jpeg" });
    Object.defineProperty(file, "arrayBuffer", { value: async () => bytes.buffer });
    fireEvent.change(screen.getByLabelText("Upload project icon"), { target: { files: [file] } });
    await screen.findByAltText("64 px preview");
    expect(screen.getByAltText("32 px preview")).toBeTruthy();
    expect(screen.getByAltText("16 px preview")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.change(screen.getByLabelText("Upload project icon"), { target: { files: [new File([new Uint8Array(256 * 1024 + 1)], "too-large.png")] } });
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "too-large.png is 256.0 KB; the limit is 256 KB.");
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect(onSave).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Use automatic" }));
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("resets a chosen icon to automatic", async () => {
    const { user, onSave } = picker({ kind: "emoji", emoji: "🚀" });
    await user.click(screen.getByRole("button", { name: "Choose project icon" }));
    await user.click(screen.getByRole("button", { name: "Use automatic" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith({ kind: "automatic" }, undefined);
  });

  it("shows only the read-only tile without rename authority", () => {
    picker({ kind: "emoji", emoji: "🚀" }, false);
    expect(screen.queryByRole("button", { name: "Choose project icon" })).toBeNull();
    expect(screen.getByLabelText("Project icon").textContent).toBe("🚀");
  });

  it("carries the saved icon through the sidebar adapter and Projects table glyph", () => {
    const project = { ...account.projects[0]!, project_icon: { kind: "emoji" as const, emoji: "🧪" } };
    account.projects[0] = project;
    render(<ClientContext.Provider value={{ account } as unknown as ConversationClient}><ProjectFavicon project={toEnvironmentProject(project)} /><ProjectGlyph projectId={project.id} projectName={project.name} /></ClientContext.Provider>);
    expect(screen.getAllByText("🧪")).toHaveLength(2);
    account.projects[0] = { ...accountFixture.projects[0]!, slug: "example-project" };
  });
});
