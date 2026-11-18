// The Work board routes, loaded as one chunk the first time either matches.
import type { ReactElement } from "react";

import { useShell } from "../App.tsx";
import { WorkBoard } from "./WorkBoard.tsx";

export function AllProjectsWorkBoard(): ReactElement {
  const { projectId } = useShell();
  return <WorkBoard projectId={projectId || null} />;
}

/** Reads the project out of the path so the board stays a pure component. */
export function ProjectWorkBoard(): ReactElement {
  return <AllProjectsWorkBoard />;
}

/** A project board reached by its readable slug; the route resolved the id. */
export function ReadableProjectWorkBoard(): ReactElement {
  return <AllProjectsWorkBoard />;
}
