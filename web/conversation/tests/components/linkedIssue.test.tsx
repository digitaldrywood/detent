// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import { NewIssueDialog } from "../../src/app/work/NewIssue.tsx";
import type { AccountProject } from "../../src/contracts/account.ts";

const { create, role, templates, api } = vi.hoisted(() => {
  const create = vi.fn(async (_input: { key: string; challengeToken?: string }) => ({ work_item_id: "wi_linked" }));
  const templates = {
    bug: ["Steps to reproduce", "Expected", "Actual", "Where", "Environment"],
    change_request: ["Current behavior", "Desired behavior", "Why"],
  };
  return { create, templates, role: { value: "member" }, api: {
    createFirstIssue: create,
    reporterTemplates: vi.fn(async () => ({ reporter_templates: templates })),
  } };
});
vi.mock("../../src/app/account/context.ts", () => ({
  useAccountApi: () => api,
  useAccountBootstrap: () => ({ actor: { role: role.value } }),
}));

afterEach(() => {
  cleanup();
  create.mockReset();
  create.mockResolvedValue({ work_item_id: "wi_linked" });
  api.reporterTemplates.mockClear();
  api.reporterTemplates.mockImplementation(async () => ({ reporter_templates: templates }));
  role.value = "member";
});

it("links a GitHub issue without requiring a copied title or body", async () => {
  const project: AccountProject = {
    id: "prj_example", name: "Orders", profile: "native", can_write: true,
    can_manage_runners: false,
    states: [{ name: "Todo", terminal: false, dispatchable: true }],
  };
  const created = vi.fn();
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[project]} projectId={project.id} onCreated={created} />);
  fireEvent.change(screen.getByLabelText("GitHub issue URL (optional)"), { target: { value: "https://github.com/example-org/example-project/issues/12" } });
  fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
  await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
    githubIssueUrl: "https://github.com/example-org/example-project/issues/12", title: "", body: "", state: "Todo",
  })));
  expect(created).toHaveBeenCalledOnce();
});

it.each(["bug", "change_request"])("requires a Reporter to choose %s and submits the selected type", async (type) => {
  role.value = "reporter";
  const project: AccountProject = {
    id: "prj_example", name: "Example", profile: "native", can_write: true,
    can_manage_runners: false, states: [{ name: "Triage", terminal: false, dispatchable: false }],
  };
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[project]} projectId={project.id} onCreated={() => {}} />);
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Tester report" } });
  expect((screen.getByRole("button", { name: "Create issue" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(screen.getByLabelText("Type"), { target: { value: type } });
  const expected = type === "bug"
    ? "## Steps to reproduce\n\n\n## Expected\n\n\n## Actual\n\n\n## Where\n\n\n## Environment"
    : "## Current behavior\n\n\n## Desired behavior\n\n\n## Why";
  await waitFor(() => expect((screen.getByLabelText("What needs doing") as HTMLTextAreaElement).value.trim()).toBe(expected));
  fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
  await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({ type, title: "Tester report", body: expected })));
});

it("uses project overrides, replaces untouched headings, and preserves tester evidence", async () => {
  api.reporterTemplates.mockResolvedValue({ reporter_templates: { bug: ["Reproduction", "Outcome"], change_request: ["Motivation"] } });
  const project: AccountProject = {
    id: "prj_example", name: "Example", profile: "native", can_write: true,
    can_manage_runners: false, states: [{ name: "Todo", terminal: false, dispatchable: true }],
  };
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[project]} projectId={project.id} onCreated={() => {}} />);
  fireEvent.change(screen.getByLabelText("Type (optional)"), { target: { value: "bug" } });
  const body = screen.getByLabelText("What needs doing") as HTMLTextAreaElement;
  await waitFor(() => expect(body.value.trim()).toBe("## Reproduction\n\n\n## Outcome"));
  fireEvent.change(screen.getByLabelText("Type (optional)"), { target: { value: "change_request" } });
  await waitFor(() => expect(body.value.trim()).toBe("## Motivation"));
  fireEvent.change(body, { target: { value: "## Motivation\nKeep my observations." } });
  fireEvent.change(screen.getByLabelText("Type (optional)"), { target: { value: "bug" } });
  await waitFor(() => expect(body.value).toBe("## Motivation\nKeep my observations."));
});

const duplicate = {
  challenge_token: "dup_example",
  possible_duplicates: [{ work_item_id: "wi_existing", number: 12, title: "Runner checkout deadlocks", web_url: "https://hub.example.test/issues/12", why: "Matching terms: runner, checkout" }],
};
const duplicateError = () => new AccountError({ status: 409, code: "possible_duplicates", message: "Possible duplicate issues found", duplicateChallenge: duplicate });
const duplicateProject: AccountProject = {
  id: "prj_example", name: "Example", profile: "native", can_write: true,
  can_manage_runners: false, states: [{ name: "Backlog", terminal: false, dispatchable: false }],
};

it("offers Create anyway, retains the report on failure and retries the same confirmation", async () => {
  create.mockRejectedValueOnce(duplicateError()).mockRejectedValueOnce(new AccountError({ status: 503, code: "service_unavailable", message: "Service unavailable" }));
  const created = vi.fn();
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[duplicateProject]} projectId={duplicateProject.id} onCreated={created} />);
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Runner checkout deadlocks" } });
  fireEvent.change(screen.getByLabelText("What needs doing"), { target: { value: "Independent observation" } });
  fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
  fireEvent.click(await screen.findByRole("button", { name: "Create anyway" }));
  expect(await screen.findByText(/Service unavailable.*report is retained/)).toBeTruthy();
  expect((screen.getByLabelText("Title") as HTMLInputElement).value).toBe("Runner checkout deadlocks");
  expect((screen.getByLabelText("What needs doing") as HTMLTextAreaElement).value).toBe("Independent observation");
  expect(screen.getByRole("link", { name: "#12 Runner checkout deadlocks" }).getAttribute("href")).toBe(duplicate.possible_duplicates[0]!.web_url);
  fireEvent.click(screen.getByRole("button", { name: "Create anyway" }));
  await waitFor(() => expect(created).toHaveBeenCalledOnce());
  const [initial, confirmation, retry] = create.mock.calls.map((call) => call[0]);
  expect(confirmation).toEqual({ ...initial, challengeToken: duplicate.challenge_token });
  expect(retry).toEqual(confirmation);
});

it.each(["expired", "edited", "project"])("reviews fresh suggestions when a challenge is %s", async (scenario) => {
  create.mockRejectedValueOnce(duplicateError());
  render(<NewIssueDialog open onOpenChange={() => {}} projects={[duplicateProject, { ...duplicateProject, id: "prj_other", name: "Other" }]} projectId={duplicateProject.id} onCreated={() => {}} />);
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Runner checkout deadlocks" } });
  fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
  const confirm = await screen.findByRole("button", { name: "Create anyway" });
  if (scenario === "expired") {
    create.mockRejectedValueOnce(new AccountError({ status: 422, code: "invalid_request", message: "Duplicate challenge expired" }));
    fireEvent.click(confirm);
    await screen.findByText(/Duplicate challenge expired/);
  } else if (scenario === "edited") {
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Changed report" } });
  } else {
    fireEvent.change(screen.getByLabelText("Project"), { target: { value: "prj_other" } });
  }
  fireEvent.click(await screen.findByRole("button", { name: "Create issue" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(scenario === "expired" ? 3 : 2));
  const initial = create.mock.calls[0]![0];
  const fresh = create.mock.lastCall![0];
  expect(fresh.challengeToken).toBeUndefined();
  expect(fresh.key).not.toBe(initial.key);
});
