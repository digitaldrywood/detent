import * as Schema from "effect/Schema";
import { useMemo } from "react";
import { useClient } from "../client.ts";

export const ThreadReference = Schema.Struct({
  kind: Schema.Literals(["issue", "change", "attachment"]),
  id: Schema.String,
  project_id: Schema.optional(Schema.String),
  issue_id: Schema.optional(Schema.String),
  label: Schema.optional(Schema.String),
});
export const Thread = Schema.Struct({
  id: Schema.String,
  scope: Schema.Literals(["general", "project", "issue"]),
  project_id: Schema.optional(Schema.String),
  issue_id: Schema.optional(Schema.String),
  issue_number: Schema.optional(Schema.Number),
  title: Schema.String,
  status: Schema.Literals(["open", "resolved"]),
  creator_principal: Schema.String,
  creator_name: Schema.String,
  creator_client: Schema.optional(Schema.String),
  post_count: Schema.Number,
  unread_count: Schema.Number,
  last_post_preview: Schema.String,
  created_at: Schema.String,
  updated_at: Schema.String,
  participants: Schema.Array(
    Schema.Struct({
      principal_id: Schema.String,
      name: Schema.String,
      client: Schema.optional(Schema.String),
    }),
  ),
  references: Schema.Array(ThreadReference),
});
export type TeamThread = typeof Thread.Type;
export const Post = Schema.Struct({
  sequence: Schema.Number,
  author_principal: Schema.String,
  author_name: Schema.String,
  acting_for: Schema.optional(Schema.String),
  client_name: Schema.optional(Schema.String),
  kind: Schema.Literals(["note", "finding", "question", "decision"]),
  body: Schema.String,
  created_at: Schema.String,
  references: Schema.Array(ThreadReference),
});
export type TeamPost = typeof Post.Type;
export type PostKind = TeamPost["kind"];
const Page = Schema.Struct({
  threads: Schema.Array(Thread),
  next_cursor: Schema.optional(Schema.String),
});
const Read = Schema.Struct({
  thread: Thread,
  posts: Schema.Array(Post),
  cursor: Schema.String,
  has_more: Schema.Boolean,
});
export type ThreadRead = typeof Read.Type;
export interface ThreadFilters {
  scope?: string;
  status?: string;
  project_id?: string;
  issue_id?: string;
  search?: string;
  cursor?: string;
  limit?: number;
}
export interface NewThread {
  scope: string;
  project_id?: string;
  issue_id?: string;
  title: string;
  idempotency_key: string;
}
export const THREADS_CHANGED = "detent-threads-changed";
export function makeThreadsApi(options: {
  origin: string;
  apiBase: string;
  csrfToken: string;
}) {
  async function send<A>(
    schema: Schema.Codec<A, any, never, never>,
    method: string,
    suffix: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<A> {
    const response = await fetch(
      `${options.origin}${options.apiBase}/threads${suffix}`,
      {
        method,
        credentials: "same-origin",
        signal,
        headers: {
          Accept: "application/json",
          ...(body === undefined
            ? {}
            : {
                "Content-Type": "application/json",
                "X-CSRF-Token": options.csrfToken,
              }),
        },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      },
    );
    if (!response.ok)
      throw new Error(
        response.status === 403
          ? "You do not have permission to use this thread."
          : response.status === 404
            ? "This thread is not available."
            : "The thread request could not be completed.",
      );
    const result = Schema.decodeUnknownSync(schema)(await response.json());
    if (method !== "GET" || suffix.includes("since="))
      globalThis.dispatchEvent(new Event(THREADS_CHANGED));
    return result;
  }
  return {
    unread: (
      projects: readonly string[],
      refresh: boolean,
      signal?: AbortSignal,
    ) => {
      const query = new URLSearchParams();
      for (const project of projects) query.append("project_id", project);
      if (refresh) query.set("refresh", "true");
      return send(
        Schema.Struct({
          count: Schema.NullOr(Schema.Number),
          pending: Schema.Boolean,
        }),
        "GET",
        `/unread?${query}`,
        undefined,
        signal,
      );
    },
    list: (filters: ThreadFilters = {}, signal?: AbortSignal) => {
      const query = new URLSearchParams();
      for (const [key, value] of Object.entries(filters))
        if (value !== undefined && value !== "") query.set(key, String(value));
      return send(Page, "GET", `?${query}`, undefined, signal);
    },
    get: (id: string, since = "0", signal?: AbortSignal) =>
      send(
        Read,
        "GET",
        `/${encodeURIComponent(id)}?since=${encodeURIComponent(since)}`,
        undefined,
        signal,
      ),
    create: (input: NewThread) => send(Thread, "POST", "", input),
    post: (
      id: string,
      input: { kind: PostKind; body: string; idempotency_key: string },
    ) => send(Post, "POST", `/${encodeURIComponent(id)}/posts`, input),
    resolve: (id: string, status: "open" | "resolved") =>
      send(Thread, "PATCH", `/${encodeURIComponent(id)}`, { status }),
  };
}
export type ThreadsApi = ReturnType<typeof makeThreadsApi>;
export function useThreadsApi(): ThreadsApi {
  const client = useClient();
  return useMemo(() => makeThreadsApi(client.http), [client.http]);
}
