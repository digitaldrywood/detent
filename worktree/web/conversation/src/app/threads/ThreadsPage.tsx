import React, { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import {
  ArrowLeftIcon,
  ArrowUpIcon,
  InfoIcon,
  MessageSquareIcon,
} from "lucide-react";
import ChatMarkdown from "../../components/ChatMarkdown.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Avatar } from "../../components/ui/avatar.tsx";
import { Alert, AlertDescription } from "../../components/ui/alert.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { Toggle } from "../../components/ui/toggle.tsx";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectPopup,
  SelectItem,
} from "../../components/ui/select.tsx";
import { useAccountBootstrap, useAccountApi } from "../account/context.ts";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { newCommandKey } from "../client.ts";
import { usePageTitle } from "../pageTitle.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import {
  THREADS_CHANGED,
  useThreadsApi,
  type TeamThread,
  type TeamPost,
  type PostKind,
  type ThreadFilters,
  type ThreadsApi,
  type ThreadRead,
  type NewThread,
} from "./api.ts";

export { ThreadUnreadBadge } from "./ThreadBadge.tsx";

function ScopeLabel({
  thread,
  projects,
}: {
  thread: TeamThread;
  projects: readonly { id: string; name: string }[];
}) {
  const project =
    projects.find((p) => p.id === thread.project_id)?.name ?? thread.project_id;
  return (
    <>
      {thread.scope === "general"
        ? "General"
        : `${thread.scope === "issue" ? "Issue" : "Project"} · ${project}${thread.issue_number ? ` #${thread.issue_number}` : ""}`}
    </>
  );
}

export function ThreadFiltersBar({
  value,
  onChange,
}: {
  value: ThreadFilters;
  onChange: (next: ThreadFilters) => void;
}) {
  return (
    <div className="flex flex-col gap-2 border-b border-border p-3 sm:p-4">
      <Input
        aria-label="Search threads"
        placeholder="Search threads"
        value={value.search ?? ""}
        onChange={(e) => onChange({ ...value, search: e.target.value })}
      />
      <div
        className="flex flex-wrap gap-1"
        role="group"
        aria-label="Thread status"
      >
        {(["open", "resolved"] as const).map((status) => (
          <Toggle
            key={status}
            variant="pill-outline"
            pressed={value.status === status}
            onPressedChange={() => onChange({ ...value, status })}
          >
            {status === "open" ? "Open" : "Resolved"}
          </Toggle>
        ))}
      </div>
      <div
        className="flex flex-wrap gap-1"
        role="group"
        aria-label="Thread scope"
      >
        {["", "general", "project", "issue"].map((scope) => (
          <Toggle
            key={scope}
            variant="pill-outline"
            pressed={(value.scope ?? "") === scope}
            onPressedChange={() => onChange({ ...value, scope })}
          >
            {scope === ""
              ? "Scope: All"
              : scope.charAt(0).toUpperCase() + scope.slice(1)}
          </Toggle>
        ))}
      </div>
    </div>
  );
}

export function ThreadAgentBadge({
  post,
}: {
  post: Pick<TeamPost, "author_name" | "acting_for" | "client_name">;
}) {
  return (
    <>
      <span className="min-w-0 break-words font-semibold">
        {post.author_name}
        {post.acting_for ? "’s agent" : ""}
      </span>
      {post.acting_for ? (
        <Badge variant="secondary" className="min-w-0 max-w-full">
          <span className="truncate">
            {post.client_name} · for {post.author_name}
          </span>
        </Badge>
      ) : null}
    </>
  );
}

function KindPicker({
  value,
  onChange,
  disabled = false,
  compact = false,
}: {
  value: PostKind;
  onChange: (kind: PostKind) => void;
  disabled?: boolean;
  compact?: boolean;
}) {
  const items = ["note", "finding", "question", "decision"].map((value) => ({
    value,
    label: value.charAt(0).toUpperCase() + value.slice(1),
  }));
  return (
    <Select
      items={items}
      value={value}
      disabled={disabled}
      onValueChange={(v) => {
        if (v) onChange(v as PostKind);
      }}
    >
      <SelectTrigger
        aria-label="Post type"
        size={compact ? "touch" : "default"}
        variant={compact ? "ghost" : "default"}
      >
        <SelectValue />
      </SelectTrigger>
      <SelectPopup>
        {items.map((item) => (
          <SelectItem key={item.value} value={item.value}>
            {item.label}
          </SelectItem>
        ))}
      </SelectPopup>
    </Select>
  );
}

export function ThreadReplyComposer({
  onPost,
  disabled = false,
  name,
}: {
  onPost: (kind: PostKind, body: string, key: string) => Promise<void>;
  disabled?: boolean;
  name?: string;
}) {
  const mobile = useMediaQuery("max-sm");
  const [body, setBody] = useState("");
  const [kind, setKind] = useState<PostKind>("note");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [key, setKey] = useState(newCommandKey);
  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (pending || disabled || body.trim() === "") return;
    setPending(true);
    setError(null);
    try {
      await onPost(kind, body, key);
      setBody("");
      setKey(newCommandKey());
    } catch (error) {
      setError(
        error instanceof Error
          ? error.message
          : "The reply could not be posted.",
      );
    } finally {
      setPending(false);
    }
  }
  return (
    <form
      aria-label="Reply to thread"
      onSubmit={(event) => void submit(event)}
      className={`sticky bottom-0 flex shrink-0 gap-2 border-t border-border bg-background p-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:p-4 ${mobile ? "flex-wrap items-center" : "flex-col"}`}
    >
      {mobile ? (
        <>
          <div className="min-w-0 flex-1">
            <Input
              id="thread-reply"
              aria-label="Reply"
              placeholder="Reply"
              size="touch"
              value={body}
              maxLength={8192}
              disabled={pending || disabled}
              onChange={(e) => {
                setBody(e.target.value);
                setKey(newCommandKey());
              }}
            />
          </div>
          <KindPicker
            value={kind}
            compact
            onChange={(v) => {
              setKind(v);
              setKey(newCommandKey());
            }}
            disabled={pending || disabled}
          />
          <Button
            type="submit"
            size="touch"
            aria-label={pending ? "Posting…" : "Post"}
            disabled={pending || disabled || !body.trim()}
          >
            <ArrowUpIcon />
          </Button>
        </>
      ) : (
        <>
          {name ? <Label htmlFor="thread-reply">Reply as {name}</Label> : null}
          <Textarea
            id="thread-reply"
            aria-label="Reply"
            placeholder="Add a note, or paste something your agent found"
            value={body}
            maxLength={8192}
            disabled={pending || disabled}
            onChange={(e) => {
              setBody(e.target.value);
              setKey(newCommandKey());
            }}
          />
          <div className="flex items-center justify-between gap-3">
            <div className="flex items-center gap-2">
              <span className="text-sm text-muted-foreground">Type</span>
              <KindPicker
                value={kind}
                onChange={(v) => {
                  setKind(v);
                  setKey(newCommandKey());
                }}
                disabled={pending || disabled}
              />
            </div>
            <Button
              type="submit"
              size="touch"
              disabled={pending || disabled || !body.trim()}
            >
              {pending ? "Posting…" : "Post"}
            </Button>
          </div>
        </>
      )}
      {error ? (
        <p role="alert" className="w-full text-sm text-destructive-foreground">
          {error}
        </p>
      ) : null}
    </form>
  );
}

