// @vitest-environment jsdom
//
// The Change Request page's body: the round, its diff, the verdict, and the
// two decisions a reviewer makes on it.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import changeFixture from "../../src/contracts/fixtures/work-change-detail.json";
import attemptDiffFixture from "../../src/contracts/fixtures/work-attempt-diff.json";
import itemFixture from "../../src/contracts/fixtures/work-item.json";
import projectFixture from "../../src/contracts/fixtures/work-project.json";
import type {
  AttemptDiff,
  ChangeDetail,
  NativeIssue,
  NativeProject,
} from "../../src/contracts/work.ts";
import {
  ChangeRequestView,
  currentVersion,
  selectVersion,
  statusSentence,
  statusTone,
  type ChangeRequestViewProps,
} from "../../src/app/work/ChangeRequestPage.tsx";

// jsdom has no Worker, and the page draws the patch through @pierre/diffs'
// worker pool; the file sections are stubbed so the decisions can be checked
// without one. The renderer itself is covered by the Diff surface tests.
vi.mock("@pierre/diffs/react", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@pierre/diffs/react")>()),
  FileDiff: ({ fileDiff }: { fileDiff: { name?: string } }) => (
    <div data-testid="file-diff-stub">{fileDiff.name ?? ""}</div>
  ),
}));

afterEach(cleanup);

const NOW = Date.parse("2026-09-09T12:00:00Z");
const PROJECT = projectFixture as unknown as NativeProject;
const ISSUE = itemFixture as unknown as NativeIssue;
const CHANGE = changeFixture as unknown as ChangeDetail;
const ATTEMPT_DIFF = attemptDiffFixture as unknown as AttemptDiff;

function renderView(overrides: Partial<ChangeRequestViewProps> = {}) {
  const onReview = vi.fn(async () => {});
  const onComment = vi.fn(async () => {});
  const onSelectVersion = vi.fn();
  const props: ChangeRequestViewProps = {
    issue: ISSUE,
    project: PROJECT,
    change: CHANGE,
    version: currentVersion(CHANGE),
    diff: ATTEMPT_DIFF,
    diffLoading: false,
    canWrite: true,
    canReview: true,
    busy: false,
    now: NOW,
    viewerPrincipalId: "tok_mock",
    onSelectVersion,
    onReview,
    onComment,
    ...overrides,
  };
  render(<ChangeRequestView {...props} />);
  return { onReview, onComment, onSelectVersion };
}

