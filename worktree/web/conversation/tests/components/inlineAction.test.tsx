// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { InlineActionCard } from "../../src/app/components/InlineActionCard.tsx";

vi.mock("../../src/app/client.ts", () => ({
  useClient: () => ({ bootstrap: { actor: { principal_id: "tier-admin" }, api_base: "/api/v2", csrf_token: "csrf" } }),
}));

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

it.each(["set_runner_tier", "set_sprite_pool", "convert_runner_scope", "promote_sprites_token"])("requires approval for %s with ordinary confirmations off", async (kind) => {
  localStorage.setItem("detent:chat-confirmation:tier-admin", "off");
  const submit = vi.fn().mockResolvedValue({ ok: true });
  vi.stubGlobal("fetch", submit);
  const action = { kind, request_id: "access-change", project_id: "prj_test", arguments: { runner_id: "runner_test", isolation_tier: "native-trusted", expected_revision: 1 } };
  render(<InlineActionCard proposal={{ action, conversation_id: "conv_test" }} text="Full access lets agents use this machine's files, credentials and network." />);
  expect(submit).not.toHaveBeenCalled();
  expect(screen.queryByRole("checkbox")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
  await screen.findByText("Change completed.");
  expect(submit).toHaveBeenCalledTimes(1);
  expect(submit).toHaveBeenCalledWith(expect.any(String), expect.objectContaining({ body: JSON.stringify(action) }));
});

it("cancels a tier preview without changing access", () => {
  localStorage.setItem("detent:chat-confirmation:tier-admin", "off");
  const submit = vi.fn();
  vi.stubGlobal("fetch", submit);
  render(<InlineActionCard proposal={{ action: { kind: "set_runner_tier", request_id: "cancel", project_id: "prj_test" }, conversation_id: "conv_test" }} text="Change runner access" />);
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.getByRole("status").textContent).toBe("Cancelled.");
  expect(submit).not.toHaveBeenCalled();
});

it.each(["edit_item", "update_project_integration", "approve_project_policy", "apply_local_project_policy"])("honors the automatic submission preference for %s", async (kind) => {
  localStorage.setItem("detent:chat-confirmation:tier-admin", "off");
  const submit = vi.fn().mockResolvedValue({ ok: true });
  vi.stubGlobal("fetch", submit);
  render(<InlineActionCard proposal={{ action: { kind, request_id: "ordinary", project_id: "prj_test" }, conversation_id: "conv_test" }} text="Edit issue" />);
  await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
  await screen.findByText("Change completed.");
});

 it.each([
  { status: 409, message: "Project configuration changed; request a fresh proposal" },
  { status: 403, message: "Project-management access is required" },
 ])("shows the workflow refusal for HTTP $status", async ({ status, message }) => {
  const submit = vi.fn().mockResolvedValue({ ok: false, status, json: async () => ({ message }) });
  vi.stubGlobal("fetch", submit);
  render(<InlineActionCard proposal={{ action: { kind: "approve_project_policy", request_id: "workflow", project_id: "prj_test" }, conversation_id: "conv_test" }} text="github_pull_request: false → true" />);
  expect(submit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
  expect((await screen.findByRole("alert")).textContent).toBe(`${message} (${status}).`);
  expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
 });

it("previews a workflow approval and submits it only after Approve", async () => {
  const submit = vi.fn().mockResolvedValue({ ok: true });
  vi.stubGlobal("fetch", submit);
  const action = { kind: "approve_project_policy", request_id: "approve", project_id: "prj_test", arguments: { selected_policy_id: "policy_selected" } };
  render(<InlineActionCard proposal={{ action, conversation_id: "conv_test" }} text="gates.github_pull_request: false → true" />);
  expect(screen.getByText("gates.github_pull_request: false → true")).toBeTruthy();
  expect((screen.getByRole("checkbox", { name: "Ask me to confirm chat changes" }) as HTMLInputElement).checked).toBe(true);
  expect(submit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
  await screen.findByText("Change completed.");
  expect(submit).toHaveBeenCalledTimes(1);
  expect(submit).toHaveBeenCalledWith("/api/v2/projects/prj_test/conversations/conv_test/actions", expect.objectContaining({ body: JSON.stringify(action) }));
});

it("submits a pending workflow application once confirmations are turned off", async () => {
  const submit = vi.fn().mockResolvedValue({ ok: true });
  vi.stubGlobal("fetch", submit);
  render(<InlineActionCard proposal={{ action: { kind: "apply_local_project_policy", request_id: "apply", project_id: "prj_test" }, conversation_id: "conv_test" }} text="Application is pending until this runner reports it applied" />);
  expect(submit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("checkbox", { name: "Ask me to confirm chat changes" }));
  await screen.findByText("Change completed.");
  expect(submit).toHaveBeenCalledTimes(1);
  expect(localStorage.getItem("detent:chat-confirmation:tier-admin")).toBe("off");
});
