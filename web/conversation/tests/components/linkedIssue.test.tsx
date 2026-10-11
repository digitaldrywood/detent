// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { NewIssueDialog } from "../../src/app/work/NewIssue.tsx";
import type { AccountProject } from "../../src/contracts/account.ts";

const { create } = vi.hoisted(() => ({ create: vi.fn(async () => ({ work_item_id: "wi_linked" })) }));
vi.mock("../../src/app/account/context.ts", () => ({
  useAccountApi: () => ({ createFirstIssue: create }),
  useAccountBootstrap: () => null,
}));

afterEach(() => { cleanup(); create.mockClear(); });

it("links a GitHub issue without requiring a copied title or body", async () => {
  const project: AccountProject = {
    id: "prj_example", name: "Orders", profile: "native", can_write: true,
    can_manage_runners: false,
    states: [{ name: "Todo", terminal: false, dispatchable: true }],
  };
  const created = vi.fn();
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[project]} projectId={project.id} onCreated={created} />);
  fireEvent.change(screen.getByLabelText("GitHub issue URL (optional)"), { target: { value: "https://github.com/acme/orders/issues/12" } });
  fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
  await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
    githubIssueUrl: "https://github.com/acme/orders/issues/12", title: "", body: "", state: "Todo",
  })));
  expect(created).toHaveBeenCalledOnce();
});
