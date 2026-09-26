// @vitest-environment jsdom
//
// The workspace lifecycle hook (decisions.md §18.1): reuse an open workspace
// rather than spend another slot, adopt the one a 409 names, and learn every
// state after that from the project event stream rather than by polling.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { Workspace } from "../../src/contracts/work.ts";
import {
  isWorkspaceLive,
  isWorkspaceUsable,
  useWorkspace,
  workspaceFromEvent,
  workspaceReasonSentence,
} from "../../src/app/adapters/workspaces.ts";

afterEach(cleanup);

const CLIENT = {
  http: { origin: "", apiBase: "/api/v2/organizations/acme", csrfToken: "csrf" },
} as unknown as ConversationClient;

function workspace(overrides: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws_1",
    organization_id: "org_1",
    project_id: "proj_1",
    work_item_id: "item_1",
    ref: "refs/heads/main",
    state: "requested",
    requires: ["files"],
    // What the runner that claimed it reported. Present on the builder because
    // reuse now depends on it (§18.12): a workspace whose capabilities do not
    // satisfy the caller's `requires` is not reusable, so a fixture with none
    // would be testing the refusal rather than the reuse.
    capabilities: {
      terminal: false, files: true, diff: false, preview: false, exec: false, git: false,
    },
    read_only: true,
    idle_timeout_seconds: 1800,
    expires_at: "2026-09-11T12:00:00Z",
    created_by: "person_1",
    revision: 1,
    created_at: "2026-09-11T11:00:00Z",
    updated_at: "2026-09-11T11:00:00Z",
    ...overrides,
  } as Workspace;
}

