import { FileDiff } from "@pierre/diffs/react";
import { Columns2, ExternalLinkIcon, Rows2 } from "lucide-react";
import React from "react";

import type {
  AttemptDiff,
  AttemptDiffFile,
  ChangeDetail,
  CollaborationEvent,
  NativeAttempt,
} from "../../../contracts/work.ts";
import type { TurnId } from "../../../contracts/ui.ts";
import type { TurnDiffFileChange } from "../../../types.ts";
import { DiffPanelShell } from "../../../components/DiffPanelShell.tsx";
import { DiffWorkerPoolProvider } from "../../../components/DiffWorkerPoolProvider.tsx";
import { ChangedFilesTree } from "../../../components/chat/ChangedFilesTree.tsx";
import { DiffStatLabel, hasNonZeroStat } from "../../../components/chat/DiffStatLabel.tsx";
import { RenderErrorBoundary } from "../../../components/RenderErrorBoundary.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { Toggle } from "../../../components/ui/toggle.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import {
  DIFF_SURFACE_THEME_UNSAFE_CSS,
  getRenderablePatch,
  resolveDiffThemeName,
  resolveFileDiffPath,
} from "../../../lib/diffRendering.ts";
import { PREFERRED_HIGHLIGHTER } from "../../../lib/syntaxHighlighting.ts";
import { ageLabel } from "../../work/lib/format.ts";
import { Pill, type PillTone } from "../../work/components/IssueCard.tsx";
import type { DiffSource } from "../../adapters/surfaces.ts";

function short(sha: string | undefined | null): string {
  return sha === undefined || sha === null ? "" : sha.slice(0, 7);
}

/** A label/value row, the artifact's `.blocked .row` grid at panel width. */
function Row({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <div className="grid grid-cols-[7.5rem_1fr] gap-x-3 gap-y-1 py-1 text-xs">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 break-words">{children}</span>
    </div>
  );
}

function Mono({ children }: { children: React.ReactNode }): React.ReactElement {
  return <span className="font-mono text-[11px]">{children}</span>;
}

function summaryTone(status: string): PillTone {
  switch (status) {
    case "ready":
    case "merged":
      return "ok";
    case "blocked":
      return "err";
    case "pending":
      return "warn";
    default:
      return "mute";
  }
}

function EmptyState({
  children,
  testId,
}: {
  children: React.ReactNode;
  testId: string;
}): React.ReactElement {
  return (
    <div
      className="flex items-center justify-center px-3 py-6 text-center text-muted-foreground/70 text-xs"
      data-testid={testId}
    >
      <p>{children}</p>
    </div>
  );
}

// --- The stored attempt diff -------------------------------------------------

/**
 * Why a file the hub listed carries no patch, or null when it carries one.
 *
 * All three are §18.5's own distinctions and all three keep their counts, so
 * the file is always listed — a reader is told that `.env.local` changed and
 * that its contents are not shown, rather than being shown a file list with a
 * hole in it.
 */
function withheldReason(file: AttemptDiffFile): string | null {
  if (file.denied) {
    return "This path is on the workspace’s denylist, so its contents are not stored.";
  }
  if (file.binary) return "Binary file. The counts are stored; the contents are not.";
  if (file.patch.length === 0 && file.truncated) {
    return "This patch was larger than the stored diff allows, so only its counts were kept.";
  }
  if (file.patch.length === 0) return "The producer stored no patch for this file.";
  return null;
}

function treeKind(status: AttemptDiffFile["status"]): string {
  switch (status) {
    case "added":
      return "added";
    case "deleted":
      return "deleted";
    case "renamed":
      return "renamed";
    default:
      return "modified";
  }
}

export function toTurnDiffFiles(
  files: readonly AttemptDiffFile[],
): ReadonlyArray<TurnDiffFileChange> {
  return files.map((file) => ({
    path: file.path,
    kind: treeKind(file.status),
    additions: file.additions,
    deletions: file.deletions,
  }));
}

