// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { SlugSettings } from "../../src/app/account/Organization.tsx";

const api = vi.hoisted(() => ({ slug: vi.fn(), saveSlug: vi.fn() }));
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => null }));
vi.mock("../../src/app/entry/shared.ts", async (load) => ({ ...await load<object>(), behindSharedEntry: () => true }));

const reload = vi.fn();
beforeEach(() => {
  api.slug.mockReset().mockResolvedValue({ slug: "example-project" });
  api.saveSlug.mockReset();
  reload.mockReset();
  vi.stubGlobal("location", { reload });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it.each([undefined, "prj_example"])("keeps the edited slug and shows a rejected save for scope %s", async (projectId) => {
  api.saveSlug.mockRejectedValue(new Error("This slug is already reserved"));
  render(<SlugSettings projectId={projectId} canManage />);
  const input = screen.getByRole("textbox", { name: projectId === undefined ? "Organization slug" : "Project slug" });
  await waitFor(() => expect((input as HTMLInputElement).value).toBe("example-project"));
  const user = userEvent.setup();
  await user.clear(input);
  await user.type(input, "new-name");
  await user.click(screen.getByRole("button", { name: "Save slug" }));
  expect(await screen.findByText("This slug is already reserved")).toBeTruthy();
  expect((input as HTMLInputElement).value).toBe("new-name");
  expect(api.saveSlug).toHaveBeenCalledWith("new-name", "example-project", projectId, true);
});

it("shows members the slug without allowing edits", async () => {
  render(<SlugSettings projectId="prj_example" canManage={false} />);
  const input = screen.getByRole("textbox", { name: "Project slug" });
  await waitFor(() => expect((input as HTMLInputElement).value).toBe("example-project"));
  expect((input as HTMLInputElement).disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Save slug" })).toBeNull();
});

it.each([undefined, "prj_example"])("saves and reloads the current slug for scope %s", async (projectId) => {
  let savedSlug = "example-project";
  api.slug.mockImplementation(async () => ({ slug: savedSlug }));
  let finishSave!: () => void;
  api.saveSlug.mockImplementation(() => new Promise((resolve) => {
    finishSave = () => { savedSlug = "new-name"; resolve({ slug: savedSlug }); };
  }));
  const view = render(<SlugSettings projectId={projectId} canManage />);
  const label = projectId === undefined ? "Organization slug" : "Project slug";
  const input = screen.getByRole("textbox", { name: label });
  await waitFor(() => expect((input as HTMLInputElement).value).toBe("example-project"));
  const user = userEvent.setup();
  await user.clear(input);
  await user.type(input, "new-name");
  await user.click(screen.getByRole("button", { name: "Save slug" }));
  expect(api.saveSlug).toHaveBeenCalledWith("new-name", "example-project", projectId, true);
  expect(reload).not.toHaveBeenCalled();
  finishSave();
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  view.unmount();
  render(<SlugSettings projectId={projectId} canManage />);
  await waitFor(() => expect((screen.getByRole("textbox", { name: label }) as HTMLInputElement).value).toBe("new-name"));
  expect(api.slug).toHaveBeenLastCalledWith(projectId, true);
  expect((screen.getByRole("button", { name: "Save slug" }) as HTMLButtonElement).disabled).toBe(true);
});
