// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { GitHubIssueIntake } from "../../src/app/account/IssueIntake.tsx";

if (typeof globalThis.PointerEvent === "undefined") { globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent; }

const mocks = vi.hoisted(() => ({ read: vi.fn(), command: vi.fn() }));
const api = { issueIntake: mocks.read, commandIssueIntake: mocks.command };
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api }));

const lanes = [{ name: "Backlog", dispatchable: false, terminal: false }, { name: "Todo", dispatchable: true, terminal: false }];
const preview = () => ({ batch: { id: "intake_test", revision: 2, runner_id: "runner_test", status: "preview", destination: "", discovery: { repository: "acme/orders", include_closed: false, labels: ["bug"], cursor: "" }, page: { total: 2, next_cursor: "", issues: [12, 13].map(number => ({ number, id: `I_${number}`, url: `https://github.com/acme/orders/issues/${number}`, title: `Task ${number}`, body: "Historical source body", closed: false, labels: ["bug"] })) }, items: [] }, lanes });
beforeEach(() => { mocks.read.mockResolvedValue(preview()); mocks.command.mockResolvedValue(preview()); });
afterEach(() => { cleanup(); vi.clearAllMocks(); sessionStorage.clear(); });
const mount = () => render(<GitHubIssueIntake projectId="prj_test" repository="acme/orders" runners={[{ id: "runner_test", name: "Private runner" }]} />);

it("previews open issues and sends an explicit subset to the non-dispatchable lane", async () => {
  mount();
  await screen.findByText(/2 matching issues/);
  expect((screen.getByLabelText("Include closed history") as HTMLInputElement).checked).toBeFalsy();
  expect((screen.getByLabelText("Import destination lane") as HTMLSelectElement).value).toBe("Backlog");
  fireEvent.change(screen.getByLabelText("Select by issue number"), { target: { value: "13, 13" } });
  expect(screen.getByText(/Import 1 selected issues into Backlog/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Import selected issues" }));
  await waitFor(() => expect(mocks.command).toHaveBeenCalled());
  expect(mocks.command.mock.calls[0]?.[1]).toEqual({ action: "apply", revision: 2, numbers: [13], destination: "Backlog", allow_dispatch: false });
});

it("requires dispatch approval after selecting all matching", async () => {
  mount(); await screen.findByText(/2 matching issues/);
  fireEvent.click(screen.getByRole("button", { name: "Select all matching" }));
  fireEvent.change(screen.getByLabelText("Import destination lane"), { target: { value: "Todo" } });
  expect((screen.getByRole("button", { name: "Import selected issues" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByLabelText("Authorize dispatch of selected issues"));
  fireEvent.click(screen.getByRole("button", { name: "Import selected issues" }));
  await waitFor(() => expect(mocks.command).toHaveBeenCalled());
  expect(mocks.command.mock.calls[0]?.[1]).toMatchObject({ numbers: [12, 13], allow_dispatch: true, destination: "Todo" });
});

it("does not treat an unloaded page as all matching", async () => {
  const data = preview(); data.batch.page.next_cursor = "page2"; data.batch.page.total = 200;
  mocks.read.mockResolvedValue(data); mount(); await screen.findByText(/200 matching issues/);
  expect((screen.getByRole("button", { name: "Select all matching" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Load next 100" }));
  await waitFor(() => expect(mocks.command).toHaveBeenCalled());
  expect(mocks.command.mock.calls[0]?.[1]).toEqual({ action: "more", revision: 2 });
});

it("reports partial results and requests retry only on operator action", async () => {
  const data = { ...preview(), batch: { ...preview().batch, status: "finished", destination: "Backlog", items: [{ number: 12, status: "completed" }, { number: 13, status: "failed", error: "Source context incomplete", retry_at: "2026-10-01T00:00:00Z" }] } };
  mocks.read.mockResolvedValue(data); mount();
  await screen.findByText(/1 completed · 0 skipped · 1 incomplete/);
  expect(mocks.read).toHaveBeenCalledTimes(1);
  expect(mocks.command).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Retry incomplete intake" }));
  await waitFor(() => expect(mocks.command).toHaveBeenCalled());
  expect(mocks.command.mock.calls[0]?.[1]).toEqual({ action: "retry", revision: 2, runner_id: "runner_test" });
});
