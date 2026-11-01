// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ModelSelectionSettings } from "../../src/app/account/ModelSelectionSettings.tsx";
import { AccountError } from "../../src/app/account/api.ts";
import type { CloudModelSelection, CloudSelection } from "../../src/contracts/account.ts";

const context = vi.hoisted(() => ({ api: { modelSelection: vi.fn(), saveModelSelection: vi.fn() } }));
vi.mock("../../src/app/account/context.ts", () => ({
  useAccountApi: () => context.api,
  useAccountBootstrap: () => ({ projects: [
    { id: "project_a", labels: ["complexity:complex", "complexity:very-complex", "bug"] },
    { id: "project_b", labels: ["another-project"] },
  ] }),
}));
vi.mock("../../src/app/client.ts", () => ({ useClient: () => ({ bootstrap: { preferences: { models: [
  { id: "gpt-6.1-sol", label: "Codex Sol", provider: "codex", default: true },
  { id: "gpt-6-astra", label: "Codex Astra", provider: "codex", default: false },
  { id: "example-model", label: "Example model", provider: "example", default: false },
] } } }) }));

const selection: CloudSelection = {
  enabled: true, preset: "sol_first", normal_model: "gpt-6.1-sol", complex_model: "gpt-6-astra",
  backend_kinds: ["codex"], default_level: "normal",
  levels: {
    normal: { model: "normal", effort: "high", max_session_tokens: 1000 },
    complex: { model: "normal", effort: "high" },
    very_complex: { model: "complex", effort: "medium", max_session_duration_ms: 12000 },
  },
  stages: {
    plan: { model: "complex", effort: "low", issue_complexity: false },
    code: { issue_complexity: true },
    merge: { issue_complexity: false },
    validator: { model: "complex", effort: "medium" },
  },
  rules: [{ name: "complex_rule", level: "complex", roles: ["code"], selector: { labels: { include: ["complexity:complex"], exclude: ["excluded"] }, author_in: ["author_a"] } }],
  unavailable: "fallback", fallback_order: ["normal", "complex"],
};
let response: CloudModelSelection;
beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("PointerEvent", MouseEvent);
  response = { revision: "1", selection, effective: selection };
  context.api.modelSelection.mockImplementation(async () => response);
  context.api.saveModelSelection.mockImplementation(async (input: { selection: CloudSelection | null }) => {
    response = { revision: "2", selection: input.selection, effective: input.selection ?? selection };
    return response;
  });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

async function mount(props: { projectId?: string; canManage?: boolean } = {}) {
  render(<ModelSelectionSettings canManage={props.canManage ?? true} projectId={props.projectId} />);
  const user = userEvent.setup();
  if (props.projectId) await user.click(await screen.findByRole("button", { name: "Edit model selection" }));
  await screen.findByRole("table", { name: "Stages" });
  return user;
}
async function choose(user: ReturnType<typeof userEvent.setup>, label: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: label }));
  await user.click(await screen.findByRole("option", { name: option }));
}
async function save(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Save changes" }));
  await screen.findByText("Model selection saved.");
  return context.api.saveModelSelection.mock.calls.at(-1)![0].selection as CloudSelection;
}