describe("the Change Request page", () => {
  it("draws the round's stored diff and names the round", () => {
    renderView();
    expect(screen.getByTestId("diff-files").textContent).toContain("changed file");
    expect(screen.getByTestId("change-round-card").textContent).toContain(
      `Round ${currentVersion(CHANGE)!.number}`,
    );
    expect(screen.getAllByTestId("change-round").length).toBe(CHANGE.versions.length);
  });

  it.each([true, false])("shows criterion evidence and disclosed gaps on its round (gap: %s)", (gap) => {
    const version = currentVersion(CHANGE)!;
    const change: ChangeDetail = {
      ...CHANGE,
      reviews: [{
        review_id: "review_evidence", version_id: version.version_id, decision: "approved", body: "Reviewed",
        actor: CHANGE.reviews[0]!.actor, created_at: "2026-09-09T11:00:00Z",
        validator: {
          criteria_evidence: [
            { criterion: "Reject invalid input", kind: "receipt", reference: "make check", behavior: "Invalid input errors" },
            { criterion: "Return valid input", kind: gap ? "not_verified" : "test", reference: gap ? "No behavioral assertion" : "input_test.go:TestValid", behavior: gap ? "Not exercised" : "Valid input is returned" },
          ],
          not_verified: gap ? ["Return valid input"] : [],
        },
      }],
    };
    renderView({ change });
    const evidence = screen.getByRole("region", { name: "Acceptance evidence" });
    expect(within(evidence).getByText("Reject invalid input")).toBeTruthy();
    expect(within(evidence).getByText("make check")).toBeTruthy();
    if (gap) {
      expect(within(screen.getByRole("region", { name: "Not verified criteria" })).getByText("Return valid input")).toBeTruthy();
    } else {
      expect(within(evidence).getByText("input_test.go:TestValid")).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Not verified criteria" })).toBeNull();
    }
    cleanup();
    renderView({ change, version: { ...version, version_id: "older_round" } });
    expect(screen.queryByRole("region", { name: "Acceptance evidence" })).toBeNull();
  });

  it("approves the current round with the note typed", async () => {
    const { onReview } = renderView();
    await userEvent.type(screen.getByLabelText("Review comment"), "Ship it");
    fireEvent.click(screen.getByTestId("change-approve"));
    expect(onReview).toHaveBeenCalledWith("approved", "Ship it");
  });

  it("needs a reason before it requests changes", async () => {
    const { onReview } = renderView();
    const button = screen.getByTestId("change-request-changes") as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    await userEvent.type(screen.getByLabelText("Review comment"), "Use the tenant clock.");
    expect(button.disabled).toBe(false);
    fireEvent.click(button);
    expect(onReview).toHaveBeenCalledWith("changes_requested", "Use the tenant clock.");
  });

  it("posts a comment without a decision", async () => {
    const { onComment, onReview } = renderView();
    await userEvent.type(screen.getByLabelText("Review comment"), "Mobile too?");
    fireEvent.click(screen.getByTestId("change-comment"));
    expect(onComment).toHaveBeenCalledWith("Mobile too?");
    expect(onReview).not.toHaveBeenCalled();
  });

  it("offers no decision to a reader who may not write", () => {
    renderView({ canWrite: false, canReview: false });
    expect(screen.queryByTestId("change-review-actions")).toBeNull();
    expect(screen.getByTestId("change-reviews")).not.toBeNull();
  });

  it("lets a member with a write grant discuss without deciding", async () => {
    const { onComment, onReview } = renderView({ canWrite: true, canReview: false });
    expect(screen.queryByTestId("change-approve")).toBeNull();
    expect(screen.queryByTestId("change-request-changes")).toBeNull();
    await userEvent.type(screen.getByLabelText("Review comment"), "Please check the docs.");
    fireEvent.click(screen.getByTestId("change-comment"));
    expect(onComment).toHaveBeenCalledWith("Please check the docs.");
    expect(onReview).not.toHaveBeenCalled();
  });

  it("only approves the current round", () => {
    const older = { ...currentVersion(CHANGE)!, version_id: "version_older", number: "1" };
    const change: ChangeDetail = { ...CHANGE, versions: [older, ...CHANGE.versions] };
    renderView({ change, version: older });
    expect((screen.getByTestId("change-approve") as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId("change-status-sentence").textContent).toContain(
      "not the current version",
    );
  });

  it("switches rounds from the list", () => {
    const { onSelectVersion } = renderView();
    const rounds = screen.getAllByTestId("change-round");
    fireEvent.click(rounds[0]!);
    expect(onSelectVersion).toHaveBeenCalledWith(CHANGE.versions.at(-1)!.version_id);
  });

  it("lists the reviews on the round with their decisions", () => {
    renderView();
    const reviews = within(screen.getByTestId("change-reviews")).getAllByTestId(
      "change-review-entry",
    );
    expect(reviews.length).toBe(
      CHANGE.reviews.filter((review) => review.version_id === CHANGE.change.current_version_id)
        .length,
    );
  });

  it("says where the round stands without a diff", () => {
    renderView({ diff: null });
    expect(screen.getByTestId("change-diff-empty").textContent).toContain("No stored diff");
  });
});

describe("the round helpers", () => {
  it("selects the named version, else the current one", () => {
    const current = currentVersion(CHANGE)!;
    expect(selectVersion(CHANGE, null)?.version_id).toBe(current.version_id);
    expect(selectVersion(CHANGE, "version_missing")?.version_id).toBe(current.version_id);
    expect(selectVersion(CHANGE, current.version_id)?.version_id).toBe(current.version_id);
  });

  it.each([
    ["reviewed", "ok"],
    ["needs_evidence", "warn"],
    ["stale_policy", "err"],
    ["draft", "mute"],
    ["", "mute"],
  ])("tones %s as %s", (status, tone) => {
    expect(statusTone(status)).toBe(tone);
  });

  it("explains a change with no version", () => {
    const empty: ChangeDetail = {
      ...CHANGE,
      change: { ...CHANGE.change, current_version_id: "" },
      versions: [],
      summary: { ...CHANGE.summary, status: "draft" },
    };
    expect(statusSentence(empty, null)).toContain("No version has been published");
  });

  it("reports an approved current round as ready to land", () => {
    const reviewed: ChangeDetail = { ...CHANGE, summary: { ...CHANGE.summary, status: "reviewed" } };
    expect(statusSentence(reviewed, currentVersion(reviewed))).toContain("Ready to land");
  });

  it("says a round that needs no review lands without one", () => {
    const accepted: ChangeDetail = {
      ...CHANGE,
      summary: { ...CHANGE.summary, status: "reviewed", native_review: "not_required" },
    };
    const sentence = statusSentence(accepted, currentVersion(accepted));
    expect(sentence).toContain("No review needed");
    expect(sentence).not.toContain("Approved");
  });

  it("reports a landed round", () => {
    const landed: ChangeDetail = { ...CHANGE, summary: { ...CHANGE.summary, status: "landed" } };
    expect(statusSentence(landed, currentVersion(landed))).toBe("Landed on the base branch.");
  });
});

describe("the round's diff", () => {
  it("shows a stored diff only when it carries the round's head", async () => {
    const { diffForRound } = await import("../../src/app/work/ChangeRequestPage.tsx");
    expect(diffForRound(ATTEMPT_DIFF.head_sha, ATTEMPT_DIFF)).toBe(ATTEMPT_DIFF);
    expect(diffForRound("f".repeat(40), ATTEMPT_DIFF)).toBeNull();
    expect(diffForRound(ATTEMPT_DIFF.head_sha, { ...ATTEMPT_DIFF, files: [] })).toBeNull();
    expect(diffForRound(null, ATTEMPT_DIFF)).toBeNull();
  });

  it("withholds every decision while the diff is loading", () => {
    renderView({ diffLoading: true, diff: null });
    expect((screen.getByTestId("change-approve") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId("change-request-changes") as HTMLButtonElement).disabled).toBe(true);
  });

  it("sends back only the current round", async () => {
    const older = { ...currentVersion(CHANGE)!, version_id: "version_older", number: "1" };
    const change: ChangeDetail = { ...CHANGE, versions: [older, ...CHANGE.versions] };
    renderView({ change, version: older });
    await userEvent.type(screen.getByLabelText("Review comment"), "Too late for this round.");
    expect((screen.getByTestId("change-request-changes") as HTMLButtonElement).disabled).toBe(true);
  });
});
