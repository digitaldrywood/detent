// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FileTree as FileTreeModel, FileTreeDirectoryHandle } from "@pierre/trees";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  FileSurface,
  FilesSurface,
  type FilesClient,
} from "../../src/app/components/surfaces/FilesSurface.tsx";
import { FileBrowserPanel } from "../../src/app/components/surfaces/FileBrowserPanel.tsx";
import {
  RelayError,
  type ContentPayload,
  type FileEntry,
  type ListedPayload,
} from "../../src/app/adapters/workspaceRelay.ts";

vi.mock("@pierre/diffs/react", () => ({
  File: (props: { file: { name: string; contents: string } }) => (
    <pre data-testid="files-view-code" data-file-name={props.file.name}>
      {props.file.contents}
    </pre>
  ),
  Virtualizer: (props: { children: React.ReactNode; className?: string }) => (
    <div className={props.className}>{props.children}</div>
  ),
}));

vi.mock("../../src/components/DiffWorkerPoolProvider.tsx", () => ({
  DiffWorkerPoolProvider: (props: { children?: React.ReactNode }) => <>{props.children}</>,
}));

afterEach(cleanup);

function entry(overrides: Partial<FileEntry> = {}): FileEntry {
  return {
    name: "README.md",
    kind: "file",
    size: 12,
    modified_at: "2026-09-11T11:00:00Z",
    ignored: false,
    denied: false,
    ...overrides,
  };
}

function content(overrides: Partial<ContentPayload> = {}): ContentPayload {
  return {
    path: "README.md",
    mime: "text/markdown",
    size: 12,
    offset: 0,
    data: "hello files",
    truncated: false,
    ...overrides,
  };
}

/** A stand-in for the relay: only the two calls the surface makes. */
function client(
  listings: Record<string, readonly FileEntry[]>,
  reads: Record<string, ContentPayload | RelayError> = {},
): FilesClient & { readonly listed: string[] } {
  const listed: string[] = [];
  return {
    listed,
    list: (path) => {
      listed.push(path);
      const entries = listings[path];
      if (entries === undefined) return Promise.reject(new RelayError("not_found"));
      return Promise.resolve({ path, entries } satisfies ListedPayload);
    },
    read: (path) => {
      const answer = reads[path];
      if (answer === undefined) return Promise.reject(new RelayError("not_found"));
      return answer instanceof RelayError ? Promise.reject(answer) : Promise.resolve(answer);
    },
  };
}

function mount(props: Partial<React.ComponentProps<typeof FilesSurface>> = {}) {
  const onRetry = vi.fn();
  const utils = render(
    <FilesSurface
      state="ready"
      reason={null}
      error={null}
      loading={false}
      files={null}
      onRetry={onRetry}
      {...props}
    />,
  );
  return { ...utils, onRetry };
}

/**
 * Clicks a row the tree drew. The rows live in the tree's shadow root, which
 * testing-library does not query into, so this reaches in by the tree's own
 * `data-item-path` attribute.
 */
async function clickTreeRow(path: string): Promise<void> {
  const row = await waitFor(() => {
    const host = document.querySelector("file-tree-container");
    const found = host?.shadowRoot?.querySelector<HTMLElement>(`[data-item-path="${path}"]`);
    if (!found) throw new Error(`no tree row for ${path}`);
    return found;
  });
  row.click();
}

/** Mounts the tree alone and hands back its model, for driving expansion. */
async function mountTree(
  files: FilesClient,
  props: Partial<React.ComponentProps<typeof FileBrowserPanel>> = {},
) {
  let model: FileTreeModel | null = null;
  const onOpenFile = vi.fn();
  const utils = render(
    <FileBrowserPanel
      files={files}
      projectName="parable"
      selectedPath={null}
      onOpenFile={onOpenFile}
      theme="dark"
      onModel={(handed) => {
        model = handed;
      }}
      {...props}
    />,
  );
  await waitFor(() => expect(model).not.toBeNull());
  return { ...utils, onOpenFile, model: model as unknown as FileTreeModel };
}

