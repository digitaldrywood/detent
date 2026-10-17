// @vitest-environment jsdom
//
// The sidebar's Work destination and Browse group (design inventory A.1, A.6).
// They are the one Detent insertion in the copied `components/Sidebar.tsx`,
// and they live in `app/adapters/sidebarDestinations.tsx`.
import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { SavedView } from "../../src/app/work/lib/savedViews.tsx";
import { DEFAULT_VIEW_STATE } from "../../src/app/work/lib/viewState.ts";
const savedViews = vi.hoisted(() => ({ value: null as { views: SavedView[] } | null }));
vi.mock("../../src/app/work/lib/savedViews.tsx", async (original) => ({
  ...await original<typeof import("../../src/app/work/lib/savedViews.tsx")>(),
  useSavedViews: () => savedViews.value,
}));

import { renderSidebar, resetSidebarState } from "./sidebarHarness.tsx";

afterEach(cleanup);
beforeEach(() => { resetSidebarState(); savedViews.value = null; });

describe("the sidebar's Work navigation", () => {
  it("opens pinned views, marks the active favorite and keeps unpinned views in Views", async () => {
    const view: SavedView = { id: "view_one", name: "Active bugs", owner_id: "alice", can_edit: true, project_id: null, scope: "personal", favorite: true, state: DEFAULT_VIEW_STATE };
    savedViews.value = { views: [view, { ...view, id: "view_two", name: "Unpinned", favorite: false }] };
    const onNavigate = vi.fn();
    await renderSidebar({ navigation: { activePath: "/work/views/view_one", onNavigate } });
    const favorite = screen.getByRole("button", { name: "Active bugs" });
    expect(favorite.getAttribute("aria-current")).toBe("page");
    expect(screen.getByTestId("nav-work").getAttribute("aria-current")).toBeNull();
    expect(screen.queryByRole("button", { name: "Unpinned" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Favorites" }));
    expect(screen.queryByRole("button", { name: "Active bugs" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Favorites" }));
    fireEvent.click(screen.getByRole("button", { name: "Active bugs" }));
    expect(onNavigate).toHaveBeenLastCalledWith("/work/views/view_one");
    fireEvent.click(screen.getByRole("button", { name: "Views" }));
    expect(onNavigate).toHaveBeenLastCalledWith("/work/views");
    fireEvent.click(screen.getByRole("button", { name: "Actions for Active bugs" }));
    expect(await screen.findByRole("menuitem", { name: "Rename" })).toBeTruthy();
    expect(within(screen.getByRole("menu")).getByRole("menuitem", { name: "Delete view" })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
  });
  it("sends Work to the active project's board", async () => {
    const { onNavigate } = await renderSidebar();
    fireEvent.click(screen.getByTestId("nav-work"));
    expect(onNavigate).toHaveBeenCalledWith("/work/p/proj_alpha");
  });

  it.each([
    ["activity", "/activity"],
    ["diagnostics", "/diagnostics"],
    ["reports", "/reports"],
  ])("opens %s from Browse", async (id, destination) => {
    const { onNavigate } = await renderSidebar();
    const row = screen.getByTestId(`nav-${id}`) as HTMLButtonElement;
    expect(row.disabled).toBe(false);
    fireEvent.click(row);
    expect(onNavigate).toHaveBeenCalledWith(destination);
  });

  it.each([
    ["work", "/work/p/proj_alpha"],
    ["activity", "/activity"],
    ["diagnostics", "/diagnostics"],
    ["reports", "/reports"],
  ])("prefetches %s on hover and on focus", async (id, destination) => {
    const onPrefetch = vi.fn();
    await renderSidebar({ navigation: { activePath: "/chat", onNavigate: () => {}, onPrefetch } });
    const row = screen.getByTestId(`nav-${id}`);
    fireEvent.pointerEnter(row);
    fireEvent.focus(row);
    expect(onPrefetch.mock.calls).toEqual([[destination], [destination]]);
  });

  it("sends Work to the all-projects board when no project is chosen", async () => {
    const { onNavigate } = await renderSidebar({ activeProjectId: null });
    fireEvent.click(screen.getByTestId("nav-work"));
    expect(onNavigate).toHaveBeenCalledWith("/work");
  });

  it("marks Work current on every work route", async () => {
    await renderSidebar({
      navigation: { activePath: "/work/i/wi_1", onNavigate: () => {} },
    });
    expect(screen.getByTestId("nav-work").getAttribute("aria-current")).toBe(
      "page",
    );
  });

  // §17.4: Pull requests, Usage and Settings left Browse — the footer icon row
  // already reaches all three, and one destination in two places in the same
  // sidebar is a duplicate, not a convenience.
  it("keeps the footer's destinations out of Browse", async () => {
    await renderSidebar();
    expect(screen.queryByTestId("nav-changes")).toBeNull();
    expect(screen.queryByTestId("nav-fleet")).toBeNull();
    expect(screen.queryByTestId("nav-settings")).toBeNull();
  });

  it("disables the destinations this client does not serve", async () => {
    const { onNavigate } = await renderSidebar();
    for (const id of ["library"]) {
      const row = screen.getByTestId(`nav-${id}`) as HTMLButtonElement;
      expect(row.disabled, id).toBe(true);
      fireEvent.click(row);
    }
    expect(onNavigate).not.toHaveBeenCalled();
    // The tooltip's text is on a wrapper the keyboard can reach, so the reason
    // is available without a pointer.
    expect(screen.getByLabelText("Library: coming soon")).not.toBeNull();
  });

  it("renders no navigation at all when the shell gives it none", async () => {
    await renderSidebar({ navigation: undefined });
    expect(screen.queryByTestId("nav-work")).toBeNull();
    expect(screen.queryByTestId("nav-activity")).toBeNull();
  });

  it("collapses Browse with the section header", async () => {
    await renderSidebar();
    fireEvent.click(screen.getByTestId("sidebar-browse-shelf-toggle"));
    expect(screen.queryByTestId("nav-activity")).toBeNull();
  });
});
