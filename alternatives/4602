// @vitest-environment jsdom
//
// The markdown renderer's security properties. The hand-written parser this
// replaced held them by construction (it never parsed HTML at all); the
// `react-markdown` pipeline holds them because `rehype-raw` is not in it and
// because the default URL transform strips dangerous schemes. Both are worth a
// test, because both are one plugin away from being lost.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Markdown } from "../../src/app/components/Markdown.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

const attachmentId = `att_${"a".repeat(32)}`;
const attachment = { id: attachmentId, project_id: "prj_current", name: "photo.png", content_type: "image/png", size: 120, width: 800, height: 400 };
const attachmentClient = { http: { origin: "", apiBase: "/organizations/org_current/api/v2", csrfToken: "csrf" } } as ConversationClient;

function cloudMarkdown(source: string, projectId = "prj_current") {
  return <ClientContext value={attachmentClient}><Markdown source={source} projectId={projectId} /></ClientContext>;
}

describe("Markdown", () => {
  it("resolves an attachment in the current scope and opens its image preview", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(attachment)));
    render(cloudMarkdown(`![A photo](attachment:${attachmentId})`));
    const image = await screen.findByRole("button", { name: "Preview A photo" });
    expect(image.getAttribute("src")).toBe(`/organizations/org_current/api/v2/projects/prj_current/attachments/${attachmentId}`);
    expect(image.getAttribute("width")).toBe("800");
    expect(image.getAttribute("data-markdown-copy")).toBe(`![A photo](attachment:${attachmentId})`);
    expect(image.style.maxWidth).toBe("100%");
    expect(fetch.mock.calls[0]?.[0]).toBe(`/organizations/org_current/api/v2/projects/prj_current/attachments/${attachmentId}/metadata`);
    fireEvent.click(image);
    expect(screen.getByRole("dialog", { name: "Expanded image preview" })).toBeTruthy();
  });

  it("renders a non-image attachment with its stored name, size and download URL", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ ...attachment, name: "report.txt", content_type: "text/plain", width: 0, height: 0 })));
    render(cloudMarkdown(`[Report](attachment:${attachmentId})`));
    const link = await screen.findByTestId("issue-attachment-file");
    expect(link.textContent).toContain("report.txt");
    expect(link.textContent).toContain("KB");
    expect(link.getAttribute("download")).toBe("report.txt");
    expect(link.getAttribute("data-markdown-copy")).toBe(`[report.txt](attachment:${attachmentId})`);
    expect(link.getAttribute("href")).toBe(`/organizations/org_current/api/v2/projects/prj_current/attachments/${attachmentId}`);
  });

  it.each(["missing", "other project", "other id"])("renders %s attachments as unavailable with no byte URL", async (kind) => {
    const file = kind === "other project" ? { ...attachment, project_id: "prj_other" } : { ...attachment, id: `att_${"b".repeat(32)}` };
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(file), { status: kind === "missing" ? 404 : 200 }));
    const { container } = render(cloudMarkdown(`![private](attachment:${attachmentId})`));
    await screen.findByTestId("attachment-unavailable");
    expect(container.querySelector("img, a")).toBeNull();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch.mock.calls[0]?.[0]).toBe(`/organizations/org_current/api/v2/projects/prj_current/attachments/${attachmentId}/metadata`);
  });

  it("forgets a resolved image immediately when the project changes", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(attachment)));
    const { container, rerender } = render(cloudMarkdown(`![photo](attachment:${attachmentId})`));
    await screen.findByTestId("issue-attachment-image");
    fetch.mockImplementation(async () => new Response("{}", { status: 404 }));
    rerender(cloudMarkdown(`![photo](attachment:${attachmentId})`, "prj_other"));
    expect(container.querySelector("img")).toBeNull();
    await waitFor(() => expect(screen.getByTestId("attachment-unavailable")).toBeTruthy());
  });
  it("never renders raw HTML from a reply", () => {
    const { container } = render(
      <Markdown source={"<img src=x onerror=alert(1)> <script>alert(1)</script>"} />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
    expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it.each([
    "javascript:alert(1)",
    "  javascript:alert(1)",
    "data:text/html,<script>",
    "vbscript:msgbox",
  ])("strips %s from a link", (href) => {
    const { container } = render(<Markdown source={`[click](${href})`} />);
    for (const anchor of container.querySelectorAll("a")) {
      const value = anchor.getAttribute("href") ?? "";
      expect(value).not.toMatch(/javascript|vbscript|^data:/i);
    }
  });

  it("keeps an http link and opens it out of the client", () => {
    const { container } = render(<Markdown source="[docs](https://example.test/x)" />);
    const anchor = container.querySelector("a");
    expect(anchor?.getAttribute("href")).toBe("https://example.test/x");
    expect(anchor?.getAttribute("rel")).toBe("noopener noreferrer");
    expect(anchor?.getAttribute("target")).toBe("_blank");
  });

  it("keeps a same-origin path so links back to Detent pages work", () => {
    const { container } = render(<Markdown source="[project](/projects/proj_1)" />);
    expect(container.querySelector("a")?.getAttribute("href")).toBe("/projects/proj_1");
  });

  // A `#` in a reply must not contend with the route for the one `h1` (B.14).
  it("demotes a reply's headings below the route's own", () => {
    const { container } = render(<Markdown source={"# Title\n\n## Section"} />);
    expect(container.querySelector("h1")).toBeNull();
    expect(container.querySelector("h2")?.textContent).toBe("Title");
    expect(container.querySelector("h3")?.textContent).toBe("Section");
  });

  it("renders a fenced block with the code chrome, its language and a copy action", () => {
    const { container } = render(<Markdown source={"```go\nfunc main() {}\n```"} />);
    const block = container.querySelector(".chat-markdown-codeblock");
    expect(block?.getAttribute("data-language")).toBe("go");
    expect(screen.getByLabelText("Language: go")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy code" })).toBeTruthy();
    expect(screen.getByRole("toolbar", { name: "Code block actions" })).toBeTruthy();
  });

  // §13.7 and §14: a reference the hub resolved is a link into Work, opened
  // in place; text with no resolved reference is left as text.
  it("links a resolved reference and leaves an unresolved one alone", () => {
    const { container } = render(
      <Markdown
        source="Blocked on #123, and maybe #999."
        references={[{ kind: "issue", id: "wi_1", label: "#123", url: "/work/i/wi_1" }]}
      />,
    );

    const link = container.querySelector("a");
    expect(link?.getAttribute("href")).toBe("/work/i/wi_1");
    expect(link?.textContent).toBe("#123");
    expect(container.textContent).toContain("#999");
    expect(container.querySelectorAll("a")).toHaveLength(1);
  });

  it("renders GitHub tables, which the reply format uses", () => {
    const { container } = render(
      <Markdown source={"| a | b |\n| - | - |\n| 1 | 2 |"} />,
    );
    expect(container.querySelectorAll("table")).toHaveLength(1);
    expect(container.querySelectorAll("td")).toHaveLength(2);
  });

  // The rest of GFM, which the hand-written renderer also carried: task lists,
  // strikethrough and autolinks.
  it("renders the rest of GFM", () => {
    const { container } = render(
      <Markdown source={"- [x] done\n- [ ] todo\n\n~~gone~~ and https://example.test/x"} />,
    );
    const boxes = container.querySelectorAll('input[type="checkbox"]');
    expect(boxes).toHaveLength(2);
    expect((boxes[0] as HTMLInputElement).checked).toBe(true);
    expect((boxes[1] as HTMLInputElement).checked).toBe(false);
    expect(container.querySelector("del")?.textContent).toBe("gone");
    expect(container.querySelector('a[href="https://example.test/x"]')).toBeTruthy();
  });

  it("renders a GitHub alert as the callout", () => {
    const { container } = render(
      <Markdown source={"> [!WARNING]\n> The lease is about to expire."} />,
    );
    expect(container.textContent).toContain("The lease is about to expire.");
    expect(container.textContent).not.toContain("[!WARNING]");
    expect(container.querySelector("[class*='alert'], [data-alert]")).toBeTruthy();
  });
});