function ThreadReferences({ refs }: { refs: TeamThread["references"] }) {
  const api = useAccountApi();
  if (refs.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-2">
      {refs.map((ref) => {
        const label =
          ref.label ||
          `${ref.kind === "issue" ? "Issue" : ref.kind === "change" ? "Change" : "Attachment"} ${ref.id}`;
        const key = `${ref.kind}:${ref.id}`;
        if (ref.kind === "attachment" && ref.project_id) {
          return (
            <Badge
              key={key}
              variant="secondary"
              size="lg"
              render={<a href={api.attachmentUrl(ref.project_id, ref.id)} />}
            >
              {label}
            </Badge>
          );
        }
        if (ref.kind === "issue") {
          return (
            <Badge
              key={key}
              variant="secondary"
              size="lg"
              render={
                <Link
                  to="/work/i/$workItemId"
                  params={{ workItemId: ref.id }}
                />
              }
            >
              {label}
            </Badge>
          );
        }
        if (ref.kind === "change" && ref.issue_id) {
          return (
            <Badge
              key={key}
              variant="secondary"
              size="lg"
              render={
                <Link
                  to="/work/i/$workItemId/changes/$changeId"
                  params={{ workItemId: ref.issue_id, changeId: ref.id }}
                />
              }
            >
              {label}
            </Badge>
          );
        }
        return (
          <Badge key={key} variant="secondary" size="lg">
            {label}
          </Badge>
        );
      })}
    </div>
  );
}

