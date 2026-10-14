// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TestPlanProgress } from "../../src/app/work/components/TestPlanProgress.tsx";
import type { NativeTestPlan } from "../../src/contracts/work.ts";

afterEach(() => {
  cleanup();
});

const plan: NativeTestPlan = {
  passed: 1,
  failed: 1,
  blocked: 0,
  untested: 1,
  items: [
    { index: 1, text: "Login", result: "pass", tester: { kind: "human", principal_id: "tok_1", display_name: "tester@example.test" }, recorded_at: "2026-10-13T12:00:00Z", linked_issues: [] },
    { index: 2, text: "Quote builder: send", result: "fail", note: "Send does nothing", tester: { kind: "human", principal_id: "tok_1" }, recorded_at: "2026-10-13T12:05:00Z", linked_issues: [{ work_item_id: "wi_finding", number: 42 }] },
    { index: 3, text: "Invoices", linked_issues: [] },
  ],
};

describe("the test plan progress", () => {
  it("shows the counts and each item's last result, tester and linked issues", () => {
    const onOpenIssue = vi.fn();
    render(<TestPlanProgress plan={plan} canRecord={false} onRecord={vi.fn()} onOpenIssue={onOpenIssue} />);
    expect(screen.getByTestId("test-plan-counts").textContent).toBe("1 passed · 1 failed · 0 blocked · 1 untested");
    const rows = screen.getAllByTestId("test-item");
    expect(rows.map((row) => within(row).getByTestId("test-item-result").textContent)).toEqual(["Passed", "Failed", "Untested"]);
    expect(rows[0]!.textContent).toContain("tester@example.test");
    expect(rows[1]!.textContent).toContain("Send does nothing");
    expect(screen.queryByRole("button", { name: /Record a result/ })).toBeNull();
    fireEvent.click(within(rows[1]!).getByRole("button", { name: "#42" }));
    expect(onOpenIssue).toHaveBeenCalledWith("wi_finding");
  });

  it("records a result for the clicked item with its note and related issue", async () => {
    const onRecord = vi.fn().mockResolvedValue(undefined);
    render(<TestPlanProgress plan={plan} canRecord onRecord={onRecord} onOpenIssue={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Record a result for item 3: Invoices" }));
    fireEvent.change(await screen.findByLabelText("Note (optional)"), { target: { value: "Totals wrong" } });
    fireEvent.change(screen.getByLabelText("Issue number filed for a failure (optional)"), { target: { value: "42" } });
    fireEvent.click(screen.getByTestId("test-item-fail"));
    await waitFor(() => expect(onRecord).toHaveBeenCalledWith({ item: 3, text: "Invoices", result: "fail", note: "Totals wrong", relatedIssueNumber: 42 }));
  });

  it("keeps the form open with the hub's reason when recording fails", async () => {
    const onRecord = vi.fn().mockRejectedValue(new Error("Item results are recorded only on native issues labeled test-plan"));
    render(<TestPlanProgress plan={plan} canRecord onRecord={onRecord} onOpenIssue={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Record a result for item 1: Login" }));
    fireEvent.click(await screen.findByTestId("test-item-blocked"));
    expect((await screen.findByRole("alert")).textContent).toContain("labeled test-plan");
    expect(onRecord).toHaveBeenCalledWith({ item: 1, text: "Login", result: "blocked", note: "", relatedIssueNumber: undefined });
  });
});
