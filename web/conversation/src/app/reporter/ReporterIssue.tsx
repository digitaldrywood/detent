import { IssueThreads } from "../threads/IssueThreads.tsx";
import React from "react";
import { Link, useParams, useRouteContext } from "@tanstack/react-router";
import { XCircleIcon } from "lucide-react";
import type { NativeIssue, NativeComment } from "../../contracts/work.ts";
import { Input } from "../../components/ui/input.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Button } from "../../components/ui/button.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useMutation, useResource } from "../account/useResource.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { newWorkKey } from "../work/lib/workHttp.ts";
import {
  useAttachmentDraft,
  attachmentInputHandlers,
} from "../work/components/AttachmentEditor.tsx";
import { IssueComposer } from "../work/components/IssueComposer.tsx";
import { Markdown } from "../components/Markdown.tsx";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { ReporterPage } from "./ReporterPage.tsx";
import { REPORTER_GROUPS } from "./MyIssues.tsx";

export function reportSections(body: string, headings: readonly string[]) {
  const sections = new Map<string, string>();
  const remaining: string[] = [];
  let current: string | null = null;
  let fence = "";
  for (const line of body.split(/\r?\n/)) {
    const delimiter = line.match(/^\s*(`{3,}|~{3,})/);
    const inCode = fence !== "";
    if (delimiter) {
      if (!fence) fence = delimiter[1]!;
      else if (
        delimiter[1]![0] === fence[0] &&
        delimiter[1]!.length >= fence.length
      )
        fence = "";
    }
    const heading =
      !inCode && !delimiter ? line.match(/^## (.+)$/)?.[1] : undefined;
    if (heading && headings.includes(heading) && !sections.has(heading)) {
      current = heading;
      sections.set(heading, "");
    } else if (
      !inCode &&
      !delimiter &&
      /^!?\[.*\]\(attachment:[^)]+\)\s*$/.test(line)
    ) {
      remaining.push(line);
    } else if (current === null) {
      remaining.push(line);
    } else {
      sections.set(current, `${sections.get(current)}\n${line}`);
    }
  }
  for (const [heading, text] of sections) sections.set(heading, text.trim());
  return { sections, remaining: remaining.join("\n").trim() };
}

export function ReporterEditForm({
  issue,
  headings,
  onSave,
}: {
  issue: NativeIssue;
  headings: readonly string[];
  onSave: (title: string, body: string) => Promise<void>;
}) {
  const initial = reportSections(issue.body, headings);
  const [title, setTitle] = React.useState(issue.title);
  const [values, setValues] = React.useState(() =>
    headings.map((heading) => initial.sections.get(heading) ?? ""),
  );
  const [attachments, setAttachments] = React.useState(initial.remaining);
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const upload = useAttachmentDraft(issue.project_id, setAttachments);
  const input = React.useRef<HTMLInputElement>(null);
  const reset = () => {
    setTitle(issue.title);
    setValues(headings.map((heading) => initial.sections.get(heading) ?? ""));
    setAttachments(initial.remaining);
    setError(null);
  };
  return (
    <form
      className="space-y-4 rounded-xl border border-border bg-card p-4 sm:p-5"
      onSubmit={(event) => {
        event.preventDefault();
        if (
          pending ||
          upload.uploading ||
          !title.trim() ||
          values.some((value) => !value.trim())
        )
          return;
        setPending(true);
        setError(null);
        const body =
          headings
            .map(
              (heading, index) => `## ${heading}\n\n${values[index]!.trim()}`,
            )
            .join("\n\n") +
          (attachments.trim() ? `\n\n${attachments.trim()}` : "");
        void onSave(title.trim(), body)
          .catch((cause: unknown) =>
            setError(cause instanceof Error ? cause.message : String(cause)),
          )
          .finally(() => setPending(false));
      }}
    >
      <div className="space-y-2">
        <Label htmlFor="report-title">Title</Label>
        <Input
          id="report-title"
          required
          value={title}
          disabled={pending}
          onChange={(event) => setTitle(event.currentTarget.value)}
        />
      </div>
      {headings.map((heading, index) => (
        <div key={heading} className="space-y-2">
          <Label htmlFor={`report-section-${index}`}>{heading}</Label>
          <Textarea
            autoGrow={false}
            id={`report-section-${index}`}
            required
            rows={index === 0 ? 5 : 3}
            value={values[index]}
            disabled={pending}
            onChange={(event) => {
              const next = event.currentTarget.value;
              setValues((current) =>
                current.map((value, position) =>
                  position === index ? next : value,
                ),
              );
            }}
          />
        </div>
      ))}
      <div
        className="space-y-2"
        {...attachmentInputHandlers(upload.addFiles, pending)}
      >
        <p className="text-sm font-medium">Attachments</p>
        {attachments ? (
          <Markdown source={attachments} projectId={issue.project_id} />
        ) : null}
        <input
          ref={input}
          type="file"
          multiple
          hidden
          aria-label="Attachments"
          disabled={pending}
          onChange={(event) => {
            upload.addFiles(Array.from(event.currentTarget.files ?? []));
            event.currentTarget.value = "";
          }}
        />
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending || upload.uploading}
          onClick={() => input.current?.click()}
        >
          {upload.uploading ? "Uploading…" : "Add file"}
        </Button>
      </div>
      {upload.errors.map((message) => (
        <p
          role="alert"
          key={message}
          className="text-sm text-destructive-foreground"
        >
          {message}
        </p>
      ))}
      {error ? (
        <p role="alert" className="text-sm text-destructive-foreground">
          {error}
        </p>
      ) : null}
      <div className="flex justify-end gap-2">
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending || upload.uploading}
          onClick={reset}
        >
          Discard
        </Button>
        <Button
          type="submit"
          size="sm"
          disabled={
            pending ||
            upload.uploading ||
            !title.trim() ||
            values.some((value) => !value.trim())
          }
        >
          Save changes
        </Button>
      </div>
    </form>
  );
}

