import userEvent from "@testing-library/user-event";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ThreadAgentBadge,
  ThreadFiltersBar,
  ThreadReplyComposer,
  ThreadUnreadBadge,
} from "../../src/app/threads/ThreadsPage.tsx";
import type { ThreadFilters } from "../../src/app/threads/api.ts";
import * as threadsApi from "../../src/app/threads/api.ts";
import { ThreadsSidebarBadge } from "../../src/app/threads/ThreadBadge.tsx";
import { ClientContext } from "../../src/app/client.ts";
import { makeClient } from "../../src/runtime/bootstrap.ts";
import type { Bootstrap } from "../../src/contracts/index.ts";
import bootstrapFixture from "../../src/contracts/fixtures/bootstrap.json";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("team threads", () => {
  it("preserves search and status when the scope filter changes", () => {
    let current: ThreadFilters = { status: "open" };
    const onChange = vi.fn((value: ThreadFilters) => {
      current = value;
      view.rerender(<ThreadFiltersBar value={current} onChange={onChange} />);
    });
    const view = render(
      <ThreadFiltersBar value={current} onChange={onChange} />,
    );
    fireEvent.change(screen.getByRole("textbox", { name: "Search threads" }), {
      target: { value: "research" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Resolved" }));
    fireEvent.click(screen.getByRole("button", { name: "Issue" }));
    expect(current).toEqual({
      status: "resolved",
      search: "research",
      scope: "issue",
    });
    expect(
      screen
        .getByRole("button", { name: "Issue" })
        .getAttribute("aria-pressed"),
    ).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Scope: All" }));
    expect(current.scope).toBe("");
  });

  it("shows an unread badge only when threads have unseen posts", () => {
    const view = render(<ThreadUnreadBadge count={3} />);
    expect(screen.getByLabelText("3 unread threads").textContent).toBe("3");
    view.rerender(<ThreadUnreadBadge count={0} />);
    expect(screen.queryByLabelText(/unread threads/)).toBeNull();
  });

  it("reads one summary per refresh without walking thread history and cancels stale results", async () => {
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    const unread = vi
      .fn<threadsApi.ThreadsApi["unread"]>()
      .mockResolvedValueOnce({ count: 1, pending: false })
      .mockImplementationOnce(async () => {
        await pending;
        return { count: 99, pending: false };
      })
      .mockResolvedValueOnce({ count: 0, pending: false });
    const list = vi.fn<threadsApi.ThreadsApi["list"]>();
    vi.spyOn(threadsApi, "useThreadsApi").mockReturnValue({
      ...threadsApi.makeThreadsApi({ origin: "", apiBase: "", csrfToken: "" }),
      unread,
      list,
    });
    const client = makeClient({ bootstrap: bootstrapFixture as Bootstrap });
    try {
      render(
        <ClientContext.Provider value={client}>
          <ThreadsSidebarBadge />
        </ClientContext.Provider>,
      );
      expect(list).not.toHaveBeenCalled();
      expect(
        (await screen.findByLabelText("1 unread threads")).textContent,
      ).toBe("1");
      expect(unread).toHaveBeenCalledTimes(1);
      act(() =>
        globalThis.dispatchEvent(new Event(threadsApi.THREADS_CHANGED)),
      );
      const superseded = unread.mock.calls[1]?.[2];
      act(() => globalThis.dispatchEvent(new Event("focus")));
      await waitFor(() =>
        expect(screen.queryByLabelText(/unread threads/)).toBeNull(),
      );
      expect(unread).toHaveBeenCalledTimes(3);
      expect(superseded?.aborted).toBe(true);
      expect(list).not.toHaveBeenCalled();
      await act(async () => {
        release();
        await pending;
      });
      expect(screen.queryByLabelText(/unread threads/)).toBeNull();
    } finally {
      cleanup();
      release();
      await pending;
      client.handles.clear();
    }
  });

  it("checks pending summaries once per timer tick without requeueing their scan", async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(0.5);
    const unread = vi
      .fn<threadsApi.ThreadsApi["unread"]>()
      .mockResolvedValueOnce({ count: null, pending: true })
      .mockResolvedValueOnce({ count: null, pending: true })
      .mockResolvedValueOnce({ count: 2, pending: false });
    const list = vi.fn<threadsApi.ThreadsApi["list"]>();
    vi.spyOn(threadsApi, "useThreadsApi").mockReturnValue({
      ...threadsApi.makeThreadsApi({ origin: "", apiBase: "", csrfToken: "" }),
      unread,
      list,
    });
    const client = makeClient({ bootstrap: bootstrapFixture as Bootstrap });
    try {
      await act(async () => {
        render(
          <ClientContext.Provider value={client}>
            <ThreadsSidebarBadge />
          </ClientContext.Provider>,
        );
      });
      expect(screen.getByLabelText("Loading unread threads")).toBeTruthy();
      expect(unread).toHaveBeenCalledTimes(1);
      for (const expected of [2, 3]) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(1500);
        });
        expect(unread).toHaveBeenCalledTimes(expected);
        expect(unread.mock.calls[expected - 1]?.[1]).toBe(false);
      }
      expect(screen.getByLabelText("2 unread threads").textContent).toBe("2");
      expect(list).not.toHaveBeenCalled();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(unread).toHaveBeenCalledTimes(3);
    } finally {
      cleanup();
      client.handles.clear();
      vi.useRealTimers();
    }
  });

  it("attributes agent posts to their human and client", () => {
    const view = render(
      <ThreadAgentBadge
        post={{
          author_name: "Alex",
          acting_for: "principal_alex",
          client_name: "Example client",
        }}
      />,
    );
    expect(screen.getByText("Alex’s agent")).toBeTruthy();
    expect(screen.getByText("Example client · for Alex")).toBeTruthy();
    view.rerender(<ThreadAgentBadge post={{ author_name: "Alex" }} />);
    expect(screen.getByText("Alex")).toBeTruthy();
    expect(screen.queryByText(/for Alex/)).toBeNull();
  });

  it("keeps a failed reply and request identity for retry, then clears on success", async () => {
    const onPost = vi
      .fn()
      .mockRejectedValueOnce(new Error("Reply unavailable"))
      .mockResolvedValue(undefined);
    render(<ThreadReplyComposer onPost={onPost} />);
    expect(
      (screen.getByRole("button", { name: "Post" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Reply" }), {
      target: { value: "Research finding" },
    });
    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: "Post type" }));
    await user.click(await screen.findByRole("option", { name: "Finding" }));
    fireEvent.click(screen.getByRole("button", { name: "Post" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Reply unavailable",
    );
    expect(
      (screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement)
        .value,
    ).toBe("Research finding");
    fireEvent.click(screen.getByRole("button", { name: "Post" }));
    await waitFor(() =>
      expect(
        (screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement)
          .value,
      ).toBe(""),
    );
    expect(onPost).toHaveBeenCalledTimes(2);
    expect(onPost.mock.calls[0]).toEqual(onPost.mock.calls[1]);
    expect(onPost.mock.calls[0]?.slice(0, 2)).toEqual([
      "finding",
      "Research finding",
    ]);
  });

  it("disables repeat submissions while a reply is pending", async () => {
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    const onPost = vi.fn(() => pending);
    try {
      render(<ThreadReplyComposer onPost={onPost} />);
      fireEvent.change(screen.getByRole("textbox", { name: "Reply" }), {
        target: { value: "Note" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Post" }));
      expect(
        (screen.getByRole("button", { name: "Posting…" }) as HTMLButtonElement)
          .disabled,
      ).toBe(true);
      fireEvent.submit(screen.getByRole("form", { name: "Reply to thread" }));
      expect(onPost).toHaveBeenCalledOnce();
    } finally {
      await act(async () => {
        release();
        await pending;
      });
    }
  });
});