describe("the Files surface", () => {
  it("always draws the panel shell", () => {
    mount({ state: "requested", files: null });
    expect(screen.getByTestId("files-surface")).toBeTruthy();
    expect(document.querySelector("[data-surface-subheader]")).toBeTruthy();
  });

  // A request no runner claims sits in `requested` for the whole request
  // timeout, so the wait is words and a clock rather than skeleton rows.
  it("says what it is waiting for, and for how long, while the workspace is requested", () => {
    mount({
      state: "requested",
      session: { requestedAt: new Date(Date.now() - 80_000).toISOString() },
    });
    const waiting = screen.getByTestId("files-waiting");
    expect(waiting.textContent).toContain(
      "Waiting for a runner that can serve files for this project",
    );
    expect(screen.getByTestId("files-elapsed").textContent).toBe("1m 20s elapsed");
    expect(document.querySelector("[data-testid='files-waiting'] + *")).toBeNull();
  });

  it("names the runner checking the worktree out, and keeps the skeleton for it alone", () => {
    mount({ state: "starting", session: { runnerId: "rnr_01J" } });
    expect(screen.getByTestId("files-starting").textContent).toContain(
      "Checking out the worktree on rnr_01J",
    );

    expect(document.querySelector("[aria-hidden='true'] [role='status']")).toBeTruthy();
  });

  it("says the runner may come back while the workspace is unreachable", () => {
    mount({ state: "unreachable" });
    expect(screen.getByTestId("files-unreachable").textContent).toContain(
      "The runner stopped answering.",
    );
  });

  it("shows the reason and a retry when the workspace failed", async () => {
    const { onRetry } = mount({ state: "failed", reason: "no_runner" });
    expect(screen.getByTestId("files-unavailable").textContent).toContain(
      "No runner reported the files capability",
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("shows the reason when the workspace closed", () => {
    mount({ state: "closed", reason: "expired" });
    expect(screen.getByTestId("files-unavailable").textContent).toContain("idle timeout");
  });

  it("reports a request that failed outright, with its own retry", async () => {
    const { onRetry } = mount({ state: null, error: "You have 3 workspaces open." });
    expect(screen.getByTestId("files-error").textContent).toBe("You have 3 workspaces open.");
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("draws the explorer as the whole surface while nothing is open", async () => {
    const files = client({ "": [entry()] });
    mount({ state: "ready", files, projectName: "parable" });
    await waitFor(() => expect(files.listed).toEqual([""]));
    expect(screen.getByTestId("files-tree")).toBeTruthy();
    expect(screen.getByRole("searchbox", { name: "Search parable files" })).toBeTruthy();
    expect(screen.queryByTestId("files-view")).toBeNull();
    expect(document.querySelector("[data-file-breadcrumbs]")).toBeNull();
  });

  it("reads the file a row opens, draws its code beside the explorer, and names the path above it", async () => {
    const files = client(
      { "": [entry({ name: "src", kind: "dir" })], src: [entry({ name: "main.go" })] },
      { "src/main.go": content({ path: "src/main.go", data: "package main" }) },
    );
    const onOpenFile = vi.fn();
    mount({ state: "ready", files, projectName: "parable", onOpenFile });
    await waitFor(() => expect(files.listed).toEqual(["", "src"]));

    await clickTreeRow("src/main.go");
    await waitFor(() => expect(screen.getByTestId("files-view-code").textContent).toBe("package main"));
    expect(screen.getByTestId("files-view-code").getAttribute("data-file-name")).toBe("src/main.go");
    // Pierre's file view carries the path so the grammar comes from it.
    // The explorer stays beside the code, and the path sits above it.
    expect(screen.getByTestId("files-tree")).toBeTruthy();
    const crumbs = document.querySelector("[data-file-breadcrumbs]");
    expect(crumbs?.textContent).toContain("parable");
    expect(crumbs?.textContent).toContain("main.go");
    // And the panel was offered the file as its own tab.
    expect(onOpenFile).toHaveBeenCalledWith("src/main.go");
  });

  it("never reads a denied entry and shows the redacted state instead", async () => {
    const files = client({ "": [entry({ name: ".env", denied: true })] });
    const read = vi.spyOn(files, "read");
    const onOpenFile = vi.fn();
    mount({ state: "ready", files, onOpenFile });
    await waitFor(() => expect(files.listed).toEqual([""]));
    await clickTreeRow(".env");
    await waitFor(() => expect(screen.getByTestId("files-view-denied")).toBeTruthy());
    expect(screen.getByTestId("files-view-denied").textContent).toContain("denylist");
    expect(read).not.toHaveBeenCalled();
    expect(onOpenFile).not.toHaveBeenCalled();
  });

  it("shows the too-large state for a read the runner refused", async () => {
    const files = client({ "": [entry({ name: "huge.bin" })] }, { "huge.bin": new RelayError("too_large") });
    mount({ state: "ready", files });
    await waitFor(() => expect(files.listed).toEqual([""]));
    await clickTreeRow("huge.bin");
    await waitFor(() =>
      expect(screen.getByTestId("files-view-too-large").textContent).toBe(
        "This file is larger than the 2 MB read limit.",
      ),
    );
  });
});

describe("the tree", () => {
  // The preview worktree, exactly: one directory that opens a level deep and
  // four files, with README.md last and main.go the row above it.
  const PREVIEW_WORKTREE: Record<string, readonly FileEntry[]> = {
    "": [
      entry({ name: "internal", kind: "dir" }),
      entry({ name: ".env", denied: true }),
      entry({ name: "go.mod" }),
      entry({ name: "main.go" }),
      entry({ name: "README.md" }),
    ],
    internal: [entry({ name: "app.go" })],
  };

  function previewWorktreeClient() {
    return client(PREVIEW_WORKTREE);
  }

  /**
   * The same worktree with one directory's listing held open.
   *
   * Holding it is what makes "what did the first paint show" a question with
   * one answer: the root's listing resolves, the tree paints, and the child
   * listing has provably not arrived yet. Sampling after a `waitFor` instead
   * would let both land and could never see a row appear late.
   */
  function heldChildClient(held: string) {
    let release = (): void => {};
    const gate = new Promise<void>((resolve) => {
      release = () => resolve();
    });
    const base = client(PREVIEW_WORKTREE);
    // The request is recorded when it is made, not when it is answered, so a
    // test can say "asked for and not yet answered" — which is the whole
    // window this is here to look inside.
    const listed: string[] = [];
    return {
      ...base,
      listed,
      release,
      list: (path: string) => {
        listed.push(path);
        return path === held ? gate.then(() => base.list(path)) : base.list(path);
      },
    };
  }

  /** Every row the tree is drawing, in the order it draws them. */
  function visibleRowPaths(model: FileTreeModel): string[] {
    return model.getVisibleRows(0, model.getVisibleCount()).map((row) => row.path);
  }

  // The regression for clicking README.md and getting main.go.
  //
  // The tree opens the root's directories a level deep, and their rows used to
  // be added after the first paint, when each directory's own listing arrived.
  // That inserts a row above every file in the root and shifts each of them
  // down one — so a reader (or Pierre's recycled row element) who was already
  // on README.md lands on the row that was above it, which is main.go.
  //
  // The fix is that the first paint already has them, so the assertion is
  // about what the tree shows the moment it shows anything: no row the reader
  // can see moves under them afterwards.
  it("paints the root's one-level-deep rows at once, so no row shifts afterwards", async () => {
    const files = heldChildClient("internal");
    const { model } = await mountTree(files);
    // The root has been asked for and `internal` is still held, so anything
    // drawn now was drawn without the child listing having arrived.
    await waitFor(() => expect(files.listed).toContain("internal"));

    // Nothing may be on screen until the rows are the rows: painting five and
    // then inserting a sixth is what moved README.md under the reader.
    expect(visibleRowPaths(model)).toEqual([]);

    files.release();
    await waitFor(() => expect(visibleRowPaths(model).length).toBeGreaterThan(0));
    const first = visibleRowPaths(model);
    expect(first).toContain("internal/app.go");
    expect(first.at(-1)).toBe("README.md");

    // Nothing moved afterwards: the order first painted is the settled order.
    await waitFor(() => expect(files.listed).toEqual(["", "internal"]));
    expect(visibleRowPaths(model)).toEqual(first);
  });

  // The click the Playwright spec makes, at the level the row-to-path mapping
  // can be asserted: the last row the tree draws is README.md, and selecting
  // that row opens README.md rather than its neighbour.
  it("opens the last row's own file after the one-level expand", async () => {
    const files = previewWorktreeClient();
    const { model, onOpenFile } = await mountTree(files);
    await waitFor(() => expect(files.listed).toEqual(["", "internal"]));

    const rows = visibleRowPaths(model);
    const last = rows.at(-1);
    expect(last).toBe("README.md");
    if (last === undefined) throw new Error("the tree drew no rows");

    await clickTreeRow(last);
    await waitFor(() => expect(onOpenFile).toHaveBeenCalledTimes(1));
    expect(onOpenFile.mock.calls[0]?.[1]).toBe("README.md");
    expect(onOpenFile.mock.calls[0]?.[0]).toMatchObject({ name: "README.md" });
  });

  it("lists the root, then its directories one level deep, then deeper only on expand", async () => {
    const files = client({
      "": [entry({ name: "src", kind: "dir" }), entry({ name: "docs", kind: "dir" }), entry()],
      src: [entry({ name: "lib", kind: "dir" }), entry({ name: "main.ts" })],
      docs: [entry({ name: "guide.md" })],
      "src/lib": [entry({ name: "util.ts" })],
    });
    const { model } = await mountTree(files);

    await waitFor(() => expect(files.listed).toEqual(["", "docs", "src"]));
    expect(model.getItem("src/main.ts")).not.toBeNull();
    expect(model.getItem("docs/guide.md")).not.toBeNull();
    // Nothing below the first level has been read yet.
    expect(model.getItem("src/lib/util.ts")).toBeNull();

    const lib = model.getItem("src/lib/") as FileTreeDirectoryHandle | null;
    expect(lib?.isDirectory()).toBe(true);
    if (lib === null) throw new Error("src/lib is not a row");
    lib.expand();
    await waitFor(() => expect(files.listed).toEqual(["", "docs", "src", "src/lib"]));
    await waitFor(() => expect(model.getItem("src/lib/util.ts")).not.toBeNull());

    // Collapsing and re-expanding costs no second listing.
    lib.collapse();
    lib.expand();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(files.listed).toEqual(["", "docs", "src", "src/lib"]);
  });

  it("fetches a lone subdirectory eagerly so the flattened chain settles at once", async () => {
    const files = client({
      "": [entry({ name: ".agents", kind: "dir" })],
      ".agents": [entry({ name: "skills", kind: "dir" })],
      ".agents/skills": [entry({ name: "README.md" })],
    });
    const { model } = await mountTree(files);
    await waitFor(() => expect(files.listed).toEqual(["", ".agents", ".agents/skills"]));
    expect(model.getItem(".agents/skills/README.md")).not.toBeNull();
  });

  it("opens the file a row selects, with the entry so denial is known", async () => {
    const files = client({ "": [entry(), entry({ name: ".env", denied: true })] });
    const { model, onOpenFile } = await mountTree(files);
    await waitFor(() => expect(model.getItem("README.md")).not.toBeNull());
    model.getItem("README.md")?.select();
    await waitFor(() => expect(onOpenFile).toHaveBeenCalledTimes(1));
    expect(onOpenFile.mock.calls[0]?.[1]).toBe("README.md");
    expect(onOpenFile.mock.calls[0]?.[0]).toMatchObject({ name: "README.md", denied: false });

    model.getItem(".env")?.select();
    await waitFor(() => expect(onOpenFile).toHaveBeenCalledTimes(2));
    expect(onOpenFile.mock.calls[1]?.[0]).toMatchObject({ name: ".env", denied: true });
  });

  it("reveals a file opened from outside the tree by listing and expanding its ancestors", async () => {
    const files = client({
      "": [entry({ name: "apps", kind: "dir" })],
      apps: [entry({ name: "api", kind: "dir" }), entry({ name: "web", kind: "dir" })],
      "apps/api": [entry({ name: "cmd", kind: "dir" })],
      "apps/api/cmd": [entry({ name: "main.go" })],
      "apps/web": [entry({ name: "index.html" })],
    });
    const { model, onOpenFile, rerender } = await mountTree(files);
    await waitFor(() => expect(files.listed).toEqual(["", "apps"]));

    rerender(
      <FileBrowserPanel
        files={files}
        projectName="parable"
        selectedPath="apps/api/cmd/main.go"
        onOpenFile={onOpenFile}
        theme="dark"
      />,
    );
    await waitFor(() => expect(model.getItem("apps/api/cmd/main.go")?.isSelected()).toBe(true));
    expect(files.listed).toEqual(["", "apps", "apps/api", "apps/api/cmd"]);
    // A reveal is an echo of an already-open file, not a request to open it.
    expect(onOpenFile).not.toHaveBeenCalled();
  });

  it("reports a root listing the runner refused", async () => {
    const files = client({});
    await mountTree(files);
    await waitFor(() =>
      expect(screen.getByTestId("files-tree-error").textContent).toBe(
        "That path is no longer in the worktree.",
      ),
    );
  });

  it("drops every listing on refresh", async () => {
    const files = client({ "": [entry()] });
    await mountTree(files);
    await waitFor(() => expect(files.listed).toEqual([""]));
    // `fireEvent`, not `userEvent`: the button sits in a tooltip trigger, and
    // user-event's hover sequence never settles against base-ui's tooltip in
    // jsdom. The click is what is under test.
    fireEvent.click(screen.getByRole("button", { name: "Refresh workspace files" }));
    await waitFor(() => expect(files.listed).toEqual(["", ""]));
  });
});

describe("the single-file surface", () => {
  it("reads its own path and says so when there is no workspace", async () => {
    const files = client(
      { "": [entry({ name: "src", kind: "dir" })], src: [entry({ name: "main.ts" })] },
      { "src/main.ts": content({ path: "src/main.ts", data: "export {};" }) },
    );
    const { rerender } = render(<FileSurface path="src/main.ts" files={null} projectName="parable" />);
    expect(screen.getByTestId("file-surface-unavailable").textContent).toContain(
      "needs an open workspace",
    );
    rerender(<FileSurface path="src/main.ts" files={files} projectName="parable" />);
    await waitFor(() => expect(screen.getByTestId("files-view-code").textContent).toBe("export {};"));
    // The path above the code, project first.
    const crumbs = document.querySelector("[data-file-breadcrumbs]");
    expect(crumbs?.textContent).toContain("parable");
    expect(crumbs?.textContent).toContain("src");
    expect(crumbs?.textContent).toContain("main.ts");
    // And the explorer beside it, revealed on the open file.
    expect(screen.getByTestId("files-tree")).toBeTruthy();
    await waitFor(() => expect(files.listed).toContain("src"));
  });

  it("shows the too-large state for a read the runner refused", async () => {
    const files = client({ "": [entry({ name: "huge.bin" })] }, { "huge.bin": new RelayError("too_large") });
    render(<FileSurface path="huge.bin" files={files} />);
    await waitFor(() =>
      expect(screen.getByTestId("files-view-too-large").textContent).toBe(
        "This file is larger than the 2 MB read limit.",
      ),
    );
  });

  it("renders an image rather than decoding it as text", async () => {
    const files = client(
      { "": [entry({ name: "logo.png" })] },
      {
        "logo.png": content({
          path: "logo.png",
          mime: "image/png",
          data: "aGk=",
          encoding: "base64",
        }),
      },
    );
    render(<FileSurface path="logo.png" files={files} />);
    const image = await screen.findByRole("img");
    expect(image.getAttribute("src")).toBe("data:image/png;base64,aGk=");
  });

  it("can hide the explorer and bring it back", async () => {
    const files = client({ "": [entry()] }, { "README.md": content() });
    render(<FileSurface path="README.md" files={files} />);
    await screen.findByTestId("files-view-code");
    expect(screen.getByTestId("files-tree")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Hide file explorer" }));
    expect(screen.queryByTestId("files-tree")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show file explorer" }));
    expect(screen.getByTestId("files-tree")).toBeTruthy();
  });
});
