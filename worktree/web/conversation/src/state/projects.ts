import * as Cause from "effect/Cause";
import { AsyncResult, Atom } from "effect/unstable/reactivity";

/** One hit, in the shape `workspaceBasenameLookup.ts` reads. */
export interface ProjectSearchEntry {
  readonly path: string;
  readonly kind: "file" | "directory";
}

export interface ProjectSearchResult {
  readonly entries: ReadonlyArray<ProjectSearchEntry>;
}

const UNAVAILABLE = Atom.make(
  AsyncResult.failure<ProjectSearchResult, Error>(
    Cause.fail(new Error("Project file search needs a runner that reports the files capability.")),
  ),
).pipe(Atom.withLabel("detent-project-search:unavailable"));

export const projectEnvironment = {
  searchEntries: (_target: {
    readonly environmentId: string;
    readonly input: {
      readonly cwd: string;
      readonly query: string;
      readonly limit: number;
      readonly kind: string;
    };
  }): Atom.Atom<AsyncResult.AsyncResult<ProjectSearchResult, Error>> => UNAVAILABLE,

  writeFile: "project.writeFile",
} as const;