function DiffFileSection({
  file,
  diffId,
  diffStyle,
  theme,
  onMount,
}: {
  file: AttemptDiffFile;
  diffId: string;
  diffStyle: "unified" | "split";
  theme: "light" | "dark";
  onMount: (path: string, node: HTMLElement | null) => void;
}): React.ReactElement {
  const withheld = withheldReason(file);
  const renderable =
    withheld === null ? getRenderablePatch(file.patch, `attempt-diff:${diffId}:${file.path}`) : null;
  const register = React.useCallback(
    (node: HTMLElement | null) => onMount(file.path, node),
    [file.path, onMount],
  );

  return (
    <section ref={register} data-testid="diff-file-section" data-diff-path={file.path}>
      {withheld === null && renderable?.kind === "files" ? (
        <DiffWorkerPoolProvider>
          {renderable.files.map((fileDiff) => (
            <FileDiff
              key={resolveFileDiffPath(fileDiff)}
              fileDiff={fileDiff}
              options={{
                collapsed: false,
                diffStyle,
                stickyHeader: true,
                theme: resolveDiffThemeName(theme),
                themeType: theme,
                preferredHighlighter: PREFERRED_HIGHLIGHTER,
                unsafeCSS: DIFF_SURFACE_THEME_UNSAFE_CSS,
              }}
            />
          ))}
        </DiffWorkerPoolProvider>
      ) : (
        <div className="border-border/60 border-b">
          <div className="flex h-8 items-center gap-2 px-3">
            <span className="min-w-0 truncate font-mono text-[11px]">
              {file.old_path === undefined || file.old_path === "" || file.old_path === file.path
                ? file.path
                : `${file.old_path} → ${file.path}`}
            </span>
            <span className="ml-auto shrink-0 font-mono text-[10px] tabular-nums">
              <DiffStatLabel additions={file.additions} deletions={file.deletions} />
            </span>
          </div>
          <EmptyState testId={file.denied ? "diff-file-denied" : "diff-file-withheld"}>
            {withheld ??
              (renderable?.kind === "raw" ? renderable.reason : null) ??
              "This patch could not be parsed."}
          </EmptyState>
        </div>
      )}
    </section>
  );
}

function AttemptDiffBody({
  diff,
  theme,
}: {
  diff: AttemptDiff;
  theme: "light" | "dark";
}): React.ReactElement {
  const [expanded, setExpanded] = React.useState(true);
  const [diffStyle, setDiffStyle] = React.useState<"unified" | "split">("unified");
  const sections = React.useRef(new Map<string, HTMLElement>());
  const registerSection = React.useCallback((path: string, node: HTMLElement | null) => {
    if (node === null) sections.current.delete(path);
    else sections.current.set(path, node);
  }, []);

  const turnId = diff.attempt_id as TurnId;
  const openFile = React.useCallback((_turn: TurnId, path?: string) => {
    if (path === undefined) return;
    sections.current.get(path)?.scrollIntoView({ block: "start", behavior: "smooth" });
  }, []);

  const treeFiles = React.useMemo(() => toTurnDiffFiles(diff.files), [diff.files]);
  const hasDirectories = treeFiles.some((file) => /[/\\]/.test(file.path));

  return (
    <div className="min-h-0 flex-1 overflow-auto" data-testid="diff-attempt">
      <div className="border-border/60 border-b" data-testid="diff-files">
        <div className="flex items-center justify-between gap-2 px-3 pt-2">
          <span className="text-muted-foreground text-xs">
            {diff.files.length} changed file{diff.files.length === 1 ? "" : "s"}
          </span>
          {hasDirectories ? (
            <Button
              size="xs"
              variant="ghost-muted"
              aria-label={expanded ? "Collapse all folders" : "Expand all folders"}
              onClick={() => setExpanded((value) => !value)}
            >
              {expanded ? "Collapse all" : "Expand all"}
            </Button>
          ) : null}
        </div>
        <ChangedFilesTree
          key={`${turnId}:${expanded}`}
          turnId={turnId}
          files={treeFiles}
          allDirectoriesExpanded={expanded}
          resolvedTheme={theme}
          onOpenTurnDiff={openFile}
        />
      </div>
      <RenderErrorBoundary
        fallback={
          <EmptyState testId="diff-render-error">
            This diff could not be drawn. Reload the surface to try again.
          </EmptyState>
        }
      >
        {diff.files.map((file) => (
          <DiffFileSection
            key={file.path}
            file={file}
            diffId={diff.id}
            diffStyle={diffStyle}
            theme={theme}
            onMount={registerSection}
          />
        ))}
      </RenderErrorBoundary>
      {diff.truncated ? (
        <EmptyState testId="diff-truncated">
          Some patches in this diff were larger than the stored limit and were cut. The counts above
          are complete.
        </EmptyState>
      ) : null}
      <DiffStyleToggle value={diffStyle} onChange={setDiffStyle} />
    </div>
  );
}

/**
 * The unified/split control. `@pierre/diffs` renders both, so this is their
 * option and not a Detent invention; it lives at the foot of the scroller
 * because the 40px sub-header is already the round's.
 */
function DiffStyleToggle({
  value,
  onChange,
}: {
  value: "unified" | "split";
  onChange: (next: "unified" | "split") => void;
}): React.ReactElement {
  const split = value === "split";
  return (
    <div className="flex justify-end px-3 py-2">
      <Tooltip>
        <TooltipTrigger
          render={
            <Toggle
              className="shrink-0"
              pressed={split}
              onPressedChange={(next: boolean) => onChange(next ? "split" : "unified")}
              aria-label={split ? "Show a unified diff" : "Show a split diff"}
              variant="ghost"
              size="sm"
            >
              {split ? <Rows2 className="size-3.5" /> : <Columns2 className="size-3.5" />}
            </Toggle>
          }
        />
        <TooltipPopup>{split ? "Show a unified diff" : "Show a split diff"}</TooltipPopup>
      </Tooltip>
    </div>
  );
}

