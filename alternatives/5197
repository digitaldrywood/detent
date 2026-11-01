// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it } from "vitest";

import { ComposerContextStrip } from "../../src/app/components/ComposerContextStrip.tsx";

afterEach(cleanup);

function renderStrip(overrides: Partial<React.ComponentProps<typeof ComposerContextStrip>> = {}) {
  return render(
    <ComposerContextStrip projectName="alpha" issueIdentifier={null} {...overrides} />,
  );
}

describe("the composer's context strip", () => {
  it("names the project while the chat is unlinked", () => {
    renderStrip();
    expect(screen.getByTestId("composer-scope-menu").textContent).toContain("Project alpha");
  });

  // Michael's review, September 11: "dont need this when on a blank chat."
  it("says nothing on the right of a draft", () => {
    renderStrip({ draft: true });
    expect(screen.getByTestId("composer-context-issue").textContent).toBe("");
    expect(screen.queryByText("No linked issue")).toBeNull();
  });

  // A chat that has been sent and is still unlinked keeps saying so: there the
  // absence is a fact about that chat rather than about every new chat.
  it("still says a sent chat has no linked issue", () => {
    renderStrip();
    expect(screen.getByTestId("composer-context-issue").textContent).toBe("No linked issue");
  });

  it("names the linked issue on the left", () => {
    renderStrip({ issueIdentifier: "alpha#12", issueLane: "In progress" });
    expect(screen.getByTestId("composer-scope-menu").textContent).toContain("Issue #12");
  });

  it("carries the pull request and the branch of a linked issue", () => {
    renderStrip({
      issueIdentifier: "alpha#12",
      issueLane: "In progress",
      pullRequest: {
        number: 974,
        url: "https://github.com/acme/repo/pull/974",
        branch: "fix/openimport-deductible-labels",
        state: "open",
        draft: false,
      },
    });
    const link = screen.getByTestId("composer-context-pull-request");
    expect(link.textContent).toBe("#974");
    expect(link.getAttribute("href")).toBe("https://github.com/acme/repo/pull/974");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
    expect(screen.getByTestId("composer-context-branch").textContent).toContain(
      "fix/openimport-deductible-labels",
    );
  });

  it("shows the branch alone while the issue has no pull request yet", () => {
    renderStrip({
      issueIdentifier: "alpha#12",
      pullRequest: { number: null, url: "", branch: "feat/lease", state: "open", draft: false },
    });
    expect(screen.queryByTestId("composer-context-pull-request")).toBeNull();
    expect(screen.getByTestId("composer-context-branch").textContent).toContain("feat/lease");
  });

  it("falls back to the issue's lane when the hub knows of neither", () => {
    renderStrip({ issueIdentifier: "alpha#12", issueLane: "In progress", pullRequest: null });
    expect(screen.getByTestId("composer-context-issue").textContent).toBe("In progress");
  });
});
