// @vitest-environment jsdom
//
// The issue page's composer (decisions.md §19.5).
//
// What is asserted here is the shape Michael asked for: the issue page's
// composer is a tracker's comment box and nothing else. There is no runner
// mode any more (his review of September 12) — a turn is steered from the
// conversation surface the Activity feed's live row opens — so nothing on this
// card configures a turn, takes a file, or chooses what the send does.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  IssueComposer,
  issueComposerSlashCommands,
} from "../../src/app/work/components/IssueComposer.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import { AttachmentEditor } from "../../src/app/work/components/AttachmentEditor.tsx";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function setup(overrides: Partial<React.ComponentProps<typeof IssueComposer>> = {}) {
  const onComment = vi.fn(async () => {});
  const composer = <IssueComposer canWrite onComment={onComment} {...overrides} />;
  const client = { http: { origin: "", apiBase: "/organizations/org_current/api/v2", csrfToken: "csrf" } } as ConversationClient;
  render(overrides.projectId === undefined ? composer : <ClientContext value={client}>{composer}</ClientContext>);
  return { onComment };
}

describe("the issue composer", () => {
  it.each(["drop", "attach"])("accepts %s files in the shared issue body editor", async (gesture) => {
    const nativeForm = await new Response("", { headers: { "Content-Type": "application/x-www-form-urlencoded" } }).formData();
    vi.stubGlobal("FormData", nativeForm.constructor);
    const id = `att_${"a".repeat(32)}`;
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ id, project_id: "prj_current", name: "notes.txt", content_type: "text/plain", size: 5, width: 0, height: 0 })));
    const client = { http: { origin: "", apiBase: "/organizations/org_current/api/v2", csrfToken: "csrf" } } as ConversationClient;
    function Editor() {
      const [value, setValue] = React.useState("Keep this text");
      return <AttachmentEditor projectId="prj_current" value={value} onChange={setValue} onUploadingChange={() => {}} aria-label="Issue body" />;
    }
    render(<ClientContext value={client}><Editor /></ClientContext>);
    const file = new File(["hello"], "notes.txt", { type: "text/plain" });
    if (gesture === "drop") {
      expect(fireEvent.drop(screen.getByLabelText("Issue body"), { dataTransfer: { types: ["Files"], files: [file] } })).toBe(false);
    } else {
      const click = vi.spyOn(HTMLInputElement.prototype, "click");
      fireEvent.click(screen.getByRole("button", { name: "Attach files" }));
      expect(click).toHaveBeenCalledOnce();
      fireEvent.change(screen.getByTestId("attachment-input"), { target: { files: [file] } });
    }
    await waitFor(() => expect((screen.getByLabelText("Issue body") as HTMLTextAreaElement).value).toBe(`Keep this text\n\n[notes.txt](attachment:${id})\n`));
    expect((fetch.mock.calls[0]?.[1]?.body as FormData).get("file")).toBe(file);
  });
  it("pastes files into markdown, keeps uploads unsendable and saves the finished reference", async () => {
    const nativeForm = await new Response("", { headers: { "Content-Type": "application/x-www-form-urlencoded" } }).formData();
    vi.stubGlobal("FormData", nativeForm.constructor);
    let release!: (response: Response) => void;
    const fetch = vi.spyOn(globalThis, "fetch").mockReturnValue(new Promise((resolve) => { release = resolve; }));
    const { onComment } = setup({ projectId: "prj_current" });
    const editor = screen.getByLabelText("Comment");
    const file = new File(["png"], "capture.png", { type: "image/png" });
    fireEvent.paste(editor, { clipboardData: { files: [file] } });
    await waitFor(() => expect(editor.textContent).toContain("Uploading capture.png"));
    expect((screen.getByLabelText("Waiting for the upload to finish") as HTMLButtonElement).disabled).toBe(true);
    expect(fetch.mock.calls[0]?.[0]).toBe("/organizations/org_current/api/v2/projects/prj_current/attachments");
    expect(fetch.mock.calls[0]?.[1]?.headers).toMatchObject({ "X-CSRF-Token": "csrf" });
    expect((fetch.mock.calls[0]?.[1]?.body as FormData).get("file")).toBe(file);
    const id = `att_${"a".repeat(32)}`;
    await act(async () => release(new Response(JSON.stringify({ id, project_id: "prj_current", name: "capture.png", content_type: "image/png", size: 3, width: 1, height: 1 }))));
    await waitFor(() => expect(editor.textContent).toContain(`![capture.png](attachment:${id})`));
    fireEvent.click(screen.getByLabelText("Send message"));
    await waitFor(() => expect(onComment).toHaveBeenCalledWith(`![capture.png](attachment:${id})`));
  });

  it("removes a failed upload placeholder and shows its error inline", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ code: "payload_too_large", message: "File exceeds the limit" }), { status: 413 }));
    setup({ projectId: "prj_current" });
    const editor = screen.getByLabelText("Comment");
    fireEvent.paste(editor, { clipboardData: { files: [new File(["png"], "large.png", { type: "image/png" })] } });
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("large.png: File exceeds the limit"));
    expect(editor.textContent).not.toContain("Uploading");
    expect(editor.textContent).not.toContain("attachment:");
  });
  // Linear's box: a prompt and a send. A comment has no model, no reasoning
  // effort and no runtime access to choose.
  it("shows no turn pickers", () => {
    setup();
    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      expect(screen.queryByLabelText(label)).toBeNull();
    }
    expect(screen.getByLabelText("Comment").getAttribute("aria-placeholder")).toBe(
      "Leave a comment…",
    );
  });

  // The strip under the card, the shortcut line under that and the mode toggle
  // in the footer were each the weight this page asked to lose.
  it("carries no context strip, no hint line, no mode toggle and no scope sentence", () => {
    setup();
    expect(document.querySelector("[data-slot='composer-context-strip']")).toBeNull();
    expect(document.getElementById("dc-issue-composer-hint")).toBeNull();
    expect(screen.queryByTestId("issue-composer-mode")).toBeNull();
    expect(screen.queryByTestId("mode-comment")).toBeNull();
    expect(screen.queryByTestId("mode-runner")).toBeNull();
    expect(screen.queryByTestId("issue-composer-scope")).toBeNull();
    // An `aria-describedby` pointing at a hint that is no longer there is
    // worse than none at all.
    expect(screen.getByLabelText("Comment").getAttribute("aria-describedby")).toBeNull();
  });

  // The hub's comment endpoint takes a body and nothing else, so a paperclip
  // here — even a disabled one — would promise a feature that is not coming.
  it("offers no attach control", () => {
    setup();
    expect(screen.queryByTestId("composer-attach")).toBeNull();
    expect(screen.queryByTestId("composer-attach-input")).toBeNull();
  });

  it("says why a reader who cannot write may not comment", () => {
    setup({ canWrite: false });
    expect(screen.getByLabelText("Comment").getAttribute("aria-disabled")).toBe("true");
  });
});

// The list itself needs layout jsdom has not got
// (`tests/visual/conversation.spec.js` drives it in a browser). What the page
// puts *in* the menu is checkable here.
describe("the issue composer's slash commands", () => {
  it("offers the shortcuts panel when the page can open it", () => {
    const onOpenShortcuts = vi.fn();
    const commands = issueComposerSlashCommands({ onOpenShortcuts });
    expect(commands.map((command) => command.name)).toEqual(["shortcuts"]);
    commands[0]?.run();
    expect(onOpenShortcuts).toHaveBeenCalledTimes(1);
  });

  // No mode to switch and no runner to send to: the page adds nothing, and
  // `/clear` — the one built-in a comment box has — comes from `Composer`.
  it("adds nothing when the page cannot open the shortcuts", () => {
    expect(issueComposerSlashCommands({ onOpenShortcuts: null })).toEqual([]);
  });
});