export interface DiffSurfaceProps {
  readonly change: ChangeDetail | null;
  readonly source: DiffSource;
  readonly attempts: readonly NativeAttempt[];
  readonly history: readonly CollaborationEvent[];
  readonly now: number;
  readonly theme?: "light" | "dark";
  readonly onReload: () => void;
}

export function DiffSurface({
  change,
  source,
  attempts,
  history,
  now,
  theme = "light",
  onReload,
}: DiffSurfaceProps): React.ReactElement {
  const current =
    change === null
      ? null
      : (change.versions.find(
          (version) => version.version_id === change.change.current_version_id,
        ) ??
        change.versions.at(-1) ??
        null);
  const attempt = attempts.at(-1) ?? null;
  const checkpoint = attempt?.checkpoint ?? attempt?.handoff ?? null;
  const diff = source.kind === "attempt" ? source.diff : null;
  const stat = React.useMemo(
    () =>
      (diff?.files ?? []).reduce(
        (total, file) => ({
          additions: total.additions + file.additions,
          deletions: total.deletions + file.deletions,
        }),
        { additions: 0, deletions: 0 },
      ),
    [diff],
  );

  const header = (
    <>
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <span
          className="inline-flex h-6 max-w-full items-center gap-1 rounded-md bg-accent px-2 font-medium text-accent-foreground text-xs"
          data-testid="diff-round"
        >
          <span className="truncate">
            {current === null && diff !== null
              ? `head ${short(diff.head_sha) || "—"}`
              : `Round ${current?.number ?? "—"} · head ${short(diff?.head_sha ?? current?.head_sha) || "—"}`}
          </span>
        </span>
        {diff === null || !hasNonZeroStat(stat) ? null : (
          <span className="shrink-0 text-xs" data-testid="diff-stat">
            <DiffStatLabel additions={stat.additions} deletions={stat.deletions} layout="inline" />
          </span>
        )}
      </div>
      {change === null ? null : (
        <Pill tone={summaryTone(change.summary.status)} data-testid="diff-status">
          {change.summary.status === "" ? "unknown" : change.summary.status}
        </Pill>
      )}
      <Button size="xs" variant="ghost" aria-label="Reload the change" onClick={onReload}>
        Reload
      </Button>
    </>
  );

  return (
    <DiffPanelShell mode="sheet" header={header}>
      {diff === null ? (
        <div className="min-h-0 flex-1 overflow-auto" data-testid="diff-surface">
          <EmptyState testId="diff-empty">
            {source.kind === "unavailable"
              ? source.reason
              : // §18.5's other source. A change version's code is one opaque
                // artifact with no file names in it, so what follows is the
                // round this issue published rather than its patch — and the
                // reason there is no patch is that no attempt posted one, not
                // that the hub cannot serve one.
                "No attempt has posted a diff for this issue yet. The round it published is below."}
          </EmptyState>

          {current === null ? null : (
            <section
              className="m-3 rounded-lg border border-border bg-card p-3"
              data-testid="diff-round-card"
            >
              <h3 className="mb-1.5 font-medium text-sm">Round {current.number}</h3>
              <Row label="repository">
                <Mono>{current.repository}</Mono>
              </Row>
              <Row label="head">
                <Mono>{current.head_sha}</Mono>
              </Row>
              <Row label="base">
                <Mono>{current.base_sha}</Mono>
              </Row>
              <Row label="merge base">
                <Mono>{current.merge_base_sha}</Mono>
              </Row>
              <Row label="code artifact">
                <Mono>{current.code.uri}</Mono>
              </Row>
              <Row label="digest">
                <Mono>{current.code.sha256}</Mono>
              </Row>
              <Row label="availability">{current.code.availability}</Row>
              {current.external === undefined ? null : (
                <Row label="pull request">
                  <a
                    className="inline-flex items-center gap-1 text-primary underline-offset-4 hover:underline"
                    href={current.external.url}
                    rel="noreferrer noopener"
                    target="_blank"
                    data-testid="diff-pr-link"
                  >
                    #{current.external.id}
                    <ExternalLinkIcon className="size-3" />
                  </a>
                </Row>
              )}
            </section>
          )}

          <DiffContext
            change={change}
            checkpoint={checkpoint}
            attempt={attempt}
            history={history}
            now={now}
          />
        </div>
      ) : (
        <AttemptDiffBody diff={diff} theme={theme} />
      )}
    </DiffPanelShell>
  );
}

