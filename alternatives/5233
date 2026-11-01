import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { makeAccountApi, type AccountApi } from "../../src/app/account/api.ts";
import { plainSearchOptions } from "../../src/app/lib/searchParams.ts";
import { ReportsRoute } from "../../src/app/reports/ReportsPage.tsx";
import type { OrganizationReports } from "../../src/contracts/reports.ts";
import organizationFixture from "../../src/contracts/fixtures/organizationReports.json";
import projectFixture from "../../src/contracts/fixtures/reports.json";

const facets = { labels: ["cloud", "ui"], filed_by: [{ id: "person_one", name: "Example person" }], runners: [{ id: "runner_one", name: "Build runner" }, { id: "runner_two", name: "Hosted runners" }], models: [], stages: [] };

const projects = [
  { id: "prj_alpha", name: "Alpha", can_write: true },
  { id: "prj_beta", name: "Beta", can_write: true },
];
let api: AccountApi;
let activeProjectId: string | null;
type MutableMetrics = { -readonly [K in keyof OrganizationReports["current"]]: OrganizationReports["current"][K] };
let response: Omit<typeof organizationFixture, "current" | "previous"> & { current: MutableMetrics; previous: MutableMetrics };
let requests: URL[];
const changeProject = vi.fn((id: string) => { activeProjectId = id; });

vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api }));
vi.mock("../../src/app/adapters/sidebarData.tsx", async (importOriginal) => ({ ...await importOriginal<typeof import("../../src/app/adapters/sidebarData.tsx")>(), useSidebarData: () => ({ projects, activeProjectId, onProjectChange: changeProject }) }));

beforeEach(() => {
  activeProjectId = null;
  response = structuredClone(organizationFixture);
  requests = [];
  changeProject.mockClear();
  api = makeAccountApi({ origin: "https://reports.test", apiBase: "/organizations/org_example/api", csrfToken: "test", fetch: vi.fn(async (input) => {
    const url = new URL(String(input), "https://reports.test");
    requests.push(url);
    const payload = url.pathname.endsWith("/options") ? facets : url.searchParams.get("scope") === "organization" ? response : projectFixture;
    return new Response(JSON.stringify(payload), { headers: { "Content-Type": "application/json" } });
  }) });
});
afterEach(cleanup);

async function openReports(path = "/reports") {
  const root = createRootRoute({ component: Outlet });
  const overview = createRoute({ getParentRoute: () => root, path: "/reports", component: ReportsRoute });
  const tab = createRoute({ getParentRoute: () => root, path: "/reports/$tab", component: ReportsRoute });
  const router = createRouter({ routeTree: root.addChildren([overview, tab]), history: createMemoryHistory({ initialEntries: [path] }), ...plainSearchOptions });
  await act(async () => { await router.load(); });
  render(<RouterProvider router={router} />);
  await waitFor(() => expect(screen.queryByRole("status", { name: "Loading reports" })).toBeNull());
  return router;
}

function organizationReads() {
  return requests.filter((url) => url.searchParams.get("scope") === "organization");
}

function panel(title: string) {
  const heading = screen.getByRole("heading", { name: title });
  return within(heading.closest("section")!);
}

