// @vitest-environment jsdom
//
// The update pill asks `/app/updates`, which the hub answers only for a
// reader who manages runners. Everybody else gets no pill and no request.
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import { SidebarUpdatePill } from "../../src/components/sidebar/SidebarUpdatePill.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the sidebar update pill", () => {
  it.each([
    { name: "a member without runner management", role: "member" },
    { name: "a viewer", role: "viewer" },
  ])("renders nothing and asks nothing for $name", ({ role }) => {
    const fetchSpy = vi.fn(async () => new Response("{}", { status: 404 }));
    vi.stubGlobal("fetch", fetchSpy);
    const client = { account: { actor: { role, can_manage: false, can_manage_runners: false } } } as unknown as ConversationClient;
    const { container } = render(
      <ClientContext.Provider value={client}>
        <SidebarUpdatePill />
      </ClientContext.Provider>,
    );
    expect(container.innerHTML).toBe("");
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});
