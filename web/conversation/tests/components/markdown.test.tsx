// @vitest-environment jsdom
//
// The markdown renderer's security properties. The hand-written parser this
// replaced held them by construction (it never parsed HTML at all); the
// `react-markdown` pipeline holds them because `rehype-raw` is not in it and
// because the default URL transform strips dangerous schemes. Both are worth a
// test, because both are one plugin away from being lost.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { Markdown } from "../../src/app/components/Markdown.tsx";

afterEach(cleanup);

describe("Markdown", () => {
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