describe("organization Reports Overview", () => {
  it.each([
    ["project", { range: "30d" }],
    ["project", { labels: "cloud" }],
    ["organization", { range: "30d" }],
    ["organization", { labels: "cloud" }],
  ] as const)("shows a failed %s read after changing %j with cached data", async (scope, search) => {
    activeProjectId = scope === "project" ? "prj_alpha" : null;
    const router = await openReports();
    expect(screen.getByLabelText("Headline metrics")).toBeTruthy();
    let rejectRead!: (error: Error) => void;
    const nextRead = new Promise<never>((_, reject) => { rejectRead = reject; });
    const read = vi.spyOn(api, scope === "project" ? "reports" : "organizationReports").mockReturnValueOnce(nextRead);
    await act(async () => { await router.navigate({ to: "/reports", search: { project: scope === "project" ? "prj_alpha" : "all", ...search } as never }); });
    await waitFor(() => expect(read).toHaveBeenCalled());
    for (const button of screen.getAllByRole("button", { name: "Refresh reports" })) expect.soft((button as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { rejectRead(new Error("Report read failed")); });
    expect(await screen.findByText("Reports are temporarily unavailable.")).toBeTruthy();
    expect(screen.queryByLabelText("Headline metrics")).toBeNull();
    expect(screen.queryByRole("heading", { name: scope === "project" ? "Shipped per day" : "By project" })).toBeNull();
    for (const button of screen.getAllByRole("button", { name: "Refresh reports" })) expect((button as HTMLButtonElement).disabled).toBe(false);
  });

  it("loads at sidebar organization scope and renders every approved section and project tooltip", async () => {
    const router = await openReports();
    expect(router.state.location.search).toMatchObject({ project: "all" });
    expect(organizationReads()[0]?.searchParams.get("project")).toBe("all");
    expect(screen.getByRole("combobox", { name: "Project" }).textContent).toContain("All projects");
    const metrics = screen.getByLabelText("Headline metrics");
    for (const label of ["Filed", "Completed", "Net backlog", "Changes landed", "Cost per completed", "Rework"]) expect(within(metrics).getByText(label)).toBeTruthy();
    expect(within(metrics).getByText("142")).toBeTruthy();
    expect(within(metrics).getByText("plus 9 cancelled")).toBeTruthy();
    for (const title of ["Filed vs completed per day", "Who filed", "Who completed", "By project"]) expect(screen.getByRole("heading", { name: title })).toBeTruthy();
    expect(panel("Who filed").getByText("54")).toBeTruthy();
    expect(panel("Who completed").getByRole("link", { name: "Build runner" })).toBeTruthy();
    expect(panel("Who completed").getByText("Hand-landed by people")).toBeTruthy();
    const table = screen.getByRole("table", { name: "Productivity by project" });
    expect(within(table).getByRole("row", { name: /Alpha 96 88/ }).textContent).toContain("84");
    expect(within(table).getByRole("row", { name: /Beta 46 40/ }).textContent).toContain("Not recorded");
    fireEvent.keyDown(screen.getByRole("img", { name: "Filed and completed issues per bucket" }), { key: "ArrowRight" });
    expect(screen.getByRole("status").textContent).toContain("Alpha");
    expect(screen.getByRole("status").textContent).toContain("12 filed · 12 completed");
  });

  it("refetches all sections for range, compare and every filter and keeps them in the URL", async () => {
    const router = await openReports();
    response.current.filed = 25;
    response.series[0]!.filed = 25;
    response.filed_by[0]!.current = 25;
    response.completed_by[0]!.current = 20;
    response.projects[0]!.current.filed = 25;
    await act(async () => { await router.navigate({ to: "/reports", search: { project: "all", range: "30d", compare: "off", projects: ["prj_alpha", "prj_beta"], labels: ["cloud", "ui"], priorities: "High", types: "bug", filed_by: "person_one", runners: "runner_one" } as never }); });
    await waitFor(() => expect(within(screen.getByLabelText("Headline metrics")).getByText("25")).toBeTruthy());
    const query = organizationReads().at(-1)!.searchParams;
    expect(query.get("range")).toBe("30d");
    expect(query.get("compare")).toBe("off");
    expect(query.getAll("projects")).toEqual(["prj_alpha", "prj_beta"]);
    expect(query.getAll("labels")).toEqual(["cloud", "ui"]);
    expect(query.get("priorities")).toBe("1");
    expect(query.get("types")).toBe("bug");
    expect(query.get("filed_by")).toBe("person_one");
    expect(query.get("runners")).toBe("runner_one");
    expect(panel("Who filed").getByText("25")).toBeTruthy();
    expect(panel("Who completed").getByText("20")).toBeTruthy();
    expect(screen.getByRole("row", { name: /Alpha 25 88/ })).toBeTruthy();
    expect(screen.queryByText("Prior 30d completed")).toBeNull();
    expect(router.state.location.search).toMatchObject({ project: "all", compare: "off", filed_by: "person_one" });
    fireEvent.keyDown(screen.getByRole("img", { name: "Filed and completed issues per bucket" }), { key: "ArrowRight" });
    expect(screen.getByRole("status").textContent).toContain("25");
    await act(async () => { await router.navigate({ to: "/reports", search: { project: "all", from: "2026-10-01", to: "2026-10-05" } as never }); });
    await waitFor(() => expect(organizationReads().at(-1)!.searchParams.get("to")).toBe("2026-10-05"));
    expect(organizationReads().at(-1)!.searchParams.get("from")).toBe("2026-10-01");
    const projectLink = new URL(screen.getByRole("link", { name: "Beta" }).getAttribute("href")!, "https://reports.test");
    expect(projectLink.searchParams.get("from")).toBe("2026-10-01");
    expect(projectLink.searchParams.get("to")).toBe("2026-10-05");
    expect(projectLink.searchParams.has("compare")).toBe(false);
  });

  it("applies project, filer and runner choices and preserves range and compare in project links", async () => {
    const router = await openReports("/reports?range=90d&compare=off");
    const link = screen.getByRole("link", { name: "Beta" });
    const href = new URL(link.getAttribute("href")!, "https://reports.test");
    expect(href.searchParams.get("project")).toBe("prj_beta");
    expect(href.searchParams.get("range")).toBe("90d");
    expect(href.searchParams.get("compare")).toBe("off");
    fireEvent.click(screen.getByRole("combobox", { name: "Project, filter" }));
    fireEvent.click(await screen.findByRole("option", { name: "Alpha" }));
    await waitFor(() => expect(organizationReads().at(-1)!.searchParams.get("projects")).toBe("prj_alpha"));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search project values" }), { key: "Escape" });
    fireEvent.click(screen.getByRole("combobox", { name: "Filter by filer" }));
    fireEvent.click(await screen.findByRole("option", { name: "Example person" }));
    await waitFor(() => expect(organizationReads().at(-1)!.searchParams.get("filed_by")).toBe("person_one"));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search filed by values" }), { key: "Escape" });
    fireEvent.click(screen.getByRole("combobox", { name: "Filter by runner" }));
    const option = await screen.findByRole("option", { name: "Build runner" });
    fireEvent.click(option);
    await waitFor(() => expect(organizationReads().at(-1)!.searchParams.getAll("runners")).not.toEqual([]));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search runner values" }), { key: "Escape" });
    await act(async () => { await router.navigate({ to: "/reports", search: { project: "all", range: "90d", compare: "off" } as never }); });
    fireEvent.click(screen.getByRole("link", { name: "Beta" }));
    await waitFor(() => expect(requests.some((url) => url.searchParams.get("project") === "prj_beta")).toBe(true));
    expect(changeProject).toHaveBeenCalledWith("prj_beta");
    expect(screen.getByRole("heading", { name: "Shipped per day" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Who filed" })).toBeNull();
  });

  it("shows empty and not-recorded treatments without inventing measured values", async () => {
    const empty = { filed: 0, completed: 0, cancelled: 0, net_backlog: 0, changes_landed: 0, worked: 0, reworked: 0, completion_cost_usd: 0, cost_per_completed_usd: null, cycle_p50_seconds: null, rework_percent: null };
    response = { ...response, current: empty, previous: empty, projects: [], series: [], previous_series: [], filed_by: [], completed_by: [] };
    await openReports();
    expect(screen.getByText("No issues filed in this range.")).toBeTruthy();
    expect(screen.getByText("No issues completed in this range.")).toBeTruthy();
    expect(screen.getByText("No projects match these filters.")).toBeTruthy();
    expect(screen.getAllByTitle("Not recorded").length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText("$0.00")).toBeNull();
    expect(screen.queryByText("0%")).toBeNull();
  });

  it.each([false, true])("distinguishes prior availability %s from recorded zero activity", async (recorded) => {
    response.prior_recorded = recorded;
    response.previous = { ...response.previous, filed: 0, completed: 0, cancelled: 0, net_backlog: 0, changes_landed: 0, worked: 0, reworked: 0, completion_cost_usd: 0, cost_per_completed_usd: null, cycle_p50_seconds: null, rework_percent: null };
    response.previous_series = response.previous_series.map((bucket) => ({ ...bucket, filed: 0, completed: 0 }));
    const router = await openReports();
    const metrics = within(screen.getByLabelText("Headline metrics"));
    expect(metrics.getByText("142")).toBeTruthy();
    expect(metrics.getAllByText("No prior data")).toHaveLength(recorded ? 2 : 6);
    expect(screen.queryByText("Prior 7d completed") !== null).toBe(recorded);
    fireEvent.keyDown(screen.getByRole("img", { name: "Filed and completed issues per bucket" }), { key: "ArrowRight" });
    expect(screen.getByRole("status").textContent?.includes("Prior completed")).toBe(recorded);
    await act(async () => { await router.navigate({ to: "/reports", search: { project: "all", compare: "off" } as never }); });
    await waitFor(() => expect(metrics.queryByText("No prior data")).toBeNull());
  });

  it.each(["/reports?models=example-model&stages=code&labels=cloud&range=30d&compare=off", "/reports?project=all&models=example-model&stages=code&labels=cloud&range=30d&compare=off"])("clears unsupported organization filters on shared view %s", async (path) => {
    const router = await openReports(path);
    await waitFor(() => expect(router.state.location.search).toEqual({ project: "all", labels: "cloud", range: "30d", compare: "off" }));
    expect(organizationReads().length).toBeGreaterThan(0);
    for (const request of organizationReads()) {
      expect(request.searchParams.has("models")).toBe(false);
      expect(request.searchParams.has("stages")).toBe(false);
      expect(request.searchParams.get("labels")).toBe("cloud");
    }
    expect(screen.queryByRole("combobox", { name: /Model/ })).toBeNull();
    expect(screen.queryByRole("combobox", { name: /Stage/ })).toBeNull();
    expect(screen.getByRole("combobox", { name: /Label/ }).textContent).toContain("cloud");
  });

  it("clears model and stage filters when switching from project Cost to organization Overview", async () => {
    const user = userEvent.setup();
    activeProjectId = "prj_alpha";
    const router = await openReports("/reports/cost?project=prj_alpha&models=example-model&stages=code&labels=cloud");
    expect(screen.getByRole("combobox", { name: /Model/ }).textContent).toContain("example-model");
    expect(screen.getByRole("combobox", { name: /Stage/ }).textContent).toContain("code");
    expect(requests.some((url) => url.searchParams.get("project") === "prj_alpha" && url.searchParams.get("models") === "example-model" && url.searchParams.get("stages") === "code")).toBe(true);
    await user.click(screen.getByRole("combobox", { name: "Project" }));
    await user.click(await screen.findByRole("option", { name: "All projects" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ project: "all", labels: "cloud" }));
    await user.click(screen.getByRole("tab", { name: "Overview" }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "Who filed" })).toBeTruthy());
    expect(router.state.location.search).toEqual({ project: "all", labels: "cloud" });
    for (const request of organizationReads()) {
      expect(request.searchParams.has("models")).toBe(false);
      expect(request.searchParams.has("stages")).toBe(false);
    }
    expect(screen.queryByRole("combobox", { name: /Model/ })).toBeNull();
    expect(screen.queryByRole("combobox", { name: /Stage/ })).toBeNull();
  });

  it.each(["flow", "quality", "cost", "failures"])("keeps organization %s empty", async (tab) => {
    await openReports(`/reports/${tab}`);
    expect(screen.getByText("Choose a project in the sidebar to view its reports.")).toBeTruthy();
    expect(organizationReads()).toHaveLength(0);
    expect(screen.queryByRole("heading", { name: "By project" })).toBeNull();
  });

  it.each([
    ["flow", "Cycle time per day"],
    ["quality", "Why work went back, per day"],
    ["cost", "Spend per day by stage"],
    ["failures", "Failed attempts per day"],
  ])("keeps project %s content", async (tab, heading) => {
    activeProjectId = "prj_alpha";
    await openReports(`/reports/${tab}`);
    expect(organizationReads()).toHaveLength(0);
    expect(screen.getByRole("heading", { name: heading })).toBeTruthy();
  });

  it("keeps the project Overview and Reports read", async () => {
    activeProjectId = "prj_alpha";
    await openReports();
    expect(organizationReads()).toHaveLength(0);
    expect(requests.some((url) => url.searchParams.get("project") === "prj_alpha")).toBe(true);
    expect(screen.getByRole("heading", { name: "Shipped per day" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Project, filter" })).toBeNull();
  });
});