function ThreadView({
  api,
  id,
  projects,
  name,
  canWrite,
  onBack,
}: {
  api: ThreadsApi;
  id: string;
  projects: readonly { id: string; name: string }[];
  name?: string;
  canWrite: boolean;
  onBack: () => void;
}) {
  const [read, setRead] = useState<ThreadRead | null>(null);
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (read) end.current?.scrollIntoView({ block: "end" });
  }, [read?.cursor]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    setRead(null);
    setError(null);
    void api
      .get(id, "0", controller.signal)
      .then(setRead)
      .catch((e) => {
        if (!controller.signal.aborted) setError(e.message);
      });
    return () => controller.abort();
  }, [api, id]);
  async function more() {
    if (!read || busy) return;
    setBusy(true);
    setError(null);
    try {
      const next = await api.get(id, read.cursor);
      setRead({ ...next, posts: [...read.posts, ...next.posts] });
    } catch (e) {
      setError(e instanceof Error ? e.message : "Posts could not be loaded.");
    } finally {
      setBusy(false);
    }
  }
  async function resolve() {
    if (!read || busy) return;
    setBusy(true);
    setError(null);
    try {
      const thread = await api.resolve(
        id,
        read.thread.status === "open" ? "resolved" : "open",
      );
      setRead({ ...read, thread });
    } catch (e) {
      setError(
        e instanceof Error ? e.message : "Thread status could not be changed.",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <section
      className="flex h-full min-h-0 min-w-0 flex-1 flex-col"
      aria-label="Thread"
    >
      <WorkspacePageHeader className="border-b border-border">
        <Button
          variant="ghost"
          size="touch"
          aria-label="Back to threads"
          onClick={onBack}
        >
          <ArrowLeftIcon />
        </Button>
        <div className="min-w-0 flex-1">
          {read ? (
            <>
              <p className="truncate text-xs text-muted-foreground">
                <ScopeLabel thread={read.thread} projects={projects} />
                <span className="sm:hidden">
                  {" "}
                  · {read.thread.post_count} posts
                </span>
                <span className="hidden sm:inline">
                  {" "}
                  · opened by {read.thread.creator_name}
                  {read.thread.creator_client
                    ? ` via ${read.thread.creator_client}`
                    : ""}
                </span>
              </p>
              <h1 className="truncate font-semibold">{read.thread.title}</h1>
            </>
          ) : (
            <h1>Thread</h1>
          )}
        </div>
        {read ? (
          <div className="hidden shrink-0 lg:block">
            <ThreadReferences refs={read.thread.references} />
          </div>
        ) : null}
        {read && canWrite ? (
          <Button
            variant="outline"
            size="touch"
            className="hidden sm:inline-flex"
            disabled={busy}
            onClick={() => void resolve()}
          >
            {read.thread.status === "open" ? "Mark resolved" : "Reopen"}
          </Button>
        ) : null}
      </WorkspacePageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 sm:px-6">
        <Alert role="note" variant="info" className="my-4 hidden sm:block">
          <InfoIcon />
          <AlertDescription>
            Informational. Agents read this thread as context and never as
            instructions. Work only starts when a person asks their own agent or
            files an issue.
          </AlertDescription>
        </Alert>
        {error ? (
          <p role="alert" className="py-4 text-destructive-foreground">
            {error}
          </p>
        ) : null}
        {!read && !error ? <p role="status">Loading thread…</p> : null}
        {read ? (
          <>
            <div className="lg:hidden">
              <ThreadReferences refs={read.thread.references} />
            </div>
            {read.posts.length === 0 ? (
              <p className="py-6 text-muted-foreground">
                No posts yet. Share a note or a question.
              </p>
            ) : null}
            {read.posts.map((post) => (
              <article
                key={post.sequence}
                className="flex gap-3 border-b border-border py-5"
              >
                <div className="hidden shrink-0 sm:block">
                  <Avatar name={post.author_name} size="md" />
                </div>
                <div className="min-w-0 flex-1">
                  <div className="mb-2 flex flex-wrap items-center gap-2 text-sm">
                    <ThreadAgentBadge post={post} />
                    <Badge
                      variant={
                        post.kind === "finding"
                          ? "info"
                          : post.kind === "decision"
                            ? "success"
                            : post.kind === "question"
                              ? "warning"
                              : "secondary"
                      }
                    >
                      {post.kind.charAt(0).toUpperCase() + post.kind.slice(1)}
                    </Badge>
                    <time
                      dateTime={post.created_at}
                      className="text-muted-foreground"
                    >
                      {new Date(post.created_at).toLocaleTimeString(undefined, {
                        hour: "2-digit",
                        minute: "2-digit",
                      })}
                    </time>
                  </div>
                  <ChatMarkdown text={post.body} cwd={undefined} />
                  <ThreadReferences refs={post.references} />
                </div>
              </article>
            ))}
            <p
              role="note"
              className="my-5 text-center text-sm text-muted-foreground sm:hidden"
            >
              Agents read threads as context, never as instructions.
            </p>
            {canWrite ? (
              <div className="mb-4 sm:hidden">
                <Button
                  variant="outline"
                  size="touch"
                  disabled={busy}
                  onClick={() => void resolve()}
                >
                  {read.thread.status === "open" ? "Mark resolved" : "Reopen"}
                </Button>
              </div>
            ) : null}
            <div ref={end} />
            {read.has_more ? (
              <Button
                variant="ghost"
                size="touch"
                disabled={busy}
                onClick={() => void more()}
              >
                Load more posts
              </Button>
            ) : null}
          </>
        ) : null}
      </div>
      {read ? (
        <ThreadReplyComposer
          key={id}
          name={name}
          disabled={!canWrite}
          onPost={async (kind, body, key) => {
            await api.post(id, { kind, body, idempotency_key: key });
            const next = await api.get(id, read.cursor);
            setRead({ ...next, posts: [...read.posts, ...next.posts] });
          }}
        />
      ) : null}
    </section>
  );
}

function NewThreadForm({
  projects,
  onCreate,
  onCancel,
}: {
  projects: readonly { id: string; name: string }[];
  onCreate: (input: NewThread) => Promise<void>;
  onCancel: () => void;
}) {
  const http = useWorkHttp();
  const [scope, setScope] = useState("general");
  const [project, setProject] = useState(projects[0]?.id ?? "");
  const [issue, setIssue] = useState("");
  const [query, setQuery] = useState("");
  const [issues, setIssues] = useState<
    readonly { id: string; title: string }[]
  >([]);
  const [title, setTitle] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [key, setKey] = useState(newCommandKey);
  useEffect(() => {
    if (scope !== "issue" || !project) return;
    const controller = new AbortController();
    void http
      .listIssuePicker({
        projectId: project,
        q: query,
        limit: 25,
        signal: controller.signal,
      })
      .then((page) =>
        setIssues(
          page.items.map((row) => ({
            id: row.work_item_id,
            title: `#${row.number} ${row.title}`,
          })),
        ),
      )
      .catch((e) => {
        if (!controller.signal.aborted) setError(e.message);
      });
    return () => controller.abort();
  }, [http, scope, project, query]);
  return (
    <section className="flex min-w-0 flex-1 flex-col">
      <WorkspacePageHeader className="border-b border-border">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Cancel new thread"
          onClick={onCancel}
        >
          <ArrowLeftIcon />
        </Button>
        <h1 className="font-semibold">New thread</h1>
      </WorkspacePageHeader>
      <form
        className="flex max-w-2xl flex-col gap-5 p-4 sm:p-6"
        aria-label="New thread"
        onSubmit={(event) => {
          event.preventDefault();
          if (pending) return;
          setPending(true);
          setError(null);
          void onCreate({
            scope,
            title,
            idempotency_key: key,
            ...(scope === "project" ? { project_id: project } : {}),
            ...(scope === "issue" ? { issue_id: issue } : {}),
          })
            .catch((e) => setError(e.message))
            .finally(() => setPending(false));
        }}
      >
        <div className="flex flex-col gap-2">
          <Label htmlFor="thread-title">Title</Label>
          <Input
            id="thread-title"
            required
            maxLength={200}
            value={title}
            disabled={pending}
            onChange={(e) => {
              setTitle(e.target.value);
              setKey(newCommandKey());
            }}
          />
        </div>
        <div className="flex flex-col gap-2">
          <Label>Scope</Label>
          <Select
            value={scope}
            disabled={pending}
            items={[
              { value: "general", label: "General" },
              { value: "project", label: "Project" },
              { value: "issue", label: "Issue" },
            ]}
            onValueChange={(value) => {
              if (value) {
                setScope(value);
                setKey(newCommandKey());
              }
            }}
          >
            <SelectTrigger aria-label="Thread scope">
              <SelectValue />
            </SelectTrigger>
            <SelectPopup>
              {["general", "project", "issue"].map((v) => (
                <SelectItem key={v} value={v}>
                  {v.charAt(0).toUpperCase() + v.slice(1)}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        </div>
        {scope !== "general" ? (
          <div className="flex flex-col gap-2">
            <Label>Project</Label>
            <Select
              value={project}
              disabled={pending}
              items={projects.map((p) => ({ value: p.id, label: p.name }))}
              onValueChange={(value) => {
                if (value) {
                  setProject(value);
                  setIssue("");
                  setKey(newCommandKey());
                }
              }}
            >
              <SelectTrigger aria-label="Project">
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {projects.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </div>
        ) : null}
        {scope === "issue" ? (
          <div className="flex flex-col gap-2">
            <Input
              aria-label="Search issues"
              placeholder="Search issues"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <Select
              value={issue}
              disabled={pending}
              items={issues.map((row) => ({ value: row.id, label: row.title }))}
              onValueChange={(value) => {
                if (value) {
                  setIssue(value);
                  setKey(newCommandKey());
                }
              }}
            >
              <SelectTrigger aria-label="Issue">
                <SelectValue placeholder="Choose an issue" />
              </SelectTrigger>
              <SelectPopup>
                {issues.map((row) => (
                  <SelectItem key={row.id} value={row.id}>
                    {row.title}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </div>
        ) : null}
        <p className="text-sm text-muted-foreground">
          General threads are shared with your organization. Project and issue
          threads are shared with people who can access that project.
        </p>
        {error ? (
          <p role="alert" className="text-destructive-foreground">
            {error}
          </p>
        ) : null}
        <div className="flex gap-2">
          <Button
            type="submit"
            size="touch"
            disabled={
              pending ||
              !title.trim() ||
              (scope !== "general" && !project) ||
              (scope === "issue" && !issue)
            }
          >
            {pending ? "Creating…" : "Create thread"}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="touch"
            disabled={pending}
            onClick={onCancel}
          >
            Cancel
          </Button>
        </div>
      </form>
    </section>
  );
}

export function ThreadsRoute() {
  usePageTitle("Threads");
  const api = useThreadsApi();
  const account = useAccountBootstrap();
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { thread?: string };
  const selected = params.thread;
  const go = (path: string) => {
    void navigate({ to: path as never });
  };
  const projects = account?.projects ?? [];
  const canWrite = account?.actor.role !== "viewer";
  const [filters, setFilters] = useState<ThreadFilters>({ status: "open" });
  const [threads, setThreads] = useState<readonly TeamThread[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setPending(true);
    setError(null);
    const load = () => {
      void api
        .list(filters, controller.signal)
        .then((page) => {
          setThreads(page.threads);
          setCursor(page.next_cursor);
        })
        .catch((e) => {
          if (!controller.signal.aborted) setError(e.message);
        })
        .finally(() => {
          if (!controller.signal.aborted) setPending(false);
        });
    };
    load();
    globalThis.addEventListener(THREADS_CHANGED, load);
    return () => {
      controller.abort();
      globalThis.removeEventListener(THREADS_CHANGED, load);
    };
  }, [api, filters, selected]);
  return (
    <main className="flex h-full min-h-0 min-w-0 flex-1">
      <section
        aria-label="Threads"
        className={`${selected ? "hidden md:flex" : "flex"} min-h-0 min-w-0 flex-1 flex-col border-r border-border md:w-80 md:max-w-sm md:flex-none`}
      >
        <WorkspacePageHeader className="border-b border-border">
          <h1 className="flex-1 font-semibold">Threads</h1>
          {canWrite ? (
            <Button size="touch" onClick={() => go("/threads/new")}>
              New thread
            </Button>
          ) : null}
        </WorkspacePageHeader>
        <ThreadFiltersBar value={filters} onChange={setFilters} />
        <div className="min-h-0 flex-1 overflow-y-auto">
          {error ? (
            <p role="alert" className="p-4 text-destructive-foreground">
              {error}
            </p>
          ) : null}
          {pending ? (
            <p role="status" className="p-4">
              Loading threads…
            </p>
          ) : null}
          {!pending && !error && threads.length === 0 ? (
            <p className="p-4 text-muted-foreground">
              No threads match these filters.
            </p>
          ) : null}
          {threads.map((thread) => (
            <button
              type="button"
              key={thread.id}
              onClick={() => go(`/threads/${thread.id}`)}
              aria-current={selected === thread.id ? "page" : undefined}
              className="flex w-full min-w-0 flex-col gap-1 border-b border-border p-4 text-left hover:bg-accent aria-[current=page]:bg-accent focus-visible:outline focus-visible:outline-ring"
            >
              <span className="flex items-center gap-2 text-xs text-muted-foreground">
                {thread.unread_count ? (
                  <span
                    aria-label="Unread"
                    className="size-2 rounded-full bg-primary"
                  />
                ) : null}
                <span className="min-w-0 flex-1 truncate">
                  <ScopeLabel thread={thread} projects={projects} />
                </span>
                <time dateTime={thread.updated_at}>
                  {formatRelativeTimeLabel(thread.updated_at)}
                </time>
              </span>
              <span className="font-semibold">{thread.title}</span>
              <span className="line-clamp-1 text-sm text-muted-foreground">
                {thread.last_post_preview || "No posts yet"}
              </span>
              <span className="flex flex-wrap gap-1">
                <Badge variant="secondary">{thread.post_count} posts</Badge>
                {thread.participants.length ? (
                  <Badge variant="secondary" className="min-w-0 max-w-full">
                    <span className="truncate">
                      {thread.participants
                        .map((p) => p.name + (p.client ? "’s agent" : ""))
                        .join(", ")}
                    </span>
                  </Badge>
                ) : null}
              </span>
            </button>
          ))}
          {cursor ? (
            <Button
              variant="ghost"
              size="touch"
              disabled={pending}
              onClick={() => {
                setPending(true);
                void api
                  .list({ ...filters, cursor })
                  .then((page) => {
                    setThreads((old) => [...old, ...page.threads]);
                    setCursor(page.next_cursor);
                  })
                  .catch((e) => setError(e.message))
                  .finally(() => setPending(false));
              }}
            >
              Load more threads
            </Button>
          ) : null}
        </div>
      </section>
      {selected === "new" ? (
        <NewThreadForm
          projects={projects}
          onCancel={() => go("/threads")}
          onCreate={async (input) => {
            const thread = await api.create(input);
            go(`/threads/${thread.id}`);
          }}
        />
      ) : selected ? (
        <ThreadView
          key={selected}
          api={api}
          id={selected}
          projects={projects}
          name={account?.actor.first_name || account?.actor.email}
          canWrite={canWrite}
          onBack={() => go("/threads")}
        />
      ) : (
        <div className="hidden flex-1 flex-col items-center justify-center gap-3 text-muted-foreground md:flex">
          <MessageSquareIcon className="size-6" />
          <p>Select a thread to read and reply.</p>
        </div>
      )}
    </main>
  );
}
