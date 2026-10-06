// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
  type AnyRoute,
} from "@tanstack/react-router";
import type React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { designSystemRoutes, isDesignSystemPath, makeRouter } from "../src/app/router.tsx";
import { groupEntries, matchesQuery, normalizeCatalog, rawCatalog, type CatalogEntry } from "../src/design-system/gallery/catalog.ts";
import { applyDocumentTheme, SpecimenStage, type FrameTheme } from "../src/design-system/gallery/frame.tsx";
import { docFor, galleryEntries, REGISTRY } from "../src/design-system/gallery/registry.tsx";
import type { Specimen } from "../src/design-system/gallery/specimen.tsx";
import { normalizeTokens } from "../src/design-system/gallery/tokens.ts";

// jsdom has no Worker, canvas or constructable stylesheets. The diff and file
// surfaces highlight in a worker pool, the file tree adopts a stylesheet and
// the terminal measures glyphs on a canvas; stand those in as their own tests
// do (`tests/components/filesSurface.test.tsx`), so the specimens still mount
// the real surfaces around them.
vi.mock("@pierre/diffs/react", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@pierre/diffs/react")>();
  return {
    ...actual,
    File: (props: { file: { name: string; contents: string } }) => (
      <pre data-file-name={props.file.name}>{props.file.contents}</pre>
    ),
    FileDiff: () => <div data-testid="file-diff" />,
    Virtualizer: (props: { children: React.ReactNode; className?: string }) => (
      <div className={props.className}>{props.children}</div>
    ),
  };
});

vi.mock("../src/components/DiffWorkerPoolProvider.tsx", () => ({
  DiffWorkerPoolProvider: (props: { children?: React.ReactNode }) => <>{props.children}</>,
}));

if (typeof CSSStyleSheet !== "undefined" && typeof CSSStyleSheet.prototype.replaceSync !== "function") {
  Object.defineProperty(CSSStyleSheet.prototype, "replaceSync", { configurable: true, value: () => undefined });
}
if (typeof HTMLCanvasElement !== "undefined") {
  Object.defineProperty(HTMLCanvasElement.prototype, "getContext", { configurable: true, value: () => null });
}

// Base UI's pointer handlers read PointerEvent, which jsdom lacks.
if (typeof globalThis.PointerEvent !== "function") {
  Object.defineProperty(globalThis, "PointerEvent", { configurable: true, value: MouseEvent });
}

afterEach(() => {
  cleanup();
  document.documentElement.className = "";
  delete document.documentElement.dataset.theme;
});

/** The catalog the gallery reads. */
function catalog(): CatalogEntry[] {
  return normalizeCatalog(rawCatalog());
}

const SPECIMENS: Array<[string, Specimen]> = Object.entries(REGISTRY).flatMap(([id, doc]) =>
  "specimens" in doc ? doc.specimens.map((specimen): [string, Specimen] => [`${id}/${specimen.id}`, specimen]) : [],
);

/** Renders one specimen the way its frame does, inside a router (toasts and links need one). */
async function renderSpecimen(specimen: Specimen, theme: FrameTheme) {
  applyDocumentTheme(theme);
  const root = createRootRoute({ component: () => <SpecimenStage specimen={specimen} /> });
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
  await (router as unknown as { load: () => Promise<void> }).load();
  return render(<RouterProvider router={router as never} />);
}

describe("design-system gallery coverage", () => {
  it("has a specimen or a reasoned exclusion for every available catalog entry", () => {
    const missing = catalog()
      .filter((entry) => entry.status === "available")
      .filter((entry) => docFor(entry.id) === undefined)
      .map((entry) => entry.id);
    expect(missing).toEqual([]);
  });

  it("states a concrete reason for every exclusion", () => {
    for (const [id, doc] of Object.entries(REGISTRY)) {
      if ("excluded" in doc) expect(doc.excluded.trim().length, id).toBeGreaterThan(20);
      else expect(doc.specimens.length, id).toBeGreaterThan(0);
    }
  });

  it("gives every specimen a unique id within its entry", () => {
    for (const [id, doc] of Object.entries(REGISTRY)) {
      if (!("specimens" in doc)) continue;
      const ids = doc.specimens.map((specimen) => specimen.id);
      expect(new Set(ids).size, id).toBe(ids.length);
    }
  });

  it("lists registry entries the catalog does not name yet", () => {
    const entries = galleryEntries([]);
    expect(entries.map((entry) => entry.id).sort()).toEqual(Object.keys(REGISTRY).sort());
  });
});