/** The one EventSource the hook opens, captured so a test can emit into it. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  readonly listeners = new Map<string, Set<EventListener>>();
  closed = false;

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const set = this.listeners.get(type) ?? new Set<EventListener>();
    set.add(listener);
    this.listeners.set(type, set);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.closed = true;
  }

  emit(type: string, data: unknown): void {
    const event = { data: JSON.stringify(data) } as MessageEvent<string>;
    for (const listener of this.listeners.get(type) ?? []) listener(event as unknown as Event);
  }
}

function Probe(props: { requires?: readonly string[] }): React.ReactElement {
  const handle = useWorkspace({
    projectId: "proj_1",
    workItemId: "item_1",
    requires: props.requires ?? ["files"],
  });
  return (
    <div>
      <span data-testid="state">{handle.state ?? "none"}</span>
      <span data-testid="reason">{handle.reason ?? "none"}</span>
      <span data-testid="error">{handle.error ?? "none"}</span>
      <span data-testid="id">{handle.workspace?.id ?? "none"}</span>
    </div>
  );
}

function mount(probe: React.ReactElement = <Probe />) {
  return render(<ClientContext.Provider value={CLIENT}>{probe}</ClientContext.Provider>);
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  FakeEventSource.instances = [];
  Object.defineProperty(globalThis, "EventSource", {
    configurable: true,
    writable: true,
    value: FakeEventSource,
  });
  fetchMock = vi.fn();
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    writable: true,
    value: fetchMock,
  });
  globalThis.localStorage?.clear();
});

describe("useWorkspace", () => {
  it("reuses an open workspace instead of opening a second one", async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(200, { workspaces: [workspace({ id: "ws_open", state: "ready" })] })),
    );
    mount();
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_open"));
    expect(screen.getByTestId("state").textContent).toBe("ready");
    // One GET, and no POST at all: a slot that already exists is not spent again.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/workspaces?work_item=item_1");
  });

  it("skips a closed workspace and opens a new one, carrying `requires`", async () => {
    fetchMock.mockImplementation((_url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "GET") {
        return Promise.resolve(json(200, { workspaces: [workspace({ state: "closed" })] }));
      }
      return Promise.resolve(json(201, workspace({ id: "ws_new", state: "requested" })));
    });
    mount();
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_new"));

    const post = fetchMock.mock.calls.find((call) => (call[1] as RequestInit).method === "POST");
    expect(post).toBeDefined();
    const init = post?.[1] as RequestInit;
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("csrf");
    expect(JSON.parse(String(init.body))).toMatchObject({
      work_item_id: "item_1",
      requires: ["files"],
    });
  });

  it("adopts the workspace a 409 names rather than reporting the conflict", async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "POST") {
        return Promise.resolve(
          json(409, {
            code: "workspace_exists",
            message: "already open",
            details: { workspace_id: "ws_existing" },
          }),
        );
      }
      if (String(url).endsWith("/workspaces/ws_existing")) {
        return Promise.resolve(json(200, workspace({ id: "ws_existing", state: "starting" })));
      }
      return Promise.resolve(json(200, { workspaces: [] }));
    });
    mount();
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_existing"));
    expect(screen.getByTestId("state").textContent).toBe("starting");
    expect(screen.getByTestId("error").textContent).toBe("none");
  });

  it("names the cap that was hit when the create is refused", async () => {
    fetchMock.mockImplementation((_url: string, init?: RequestInit) =>
      Promise.resolve(
        (init?.method ?? "GET") === "POST"
          ? json(422, {
              code: "workspace_limit",
              message: "too many",
              details: { scope: "person", limit: 3 },
            })
          : json(200, { workspaces: [] }),
      ),
    );
    mount();
    await waitFor(() =>
      expect(screen.getByTestId("error").textContent).toBe(
        "You have 3 workspaces open, which is the limit. Close one and try again.",
      ),
    );
  });

  it("takes every state after the first read from the project event stream", async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(200, { workspaces: [workspace({ state: "requested" })] })),
    );
    mount();
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("requested"));
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const source = FakeEventSource.instances[0]!;
    expect(source.url).toContain("/projects/proj_1/events");

    source.emit("workspace.starting", workspace({ state: "starting" }));
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("starting"));
    source.emit("workspace.ready", workspace({ state: "ready" }));
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("ready"));
    source.emit("workspace.failed", workspace({ state: "failed", reason: "no_runner" }));
    await waitFor(() => expect(screen.getByTestId("reason").textContent).toBe("no_runner"));

    // Not one extra read: §18.1 makes the subscription the contract.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // §18.12: reuse is only correct where the workspace can serve the surface.
  // A files-only workspace reused for an action would have its exec frames
  // refused by the hub's channel gate, and the reader would see an action that
  // never produces output with nothing saying why.
  it("opens a second workspace rather than reusing one that cannot run an action", async () => {
    fetchMock.mockImplementation((_url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "GET") {
        return Promise.resolve(
          json(200, { workspaces: [workspace({ id: "ws_files_only", state: "ready" })] }),
        );
      }
      return Promise.resolve(json(201, workspace({ id: "ws_exec", state: "requested" })));
    });
    mount(<Probe requires={["files", "exec"]} />);
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_exec"));
    const post = fetchMock.mock.calls.find((call) => (call[1] as RequestInit).method === "POST");
    expect(JSON.parse(String((post?.[1] as RequestInit).body))).toMatchObject({
      requires: ["exec", "files"],
    });
  });

  it("reuses a workspace whose runner reported both capabilities", async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        json(200, {
          workspaces: [
            workspace({
              id: "ws_both",
              state: "ready",
              capabilities: {
                terminal: false,
                files: true,
                diff: false,
                preview: false,
                exec: true,
                git: false,
              },
            }),
          ],
        }),
      ),
    );
    mount(<Probe requires={["files", "exec"]} />);
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_both"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // A workspace nobody has claimed has reported nothing, which is "not yet
  // known" rather than "none": adopting it would be a bet on what a runner
  // will turn out to report.
  it("treats an unclaimed workspace as unable to satisfy a requirement", async () => {
    fetchMock.mockImplementation((_url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "GET") {
        return Promise.resolve(
          json(200, {
            workspaces: [{ ...workspace({ id: "ws_unclaimed" }), capabilities: null }],
          }),
        );
      }
      return Promise.resolve(json(201, workspace({ id: "ws_new", state: "requested" })));
    });
    mount(<Probe requires={["files", "exec"]} />);
    await waitFor(() => expect(screen.getByTestId("id").textContent).toBe("ws_new"));
  });

  // The same bug by a different route: adopting the workspace a 409 names,
  // when it cannot serve the surface, is no better than reusing one.
  it("reports the conflict rather than adopting a 409's workspace that cannot run an action", async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "POST") {
        return Promise.resolve(
          json(409, {
            code: "workspace_exists",
            message: "already open",
            details: { workspace_id: "ws_files_only" },
          }),
        );
      }
      if (String(url).endsWith("/workspaces/ws_files_only")) {
        return Promise.resolve(json(200, workspace({ id: "ws_files_only", state: "ready" })));
      }
      return Promise.resolve(json(200, { workspaces: [] }));
    });
    mount(<Probe requires={["files", "exec"]} />);
    await waitFor(() => expect(screen.getByTestId("error").textContent).toBe("already open"));
    expect(screen.getByTestId("id").textContent).toBe("none");
  });

  it("ignores another reader's workspace on the same project stream", async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(200, { workspaces: [workspace({ state: "requested" })] })),
    );
    mount();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    FakeEventSource.instances[0]!.emit(
      "workspace.ready",
      workspace({ id: "ws_someone_else", state: "ready" }),
    );
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(screen.getByTestId("state").textContent).toBe("requested");
  });
});

describe("workspaceFromEvent", () => {
  it("keeps its own workspace and drops everything else", () => {
    const frame = JSON.stringify(workspace({ state: "ready" }));
    expect(workspaceFromEvent(frame, "ws_1")?.state).toBe("ready");
    expect(workspaceFromEvent(frame, "ws_2")).toBeNull();
    expect(workspaceFromEvent("not json", "ws_1")).toBeNull();
    expect(workspaceFromEvent(JSON.stringify({ id: "ws_1" }), "ws_1")).toBeNull();
  });
});

describe("workspace state predicates", () => {
  it("treats only ready and idle as live, and the three exits as unusable", () => {
    expect(isWorkspaceLive("ready")).toBe(true);
    expect(isWorkspaceLive("idle")).toBe(true);
    expect(isWorkspaceLive("starting")).toBe(false);
    expect(isWorkspaceUsable("requested")).toBe(true);
    expect(isWorkspaceUsable("unreachable")).toBe(true);
    for (const state of ["closing", "closed", "failed"] as const) {
      expect(isWorkspaceUsable(state)).toBe(false);
    }
  });

  it("gives every §18.1 reason its own sentence", () => {
    expect(workspaceReasonSentence("failed", "no_runner")).toContain("No runner reported");
    expect(workspaceReasonSentence("failed", "worktree_missing")).toContain("worktree");
    expect(workspaceReasonSentence("closed", "expired")).toContain("idle timeout");
    expect(workspaceReasonSentence("failed", null)).toBe("This workspace failed.");
    expect(workspaceReasonSentence("closed", null)).toBe("This workspace has closed.");
  });
});
