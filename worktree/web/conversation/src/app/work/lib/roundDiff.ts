import { ArchiveUnavailable } from "../../../runtime/archiveDecode.ts";
import React from "react";

import type { AttemptDiff, ChangeDetail, ChangeVersion } from "../../../contracts/work.ts";
import type { WorkHttp } from "./workHttp.ts";

export function currentVersion(change: ChangeDetail): ChangeVersion | null {
  return (
    change.versions.find(
      (version) => version.version_id === change.change.current_version_id,
    ) ??
    change.versions.at(-1) ??
    null
  );
}

export function diffForRound(
  headSha: string | null,
  candidate: AttemptDiff | null,
): AttemptDiff | null {
  if (headSha === null || candidate === null || candidate.files.length === 0) return null;
  return candidate.head_sha === headSha ? candidate : null;
}

export function useRoundDiff(
  http: WorkHttp,
  projectId: string | null,
  workItemId: string,
  version: ChangeVersion | null,
): { diff: AttemptDiff | null; loading: boolean; error: string | null } {
  const [state, setState] = React.useState<{
    versionId: string | null;
    diff: AttemptDiff | null;
    loading: boolean;
    error: string | null;
  }>({ versionId: null, diff: null, loading: false, error: null });
  const attemptId = version?.attempt_id ?? null;
  const versionId = version?.version_id ?? null;
  const headSha = version?.head_sha ?? null;
  React.useEffect(() => {
    if (projectId === null || versionId === null || headSha === null) {
      setState({ versionId, diff: null, loading: false, error: null });
      return;
    }
    let cancelled = false;
    setState({ versionId, diff: null, loading: true, error: null });
    void (async () => {
      const byAttempt =
        attemptId === null
          ? null
          : await http.getAttemptDiff(projectId, attemptId).catch((error: unknown) => { if (error instanceof ArchiveUnavailable) throw error; return null; });
      const found =
        diffForRound(headSha, byAttempt) ??
        (await http
          .getWorkItemDiff(projectId, workItemId)
          .then((answer) => answer.diff)
          .catch((error: unknown) => { if (error instanceof ArchiveUnavailable) throw error; return null; }));
      if (cancelled) return;
      setState({ versionId, diff: diffForRound(headSha, found), loading: false, error: null });
    })().catch((error: unknown) => {
      if (!cancelled) setState({ versionId, diff: null, loading: false, error: error instanceof Error ? error.message : "Archived detail is unavailable." });
    });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, attemptId, versionId, headSha]);
  if (state.versionId !== versionId) return { diff: null, loading: versionId !== null, error: null };
  return { diff: state.diff, loading: state.loading, error: state.error };
}
