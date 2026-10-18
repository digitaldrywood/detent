// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import React from "react";
import { afterEach, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import { useRightPanelWorkspace } from "../../src/app/components/RightPanel.tsx";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { NativeAttempt, Workspace } from "../../src/contracts/work.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

function Panel({ scopeKey = "item_1", attemptId = "attempt_1", workItemId = "item_1" }: { scopeKey?: string; attemptId?: string; workItemId?: string }): React.ReactElement {
  const panel = useRightPanelWorkspace({
    scopeKey,
    projectId: "proj_1",
    workItemId,
    surfaces: {
      loading: false, error: null, change: null, changedFiles: [], attemptDiff: null,
      source: { kind: "unavailable", reason: "No change" }, pullRequest: null,
      agents: { workflows: [], directAgents: [], runningCount: 0, waitingCount: 0, idleCount: 0, settledCount: 0, totalTokens: 0, hasAgents: false, liveCount: 0 }, reload: () => {},
    },
    attempts: [{ attempt_id: attemptId, status: "running", started_at: "2026-10-02T11:00:00Z" } as NativeAttempt], history: [], now: Date.now(),
  });
  return <>{panel.panel}</>;
}

const CLIENT = {
  http: { origin: "", apiBase: "/api/v2/organizations/acme", csrfToken: "csrf" },
  bootstrap: {
    organization: { id: "acme" },
    actor: { principal_id: "person_1" },
    feature: { workspaces: true },
    projects: [{ id: "proj_1", can_write: true, capabilities: { files: true } }],
  },
} as unknown as ConversationClient;

function view(props: React.ComponentProps<typeof Panel> = {}, client = CLIENT) {
  return <ClientContext.Provider value={client}><Panel {...props} /></ClientContext.Provider>;
}

it("opens Files from the enabled picker through the panel owner and workspace request", async () => {
  const fetch = vi.fn(() => Promise.resolve(new Response(JSON.stringify({
    code: "forbidden", message: "The runner grant was revoked.",
  }), { status: 403, headers: { "Content-Type": "application/json" } })));
  vi.stubGlobal("fetch", fetch);
  render(view());
  expect(fetch).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: /Files.*Browse and read/ }));
  await waitFor(() => expect(screen.getByText("The runner grant was revoked.")).toBeDefined());
  expect(screen.queryByLabelText("Open a surface")).toBeNull();
  expect(JSON.parse(localStorage.getItem("detent:right-panel-state:v1")!)["item_1"].activeSurfaceId).toBe("files");
});

class FileSocket extends EventTarget {
  static OPEN = 1;
  static instances: FileSocket[] = [];
  readonly readyState = 1;
  readonly frames: Array<{ channel: string; type: string; payload: { path: string } }> = [];
  closed = false;

