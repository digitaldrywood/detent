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

it.each(["set_runner_tier", "set_sprite_pool"])("requires approval for %s with ordinary confirmations off", async (kind) => {
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

it("honors the automatic submission preference for ordinary actions", async () => {
  localStorage.setItem("detent:chat-confirmation:tier-admin", "off");
  const submit = vi.fn().mockResolvedValue({ ok: true });
  vi.stubGlobal("fetch", submit);
  render(<InlineActionCard proposal={{ action: { kind: "edit_item", request_id: "ordinary", project_id: "prj_test" }, conversation_id: "conv_test" }} text="Edit issue" />);
  await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
  await screen.findByText("Change completed.");
});
