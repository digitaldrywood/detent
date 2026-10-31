import { redirect } from "@tanstack/react-router";
import { lastAccountBootstrap } from "../../runtime/bootstrap.ts";

export async function resolveReadable(projectSlug: string, number?: string): Promise<{ resolvedProjectId: string; resolvedWorkItemId: string }> {
  const account = lastAccountBootstrap();
  if (account === null) throw new Error("Account unavailable");
  const path = `${account.api_base}/readable/${encodeURIComponent(projectSlug)}${number === undefined ? "" : `/issues/${encodeURIComponent(number)}`}`;
  const response = await fetch(path, { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (!response.ok) throw new Error("Issue or project not found");
  const data = await response.json() as { project_id: string; work_item_id: string };
  return { resolvedProjectId: data.project_id, resolvedWorkItemId: data.work_item_id };
}

export function redirectLegacyProject(projectId: string, search: Record<string, unknown>): void {
  const account = lastAccountBootstrap();
  const project = account?.projects.find((project) => project.id === projectId);
  if (!account?.organization.slug || !project?.slug) return;
  if (!document.querySelector('meta[name="detent-router-base"]')?.getAttribute("content")) {
    const canonical = new URL(`/${account.organization.slug}/${project.slug}`, globalThis.location.origin);
    for (const [key, value] of Object.entries(search)) { if (value !== undefined && value !== null) canonical.searchParams.set(key, String(value)); }
    throw redirect({ href: canonical.href, reloadDocument: true });
  }
  throw redirect({ to: "/$projectSlug", params: { projectSlug: project.slug }, search });
}

export async function redirectLegacyIssue(workItemId: string, search: Record<string, unknown>, changeId?: string): Promise<void> {
  const account = lastAccountBootstrap();
  if (!account?.organization.slug) return;
  const response = await fetch(`${account.api_base}/work-items/${encodeURIComponent(workItemId)}`, { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (!response.ok) throw new Error("Issue not found");
  const issue = await response.json() as { project_id: string; number: number; web_url: string };
  const canonicalSearch = { ...search };
  delete canonicalSearch.project;
  const canonical = new URL(issue.web_url, globalThis.location.origin);
  if (changeId !== undefined) canonical.pathname += `/changes/${encodeURIComponent(changeId)}`;
  for (const [key, value] of Object.entries(canonicalSearch)) { if (value !== undefined && value !== null) canonical.searchParams.set(key, String(value)); }
  const routerBase = document.querySelector('meta[name="detent-router-base"]')?.getAttribute("content");
  if (!routerBase) throw redirect({ href: canonical.href, reloadDocument: true });
  const project = account.projects.find((project) => project.id === issue.project_id);
  if (!project?.slug) throw new Error("Project not found");
  if (changeId === undefined) throw redirect({ to: "/$projectSlug/issues/$number", params: { projectSlug: project.slug, number: String(issue.number) }, search: canonicalSearch });
  throw redirect({ to: "/$projectSlug/issues/$number/changes/$changeId", params: { projectSlug: project.slug, number: String(issue.number), changeId }, search: canonicalSearch });
}
