// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createMemoryHistory, Link, Outlet, RouterProvider, useRouteContext } from "@tanstack/react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { makeRouter } from "../src/app/router.tsx";
import { loadBootstrap } from "../src/runtime/bootstrap.ts";
import { resetHubPaths } from "../src/runtime/basePath.ts";
import accountBootstrapFixture from "../src/contracts/fixtures/account-bootstrap.json";

vi.mock("../src/app/App.tsx", () => ({
  Shell: () => <><nav><Link to="/work">Work navigation</Link></nav><Outlet /></>,
}));
vi.mock("../src/app/work/IssuePage.tsx", () => ({
  IssuePage: () => {
    const context = useRouteContext({ strict: false }) as { resolvedWorkItemId: string };
    return <h1>Issue {context.resolvedWorkItemId}</h1>;
  },
}));
vi.mock("../src/app/work/WorkRoutes.tsx", () => ({
  AllProjectsWorkBoard: () => <h1>Work board</h1>,
  ReadableProjectWorkBoard: () => <h1>Project board</h1>,
  ProjectWorkBoard: () => <h1>Legacy project board</h1>,
}));

const releases: Array<() => void> = [];

beforeEach(async () => {
  await loadBootstrap("", async () => Response.json({ ...accountBootstrapFixture, organization: { ...accountBootstrapFixture.organization, slug: "example-org" } }));
});

afterEach(async () => {
  cleanup();
  await act(async () => { for (const release of releases.splice(0)) release(); });
  vi.restoreAllMocks();
  resetHubPaths();
});

async function open(path: string) {
  const router = makeRouter(createMemoryHistory({ initialEntries: [path] }), "/");
  await router.load();
  render(<RouterProvider router={router} />);
  return router;
}

it("keeps Shell navigation after 503 and reruns the resolver only on manual retry", async () => {
  const fetch = vi.spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(new Response(null, { status: 503 }))
    .mockResolvedValue(Response.json({ project_id: "project", work_item_id: "recovered" }));
  await open("/example-project/issues/1");
  expect(await screen.findByText("The hub could not complete the request.")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Work navigation" })).toBeTruthy();
  expect(fetch).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("heading", { name: "Issue recovered" })).toBeTruthy();
  expect(fetch).toHaveBeenCalledTimes(2);
});

it.each([
  [401, "Your session has expired. Sign in again."],
  [403, "You do not have permission to do that."],
  [404, "Issue or project not found"],
])("renders truthful HTTP %i failure inside Shell", async (status, message) => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status }));
  await open("/example-project/issues/1");
  expect(await screen.findByText(message)).toBeTruthy();
  expect(screen.getByRole("link", { name: "Work navigation" })).toBeTruthy();
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("replaces the old issue while the next resolver is pending", async () => {
  let release!: () => void;
  let started!: () => void;
  const pending = new Promise<Response>((resolve) => {
    release = () => resolve(Response.json({ project_id: "project", work_item_id: "second" }));
  });
  const requested = new Promise<void>((resolve) => { started = resolve; });
  releases.push(release);
  vi.spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(Response.json({ project_id: "project", work_item_id: "first" }))
    .mockImplementation(() => { started(); return pending; });
  const router = await open("/example-project/issues/1");
  expect(await screen.findByRole("heading", { name: "Issue first" })).toBeTruthy();
  let navigation!: Promise<void>;
  await act(async () => {
    navigation = router.navigate({ to: "/$projectSlug/issues/$number", params: { projectSlug: "example-project", number: "2" } });
    await requested;
  });
  expect(await screen.findByText("Opening…")).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "Issue first" })).toBeNull();
  expect(screen.getByRole("link", { name: "Work navigation" })).toBeTruthy();
  await act(async () => { release(); await navigation; });
  expect(await screen.findByRole("heading", { name: "Issue second" })).toBeTruthy();
});

it.each([
  "/example-project",
  "/example-project/issues/1/changes/change",
  "/work/i/example-issue",
  "/work/i/example-issue/changes/change",
])("contains resolver failures inside Shell at %s", async (path) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 503 }));
  await open(path);
  expect(await screen.findByText("The hub could not complete the request.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Work navigation" })).toBeTruthy();
});
