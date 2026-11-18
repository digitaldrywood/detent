// @vitest-environment jsdom
vi.mock("../../src/app/client.ts", () => ({ useClient: () => ({ bootstrap: { projects: [{ id: "project_one", name: "Example project" }, { id: "project_two", name: "Second project" }] } }) }));
vi.mock("../../src/app/components/ProjectGlyph.tsx", () => ({ ProjectGlyph: () => null }));
import React from "react";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  SavedViewsProvider,
  useSavedViews,
  type SavedView,
} from "../../src/app/work/lib/savedViews.tsx";
import {
  SavedViewControls,
  SaveViewDialog,
  ViewActions,
  ViewsPage,
} from "../../src/app/work/SavedViews.tsx";
import {
  DEFAULT_VIEW_STATE,
  type WorkViewState,
} from "../../src/app/work/lib/viewState.ts";

const fixture = vi.hoisted(() => ({
  http: {
    listSavedViews: vi.fn(),
    saveView: vi.fn(),
    favoriteView: vi.fn(),
    deleteView: vi.fn(),
  },
  navigate: vi.fn(),
}));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({
  useWorkHttp: () => fixture.http,
}));
vi.mock("@tanstack/react-router", () => ({
  useNavigate: () => fixture.navigate,
}));
vi.mock("../../src/app/work/components/WorkTopBar.tsx", () => ({
  WorkTopBar: () => <h1>Views</h1>,
}));
vi.mock("../../src/app/work/WorkBoard.tsx", () => ({ WorkBoard: () => null }));
vi.mock("../../src/app/App.tsx", () => ({
  useShell: () => ({ connection: {} }),
}));
vi.mock("../../src/app/pageTitle.ts", () => ({ usePageTitle: () => {} }));

const state: WorkViewState = {
  ...DEFAULT_VIEW_STATE,
  view: "list",
  tab: "closed",
  archived: true,
  completedWindow: "7d",
  label: ["bug"],
};
const view: SavedView = {
  id: "view_one",
  owner_id: "alice",
  can_edit: true,
  name: "Closed bugs",
  scope: "personal",
  project_id: "project_one", project_ids: ["project_one"],
  favorite: true,
  state,
};

function Controls({
  initial = state,
  active = false,
}: {
  initial?: WorkViewState;
  active?: boolean;
}): React.ReactElement {
  const saved = useSavedViews();
  const [current, setCurrent] = React.useState(initial);
  const selected = active ? saved?.views[0] : undefined;
  return (
    <>
      <button
        onClick={() =>
          setCurrent({
            ...current,
            sort: current.sort === "title" ? "updated" : "title",
          })
        }
      >
        Edit sort
      </button>
      <SavedViewControls
        state={current}
        projectId="project_one"
        active={selected}
        onChange={setCurrent}
      />
      <output>{current.sort}</output>
    </>
  );
}
function Actions(): React.ReactElement | null {
  const saved = useSavedViews();
  return saved?.views[0] ? <ViewActions view={saved.views[0]} /> : null;
}
function mount(children: React.ReactNode): void {
  render(<SavedViewsProvider>{children}</SavedViewsProvider>);
}

