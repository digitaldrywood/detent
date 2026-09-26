import { AsyncResult, Atom } from "effect/unstable/reactivity";

import type { EnvironmentId } from "../contracts/index.ts";
import type { VcsStatusResult } from "../contracts/ui.ts";

const UNRESOLVED = Atom.make(AsyncResult.initial<VcsStatusResult, never>(false)).pipe(
  Atom.withLabel("detent-vcs-status:absent"),
);

export const vcsEnvironment = {
  status(_input: {
    environmentId: EnvironmentId;
    input: { cwd: string };
  }): Atom.Atom<AsyncResult.AsyncResult<VcsStatusResult, never>> {
    return UNRESOLVED;
  },
};
