import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SlackSettings } from "../../src/app/settings/SlackSettings.tsx";
import { SpritesCard } from "../../src/app/account/SpritesCard.tsx";
import { IntegrationsSettings } from "../../src/app/settings/Settings.tsx";
import { searchSettings, settingsSearchItems } from "../../src/components/settings/settingsSearch.ts";

const api = vi.hoisted(() => ({
  slackIntegration: vi.fn(), saveSlackIntegration: vi.fn(), testSlackIntegration: vi.fn(),
  spritesSecret: vi.fn(), setSpritesSecret: vi.fn(), removeSpritesSecret: vi.fn(),
}));
const account = { actor: { can_manage: true }, projects: [{ id: "project", name: "Example project" }] };
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => account }));

const savedWebhook = "saved-webhook-sentinel";
const savedToken = "saved-token-sentinel";
const configured = { webhook: savedWebhook, channel_name: "#operations", last_success_at: "2026-10-21T10:00:00Z", last_failure_at: null, last_delivery_failed: false, last_status_code: 200 };
const empty = { ...configured, webhook: "", channel_name: "", last_success_at: null };
const connected = { present: true, organization_slug: "example-org", token: savedToken };

beforeEach(() => {
  vi.resetAllMocks();
  api.slackIntegration.mockResolvedValue(configured);
  api.saveSlackIntegration.mockResolvedValue(configured);
  api.testSlackIntegration.mockResolvedValue(configured);
  api.spritesSecret.mockResolvedValue(connected);
  api.setSpritesSecret.mockResolvedValue(connected);
  api.removeSpritesSecret.mockResolvedValue({ present: false });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it.each([
  { title: "Slack incoming webhook", help: "Post attention findings when they open and resolve.", security: "Detent never shows the webhook again. Replace or disable it here.", component: <SlackSettings /> },
  { title: "Sprites organization token", help: "Projects inherit this token unless they set an override. Use a dedicated Fly organization and set a spend alert.", security: "Detent never shows the token again; revoke it in Sprites.", component: <SpritesCard projectId="" canManage /> },
])("makes $title help and security available by keyboard", async ({ title, help, security, component }) => {
  render(component);
  const user = userEvent.setup();
  await user.tab();
  expect(document.activeElement).toBe(screen.getByRole("button", { name: `About ${title}` }));
  expect(await screen.findByText(help)).toBeTruthy();
  await user.tab();
  expect(document.activeElement).toBe(screen.getByRole("button", { name: `${title} security` }));
  expect(await screen.findByText(security)).toBeTruthy();
  expect(screen.getByText("Encrypted · write-only")).toBeTruthy();
});

describe("organization Slack row", () => {
  it.each([
    { name: "configured", value: configured, text: /#operations · delivered/, tone: "bg-success", action: "Replace webhook" },
    { name: "failed", value: { ...configured, last_delivery_failed: true, last_status_code: 404 }, text: "Last delivery failed · HTTP 404", tone: "bg-warning", action: "Replace webhook" },
    { name: "empty", value: empty, text: "Not set up", tone: "bg-muted-foreground/50", action: "Set webhook" },
  ])("renders $name without a saved secret and discards cancelled edits", async ({ value, text, tone, action }) => {
    vi.spyOn(Date, "now").mockReturnValue(new Date("2026-10-21T13:00:00Z").getTime());
    api.slackIntegration.mockResolvedValue(value);
    render(<SlackSettings />);
    const status = await screen.findByText(text);
    expect(status.parentElement?.innerHTML).toContain(tone);
    if (value === configured) expect(status.textContent).toContain("3h ago");
    expect(screen.queryByLabelText("Slack webhook URL")).toBeNull();
    expect(screen.queryByRole("button", { name: "Send test message" }) !== null).toBe(Boolean(value.webhook));
    expect(screen.queryByRole("button", { name: "Disable" }) !== null).toBe(Boolean(value.webhook));
    expect(document.body.innerHTML).not.toContain(savedWebhook);
    fireEvent.click(screen.getByRole("button", { name: action }));
    const input = screen.getByLabelText("Slack webhook URL") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.value).toBe("");
    expect(document.body.innerHTML).not.toContain(savedWebhook);
    fireEvent.change(input, { target: { value: "discarded-webhook" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Channel name (optional, for display)" }), { target: { value: "#discarded" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByLabelText("Slack webhook URL")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: action }));
    expect((screen.getByLabelText("Slack webhook URL") as HTMLInputElement).value).toBe("");
    expect((screen.getByRole("textbox", { name: "Channel name (optional, for display)" }) as HTMLInputElement).value).toBe(value.channel_name);
    expect(api.saveSlackIntegration).not.toHaveBeenCalled();
  });

  it("preserves save, test and disable calls and notices", async () => {
    api.slackIntegration.mockResolvedValue(empty);
    render(<SlackSettings />);
    await screen.findByText("Not set up");
    fireEvent.click(screen.getByRole("button", { name: "Set webhook" }));
    await waitFor(() => expect((screen.getByLabelText("Slack webhook URL") as HTMLInputElement).disabled).toBe(false));
    fireEvent.change(screen.getByLabelText("Slack webhook URL"), { target: { value: "https://hooks.slack.com/services/example" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Channel name (optional, for display)" }), { target: { value: "#operations" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Slack notifications saved.");
    expect(api.saveSlackIntegration).toHaveBeenLastCalledWith({ webhook: "https://hooks.slack.com/services/example", channel_name: "#operations" });
    expect(screen.queryByLabelText("Slack webhook URL")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Send test message" }));
    await screen.findByText("Test message sent.");
    expect(api.testSlackIntegration).toHaveBeenCalledWith();
    api.saveSlackIntegration.mockResolvedValue(empty);
    fireEvent.click(screen.getByRole("button", { name: "Disable" }));
    await screen.findByText("Slack notifications disabled.");
    expect(api.saveSlackIntegration).toHaveBeenLastCalledWith({ webhook: "", channel_name: "" });
    expect(screen.getByRole("button", { name: "Set webhook" })).toBeTruthy();
  });

  it.each(["save", "test", "disable"])("keeps %s errors free of customer content", async (action) => {
    api.saveSlackIntegration.mockRejectedValue(new Error(savedWebhook));
    api.testSlackIntegration.mockRejectedValue(new Error(savedWebhook));
    render(<SlackSettings />);
    await screen.findByRole("button", { name: "Replace webhook" });
    if (action === "save") {
      fireEvent.click(screen.getByRole("button", { name: "Replace webhook" }));
      fireEvent.change(screen.getByLabelText("Slack webhook URL"), { target: { value: savedWebhook } });
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
    } else fireEvent.click(screen.getByRole("button", { name: action === "test" ? "Send test message" : "Disable" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(action === "test" ? "Could not send the test message." : "Could not save Slack settings.");
    expect(document.body.innerHTML).not.toContain(savedWebhook);
  });

  it("reports a refused test delivery using the existing notice", async () => {
    api.testSlackIntegration.mockResolvedValue({ ...configured, last_delivery_failed: true, last_status_code: 403 });
    render(<SlackSettings />);
    fireEvent.click(await screen.findByRole("button", { name: "Send test message" }));
    await screen.findByText("Test message failed. Check the webhook and try again.");
    expect(screen.getByText("Last delivery failed · HTTP 403")).toBeTruthy();
  });
});

describe("organization Sprites row", () => {
  it.each([false, true])("opens an empty editor and discards cancellation when connected is %s", async (present) => {
    api.spritesSecret.mockResolvedValue({ ...connected, present });
    render(<SpritesCard projectId="" canManage />);
    await screen.findByText(present ? /Connected to example-org/ : "Not set up");
    expect(api.spritesSecret).toHaveBeenCalledWith("");
    const action = present ? "Replace token" : "Set token";
    expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
    if (present) expect(screen.getByRole("link", { name: "Manage in Sprites" }).getAttribute("href")).toBe("https://sprites.dev/account");
    expect(document.body.innerHTML).not.toContain(savedToken);
    fireEvent.click(screen.getByRole("button", { name: action }));
    const input = screen.getByLabelText("Sprites organization token") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.value).toBe("");
    fireEvent.change(input, { target: { value: savedToken } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: action }));
    expect((screen.getByLabelText("Sprites organization token") as HTMLInputElement).value).toBe("");
    expect(api.setSpritesSecret).not.toHaveBeenCalled();
  });

  it.each([false, true])("preserves organization set and replace calls when refusal is %s", async (refuse) => {
    api.spritesSecret.mockResolvedValue({ present: false });
    if (refuse) api.setSpritesSecret.mockRejectedValue(new Error(savedToken));
    render(<SpritesCard projectId="" canManage />);
    await screen.findByText("Not set up");
    fireEvent.click(screen.getByRole("button", { name: "Set token" }));
    fireEvent.change(screen.getByLabelText("Sprites organization token"), { target: { value: savedToken } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(api.setSpritesSecret).toHaveBeenCalledWith("", savedToken);
    if (refuse) await screen.findByRole("alert");
    else {
      await screen.findByText(/Connected to example-org/);
      fireEvent.click(screen.getByRole("button", { name: "Replace token" }));
      fireEvent.change(screen.getByLabelText("Sprites organization token"), { target: { value: "replacement-token" } });
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await screen.findByRole("button", { name: "Replace token" });
      expect(api.setSpritesSecret).toHaveBeenLastCalledWith("", "replacement-token");
      fireEvent.click(screen.getByRole("button", { name: "Remove" }));
      await screen.findByText("Not set up");
      expect(api.removeSpritesSecret).toHaveBeenCalledWith("");
    }
    expect(document.body.innerHTML).not.toContain(savedToken);
  });

  it("does not expose token controls to a reader", async () => {
    render(<SpritesCard projectId="" canManage={false} />);
    await screen.findByText(/Connected to example-org/);
    expect(screen.queryByRole("button", { name: "Replace token" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
  });
});

it("keeps Integrations organization-only and search destinations anchored to its rows", async () => {
  const route = createRootRoute({ component: IntegrationsSettings });
  const router = createRouter({ routeTree: route, history: createMemoryHistory() });
  await act(async () => { render(<RouterProvider router={router} />); });
  await screen.findByRole("heading", { name: "Organization notifications" });
  expect(screen.getByRole("heading", { name: "Organization Sprites" })).toBeTruthy();
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByText("Example project")).toBeNull();
  expect(screen.queryByText("Sprites project override")).toBeNull();
  expect(api.spritesSecret).toHaveBeenCalledWith("");
  for (const [query, target] of [["Slack", "slack-notifications"], ["Sprites", "sprites"]]) {
    const hit = searchSettings(query!, settingsSearchItems(["integrations"]))[0];
    expect(hit?.to).toBe("/settings/integrations");
    expect(hit?.targetId).toBe(target);
    expect(document.getElementById(target!)).toBeTruthy();
  }
});
