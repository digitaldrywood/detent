import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SourceRecoveryControls } from "../../src/app/work/components/SourceRecoveryControls.tsx";
import { makeWorkHttp } from "../../src/app/work/lib/workHttp.ts";
import type { SourceRecovery } from "../../src/contracts/work.ts";

afterEach(cleanup);
const source: SourceRecovery = {
  work_item_id: "item", revision: "7", source_runner_name: "", source_machine_id: "machine_A", source_runner_id: "runner_A",
  attempt_id: "attempt", version_id: "version", head_sha: "abc123", base_sha: "base",
  destination_runner_id: "", available: true, quiesced: true,
  reason: "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force",
  destinations: [{ runner_id: "runner_B", name: "Destination" }],
};
const response = (view: SourceRecovery) => new Response(JSON.stringify(view), { status: 200 });

describe("source recovery controls", () => {
  it.each([
    { name: "uncaptured local work", view: { ...source, available: false, reason: "Wait for source runner runner_A to capture its local checkpoint" }, canManage: true },
    { name: "partitioned owner", view: { ...source, quiesced: false, reason: "Expiry does not prove the source owner stopped" }, canManage: true },
    {name:"friendly source name",view:{...source,source_runner_name:"Source Air"},canManage:false},
 { name: "read-only operator", view: source, canManage: false },
  ])("shows recovery facts and refuses transfer controls for $name", async ({ view, canManage }) => {
    const http = makeWorkHttp({ origin: "http://fixture.test", apiBase: "/api/v2/organizations/org", csrfToken: "fixture-csrf", fetch: async () => response(view) });
    render(<SourceRecoveryControls http={http} projectId="project" itemId="item" revision="7" canManage={canManage} onTransferred={() => {}} />);
    await screen.findByText(`Source runner: ${view.source_runner_name || view.source_runner_id}`);
    expect(screen.getByText(view.reason)).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: "Destination runner" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Transfer recovery" })).toBeNull();
  });

  it("re-reads a conflicting checkpoint before another operator choice", async () => {
    let reads = 0;
    const writes: Record<string, unknown>[] = [];
    const http = makeWorkHttp({ origin: "http://fixture.test", apiBase: "/api/v2/organizations/org", csrfToken: "fixture-csrf", fetch: async (_url, init) => {
      if (init?.method !== "POST") return response(++reads === 1 ? source : { ...source, revision: "8", version_id: "replacement", head_sha: "def456" });
      writes.push(JSON.parse(String(init.body)) as Record<string, unknown>);
      if (writes.length === 1) return new Response(JSON.stringify({ code: "revision_conflict", current_revision: "8", message: "Changed" }), { status: 409 });
      return response({ ...source, revision: "9", version_id: "replacement", destination_runner_id: "runner_B" });
    } });
    const onTransferred = vi.fn();
    render(<SourceRecoveryControls http={http} projectId="project" itemId="item" revision="7" canManage onTransferred={onTransferred} />);
    fireEvent.click(await screen.findByRole("combobox", { name: "Destination runner" }));
    fireEvent.click(await screen.findByRole("option", { name: "Destination" }));
    fireEvent.click(screen.getByRole("button", { name: "Transfer recovery" }));
    await screen.findByText(/Source changed/);
    expect(screen.getByText(/def456/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Transfer recovery" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("combobox", { name: "Destination runner" }));
    fireEvent.click(await screen.findByRole("option", { name: "Destination" }));
    fireEvent.click(screen.getByRole("button", { name: "Transfer recovery" }));
    await waitFor(() => expect(onTransferred).toHaveBeenCalledOnce());
    expect(writes[1]).toMatchObject({ expected_revision: "8", version_id: "replacement" });
    expect(writes[1]?.idempotency_key).not.toBe(writes[0]?.idempotency_key);
  });

  it.each([{ name: "Destination", label: "Destination" }, { name: "", label: "runner_B" }])("retries a lost reply with the same snapshot and request identity using $label", async ({ name, label }) => {
    const view = { ...source, destinations: [{ runner_id: "runner_B", name }] };
    const writes: Record<string, unknown>[] = [];
    const onTransferred = vi.fn();
    const http = makeWorkHttp({ origin: "http://fixture.test", apiBase: "/api/v2/organizations/org", csrfToken: "fixture-csrf", fetch: async (_url, init) => {
      if (init?.method !== "POST") return response(view);
      writes.push(JSON.parse(String(init.body)) as Record<string, unknown>);
      expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("fixture-csrf");
      if (writes.length === 1) throw new Error("Reply unavailable");
      return response({ ...view, revision: "8", destination_runner_id: "runner_B" });
    } });
    render(<SourceRecoveryControls http={http} projectId="project" itemId="item" revision="7" canManage onTransferred={onTransferred} />);
    fireEvent.click(await screen.findByRole("combobox", { name: "Destination runner" }));
    fireEvent.click(await screen.findByRole("option", { name: label }));
    expect(screen.getByRole("combobox", { name: "Destination runner" }).textContent).toContain(label);
    fireEvent.click(screen.getByRole("button", { name: "Transfer recovery" }));
    await screen.findByRole("alert");
    fireEvent.click(screen.getByRole("button", { name: "Transfer recovery" }));
    await waitFor(() => expect(onTransferred).toHaveBeenCalledOnce());
    expect(writes[0]).toEqual(writes[1]);
    expect(writes[1]).toMatchObject({ expected_revision: "7", version_id: "version", destination_runner_id: "runner_B" });
  });
});
