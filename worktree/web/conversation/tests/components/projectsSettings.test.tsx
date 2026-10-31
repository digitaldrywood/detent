// @vitest-environment jsdom
import { compile } from "tailwindcss";
import theme from "tailwindcss/theme.css?raw";
import postcss from "postcss";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as Schema from "effect/Schema";

import { ProjectsSettings } from "../../src/app/settings/ProjectsSettings.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import { AccountBootstrap } from "../../src/contracts/account.ts";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import type { ProjectSummary } from "../../src/contracts/account.ts";
import { toastManager } from "../../src/components/ui/toast.tsx";
import { NewProjectProvider } from "../../src/app/projects/NewProject.tsx";
import { TooltipProvider } from "../../src/components/ui/tooltip.tsx";

const projects: readonly ProjectSummary[] = [
  {
    id: "prj_alpha",
    slug: "alpha",
    name: "Alpha",
    profile: "native",
    states: [],
    can_write: true,
    can_manage_runners: true,
    onboarding: { ready: true, steps: [] },
  },
  {
    id: "prj_beta",
    slug: "beta",
    name: "Beta",
    profile: "github_compatible",
    states: [],
    can_write: true,
    can_manage_runners: true,
    onboarding: { ready: true, steps: [] },
  },
];

let viewportStyles: HTMLStyleElement | undefined;