  constructor(readonly url: string) {
    super();
    FileSocket.instances.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  send(raw: string): void {
    const frame = JSON.parse(raw) as (typeof this.frames)[number];
    this.frames.push(frame);
    if (frame.type !== "list" && frame.type !== "read") return;
    const payload = frame.type === "list"
      ? { path: frame.payload.path, entries: [{ name: "README.md", kind: "file", size: 12, modified_at: "2026-10-02T11:00:00Z", ignored: false, denied: false }] }
      : { path: frame.payload.path, mime: "text/plain", size: 12, offset: 0, data: "safe fixture", truncated: false };
    if (frame.type === "list" || frame.type === "read") {
      queueMicrotask(() => this.dispatchEvent(new MessageEvent("message", { data: JSON.stringify({
        channel: "files", stream: "files:1", type: frame.type === "list" ? "listed" : "content", payload,
      }) })));
    }
  }

  close(): void { this.closed = true; }
}

const WORKSPACE: Workspace = {
  id: "ws_1", organization_id: "org_1", project_id: "proj_1", work_item_id: "item_1",
  attempt_id: "attempt_1", runner_id: "runner_1", machine_id: "machine_1", ref: "refs/heads/work",
  state: "ready", requires: ["files"], capabilities: { files: true, terminal: false, diff: false, preview: false, exec: false, git: false },
  read_only: true, idle_timeout_seconds: 1800, expires_at: "2026-10-02T12:00:00Z", created_by: "person_1",
  revision: 1, created_at: "2026-10-02T11:00:00Z", updated_at: "2026-10-02T11:00:00Z",
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

vi.mock("@pierre/diffs/react", () => ({
  File: (props: { file: { contents: string } }) => <pre data-testid="safe-file">{props.file.contents}</pre>,
  Virtualizer: (props: { children: React.ReactNode }) => <>{props.children}</>,
}));
vi.mock("../../src/components/DiffWorkerPoolProvider.tsx", () => ({
  DiffWorkerPoolProvider: (props: { children: React.ReactNode }) => <>{props.children}</>,
}));

it("keeps Files selected when the linked conversation arrives and reads through the selected workspace relay", async () => {
  FileSocket.instances = [];
  vi.stubGlobal("WebSocket", FileSocket);
  const fetch = vi.fn((url: string) => Promise.resolve(String(url).endsWith("/relay-tickets")
    ? json({ ticket: "scoped-fixture", expires_in: 30 })
    : json({ workspaces: [WORKSPACE] })));
  vi.stubGlobal("fetch", fetch);
  const mounted = render(view());
  fireEvent.click(screen.getByRole("button", { name: /Files.*Browse and read/ }));
  await waitFor(() => expect(screen.getByTestId("files-surface")).toBeDefined());
  mounted.rerender(view({ scopeKey: "conversation_1" }));
  expect(screen.queryByLabelText("Open a surface")).toBeNull();
  expect(screen.getByTestId("files-surface")).toBeDefined();
  await waitFor(() => expect(FileSocket.instances[0]?.frames.some((frame) => frame.type === "list")).toBe(true));
  expect(screen.queryByTestId("files-error")?.textContent).toBeUndefined();
  const row = await waitFor(() => {
    const row = document.querySelector("file-tree-container")?.shadowRoot?.querySelector<HTMLElement>('[data-item-path="README.md"]');
    if (!row) throw new Error("README.md has not been listed");
    return row;
  });
  fireEvent.click(row);
  await waitFor(() => expect(screen.getByTestId("safe-file").textContent).toBe("safe fixture"));
  expect(FileSocket.instances).toHaveLength(1);
  expect(FileSocket.instances[0]?.url).toContain("/projects/proj_1/workspaces/ws_1/relay");
  expect(FileSocket.instances[0]?.frames).toContainEqual(expect.objectContaining({ type: "list", payload: expect.objectContaining({ path: "" }) }));
  expect(FileSocket.instances[0]?.frames).toContainEqual(expect.objectContaining({ type: "read", payload: expect.objectContaining({ path: "README.md" }) }));
  expect(fetch.mock.calls.every(([url]) => String(url).includes("/projects/proj_1/workspaces"))).toBe(true);

  fetch.mockImplementation(() => new Promise<Response>(() => {}));
  mounted.rerender(view({ scopeKey: "conversation_1", attemptId: "attempt_2" }));
  expect(screen.queryByTestId("safe-file")).toBeNull();
  expect(screen.getByTestId("file-surface-unavailable")).toBeDefined();
  expect(FileSocket.instances[0]?.closed).toBe(true);
  mounted.rerender(view({ attemptId: "attempt_2" }, { ...CLIENT, bootstrap: { ...CLIENT.bootstrap, feature: { workspaces: false } } } as ConversationClient));
  expect(screen.getByText("Workspace sessions are not enabled for this organization.")).toBeDefined();
  fetch.mockClear();
  mounted.rerender(view({ workItemId: "item_2", attemptId: "attempt_2" }));
  expect(screen.getByLabelText("Open a surface")).toBeDefined();
  expect(fetch).not.toHaveBeenCalled();
});

it("explains revoked workspace availability after Files has been selected", async () => {
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
  const mounted = render(view());
  fireEvent.click(screen.getByRole("button", { name: /Files.*Browse and read/ }));
  mounted.rerender(view({}, { ...CLIENT, bootstrap: { ...CLIENT.bootstrap, feature: { workspaces: false } } } as ConversationClient));
  expect(screen.getByText("Workspace sessions are not enabled for this organization.")).toBeDefined();
  expect(screen.queryByLabelText("Open a surface")).toBeNull();
});