/**
 * The worker state, the verdict, the checks, the reviews and the activity.
 * They describe the round rather than the patch, so they sit under the change
 * version card and are not drawn over a stored diff.
 */
function DiffContext({
  change,
  checkpoint,
  attempt,
  history,
  now,
}: {
  change: ChangeDetail | null;
  checkpoint: NonNullable<NativeAttempt["checkpoint"]> | null;
  attempt: NativeAttempt | null;
  history: readonly CollaborationEvent[];
  now: number;
}): React.ReactElement {
  return (
    <>
      {checkpoint === null ? null : (
        <section
          className="m-3 rounded-lg border border-border bg-card p-3"
          data-testid="diff-state-card"
        >
          <h3 className="mb-1.5 font-medium text-sm">Worker state</h3>
          <Row label="resume">{checkpoint.resume}</Row>
          <Row label="availability">{checkpoint.availability}</Row>
          <Row label="storage">{checkpoint.storage}</Row>
          <Row label="worktree">{checkpoint.worktree_state}</Row>
          <Row label="external effect">{checkpoint.external_effect}</Row>
          <Row label="effect state">{checkpoint.effect_state}</Row>
          {checkpoint.head_sha === undefined ? null : (
            <Row label="head">
              <Mono>{checkpoint.head_sha}</Mono>
            </Row>
          )}
          {attempt === null ? null : (
            <>
              <Row label="attempt">
                <Mono>{attempt.attempt_id}</Mono>
              </Row>
              <Row label="runner">
                {[attempt.identity?.model, attempt.identity?.backend, attempt.runner_id]
                  .filter((part) => part !== undefined && part !== null)
                  .join(" · ") || "—"}
              </Row>
              <Row label="status">{attempt.status}</Row>
            </>
          )}
        </section>
      )}

      {change === null ? null : (
        <section
          className="m-3 rounded-lg border border-border bg-card p-3"
          data-testid="diff-verdict-card"
        >
          <h3 className="mb-1.5 font-medium text-sm">Verdict</h3>
          <Row label="status">{change.summary.status || "—"}</Row>
          <Row label="native review">{change.summary.native_review || "—"}</Row>
          <Row label="external review">{change.summary.external_review || "—"}</Row>
          <Row label="checks">{change.summary.checks || "—"}</Row>
          {change.summary.messages.length === 0 ? null : (
            <ul className="mt-2 list-disc space-y-1 ps-4 text-muted-foreground text-xs">
              {change.summary.messages.map((message) => (
                <li key={message}>{message}</li>
              ))}
            </ul>
          )}
        </section>
      )}

      {change === null || change.checks.length === 0 ? null : (
        <section className="m-3">
          <h3 className="mb-1 font-medium text-sm">Checks</h3>
          <ul className="space-y-1">
            {change.checks.map((check) => (
              <li
                key={check.check_run_id}
                className="flex items-center gap-2 text-xs"
                data-testid="diff-check"
              >
                <Pill tone={check.conclusion === "success" ? "ok" : "err"}>{check.conclusion}</Pill>
                <Mono>{check.workflow_id}</Mono>
                <span className="ml-auto text-muted-foreground">
                  {ageLabel(check.completed_at, now)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      {change === null || change.reviews.length === 0 ? null : (
        <section className="m-3">
          <h3 className="mb-1 font-medium text-sm">Reviews</h3>
          <ul className="space-y-2">
            {change.reviews.map((review) => (
              <li key={review.review_id} className="rounded-lg border border-border p-2.5">
                <p className="flex items-center gap-2 text-[11px] text-muted-foreground">
                  <Pill tone={review.decision === "approved" ? "ok" : "warn"}>
                    {review.decision}
                  </Pill>
                  {review.actor.principal_id} · {ageLabel(review.created_at, now)}
                </p>
                {review.body.length === 0 ? null : <p className="mt-1 text-xs">{review.body}</p>}
              </li>
            ))}
          </ul>
        </section>
      )}

      {history.length === 0 ? null : (
        <section className="m-3">
          <h3 className="mb-1 font-medium text-sm">Activity</h3>
          <ol className="space-y-2">
            {[...history]
              .reverse()
              .slice(0, 20)
              .map((event) => (
                <li
                  key={event.event_id}
                  className="flex items-baseline gap-2 text-xs"
                  data-testid="diff-activity-row"
                >
                  <Mono>{event.type}</Mono>
                  <span className="text-muted-foreground">{event.actor.principal_id}</span>
                  <span className="ml-auto shrink-0 text-muted-foreground tabular-nums">
                    {ageLabel(event.recorded_at, now)}
                  </span>
                </li>
              ))}
          </ol>
        </section>
      )}
    </>
  );
}