beforeEach(() => {
  fixture.http.listSavedViews.mockResolvedValue({ views: [] });
  fixture.http.saveView.mockImplementation(async (input, id) => ({
    ...input,
    id: id ?? "view_new",
    owner_id: "alice",
    can_edit: true,
  }));
  fixture.http.favoriteView.mockImplementation(async (_id, favorite) => ({
    ...view,
    favorite,
  }));
  fixture.http.deleteView.mockResolvedValue(undefined);
});
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("Saved Work views", () => {
  it.each([
    { ids: [], focus: null, opens: "All 2 projects", names: ["Example project", "Second project"] },
    { ids: ["project_one"], focus: "project_one", opens: "Example project", names: ["Example project"] },
    { ids: ["project_one", "project_two"], focus: "project_two", opens: "Second project", names: ["Example project", "Second project"] },
    { ids: ["project_one", "project_two"], focus: null, opens: "All 2 projects", names: ["Example project", "Second project"], selectedLanes: ["Todo"] },
  ])("saves the working set $ids opening on $opens", async ({ ids, focus, opens, names, selectedLanes }) => {
    const user = userEvent.setup();
    const savedState = { ...state, lanes: selectedLanes ?? null };
    const lanes = [{ id: "todo", name: "Todo", terminal: false, dispatchable: true, category: "unstarted" }, { id: "backlog", name: "Backlog", terminal: false, dispatchable: false, category: "backlog" }, { id: "done", name: "Done", terminal: true, dispatchable: false, category: "completed" }];
    mount(<SaveViewDialog state={savedState} projectId={focus} projectIds={ids} lanes={lanes} onClose={() => {}} onSaved={() => {}} />);
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Opens on").nextElementSibling?.textContent).toBe(opens);
    const chips = within(dialog).getByLabelText("Projects");
    for (const name of names) expect(within(chips).getByText(name)).toBeTruthy();
    if (ids.length === 1) expect(within(chips).queryByText("Second project")).toBeNull();
    expect(within(dialog).getByRole("checkbox", { name: "Pin to Favorites" }).getAttribute("aria-checked")).toBe("true");
    expect(within(dialog).getByText("Sort").nextElementSibling?.textContent).toBe("Closed");
    expect(within(dialog).getByText("Filters").nextElementSibling?.textContent).toBe("Label: bug · Including archived");
    expect(within(dialog).getByText("Layout").nextElementSibling?.textContent).toBe(selectedLanes ? "List · Lanes 1/2" : "List · Lanes 2/2");
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "Project setup");
    await user.click(within(dialog).getByRole("button", { name: "Save view" }));
    await waitFor(() => expect(fixture.http.saveView).toHaveBeenCalledWith(expect.objectContaining({ project_ids: ids, project_id: focus, state: savedState, favorite: true }), undefined));
  });

  it("updates a view with the current working set, focus and toolbar settings", async () => {
    fixture.http.listSavedViews.mockResolvedValue({ views: [view] });
    const setup = { project_ids: ["project_one", "project_two"], project_id: "project_two", state: { ...state, sort: "title" as const, lanes: ["Todo"], collapsed: ["Done"] } };
    function Current(): React.ReactElement {
      const saved = useSavedViews();
      const publish = saved?.setCurrentSetup;
      React.useLayoutEffect(() => publish?.(setup), [publish]);
      return <Actions />;
    }
    mount(<Current />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Actions for Closed bugs" }));
    await user.click(await screen.findByRole("menuitem", { name: "Update with current setup" }));
    await waitFor(() => expect(fixture.http.saveView).toHaveBeenCalledWith({ ...view, ...setup }, view.id));
  });

  it("saves the complete current state, defaults to favorites and personal, and opens the new view", async () => {
    const user = userEvent.setup();
    mount(<Controls />);
    await user.click(
      await screen.findByRole("button", { name: "Save as view" }),
    );
    const dialog = screen.getByRole("dialog");
    expect(
      within(dialog)
        .getByRole("checkbox", { name: "Pin to Favorites" })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      within(dialog)
        .getByRole("switch", { name: "Share with organization" })
        .getAttribute("aria-checked"),
    ).toBe("false");
    expect(within(dialog).getByText("Completed").nextElementSibling?.textContent).toBe("7 days");
    await user.type(
      within(dialog).getByRole("textbox", { name: "Name" }),
      "Closed bugs",
    );
    await user.click(
      within(dialog).getByRole("button", { name: "Save view" }),
    );
    await waitFor(() =>
      expect(fixture.http.saveView).toHaveBeenCalledWith(
        {
          name: "Closed bugs",
          scope: "personal",
          project_id: "project_one", project_ids: ["project_one"],
          favorite: true,
          state,
        },
        undefined,
      ),
    );
    expect(fixture.navigate).toHaveBeenCalledWith({
      to: "/work/views/view_new",
    });
  });

  it("allows saving default settings and hides only matching saved setups", async () => {
    fixture.http.listSavedViews.mockResolvedValue({ views: [view] });
    mount(
      <>
        <Controls initial={DEFAULT_VIEW_STATE} />
        <Controls />
      </>,
    );
    await waitFor(() => expect(fixture.http.listSavedViews).toHaveBeenCalled());
    expect(screen.getAllByRole("button", { name: "Save as view" })).toHaveLength(1);
  });

  it("resets edits, saves edits in place and saves a changed view as new", async () => {
    fixture.http.listSavedViews.mockResolvedValue({ views: [view] });
    const user = userEvent.setup();
    mount(<Controls active />);
    await screen.findByRole("button", { name: "Actions for Closed bugs" });
    await user.click(screen.getByRole("button", { name: "Edit sort" }));
    expect(screen.getByText("Edited")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Reset" }));
    expect(screen.queryByText("Edited")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("default");
    await user.click(screen.getByRole("button", { name: "Edit sort" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByText("Edited")).toBeNull());
    expect(fixture.http.saveView).toHaveBeenCalledWith(
      expect.objectContaining({ state: { ...state, sort: "title" } }),
      "view_one",
    );
    await user.click(screen.getByRole("button", { name: "Edit sort" }));
    await user.click(screen.getByRole("button", { name: "Save as new" }));
    await user.type(
      screen.getByRole("textbox", { name: "Name" }),
      "Another view",
    );
    await user.click(screen.getByRole("button", { name: "Save view" }));
    await waitFor(() =>
      expect(fixture.http.saveView).toHaveBeenLastCalledWith(
        expect.objectContaining({ state: { ...state, sort: "updated" } }),
        undefined,
      ),
    );
  });

  it.each([
    "Rename",
    "Duplicate",
    "Unpin from Favorites",
    "Share with organization",
    "Delete view",
  ])("runs the %s menu action", async (action) => {
    fixture.http.listSavedViews.mockResolvedValue({ views: [view] });
    const user = userEvent.setup();
    mount(<Actions />);
    await user.click(
      await screen.findByRole("button", { name: "Actions for Closed bugs" }),
    );
    await user.click(await screen.findByRole("menuitem", { name: action }));
    if (action === "Delete view") {
      expect(fixture.http.deleteView).not.toHaveBeenCalled();
      const dialog = screen.getByRole("alertdialog");
      await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
      expect(fixture.http.deleteView).not.toHaveBeenCalled();
      await user.click(
        screen.getByRole("button", { name: "Actions for Closed bugs" }),
      );
      await user.click(await screen.findByRole("menuitem", { name: action }));
      await user.click(
        within(screen.getByRole("alertdialog")).getByRole("button", {
          name: "Delete view",
        }),
      );
      await waitFor(() =>
        expect(fixture.http.deleteView).toHaveBeenCalledWith("view_one"),
      );
    } else if (action === "Unpin from Favorites") {
      await waitFor(() =>
        expect(fixture.http.favoriteView).toHaveBeenCalledWith(
          "view_one",
          false,
        ),
      );
    } else if (action === "Share with organization") {
      await waitFor(() =>
        expect(fixture.http.saveView).toHaveBeenCalledWith(
          expect.objectContaining({ scope: "organization" }),
          "view_one",
        ),
      );
    } else {
      const dialog = screen.getByRole("dialog");
      fireEvent.change(within(dialog).getByRole("textbox", { name: "Name" }), {
        target: { value: "New name" },
      });
      await user.click(
        within(dialog).getByRole("button", {
          name: action === "Rename" ? "Rename" : "Save view",
        }),
      );
      await waitFor(() =>
        expect(fixture.http.saveView).toHaveBeenCalledWith(
          expect.objectContaining({ name: "New name", state }),
          action === "Rename" ? "view_one" : undefined,
        ),
      );
    }
  });

  it("allows readers to duplicate and favorite shared views but not modify or delete them", async () => {
    fixture.http.listSavedViews.mockResolvedValue({
      views: [{ ...view, can_edit: false, scope: "organization" }],
    });
    const user = userEvent.setup();
    mount(<Controls active />);
    await user.click(
      await screen.findByRole("button", { name: "Actions for Closed bugs" }),
    );
    expect(
      await screen.findByRole("menuitem", { name: "Duplicate" }),
    ).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Rename" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Delete view" })).toBeNull();
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Edit sort" }));
    expect(screen.queryByRole("button", { name: /^Save$/ })).toBeNull();
    expect(screen.getByRole("button", { name: "Save as new" })).toBeTruthy();
  });

  it("lists personal and organization views and opens the selected view", async () => {
    fixture.http.listSavedViews.mockResolvedValue({
      views: [
        view,
        {
          ...view,
          id: "view_two",
          name: "Team work",
          scope: "organization",
          project_id: null, project_ids: [],
        },
      ],
    });
    const user = userEvent.setup();
    mount(<ViewsPage />);
    await user.click(await screen.findByRole("button", { name: "Team work" }));
    expect(fixture.navigate).toHaveBeenCalledWith({
      to: "/work/views/view_two",
    });
    expect(screen.getByText("Organization · All projects")).toBeTruthy();
  });

  it("keeps the save dialog and edits available after a failed request", async () => {
    fixture.http.saveView.mockRejectedValue(new Error("offline"));
    const user = userEvent.setup();
    mount(<Controls />);
    await user.click(
      await screen.findByRole("button", { name: "Save as view" }),
    );
    await user.type(screen.getByRole("textbox", { name: "Name" }), "My view");
    await user.click(screen.getByRole("button", { name: "Save view" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(
      screen.getByRole("textbox", { name: "Name" }).getAttribute("value"),
    ).toBe("My view");
    expect(fixture.navigate).not.toHaveBeenCalled();
  });
});