describe("model selection settings", () => {
  it.each([
    ["Follow issue level", { issue_complexity: true }],
    ["Default level (Normal)", { issue_complexity: false }],
    ["Normal model", { model: "normal", issue_complexity: false }],
    ["Complex model", { model: "complex", issue_complexity: false }],
  ])("maps the stage picker to %s without losing its ceiling", async (option, expected) => {
    response = { ...response, effective: { ...selection, stages: { plan: { model: "legacy-model", level: "very_complex", issue_complexity: true, effort: "low" } } } };
    const user = await mount();
    expect(screen.getByRole("combobox", { name: "Plan stage model" }).textContent).toContain("legacy-model");
    await choose(user, "Plan stage model", option);
    const saved = await save(user);
    expect(saved.stages?.plan).toEqual({ ...expected, effort: "low" });
    expect(saved.rules).toEqual(selection.rules);
  });

  it("round-trips inherited and legacy values unchanged when enabling a project override", async () => {
    const legacy: CloudSelection = { ...selection, levels: { ...selection.levels, custom: { model: "unlisted", effort: "max" } }, stages: { ...selection.stages, plan: { model: "gpt-6-astra", effort: "low" }, custom: { level: "custom", issue_complexity: true, effort: "max" } }, fallback_order: ["custom", "normal"] };
    response = { revision: "1", selection: null, effective: legacy };
    render(<ModelSelectionSettings projectId="project_a" canManage />);
    const user = userEvent.setup();
    await screen.findByText("Using organization default");
    expect(screen.queryByRole("region", { name: "Unsaved model selection" })).toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
    await user.click(screen.getByRole("switch", { name: "Override organization model selection" }));
    await user.click(screen.getByRole("button", { name: "Edit model selection" }));
    expect(screen.getByRole("combobox", { name: "Plan stage model" }).textContent).toContain("Complex model");
    expect(screen.getByRole("button", { name: "Code stage effort ceiling: level" }).getAttribute("aria-pressed")).toBe("true");
    expect(await save(user)).toEqual(legacy);
    cleanup();
    await mount({ projectId: "project_a" });
    expect(screen.getByRole("combobox", { name: "Custom complexity model" }).textContent).toContain("unlisted");
    expect(screen.getByRole("combobox", { name: "Custom stage model" }).textContent).toContain("Custom level");
    expect(screen.queryByRole("region", { name: "Unsaved model selection" })).toBeNull();
  });

  it("edits label chips using only the project's labels and preserves selector constraints", async () => {
    const user = await mount({ projectId: "project_a" });
    await choose(user, "Add label to Complex", "bug");
    await user.click(screen.getByRole("button", { name: "Remove complexity:complex from Complex" }));
    const saved = await save(user);
    expect(saved.rules).toEqual([{ ...selection.rules![0], selector: { author_in: ["author_a"], labels: { include: ["bug"], exclude: ["excluded"] } } }]);
    await user.click(screen.getByRole("combobox", { name: "Add label to Normal" }));
    expect(screen.queryByRole("option", { name: "another-project" })).toBeNull();
  });

  it("creates a label rule for a level that has none", async () => {
    const user = await mount();
    await choose(user, "Add label to Very complex", "complexity:very-complex");
    const saved = await save(user);
    expect(saved.rules?.[1]).toEqual({ name: "very_complex", level: "very_complex", selector: { labels: { include: ["complexity:very-complex"] } } });
  });

  it("discards changes and removes the bar when values are restored", async () => {
    const user = await mount();
    expect(screen.queryByRole("textbox")).toBeNull();
    const automatic = screen.getByRole("switch", { name: "Automatic model selection" });
    await user.click(automatic);
    expect(screen.getByRole("region", { name: "Unsaved model selection" })).toBeTruthy();
    await user.click(automatic);
    expect(screen.queryByRole("region", { name: "Unsaved model selection" })).toBeNull();
    await choose(user, "Plan stage model", "Follow issue level");
    await user.click(screen.getByRole("button", { name: "Discard" }));
    expect(screen.getByRole("combobox", { name: "Plan stage model" }).textContent).toContain("Complex model");
    expect(context.api.saveModelSelection).not.toHaveBeenCalled();
  });

  it("selects a default level with radio keys and restores the stage's level ceiling", async () => {
    const user = await mount();
    screen.getByRole("radio", { name: "Normal is the default complexity" }).focus();
    await user.keyboard("[ArrowDown]");
    await user.click(screen.getByRole("button", { name: "Code stage effort ceiling: high" }));
    await user.click(screen.getByRole("button", { name: "Code stage effort ceiling: level" }));
    const saved = await save(user);
    expect(saved.default_level).toBe("complex");
    expect(saved.stages?.code).toEqual({ issue_complexity: true, effort: "" });
  });

  it("uses the catalogue and allows a custom model ID only through Other", async () => {
    const user = await mount();
    await choose(user, "Normal model", "Codex Astra gpt-6-astra");
    expect((await save(user)).normal_model).toBe("gpt-6-astra");
    await user.click(screen.getByRole("combobox", { name: "Normal model" }));
    await user.type(await screen.findByRole("combobox", { name: "Search normal model" }), "unlisted-id");
    await user.click(await screen.findByRole("option", { name: "Other model ID…" }));
    await user.clear(screen.getByRole("textbox", { name: "Normal model: other model ID" }));
    await user.type(screen.getByRole("textbox", { name: "Normal model: other model ID" }), "unlisted-id");
    await user.click(screen.getByRole("button", { name: "Use" }));
    expect((await save(user)).normal_model).toBe("unlisted-id");
  });

  it("adds and removes fallbacks, and preserves the order when stopping with an error", async () => {
    const user = await mount();
    await choose(user, "Add fallback level", "Very complex");
    await user.click(screen.getByRole("button", { name: "Remove Complex fallback" }));
    await user.click(screen.getByRole("radio", { name: "Stop with an error" }));
    const saved = await save(user);
    expect(saved.fallback_order).toEqual(["normal", "very_complex"]);
    expect(saved.unavailable).toBe("fail");
  });

  it("reorders fallback chips with the keyboard", async () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      const isSecond = this.textContent?.includes("2 · Complex");
      const x = isSecond ? 140 : 0;
      return { x, y: 0, left: x, right: x + 120, top: 0, bottom: 30, width: 120, height: 30, toJSON: () => ({}) };
    });
    const user = await mount();
    screen.getByRole("button", { name: "Reorder Normal fallback" }).focus();
    await user.keyboard("[Space]");
    await waitFor(() => expect(screen.getByRole("button", { name: "Reorder Normal fallback" }).getAttribute("aria-pressed")).toBe("true"));
    await user.keyboard("[ArrowRight]");
    await user.keyboard("[Space]");
    await waitFor(() => expect(within(screen.getByRole("list", { name: "Fallback order" })).getAllByRole("button")[0]?.textContent).toContain("Complex"));
    expect((await save(user)).fallback_order).toEqual(["complex", "normal"]);
  });

  it("starts inherited projects with a summary and opens the existing editor for an override", async () => {
    response = { ...response, selection: null };
    render(<ModelSelectionSettings projectId="project_a" canManage />);
    const user = userEvent.setup();
    await screen.findByText("Using organization default");
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit model selection" })).toBeNull();
    await user.click(screen.getByRole("switch", { name: "Override organization model selection" }));
    expect(screen.getByText("On · 3 levels · 4 stages")).toBeTruthy();
    expect(screen.getByText("gpt-6.1-sol")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit model selection" }));
    await screen.findByRole("table", { name: "Stages" });
    await user.click(screen.getByRole("switch", { name: "Automatic model selection" }));
    await user.click(screen.getByRole("button", { name: "Collapse" }));
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByRole("region", { name: "Unsaved model selection" })).toBeTruthy();
    await save(user);
    expect(context.api.saveModelSelection.mock.calls[0]![0]).toMatchObject({ projectId: "project_a", revision: "1", selection: { ...selection, enabled: false } });
  });

  it("saves null when disabling a project override and hides inherited controls", async () => {
    const user = await mount({ projectId: "project_a" });
    await user.click(screen.getByRole("switch", { name: "Override organization model selection" }));
    expect(screen.queryByRole("switch", { name: "Automatic model selection" })).toBeNull();
    expect(screen.getByText("Using organization default")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Organization models" }).getAttribute("href")).toBe("/settings/organization/models");
    await save(user);
    expect(context.api.saveModelSelection.mock.calls[0]![0]).toMatchObject({ projectId: "project_a", revision: "1", selection: null });
  });

  it("refreshes a revision conflict and shows the existing message", async () => {
    const user = await mount();
    context.api.saveModelSelection.mockImplementation(async () => {
      response = { revision: "new", selection: { ...selection, normal_model: "new-model" }, effective: { ...selection, normal_model: "new-model" } };
      throw new AccountError({ status: 409, code: "revision_conflict", message: "Conflict" });
    });
    await user.click(screen.getByRole("switch", { name: "Automatic model selection" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByText("These settings changed. Review the latest values and save again.");
    expect(screen.getByRole("combobox", { name: "Normal model" }).textContent).toContain("new-model");
    expect(screen.queryByRole("region", { name: "Unsaved model selection" })).toBeNull();
  });

  it("renders read-only values without mutation controls for members", async () => {
    await mount({ canManage: false });
    for (const control of screen.getAllByRole("combobox")) expect(control.hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("switch", { name: "Automatic model selection" }));
    expect(screen.queryByRole("region", { name: "Unsaved model selection" })).toBeNull();
    expect(context.api.saveModelSelection).not.toHaveBeenCalled();
  });
});
