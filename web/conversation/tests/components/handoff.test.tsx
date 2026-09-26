// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { HandoffForm } from "../../src/app/components/HandoffForm.tsx";

afterEach(cleanup);

function renderForm(overrides: Partial<React.ComponentProps<typeof HandoffForm>> = {}) {
  const onSubmit = vi.fn();
  const onCancel = vi.fn();
  const onOpenExisting = vi.fn();
  const utils = render(
    <HandoffForm
      projectId="proj_alpha"
      projectName="alpha"
      messageCount={4}
      seedTitle="Flaky checkout lock renewal"
      seedObjective="The renewal returns before the handoff completes."
      submitting={false}
      failure={null}
      onSubmit={onSubmit}
      onCancel={onCancel}
      onOpenExisting={onOpenExisting}
      {...overrides}
    />,
  );
  return { ...utils, onSubmit, onCancel, onOpenExisting };
}

const share = () =>
  screen.getByLabelText<HTMLInputElement>("Share this conversation's history with the project");

const STATES = [
  { name: "Backlog", dispatchable: false },
  { name: "Todo", dispatchable: true },
  { name: "In progress", dispatchable: true },
];

describe("HandoffForm", () => {
  // §13.8 and §14: creating an issue from a chat asks what happens next, with
  // Detent's own defaults already chosen.
  it("preselects the first dispatchable lane, the default priority and later", () => {
    renderForm({ states: STATES });
    expect(screen.getByLabelText<HTMLSelectElement>("Lane").value).toBe("Todo");
    // Unset: the request asks the hub for the project's own default rather
    // than guessing it (§14).
    expect(screen.getByLabelText<HTMLSelectElement>("Priority").value).toBe("");
    expect(screen.getByLabelText<HTMLSelectElement>("Dispatch").value).toBe("later");
  });

  it("sends the next step with the link request", () => {
    const { onSubmit } = renderForm({ states: STATES });
    fireEvent.change(screen.getByLabelText("Lane"), { target: { value: "In progress" } });
    fireEvent.change(screen.getByLabelText("Dispatch"), { target: { value: "now" } });
    fireEvent.click(share());
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onSubmit).toHaveBeenCalledWith(
      expect.objectContaining({ next: { state: "In progress", dispatch: "now" } }),
    );
  });

  it("still asks about dispatch when the project publishes no states", () => {
    renderForm();
    expect(screen.queryByLabelText("Lane")).toBeNull();
    expect(screen.getByLabelText<HTMLSelectElement>("Dispatch").value).toBe("later");
  });

  it("fixes the project and says why it cannot change", () => {
    renderForm();
    const project = screen.getByLabelText<HTMLInputElement>("Project");
    expect(project.value).toBe("alpha");
    expect(project.readOnly).toBe(true);
    expect(screen.getByText("A conversation stays in the project it started in.")).toBeTruthy();
  });

  it("states the audience and how much history becomes readable", () => {
    renderForm();
    expect(screen.getByTestId("handoff-message-count").textContent).toBe(
      "All 4 messages in this chat become readable by everyone who can read project alpha.",
    );
    expect(screen.getByText("Sharing cannot be undone.")).toBeTruthy();
  });

  it("prefills from a proposal when the coordinator made one", () => {
    renderForm({
      proposal: {
        project_id: "proj_alpha",
        title: "Checkout lock renewal waits on a healthy handoff",
        objective: "Move the renewal behind the handoff acknowledgement.",
      },
    });
    expect(screen.getByLabelText<HTMLInputElement>("Title").value).toBe(
      "Checkout lock renewal waits on a healthy handoff",
    );
    expect(screen.getByLabelText<HTMLTextAreaElement>("Objective").value).toBe(
      "Move the renewal behind the handoff acknowledgement.",
    );
  });

  it("prefills from the conversation when there is no proposal", () => {
    renderForm();
    expect(screen.getByLabelText<HTMLInputElement>("Title").value).toBe(
      "Flaky checkout lock renewal",
    );
    expect(screen.getByLabelText<HTMLTextAreaElement>("Objective").value).toBe(
      "The renewal returns before the handoff completes.",
    );
  });

  it("refuses to submit until the share confirmation is ticked", () => {
    const { onSubmit } = renderForm();
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByTestId("handoff-local-error").textContent).toContain(
      "Linking shares this conversation's full history with the project.",
    );
    expect(document.activeElement).toBe(share());
  });

  it("submits share_history true once the box is ticked", () => {
    const { onSubmit } = renderForm();
    fireEvent.click(share());
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onSubmit).toHaveBeenCalledWith(
      expect.objectContaining({
        title: "Flaky checkout lock renewal",
        shareHistory: true,
        labels: [],
        priority: null,
      }),
    );
  });

  it("keeps the typed values when the request fails", () => {
    const { rerender } = renderForm();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "A better title" } });
    fireEvent.change(screen.getByLabelText("Objective"), { target: { value: "A clear objective" } });
    fireEvent.click(share());
    rerender(
      <HandoffForm
        projectId="proj_alpha"
        projectName="alpha"
        messageCount={4}
        submitting={false}
        failure={{ code: "network", message: "The hub could not be reached." }}
        onSubmit={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    expect(screen.getByLabelText<HTMLInputElement>("Title").value).toBe("A better title");
    expect(screen.getByLabelText<HTMLTextAreaElement>("Objective").value).toBe(
      "A clear objective",
    );
    expect(share().checked).toBe(true);
    expect(screen.getByTestId("handoff-failure").textContent).toContain(
      "The hub could not be reached.",
    );
  });

  // The dialog moves focus once it has opened, so this waits for the move
  // rather than for the render.
  it("focuses the confirmation when the hub asks for it", async () => {
    renderForm({
      failure: {
        code: "share_history_required",
        message: "Linking shares the whole history. Confirm before linking.",
      },
    });
    await waitFor(() => expect(document.activeElement).toBe(share()));
  });

  it("offers navigation instead of a second issue when already linked", () => {
    const { onOpenExisting } = renderForm({
      failure: {
        code: "conversation_already_linked",
        message: "That issue already has a conversation.",
        existingConversationId: "conv_other",
      },
    });
    fireEvent.click(screen.getByText("Open the linked conversation"));
    expect(onOpenExisting).toHaveBeenCalledWith("conv_other");
  });

  it("offers labels only when the project has them", () => {
    const { unmount } = renderForm();
    expect(screen.queryByText("Labels (optional)")).toBeNull();
    unmount();

    const { onSubmit } = renderForm({ labels: ["bug", "chore"] });
    fireEvent.click(share());
    fireEvent.click(screen.getByLabelText("bug"));
    // Priority is the tracker's own rank, 0-3 (§14): "High" is rank 1.
    fireEvent.change(screen.getByLabelText("Priority"), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onSubmit).toHaveBeenCalledWith(
      expect.objectContaining({
        labels: ["bug"],
        priority: 1,
        next: expect.objectContaining({ priority: 1 }),
      }),
    );
  });

  it("closes on Escape and returns control to the caller", () => {
    const { onCancel } = renderForm();
    fireEvent.keyDown(screen.getByTestId("handoff-form"), { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
