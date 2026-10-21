// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { DecisionCard } from "../../src/app/work/components/DecisionCard.tsx";
import type { DecisionRequest } from "../../src/contracts/decision.ts";

beforeEach(() => vi.stubGlobal("PointerEvent", MouseEvent));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const request: DecisionRequest = {
  attempt_id: "attempt_example",
  summary: "The installer is ready for the naming decision.",
  question: "Which naming rule should the installer use?",
  options: [
    { label: "Use a derived name", consequence: "New installs derive a name from the Hub URL.", recommended: true, return_state: "Rework" },
    { label: "Require an explicit name", consequence: "Operators supply a name for each new install.", recommended: false, return_state: "Todo" },
  ],
  custom_allowed: true,
  custom_return_state: "Rework",
};

it.each([
  { name: "recommended option", select: null, expected: { attempt_id: "attempt_example", option: 0 }, state: "Rework" },
  { name: "another option", select: "Require an explicit name", expected: { attempt_id: "attempt_example", option: 1 }, state: "Todo" },
  { name: "custom answer", select: "Something else", expected: { attempt_id: "attempt_example", custom: "Use a project name" }, state: "Rework" },
])("answers with $name and its destination", async ({ select, expected, state }) => {
  const onAnswer = vi.fn(async () => undefined);
  render(<DecisionCard request={request} canAnswer onAnswer={onAnswer} />);
  expect(screen.getByRole("radio", { name: /^Use a derived name/ }).getAttribute("aria-checked")).toBe("true");
  if (select !== null) fireEvent.click(screen.getByRole("radio", { name: new RegExp(`^${select}`) }));
  if (select === "Something else") {
    expect(screen.getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "Your answer" }), { target: { value: "  Use a project name  " } });
  }
  fireEvent.click(screen.getByRole("button", { name: "Answer" }));
  await waitFor(() => expect(onAnswer).toHaveBeenCalledWith(expected, state));
});

it("keeps the answer and shows an error when the owner refuses it", async () => {
  const onAnswer = vi.fn(async () => { throw new Error("This request has changed. Refresh the issue."); });
  render(<DecisionCard request={request} canAnswer onAnswer={onAnswer} />);
  fireEvent.change(screen.getByRole("textbox", { name: "Your answer" }), { target: { value: "Keep the current name" } });
  fireEvent.click(screen.getByRole("button", { name: "Answer" }));
  expect((await screen.findByRole("alert")).textContent).toContain("This request has changed");
  expect((screen.getByRole("textbox", { name: "Your answer" }) as HTMLTextAreaElement).value).toBe("Keep the current name");
});

it("shows the existing human action when no structured request is available", () => {
  render(<DecisionCard humanAction="Approve the migration before work resumes." canAnswer onAnswer={vi.fn()} />);
  expect(screen.getByText("Approve the migration before work resumes.")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Answer" })).toBeNull();
});

it("keeps decisions readable without write permission", () => {
  const onAnswer = vi.fn();
  render(<DecisionCard request={request} canAnswer={false} onAnswer={onAnswer} />);
  fireEvent.click(screen.getByRole("button", { name: "Answer" }));
  expect(onAnswer).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(true);
});