describe("design-system gallery specimens", () => {
  it.each(SPECIMENS)("%s renders in light and dark", async (_name, specimen) => {
    for (const theme of ["light", "dark"] as const) {
      const view = await renderSpecimen(specimen, theme);
      expect(document.documentElement.classList.contains("dark")).toBe(theme === "dark");
      expect(document.documentElement.dataset.theme).toBe(theme);
      expect(screen.queryByText(/This specimen failed to render/)).toBeNull();
      view.unmount();
    }
  });
});

describe("design-system gallery frames", () => {
  const cases: Array<[FrameTheme, boolean]> = [
    ["dark", true],
    ["light", false],
  ];
  it.each(cases)("puts %s on the document root and restores it", (theme, dark) => {
    const root = document.createElement("html");
    root.classList.add("dark");
    const restore = applyDocumentTheme(theme, root);
    expect(root.classList.contains("dark")).toBe(dark);
    expect(root.dataset.theme).toBe(theme);
    restore();
    expect(root.classList.contains("dark")).toBe(true);
    expect(root.dataset.theme).toBeUndefined();
  });
});

describe("design-system gallery search", () => {
  const entries = normalizeCatalog({
    schema: 1,
    entries: [
      { id: "dialog", name: "Dialog", kind: "primitive", group: "Overlays", status: "available", source: "src/components/ui/dialog.tsx" },
      { id: "alert-dialog", name: "Alert dialog", kind: "primitive", group: "Overlays", status: "available" },
      { id: "button", name: "Button", kind: "primitive", group: "Actions", status: "available", exports: ["buttonVariants"] },
      { id: "composer", name: "Composer", kind: "composition", group: "Conversation", status: "available" },
    ],
  });

  const queries: Array<[string, string[]]> = [
    ["", ["button", "alert-dialog", "dialog", "composer"]],
    ["dialog", ["alert-dialog", "dialog"]],
    ["DIALOG", ["alert-dialog", "dialog"]],
    ["overlays alert", ["alert-dialog"]],
    ["buttonvariants", ["button"]],
    ["composition", ["composer"]],
    ["nothing matches this", []],
  ];
  it.each(queries)("query %j lists %j", (query, expected) => {
    const ids = groupEntries(entries, query).flatMap((group) => group.entries.map((entry) => entry.id));
    expect(ids).toEqual(expected);
    expect(entries.filter((entry) => matchesQuery(entry, query)).length).toBe(expected.length);
  });

  it("filters the rendered navigation", async () => {
    const router = makeRouter(createMemoryHistory({ initialEntries: ["/design-system"] }), "/");
    await act(async () => {
      render(<RouterProvider router={router} />);
    });
    const nav = await screen.findByRole("navigation", { name: "Design system" }, { timeout: 10_000 });
    expect(within(nav).getByRole("link", { name: /^Button/ })).toBeTruthy();
    fireEvent.change(within(nav).getByRole("searchbox", { name: "Search components" }), {
      target: { value: "dialog" },
    });
    await waitFor(() => expect(within(nav).queryByRole("link", { name: /^Button/ })).toBeNull());
    expect(within(nav).getByRole("link", { name: /^Dialog/ })).toBeTruthy();
    expect(screen.getByTestId("design-system-overview")).toBeTruthy();
  });
});

describe("design-system route guard", () => {
  const root = createRootRoute() as unknown as AnyRoute;

  it("registers no routes outside a dev build", () => {
    expect(designSystemRoutes(root, false)).toEqual([]);
  });

  it("registers the gallery, entry and frame routes in a dev build", () => {
    expect(designSystemRoutes(root, true).map((route) => (route.options as { path?: string }).path)).toEqual([
      "/design-system",
      "/design-system/$entryId",
      "/design-system/frame/$entryId/$specimenId",
    ]);
  });

  const paths: Array<[string, boolean]> = [
    ["/design-system", true],
    ["/design-system/button", true],
    ["/design-system/frame/button/variants", true],
    ["/design-systems", false],
    ["/work", false],
    ["/", false],
  ];
  it.each(paths)("treats %s as a gallery path: %s", (path, expected) => {
    expect(isDesignSystemPath(path)).toBe(expected);
  });
});

describe("design-system foundations", () => {
  it("reads tokens from either shape and ignores rows without a name", () => {
    const rows = [
      { name: "--background", group: "surface", scope: "root", light: { hex: "#fafafa" }, dark: { hex: "#0a0a0a" } },
      { group: "surface" },
    ];
    expect(normalizeTokens({ tokens: rows })).toEqual([
      { name: "--background", group: "surface", scope: "root", light: "#fafafa", dark: "#0a0a0a" },
    ]);
    expect(normalizeTokens(rows)).toHaveLength(1);
    expect(normalizeTokens(undefined)).toEqual([]);
  });
});