afterEach(() => {
  cleanup();
  viewportStyles?.remove();
  viewportStyles = undefined;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function harness({
  role = "owner",
  list = projects,
  outcome = "ok",
  pending = false,
}: {
  role?: string;
  list?: readonly ProjectSummary[];
  outcome?: "ok" | "conflict" | "error" | "load-error";
  pending?: boolean;
} = {}) {
  const writes: Record<string, unknown>[] = [];
  let order = list.map((project) => project.id);
  let revision = 7;
  let loadFailure = outcome === "load-error";
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async (url, init) => {
      const path = String(url);
      if (path.endsWith("/project-rank")) {
        if (init?.method === "PUT") {
          const body = JSON.parse(String(init.body));
          writes.push(body);
          if (outcome === "conflict") {
            order = ["prj_beta", "prj_alpha"];
            revision = 9;
            return new Response(
              JSON.stringify({
                code: "revision_conflict",
                message: "private customer content",
              }),
              { status: 409 },
            );
          }
          if (outcome === "error")
            return new Response(
              JSON.stringify({
                code: "unavailable",
                message: "private customer content",
              }),
              { status: 500 },
            );
          order = body.project_ids;
          revision++;
        }
        return new Response(JSON.stringify({ revision, project_ids: order }));
      }
      if (path.endsWith("/projects")) {
        if (pending) return new Promise<Response>(() => {});
        if (loadFailure) {
          loadFailure = false;
          return new Response(
            JSON.stringify({
              code: "unavailable",
              message: "private customer content",
            }),
            { status: 500 },
          );
        }
        return new Response(JSON.stringify(list));
      }
      return new Response(JSON.stringify({}));
    });
  const account = Schema.decodeUnknownSync(AccountBootstrap)(accountFixture);
  const client = {
    http: {
      origin: "",
      apiBase: accountFixture.api_base,
      csrfToken: accountFixture.csrf_token,
    },
    account: {
      ...account,
      actor: {
        ...account.actor,
        role,
        can_manage: role === "owner" || role === "admin",
      },
    },
  } as unknown as ConversationClient;
  const onNavigate = vi.fn();
  const route = createRootRoute({
    component: () => (
      <ClientContext.Provider value={client}>
        <TooltipProvider>
          <NewProjectProvider>
            <ProjectsSettings onNavigate={onNavigate} />
          </NewProjectProvider>
        </TooltipProvider>
      </ClientContext.Provider>
    ),
  });
  const router = createRouter({
    routeTree: route,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(<RouterProvider router={router} />);
  return { writes, fetch, onNavigate };
}

function names(): string[] {
  return within(
    screen.getByRole("table", { name: "Projects in dispatch order" }),
  )
    .getAllByRole("link")
    .map((link) => link.getAttribute("aria-label")!);
}

function key(handle: HTMLElement, value: string): void {
  fireEvent.keyDown(handle, {
    key: value,
    code: value === " " ? "Space" : value,
  });
}

async function applyViewport(width: number): Promise<void> {
  vi.stubGlobal("innerWidth", width);
  const compiler = await compile(`${theme}\n@tailwind utilities;`);
  const candidates = [...document.body.querySelectorAll("[class]")].flatMap(
    (element) => [...element.classList],
  );
  const css = postcss.parse(compiler.build(candidates));
  css.walkDecls((declaration) => {
    if (declaration.prop !== "display" && declaration.prop !== "visibility")
      declaration.remove();
  });
  css.walkAtRules((rule) => {
    const query = /^\(width\s*(<|>=)\s*([\d.]+)(rem|px)\)$/.exec(rule.params);
    if (rule.name !== "media" || !query) {
      rule.remove();
      return;
    }
    const breakpoint = Number(query[2]) * (query[3] === "rem" ? 16 : 1);
    if (query[1] === "<" ? width < breakpoint : width >= breakpoint)
      rule.replaceWith(...(rule.nodes ?? []));
    else rule.remove();
  });
  css.walkRules((rule) => {
    if (rule.nodes.length === 0) rule.remove();
  });
  viewportStyles = document.createElement("style");
  viewportStyles.textContent = css.toString();
  document.head.append(viewportStyles);
}

describe("Projects dispatch order", () => {
  it.each(["owner", "admin", "member", "viewer"])(
    "renders the ordered table and allowed controls for %s",
    async (role) => {
      harness({ role, list: [...projects].reverse() });
      await screen.findByRole("table", { name: "Projects in dispatch order" });
      expect(names()).toEqual(["Settings for Beta", "Settings for Alpha"]);
      expect(
        screen
          .getByRole("link", { name: "Settings for Alpha" })
          .getAttribute("href"),
      ).toBe("/settings/projects/alpha/general");
      expect(
        screen.queryAllByRole("button", { name: /^Reorder / }),
      ).toHaveLength(role === "owner" || role === "admin" ? 2 : 0);
      expect(
        Boolean(screen.queryByRole("button", { name: "New project" })),
      ).toBe(role === "owner" || role === "admin");
      expect(screen.queryByText(/^\d+\.$/)).toBeNull();
      expect(screen.queryByText(/Project rank|Save project/)).toBeNull();
      expect(
        screen.getByRole("columnheader", { name: "Tracker" }),
      ).toBeTruthy();
      expect(screen.getAllByText("Set up").length).toBeGreaterThan(0);
      expect(screen.getAllByText("GitHub").length).toBeGreaterThan(0);
    },
  );

  it.each(["owner", "member"])(
    "keeps mobile controls accessible at 390px for %s",
    async (role) => {
      const { writes, onNavigate } = harness({
        role,
        list: [
          {
            ...projects[0]!,
            onboarding: {
              ready: false,
              steps: [
                {
                  name: "Repository",
                  state: "action_required",
                  detail: "Choose a repository",
                },
              ],
            },
          },
          projects[1]!,
        ],
      });
      await screen.findByRole("table", { name: "Projects in dispatch order" });
      await applyViewport(390);
      const row = within(screen.getByRole("row", { name: /Alpha/ }));
      expect(
        row.getByRole("cell", { name: "Alpha Native 1 setup step left" }),
      ).toBeTruthy();
      const settings = row.getByRole("link", { name: "Settings for Alpha" });
      settings.focus();
      expect(document.activeElement).toBe(settings);
      fireEvent.click(settings);
      expect(onNavigate).toHaveBeenCalledWith(
        "/settings/projects/alpha/general",
      );
      fireEvent.click(row.getByRole("button", { name: "Finish setup" }));
      expect(onNavigate).toHaveBeenCalledWith("/projects/prj_alpha/setup");
      if (role === "owner") {
        const handle = screen.getByRole("button", { name: "Reorder Beta" });
        handle.focus();
        key(handle, " ");
        key(handle, "ArrowUp");
        key(handle, " ");
        await waitFor(() => expect(writes).toHaveLength(1));
        expect(writes[0]).toMatchObject({
          expected_revision: 7,
          project_ids: ["prj_beta", "prj_alpha"],
        });
      } else {
        expect(screen.queryByRole("button", { name: /^Reorder / })).toBeNull();
        expect(
          screen.queryByRole("button", { name: "New project" }),
        ).toBeNull();
        expect(writes).toEqual([]);
      }
    },
  );

  it("saves one keyboard drop with the revision, announces moves and keeps focus", async () => {
    const { writes } = harness();
    const handle = await screen.findByRole("button", { name: "Reorder Beta" });
    handle.focus();
    key(handle, " ");
    key(handle, "ArrowUp");
    expect(
      screen
        .getAllByRole("status")
        .some((node) => node.textContent === "Beta, position 1 of 2"),
    ).toBe(true);
    expect(names()).toEqual(["Settings for Beta", "Settings for Alpha"]);
    expect(document.activeElement).toBe(handle);
    key(handle, " ");
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      expected_revision: 7,
      project_ids: ["prj_beta", "prj_alpha"],
    });
    await waitFor(() =>
      expect(handle.getAttribute("aria-pressed")).toBe("false"),
    );
    expect(
      screen
        .getAllByRole("status")
        .some((node) => node.textContent === "Dropped. Beta, position 1 of 2"),
    ).toBe(true);
  });

  it("Escape restores the original order without saving", async () => {
    const { writes } = harness();
    const handle = await screen.findByRole("button", { name: "Reorder Beta" });
    key(handle, " ");
    key(handle, "ArrowUp");
    key(handle, "Escape");
    expect(names()).toEqual(["Settings for Alpha", "Settings for Beta"]);
    expect(writes).toEqual([]);
    expect(handle.getAttribute("aria-pressed")).toBe("false");
  });

  it.each(["conflict", "error"] as const)(
    "handles a %s without exposing server error content",
    async (outcome) => {
      const toast = vi.spyOn(toastManager, "add");
      const { writes, fetch } = harness({ outcome });
      const handle = await screen.findByRole("button", {
        name: "Reorder Beta",
      });
      key(handle, " ");
      key(handle, "ArrowUp");
      key(handle, " ");
      await waitFor(() => expect(writes).toHaveLength(1));
      if (outcome === "conflict") {
        await waitFor(() =>
          expect(toast).toHaveBeenCalledWith({
            type: "info",
            title: "Project order changed elsewhere. Showing the latest order.",
          }),
        );
        expect(names()).toEqual(["Settings for Beta", "Settings for Alpha"]);
        expect(
          fetch.mock.calls.filter(
            ([url, init]) =>
              String(url).endsWith("/project-rank") && init?.method !== "PUT",
          ),
        ).toHaveLength(2);
      } else {
        await screen.findByText("Project order could not be saved. Try again.");
        expect(names()).toEqual(["Settings for Alpha", "Settings for Beta"]);
      }
      expect(screen.queryByText("private customer content")).toBeNull();
    },
  );

  it("renders a loading skeleton and a retryable safe error", async () => {
    harness({ outcome: "load-error" });
    await screen.findByText("Projects are not available");
    expect(screen.queryByText("private customer content")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("table", { name: "Projects in dispatch order" });
  });

  it("renders a skeleton while data is pending", async () => {
    harness({ pending: true });
    await screen.findByRole("status", { name: "Loading projects" });
    expect(screen.queryByRole("table")).toBeNull();
  });

  it.each(["owner", "member"])(
    "renders the empty state for %s",
    async (role) => {
      harness({ role, list: [] });
      await screen.findByText("No projects yet");
      expect(
        Boolean(screen.queryByRole("button", { name: "New project" })),
      ).toBe(role === "owner");
    },
  );

  it("preserves New project and Finish setup with a single unfinished project", async () => {
    const { onNavigate } = harness({
      list: [
        {
          ...projects[0]!,
          onboarding: {
            ready: false,
            steps: [
              {
                name: "Repository",
                state: "action_required",
                detail: "Choose a repository",
              },
            ],
          },
        },
      ],
    });
    await screen.findByRole("table");
    expect(screen.queryByRole("button", { name: "Reorder Alpha" })).toBeNull();
    expect(screen.getAllByText("1 setup step left").length).toBeGreaterThan(0);
    fireEvent.click(
      screen.getAllByRole("button", { name: "Finish setup" })[0]!,
    );
    expect(onNavigate).toHaveBeenCalledWith("/projects/prj_alpha/setup");
    fireEvent.click(screen.getByRole("button", { name: "New project" }));
    await screen.findByRole("dialog");
  });

  it("saves a pointer drop once through the same revisioned endpoint", async () => {
    class TestPointerEvent extends MouseEvent {
      readonly isPrimary = true;
      readonly pointerId = 1;
    }
    vi.stubGlobal("PointerEvent", TestPointerEvent);
    const { writes } = harness();
    const handle = await screen.findByRole("button", { name: "Reorder Beta" });
    const rows = within(screen.getByRole("table")).getAllByRole("row").slice(1);
    rows.forEach((row, index) =>
      vi
        .spyOn(row, "getBoundingClientRect")
        .mockReturnValue(new DOMRect(0, 100 + index * 50, 700, 50)),
    );
    fireEvent.pointerDown(handle, { clientX: 15, clientY: 175, button: 0 });
    fireEvent.pointerMove(document, { clientX: 15, clientY: 165 });
    await act(async () => {});
    fireEvent.pointerMove(document, { clientX: 15, clientY: 120 });
    await act(async () => {});
    fireEvent.pointerUp(document);
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      expected_revision: 7,
      project_ids: ["prj_beta", "prj_alpha"],
    });
  });
});
