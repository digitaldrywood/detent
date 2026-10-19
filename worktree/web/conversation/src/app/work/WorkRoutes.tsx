// The Work board routes, loaded as one chunk the first time either matches.
import { useParams, useRouteContext } from "@tanstack/react-router";
import type { ReactElement } from "react";

import { WorkBoard } from "./WorkBoard.tsx";

export function AllProjectsWorkBoard(): ReactElement {
  return <WorkBoard projectId={null} />;
}

/** Reads the project out of the path so the board stays a pure component. */
export function ProjectWorkBoard(): ReactElement {
  const { projectId } = useParams({ from: "/work/p/$projectId" });
  return <WorkBoard projectId={projectId} />;
}

/** A project board reached by its readable slug; the route resolved the id. */
export function ReadableProjectWorkBoard(): ReactElement {
  const { resolvedProjectId } = useRouteContext({ strict: false }) as { resolvedProjectId: string };
  return <WorkBoard projectId={resolvedProjectId} />;
}