export function ReporterIssue() {
  const params = useParams({ strict: false }) as { workItemId?: string };
  const context = useRouteContext({ strict: false }) as {
    resolvedWorkItemId?: string;
  };
  const id = context.resolvedWorkItemId ?? params.workItemId ?? "";
  const http = useWorkHttp();
  const api = useAccountApi();
  const account = useAccountBootstrap()!;
  const resource = useResource(async () => {
    const issue = await http.getWorkItemById(id);
    const [comments, templates] = await Promise.all([
      (async () => {
        const result: NativeComment[] = [];
        let cursor: string | undefined;
        do {
          const page = await http.listComments({
            projectId: issue.project_id,
            itemId: id,
            cursor,
            limit: 200,
          });
          result.push(...page.items);
          cursor = page.next_cursor;
        } while (cursor);
        return result;
      })(),
      issue.reporter_status === "submitted"
        ? api.reporterTemplates(issue.project_id)
        : Promise.resolve(null),
    ]);
    return { issue, comments, templates };
  }, [http, api, id]);
  const cancel = useMutation(async () => {
    const issue = resource.value!.issue;
    await http.transition({
      projectId: issue.project_id,
      itemId: id,
      expectedRevision: issue.revision,
      key: newWorkKey("cancel"),
      state: "Cancelled",
    });
    await resource.refresh();
  });
  const data = resource.value;
  const issue = data?.issue;
  const group = REPORTER_GROUPS.find(
    (candidate) => candidate.status === issue?.reporter_status,
  );
  const project = account.projects.find(
    (candidate) => candidate.id === issue?.project_id,
  );
  return (
    <ReporterPage
      title={issue ? `#${issue.number}` : "Issue"}
      parent={<Link to="/my-issues">My issues</Link>}
      wide
    >
      {resource.error ? (
        <p role="alert">{resource.error.message}</p>
      ) : !data || !issue ? (
        <p role="status">Loading issue…</p>
      ) : (
        <div className="grid min-w-0 gap-6 lg:grid-cols-[minmax(0,1fr)_15rem]">
          <div className="min-w-0 space-y-5">
            <p className="text-sm text-muted-foreground">
              {group?.title} ·{" "}
              {issue.reporter_status === "submitted"
                ? "waiting for the team to review"
                : group?.description}
            </p>
            {issue.reporter_status === "submitted" && data.templates ? (
              <ReporterEditForm
                key={issue.revision}
                issue={issue}
                headings={
                  data.templates.reporter_templates[issue.type ?? "bug"]
                }
                onSave={async (title, body) => {
                  await http.patchWorkItem({
                    projectId: issue.project_id,
                    itemId: id,
                    expectedRevision: issue.revision,
                    key: newWorkKey("edit-report"),
                    title,
                    body,
                  });
                  await resource.refresh();
                }}
              />
            ) : (
              <div className="space-y-4 rounded-xl border border-border bg-card p-4">
                <h1 className="text-xl font-semibold">{issue.title}</h1>
                <Markdown projectId={issue.project_id} source={issue.body} />
              </div>
            )}
            <IssueThreads issueId={data.issue.work_item_id} />
            <section className="space-y-3">
              <h2 className="text-sm font-semibold">Comments</h2>
              {data.comments.map((comment) => (
                <article
                  key={comment.comment_id}
                  className="space-y-2 rounded-xl border border-border bg-card p-4"
                >
                  <p className="text-xs text-muted-foreground">
                    {comment.actor.display_name ?? comment.actor.kind} ·{" "}
                    {formatRelativeTimeLabel(comment.created_at)}
                  </p>
                  <Markdown
                    source={comment.body}
                    projectId={issue.project_id}
                  />
                </article>
              ))}
              <IssueComposer
                variant="simple"
                projectId={issue.project_id}
                canWrite={project?.can_write ?? false}
                onComment={async (body) => {
                  await http.createComment({
                    projectId: issue.project_id,
                    itemId: id,
                    key: newWorkKey("comment"),
                    body,
                  });
                  await resource.refresh();
                }}
              />
            </section>
          </div>
          <aside className="space-y-4">
            <dl className="divide-y divide-border rounded-xl border border-border bg-card px-4 text-sm">
              {[
                ["Project", project?.name ?? issue.project_id],
                ["Type", issue.type === "bug" ? "Bug" : "Change request"],
                ["Filed by", "You"],
                ["Filed", new Date(issue.created_at).toLocaleString()],
              ].map(([label, value]) => (
                <div key={label} className="flex justify-between gap-3 py-3">
                  <dt className="text-muted-foreground">{label}</dt>
                  <dd className="break-words text-right">{value}</dd>
                </div>
              ))}
            </dl>
            {issue.reporter_status !== "cancelled" ? (
              <>
                <Button
                  className="w-full"
                  size="sm"
                  variant="destructive-outline"
                  disabled={cancel.pending}
                  onClick={() => void cancel.call()}
                >
                  <XCircleIcon aria-hidden />
                  Cancel this issue
                </Button>
                <p className="text-xs leading-relaxed text-muted-foreground">
                  The team stops working on it. It moves to Cancelled and keeps
                  its history.
                </p>
              </>
            ) : null}
            {cancel.error ? (
              <p role="alert" className="text-sm text-destructive-foreground">
                {cancel.error.message}
              </p>
            ) : null}
          </aside>
        </div>
      )}
    </ReporterPage>
  );
}
