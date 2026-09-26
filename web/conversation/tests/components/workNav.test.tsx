// @vitest-environment jsdom
//
// The sidebar's Work destination and Browse group (design inventory A.1, A.6).
// They are the one Detent insertion in the copied `components/Sidebar.tsx`,
// and they live in `app/adapters/sidebarDestinations.tsx`.
import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { renderSidebar, resetSidebarState } from "./sidebarHarness.tsx";

afterEach(cleanup);
beforeEach(resetSidebarState);

describe("the sidebar's Work navigation", () => {
  it("sends Work to the active project's board", async () => {
    const { onNavigate } = await renderSidebar();
    fireEvent.click(screen.getByTestId("nav-work"));
    expect(onNavigate).toHaveBeenCalledWith("/work/p/proj_alpha");
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
    for (const id of ["activity", "diagnostics", "reports", "library"]) {
      const row = screen.getByTestId(`nav-${id}`) as HTMLButtonElement;
      expect(row.disabled, id).toBe(true);
      fireEvent.click(row);
    }
    expect(onNavigate).not.toHaveBeenCalled();
    // The tooltip's text is on a wrapper the keyboard can reach, so the reason
    // is available without a pointer.
    expect(screen.getByLabelText("Activity: coming soon")).not.toBeNull();
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
