// In-memory mock of the hub's native work API.
//
// A sibling of `mock-hub.ts` rather than more of it: the conversation mock is
// already 1,400 lines of transport behaviour, and the work API is a different
// surface with different rules. `mock-hub.ts` mounts this with one call.
//
// It reproduces the parts of the real API the client depends on being exact,
// including the parts that are inconvenient:
//
//  - `revision` and `expected_revision` are decimal **strings**;
//  - a repeated query parameter is `422`, and so is an unknown one;
//  - a transition to a state the current state does not list is `422`;
//  - a stale `expected_revision` is `409 revision_conflict`, and — as in
//    hosted mode — the body carries **no** `current_revision`, so a client
//    that tries to recover without re-reading will fail here too;
//  - the changes list is a bare array with no envelope and no cursor;
//  - the activity stream's data is a bare integer, not JSON.
//
// It is a development and test double. Nothing here is a reference
// implementation of the hub.
import type { ServerResponse } from "node:http";

interface MockState {
  readonly operator_only?: boolean;
  readonly name: string;
  readonly terminal: boolean;
  readonly dispatchable: boolean;
  readonly transitions: readonly string[];
}

interface MockProject {
  readonly project_id: string;
  readonly organization_id: string;
  readonly name: string;
  readonly profile: string;
  readonly states: readonly MockState[];
  readonly require_dependencies: boolean;
}

interface MockIssue {
  organization_id: string;
  project_id: string;
  work_item_id: string;
  web_url: string;
  number: number;
  revision: string;
  profile: string;
  title: string;
  body: string;
  state: string;
  terminal: boolean;
  priority?: number;
  labels: string[];
  assignees: string[];
  actor: { kind: string; principal_id: string };
  created_at: string;
  updated_at: string;
  dependencies: string[];
  blockers: { work_item_id: string; project_id: string; state: string; terminal: boolean }[];
  external_references: unknown[];
  provenance?: { provider: string; external_id: string; author_id: string; created_at: string; updated_at?: string; observed_at?: string };
}

const STATES: readonly MockState[] = [
  { name: "Backlog", terminal: false, dispatchable: false, transitions: ["Todo"] },
  { name: "Todo", terminal: false, dispatchable: true, transitions: ["In Progress", "Blocked", "Backlog"] },
  {
    name: "In Progress",
    terminal: false,
    dispatchable: false,
    transitions: ["In Review", "Blocked", "Todo"],
  },
  { name: "In Review", terminal: false, dispatchable: false, transitions: ["Merging", "In Progress"] },
  { name: "Blocked", terminal: false, dispatchable: false, transitions: ["Todo"] },
  { name: "Merging", terminal: false, dispatchable: false, transitions: ["Done"] },
  { operator_only: true, name: "Done", terminal: true, dispatchable: false, transitions: [] },
];

const LANES = ["Backlog", "Todo", "In Progress", "In Review", "Blocked", "Merging", "Done"];
const LABELS = ["bug", "chore", "goal:reliability", "effort:medium", "epic:3350"];
/** The prefixes the hub owns; they never reach the label catalogue. */
const MANAGED_PREFIXES = ["priority:", "effort:", "detent:"];
/** The hub's palette, in the hub's order (`internal/hubserver/native_labels.go`). */
const LABEL_PALETTE = [
  "#6e79d6",
  "#3f9e6f",
  "#c2803a",
  "#c05b6b",
  "#4d94bb",
  "#8a6bbf",
  "#3f9a94",
  "#a8763f",
  "#5f8f45",
  "#b5607f",
];

/** The hub's FNV-1a over the folded name, so the dots match the real thing. */
function mockLabelColor(name: string): string {
  let hash = 0x811c9dc5;
  for (const character of name.trim().toLowerCase()) {
    hash ^= character.charCodeAt(0);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return LABEL_PALETTE[hash % LABEL_PALETTE.length]!;
}
const ASSIGNEES = ["michaelhvisser", "octocat", ""];
const TITLES = [
  "checkout: renewal waits on a healthy handoff",
  "board: show scheduler dispatch waits",
  "mailchimp: add connection and address-health screens",
  "runner: isolate cleanup process discovery",
  "activity: add canonical subject projection",
  "people: configurable computed profile badges",
  "connections: custom alerts from segment membership",
  "reports: cost outcomes by project",
];

function randomSuffix(): string {
  return Math.random().toString(16).slice(2, 10).padEnd(8, "0");
}

function pad(index: number): string {
  return index.toString(16).padStart(32, "0");
}

export interface WorkMock {
  /**
   * Returns true when it answered the request.
   *
   * `readBody` is a thunk, not a value: reading the request consumes the
   * stream, and the conversation mock still has to read the bodies of the
   * routes this one does not own.
   */
  handle: (input: {
    response: ServerResponse;
    url: URL;
    method: string;
    readBody: () => Promise<Record<string, unknown>>;
  }) => Promise<boolean>;
  reset: () => void;
  /** The projects it serves, for the mock hub's bootstrap payload. */
  projects: readonly string[];
  /** Closes every open activity stream. */
  close: () => void;
}

export function createWorkMock(options: {
  apiBase: string;
  organizationId: string;
  /** `id -> name`, from the mock hub's own project list. */
  projects: readonly { id: string; name: string }[];
}): WorkMock {
  const now = new Date("2026-09-09T12:00:00Z").getTime();
  let issues: MockIssue[] = [];
  let pagination = false;
  let revoked = false;
  let expired = false;
  let sequence = 40;
  let conflictOn: string | null = null;
  // Reviews and discussion posted through the mock, per change id, so a
  // decision shows up on the next detail read the way the hub's does.
  const reviews = new Map<string, Record<string, unknown>[]>();
  const discussion = new Map<string, Record<string, unknown>[]>();
  const streams = new Set<ServerResponse>();

  function build(): void {
    issues = [];
    let number = 3300;
    for (const project of options.projects) {
      const count = pagination ? (project.id === options.projects[0]?.id ? 137 : 4) : project.id === options.projects[0]?.id ? 32 : 8;
      for (let index = 0; index < count; index += 1) {
        number += 1;
        const lane = pagination ? (index === 136 || index === 134 ? "In Progress" : index === 132 || index === 133 || index < 3 ? "Todo" : "Done") : LANES[index % LANES.length]!;
        const id = `wi_${pad(number)}`;
        const created = new Date(now - (index + 1) * 3_600_000).toISOString();
        issues.push({
          organization_id: options.organizationId,
          project_id: project.id,
          work_item_id: id,
          web_url: `http://mock.local/work/i/${id}`,
          number,
          revision: "1",
          profile: "native",
          title: pagination ? (index === 136 ? "Observed later-page worker" : `Queue item ${index + 1}`) : `${TITLES[index % TITLES.length]}`,
          body:
            "The renewal path returns before the handoff completes, so a runner that lost its lease still believes it holds one.\n\n" +
            "## Acceptance\n\n- The renewal blocks until the handoff is acknowledged.\n- A lost lease is reported as lost within one tick.\n- The board says which runner holds the lease.\n",
          state: lane,
          terminal: lane === "Done",
          ...(index % 4 === 3 ? {} : { priority: index % 4 }),
          labels: index % 3 === 0 ? [LABELS[index % LABELS.length]!, "effort:medium"] : [],
          assignees: ASSIGNEES[index % ASSIGNEES.length] === "" ? [] : [ASSIGNEES[index % ASSIGNEES.length]!],
          actor: { kind: "human", principal_id: "tok_mock" },
          created_at: created,
          updated_at: new Date(now - index * 600_000).toISOString(),
          dependencies: [],
          blockers: [],
          external_references: [],
          ...(pagination && index === 120 ? { provenance: {
            provider: "github", external_id: String(number), author_id: "imported-operator", created_at: "2019-01-01T00:00:00Z",
            updated_at: "2019-02-01T00:00:00Z", observed_at: created,
          } } : {}),
        });
      }
    }
    // One real blocker, so the Blocked treatment has something behind it.
    const blocked = issues.find((issue) => issue.state === "Blocked");
    const blocker = issues.find((issue) => issue.state === "Todo");
    if (blocked !== undefined && blocker !== undefined) {
      blocked.dependencies = [blocker.work_item_id];
      blocked.blockers = [
        {
          work_item_id: blocker.work_item_id,
          project_id: blocker.project_id,
          state: blocker.state,
          terminal: false,
        },
      ];
    }
  }
  build();

  const running = () => issues.filter((issue) => issue.state === "In Progress" && (!pagination || issue.title === "Observed later-page worker")).slice(0, 3);
  const changed = () =>
    issues.filter((issue) => issue.state === "In Review" || issue.state === "Merging").slice(0, 4);

  function attemptsFor(issue: MockIssue): unknown[] {
    if (!running().includes(issue) && !changed().includes(issue)) return [];
    const live = running().includes(issue);
    return [
      {
        sequence: "1",
        identity: { role: "implementer", backend: "codex", model: "gpt-6-astra" },
        machine_id: "mac_studio",
        runner_id: "rnr_mac_studio",
        session_id: "sess_90",
        lease_id: "lease_6e19",
        fencing_token: "6",
        run_id: `run_${pad(issue.number * 2)}`,
        attempt_id: `att_${pad(issue.number * 3)}`,
        policy_id: `policy_${pad(issue.number).repeat(2)}`,
        status: live ? "running" : "succeeded",
        ...(live ? {} : { outcome: "succeeded" }),
        started_at: new Date(now - 11 * 60_000 - 52_000).toISOString(),
        updated_at: new Date(now - 30_000).toISOString(),
        checkpoint: {
          resume: "resume_session",
          availability: "available",
          storage: "local_only",
          worktree_state: "clean",
          head_sha: `a41f0c2${pad(issue.number).slice(0, 33)}`,
          external_effect: "git_push",
          effect_state: "confirmed",
          effect_id: `effect_${pad(issue.number * 5)}`,
          change: {
            change_id: `change_${pad(issue.number)}`,
            version_id: `version_${pad(issue.number)}`,
            head_sha: `a41f0c2${pad(issue.number).slice(0, 33)}`,
          },
        },
      },
    ];
  }

  function changesFor(issue: MockIssue): Record<string, unknown>[] {
    if (!changed().includes(issue)) return [];
    return [
      {
        change_id: `change_${pad(issue.number)}`,
        organization_id: issue.organization_id,
        project_id: issue.project_id,
        work_item_id: issue.work_item_id,
        linked_issues: [issue.work_item_id],
        title: `fix: ${issue.title}`,
        body: "",
        current_version_id: `version_${pad(issue.number)}`,
        revision: "2",
        created_at: issue.created_at,
        updated_at: issue.updated_at,
      },
    ];
  }

  function changeDetail(issue: MockIssue): Record<string, unknown> | null {
    const change = changesFor(issue)[0];
    if (change === undefined) return null;
    const head = `a41f0c2${pad(issue.number).slice(0, 33)}`;
    const policy = `policy_${pad(issue.number).repeat(2)}`;
    return {
      change,
      versions: [
        {
          base_sha: pad(1).slice(0, 40),
          head_sha: head,
          merge_base_sha: pad(1).slice(0, 40),
          repository: "digitaldrywood/detent",
          code: {
            kind: "code",
            uri: `artifact://artifact_${pad(issue.number)}`,
            sha256: pad(issue.number).repeat(2),
            availability: "available",
          },
          artifacts: [],
          run_id: `run_${pad(issue.number * 2)}`,
          attempt_id: `att_${pad(issue.number * 3)}`,
          policy_id: policy,
          external: {
            provider: "github",
            id: String(issue.number + 9),
            url: `https://github.com/digitaldrywood/detent/pull/${issue.number + 9}`,
          },
          version_id: `version_${pad(issue.number)}`,
          change_id: `change_${pad(issue.number)}`,
          number: "2",
          policy: {
            schema: 1,
            policy_id: policy,
            source_revision: "3f8f74e0",
            source_digest: "sha256:2c26b46b",
            config_digest: "sha256:486ea462",
            requirements: {},
            gates: { kind: "standard", required_checks: 1, merge_method: "squash" },
          },
          review_policy: {
            review_policy_id: `rp_${pad(issue.number)}`,
            policy_id: policy,
            require_review: true,
            required_checks: [],
          },
          checks: [],
          actor: { kind: "runner", principal_id: "tok_runner_1" },
          created_at: issue.updated_at,
        },
      ],
      reviews: [
        ...(issue.state === "In Review"
          ? [
              {
                review_id: `review_${pad(issue.number)}`,
                version_id: `version_${pad(issue.number)}`,
                decision: "changes_requested",
                body: "Format lastSyncAt through the tenant clock helper.",
                actor: { kind: "human", principal_id: "tok_mock" },
                created_at: issue.updated_at,
              },
            ]
          : []),
        ...(reviews.get(String(change.change_id)) ?? []),
      ],
      checks: [
        {
          check_run_id: `check_${pad(issue.number)}`,
          head_sha: head,
          run_id: `run_${pad(issue.number * 2)}`,
          policy_id: policy,
          config_digest: "sha256:486ea462",
          workflow_id: "ci.yml",
          workflow_sha256: "sha256:aa11bb22",
          source: "independent",
          conclusion: "success",
          completed_at: issue.updated_at,
          evidence: [],
          version_id: `version_${pad(issue.number)}`,
          actor: { kind: "runner", principal_id: "tok_ci" },
          received_at: issue.updated_at,
        },
      ],
      discussion: discussion.get(String(change.change_id)) ?? [],
      summary: mockSummary(issue, reviews.get(String(change.change_id)) ?? []),
    };
  }

  // The hub's rolled-up verdict, as far as the mock reproduces it: the latest
  // decision posted through the mock wins over the seeded one.
  function mockSummary(issue: MockIssue, posted: Record<string, unknown>[]): Record<string, unknown> {
    const latest = posted.at(-1);
    const decision =
      latest === undefined
        ? issue.state === "In Review"
          ? "changes_requested"
          : "approved"
        : String(latest.decision);
    if (decision === "changes_requested") {
      return {
        native_review: "changes_requested",
        external_review: "snapshot: APPROVED",
        checks: "passed",
        status: "blocked",
        messages: ["A reviewer requested changes."],
      };
    }
    return {
      native_review: decision === "approved" ? "approved" : "pending",
      external_review: "snapshot: APPROVED",
      checks: "passed",
      status: decision === "approved" ? "ready" : "pending",
      messages: [],
    };
  }

  // One attempt's stored diff (§18.5): a small real patch, so the review page
  // has files to draw. The head is the attempt's, as the runner posts it.
  function attemptDiff(issue: MockIssue): Record<string, unknown> {
    const head = `a41f0c2${pad(issue.number).slice(0, 33)}`;
    return {
      id: `diff_${pad(issue.number)}`,
      attempt_id: `att_${pad(issue.number * 3)}`,
      producer: {
        kind: "attempt",
        id: `att_${pad(issue.number * 3)}`,
        runner_id: "rnr_mac_studio",
        lease_id: "lease_6e19",
        fencing_token: 6,
      },
      generation: { source: "attempt", seq: 2 },
      base_sha: pad(1).slice(0, 40),
      head_sha: head,
      files: [
        {
          path: "README.md",
          status: "modified",
          additions: 1,
          deletions: 0,
          binary: false,
          patch:
            "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -40,6 +40,7 @@ ## Usage\n Run the fixture:\n \n     make run\n+Recorded by the stub agent.\n \n ## License\n \n",
          truncated: false,
          denied: false,
        },
      ],
      file_count: 1,
      patch_bytes: 173,
      truncated: false,
      created_at: issue.updated_at,
    };
  }

  function historyFor(issue: MockIssue): unknown[] {
    return [
      {
        event_id: `evt_${pad(issue.number)}`,
        organization_id: issue.organization_id,
        project_id: issue.project_id,
        aggregate_type: "work_item",
        aggregate_id: issue.work_item_id,
        aggregate_sequence: "1",
        type: "issue.created",
        schema_version: 1,
        recorded_at: issue.created_at,
        actor: issue.actor,
        data: { revision: "1" },
      },
      {
        event_id: `evt_${pad(issue.number + 1)}`,
        organization_id: issue.organization_id,
        project_id: issue.project_id,
        aggregate_type: "work_item",
        aggregate_id: issue.work_item_id,
        aggregate_sequence: "2",
        type: "workflow.transitioned",
        schema_version: 1,
        recorded_at: issue.updated_at,
        actor: issue.actor,
        data: {
          revision: issue.revision,
          from_state: "Todo",
          to_state: issue.state,
          reason: "user_requested",
        },
      },
    ];
  }

  function json(response: ServerResponse, status: number, payload: unknown): void {
    const body = JSON.stringify(payload);
    response.writeHead(status, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
      "Content-Length": new TextEncoder().encode(body).length,
    });
    response.end(body);
  }

  function invalid(response: ServerResponse, message: string): void {
    json(response, 422, { code: "invalid_request", message });
  }

  function validateQuery(url: URL, allowed: readonly string[], multi: readonly string[] = []): string | null {
    const seen = new Set<string>();
    for (const key of url.searchParams.keys()) {
      if (!allowed.includes(key) && key !== "limit" && key !== "cursor") {
        return "Query contains an unsupported field or value";
      }
      if (seen.has(key) && !multi.includes(key)) return "Query contains an unsupported field or value";
      seen.add(key);
    }
    return null;
  }

  const project = (id: string): MockProject | null => {
    const found = options.projects.find((candidate) => candidate.id === id);
    if (found === undefined) return null;
    return {
      project_id: found.id,
      organization_id: options.organizationId,
      name: found.name,
      profile: "native",
      states: STATES,
      require_dependencies: true,
    };
  };

  function bump(): void {
    sequence += 1;
    for (const stream of streams) stream.write(`event: activity\ndata: ${sequence}\n\n`);
  }

  const base = `${options.apiBase}/projects`;

  return {
    projects: options.projects.map((entry) => entry.id),
    reset() {
      build();
      conflictOn = null;
      sequence = 40;
      reviews.clear();
      discussion.clear();
    },
    close() {
      for (const stream of streams) stream.end();
      streams.clear();
    },
    async handle({ response, url, method, readBody }) {
      const path = url.pathname;

      // Test control: arm one scripted conflict on the next transition.
      if (path === "/__mock/work/conflict" && method === "POST") {
        const body = await readBody();
        conflictOn = typeof body.item === "string" ? body.item : "*";
        json(response, 200, { armed: conflictOn });
        return true;
      }
      if (path === "/__mock/work/pagination" && method === "POST") {
        const body = await readBody();
        if (!pagination) {
          pagination = true;
          build();
          for (const [index, choice] of [[120, "a"], [121, "b"]] as const) {
            const older = issues[index];
            if (older === undefined) continue;
            older.title = `Older title needle ${choice}`;
            older.labels = ["older-label", `choice-${choice}`];
            older.assignees = [`operator-${choice}`];
            older.priority = choice === "a" ? 0 : 1;
          }
          const active = issues.find((issue) => issue.title === "Observed later-page worker");
          if (active !== undefined) active.updated_at = new Date(now).toISOString();
        }
        if (body.openOverflow === true) {
          for (const issue of issues) {
            if (issue.state !== "Done" || issue.provenance !== undefined) continue;
            issue.state = "Todo";
            issue.terminal = false;
          }
        }
        revoked = body.revoked === true;
        expired = body.expired === true;
        json(response, 200, { ready: true });
        return true;
      }
      if (path === "/__mock/work/reset" && method === "POST") {
        pagination = false;
        revoked = false;
        expired = false;
        build();
        conflictOn = null;
        reviews.clear();
        discussion.clear();
        json(response, 200, { reset: true });
        return true;
      }
      if (path === "/__mock/work/activity" && method === "POST") {
        bump();
        json(response, 200, { sequence });
        return true;
      }

      // The hosted activity stream is a page route, not an API route.
      const events = path.match(/^\/projects\/([^/]+)\/events$/);
      if (events !== null && method === "GET") {
        response.writeHead(200, {
          "Content-Type": "text/event-stream",
          "Cache-Control": "no-store",
          Connection: "keep-alive",
        });
        response.write(`event: activity\ndata: ${sequence}\n\n`);
        streams.add(response);
        response.on("close", () => streams.delete(response));
        return true;
      }

      const itemLookup = path.match(new RegExp(`^${options.apiBase}/work-items/([^/]+)$`));
      if (itemLookup !== null && method === "GET") {
        const issue = issues.find((candidate) => candidate.work_item_id === decodeURIComponent(itemLookup[1]!));
        json(response, issue === undefined ? 404 : 200, issue ?? {
          code: "not_found", message: "Resource was not found",
        });
        return true;
      }

      if (!path.startsWith(base)) return false;
      const rest = path.slice(base.length).replace(/^\//, "");
      const segments = rest.split("/").map((segment) => decodeURIComponent(segment));
      const projectId = segments[0] ?? "";
      const found = project(projectId);

      // `.../conversations...` belongs to the conversation mock.
      if (segments[1] === "conversations") return false;
      if (found === null) return false;
      if (pagination && revoked) {
        json(response, 403, { code: "forbidden", message: "Project access revoked" });
        return true;
      }

      if (segments.length === 1 && method === "GET") {
        json(response, 200, found);
        return true;
      }

      // The project's label catalogue: the union of what its work items
      // carry, minus the prefixes the hub owns, each with a colour derived
      // from the name — `GET {nativeBase}/labels`, as
      // `internal/hubserver/native_labels.go` serves it.
      if (segments[1] === "labels" && segments.length === 2 && method === "GET") {
        const counts = new Map<string, number>();
        for (const issue of issues.filter((candidate) => candidate.project_id === projectId)) {
          for (const label of new Set(issue.labels)) {
            if (MANAGED_PREFIXES.some((prefix) => label.toLowerCase().startsWith(prefix))) continue;
            counts.set(label, (counts.get(label) ?? 0) + 1);
          }
        }
        const items = [...counts.entries()]
          .map(([name, count]) => ({ name, color: mockLabelColor(name), count }))
          .toSorted((a, b) => (b.count - a.count === 0 ? a.name.localeCompare(b.name) : b.count - a.count));
        json(response, 200, { items });
        return true;
      }

      // `GET {nativeBase}/attempts/:attempt/diff`, attempt-addressed.
      if (segments[1] === "attempts" && segments[3] === "diff" && segments.length === 4 && method === "GET") {
        const owner = issues.find(
          (issue) => issue.project_id === projectId && `att_${pad(issue.number * 3)}` === segments[2],
        );
        if (owner === undefined || !changed().includes(owner)) {
          json(response, 404, { code: "not_found", message: "Resource was not found" });
          return true;
        }
        json(response, 200, attemptDiff(owner));
        return true;
      }

      if (segments[1] !== "work-items") return false;

      const scoped = issues.filter((issue) => issue.project_id === projectId);

      if (segments.length === 2 && method === "GET") {
        const problem = validateQuery(url, ["state", "label", "assignee", "priority", "include", "archived", "q"], ["state", "label", "assignee", "priority"]);
        if (problem !== null) {
          invalid(response, problem);
          return true;
        }
        const state = url.searchParams.getAll("state");
        const label = url.searchParams.getAll("label");
        const assignee = url.searchParams.getAll("assignee");
        const priority = url.searchParams.getAll("priority");
        const limit = Math.min(Number(url.searchParams.get("limit") ?? "50"), 200);
        const q = url.searchParams.get("q")?.trim().toLowerCase() ?? "";
        const cursorScope = JSON.stringify([projectId, state, label, assignee, priority, q, url.searchParams.get("archived")]);
        let after = Number(url.searchParams.get("cursor") ?? "0");
        if (pagination && url.searchParams.has("cursor")) {
          try {
            const cursor = JSON.parse(atob(url.searchParams.get("cursor")!));
            if (expired || cursor.scope !== cursorScope || !Number.isSafeInteger(cursor.after)) throw new Error("cursor");
            after = cursor.after;
          } catch {
            invalid(response, "Cursor is invalid or expired for this project/filter");
            return true;
          }
        }
        const filtered = scoped
          .filter(() => url.searchParams.get("archived") !== "true")
          .filter((issue) => state.length === 0 || state.includes(issue.state))
          .filter((issue) => label.length === 0 || label.some((value) => issue.labels.includes(value)))
          .filter((issue) => assignee.length === 0 || assignee.some((value) => issue.assignees.includes(value)))
          .filter((issue) => priority.length === 0 || priority.includes(String(issue.priority ?? "")))
          .filter((issue) => q === "" || issue.title.toLowerCase().includes(q)
            || `${project(projectId)?.name}#${issue.number}`.toLowerCase().includes(q)
            || `${projectId}#${issue.number}`.toLowerCase().includes(q)
            || issue.labels.some((value) => value.toLowerCase().includes(q)));
        const matching = filtered.filter((issue) => issue.number > after)
          .toSorted((a, b) => a.number - b.number);
        const page = matching.slice(0, limit);
        const last = page.at(-1);
        const workIncluded = url.searchParams.get("include") === "work";
        const open = filtered.filter((issue) => !issue.terminal).toSorted((a, b) =>
          Number(running().includes(b)) - Number(running().includes(a))
          || Number(STATES.find((state) => state.name === b.state)?.dispatchable ?? false)
            - Number(STATES.find((state) => state.name === a.state)?.dispatchable ?? false)
          || b.number - a.number);
        json(response, 200, {
          items: workIncluded ? page.map((issue) => ({ ...issue, body: "" })) : page,
          ...(workIncluded ? { work: {
            items: open.slice(0, limit).map((issue) => ({ ...issue, body: "" })),
            lanes: [...new Set(filtered.map((issue) => issue.state))].map((state) => ({
              state, total: filtered.filter((issue) => issue.state === state).length,
              running: filtered.filter((issue) => issue.state === state && running().includes(issue)).length,
            })),
            truncated: open.length > limit, as_of: new Date(now).toISOString(),
          } } : {}),
          ...(last !== undefined && matching.length > page.length
            ? { next_cursor: pagination ? btoa(JSON.stringify({ scope: cursorScope, after: last.number })) : String(last.number) }
            : {}),
        });
        return true;
      }

      const itemId = segments[2];
      const issue = scoped.find((candidate) => candidate.work_item_id === itemId);
      if (issue === undefined) {
        json(response, 404, { code: "not_found", message: "Resource was not found" });
        return true;
      }
      const action = segments[3];
      if (pagination && issue.number === 3303 && action === "changes" && segments.length === 5 && method === "GET") {
        json(response, 503, { code: "unavailable", message: "Change detail unavailable" });
        return true;
      }

      if (action === undefined && method === "GET") {
        json(response, 200, issue);
        return true;
      }

      if (action === undefined && method === "PATCH") {
        const body = await readBody();
        const key = String(body.idempotency_key ?? "");
        if (key.length === 0 || key.length > 128) {
          invalid(response, "An idempotency key of at most 128 bytes is required");
          return true;
        }
        if (String(body.expected_revision ?? "") !== issue.revision) {
          // Hosted shape: no `current_revision`, and a generic message.
          json(response, 409, {
            code: "revision_conflict",
            message: "The requested operation is unavailable",
          });
          return true;
        }
        if (typeof body.title === "string") issue.title = body.title;
        if (typeof body.body === "string") issue.body = body.body;
        if (typeof body.priority === "number") issue.priority = body.priority;
        // The hub's three-way priority member: absent leaves it, a number
        // sets it, `"none"` or null removes it (`tracker.PriorityPatch`).
        if (body.priority === "none" || ("priority" in body && body.priority === null)) {
          delete issue.priority;
        }
        if (Array.isArray(body.labels)) issue.labels = body.labels as string[];
        if (Array.isArray(body.assignees)) issue.assignees = body.assignees as string[];
        issue.revision = String(Number(issue.revision) + 1);
        issue.updated_at = new Date().toISOString();
        bump();
        json(response, 200, issue);
        return true;
      }

      // The relation the issue page's Related group adds and removes:
      // `POST .../dependencies` (`changeNativeDependency`).
      if (action === "dependencies" && method === "POST") {
        const body = await readBody();
        const key = String(body.idempotency_key ?? "");
        if (key.length === 0 || key.length > 128) {
          invalid(response, "An idempotency key of at most 128 bytes is required");
          return true;
        }
        if (String(body.expected_revision ?? "") !== issue.revision) {
          json(response, 409, {
            code: "revision_conflict",
            message: "The requested operation is unavailable",
          });
          return true;
        }
        const operation = String(body.operation ?? "");
        const relatedId = String(body.related_work_item_id ?? "");
        const related = scoped.find((candidate) => candidate.work_item_id === relatedId);
        if (operation !== "add" && operation !== "remove") {
          invalid(response, "Dependency operation must be add or remove");
          return true;
        }
        if (related === undefined || related.work_item_id === issue.work_item_id) {
          invalid(response, "Dependencies cannot form a cycle");
          return true;
        }
        if (operation === "add") {
          if (!issue.dependencies.includes(relatedId)) {
            issue.dependencies = [...issue.dependencies, relatedId].toSorted();
            issue.blockers = [
              ...issue.blockers,
              {
                work_item_id: related.work_item_id,
                project_id: related.project_id,
                state: related.state,
                terminal: related.terminal,
              },
            ];
          }
        } else {
          issue.dependencies = issue.dependencies.filter((id) => id !== relatedId);
          issue.blockers = issue.blockers.filter((blocker) => blocker.work_item_id !== relatedId);
        }
        issue.revision = String(Number(issue.revision) + 1);
        issue.updated_at = new Date().toISOString();
        bump();
        json(response, 200, issue);
        return true;
      }

      if (action === "workflow" && method === "POST") {
        const body = await readBody();
        const key = String(body.idempotency_key ?? "");
        if (key.length === 0 || key.length > 128) {
          invalid(response, "An idempotency key of at most 128 bytes is required");
          return true;
        }
        const reason = String(body.reason ?? "");
        if (!["user_requested", "worker_progress", "dependency_ready"].includes(reason)) {
          invalid(response, "Transition reason is invalid");
          return true;
        }
        const target = String(body.state ?? "");
        const current = STATES.find((candidate) => candidate.name === issue.state);
        if (current === undefined || !current.transitions.includes(target)) {
          json(response, 422, {
            code: "transition_not_allowed",
            message: "The requested operation is unavailable",
          });
          return true;
        }
        if (conflictOn !== null && (conflictOn === "*" || conflictOn === issue.work_item_id)) {
          conflictOn = null;
          // The scripted conflict: the hub moved on without this client. The
          // stored revision advances, so a retry with the old one fails too.
          issue.revision = String(Number(issue.revision) + 1);
          json(response, 409, {
            code: "revision_conflict",
            message: "The requested operation is unavailable",
          });
          return true;
        }
        if (String(body.expected_revision ?? "") !== issue.revision) {
          json(response, 409, {
            code: "revision_conflict",
            message: "The requested operation is unavailable",
          });
          return true;
        }
        issue.state = target;
        issue.terminal = STATES.find((candidate) => candidate.name === target)?.terminal ?? false;
        issue.revision = String(Number(issue.revision) + 1);
        issue.updated_at = new Date().toISOString();
        bump();
        json(response, 200, issue);
        return true;
      }

      if (action === "attempts" && method === "GET") {
        if (pagination && issue.number === 3301) {
          json(response, 503, { code: "unavailable", message: "Attempt observation unavailable" });
          return true;
        }
        json(response, 200, { items: attemptsFor(issue), ...(pagination && issue.number === 3302 ? { next_cursor: "older-attempt-page" } : {}) });
        return true;
      }

      if (action === "history" && method === "GET") {
        json(response, 200, { items: historyFor(issue) });
        return true;
      }

      if (action === "comments" && method === "GET") {
        json(response, 200, { items: [] });
        return true;
      }

      if (action === "changes" && segments.length === 4 && method === "GET") {
        // A bare array, as the real endpoint serves it.
        json(response, 200, changesFor(issue));
        return true;
      }

      if (action === "changes" && segments.length === 5 && method === "GET") {
        const detail = changeDetail(issue);
        if (detail === null || (detail.change as { change_id: string }).change_id !== segments[4]) {
          json(response, 404, { code: "not_found", message: "Resource was not found" });
          return true;
        }
        json(response, 200, detail);
        return true;
      }

      if (action === "diff" && segments.length === 4 && method === "GET") {
        json(response, 200, { diff: changed().includes(issue) ? attemptDiff(issue) : null });
        return true;
      }

      // `POST .../changes/:change/versions/:version/reviews` and
      // `POST .../changes/:change/discussion`, as `change_evidence.go` serves
      // them: a key, a decision from the fixed set, and `approved` only on
      // the current version.
      if (action === "changes" && method === "POST" && (segments.length === 8 || segments.length === 6)) {
        const detail = changeDetail(issue);
        const changeId = segments[4] ?? "";
        if (detail === null || (detail.change as { change_id: string }).change_id !== changeId) {
          json(response, 404, { code: "not_found", message: "Resource was not found" });
          return true;
        }
        const body = await readBody();
        const key = String(body.idempotency_key ?? "");
        if (key.length === 0 || key.length > 128) {
          invalid(response, "An idempotency key of at most 128 bytes is required");
          return true;
        }
        const text = String(body.body ?? "");
        const currentVersion = (detail.change as { current_version_id: string }).current_version_id;
        if (segments.length === 6 && segments[5] === "discussion") {
          if (text.trim().length === 0 || text.length > 64 * 1024) {
            invalid(response, "Discussion requires 1 byte to 64 KiB");
            return true;
          }
          const versionId = body.version_id === undefined ? undefined : String(body.version_id);
          if (versionId !== undefined && versionId !== currentVersion) {
            json(response, 404, { code: "not_found", message: "Resource was not found" });
            return true;
          }
          const comment = {
            comment_id: `cmt_${randomSuffix()}`,
            ...(versionId === undefined ? {} : { version_id: versionId }),
            body: text,
            actor: { kind: "human", principal_id: "tok_mock" },
            created_at: new Date().toISOString(),
          };
          discussion.set(changeId, [...(discussion.get(changeId) ?? []), comment]);
          bump();
          json(response, 200, comment);
          return true;
        }
        if (segments.length === 8 && segments[5] === "versions" && segments[7] === "reviews") {
          const versionId = segments[6] ?? "";
          if (versionId !== currentVersion) {
            json(response, 404, { code: "not_found", message: "Resource was not found" });
            return true;
          }
          const decision = String(body.decision ?? "");
          const expected = body.expected_version_id === undefined ? "" : String(body.expected_version_id);
          if (expected !== "" && expected !== currentVersion) {
            json(response, 409, { code: "revision_conflict", message: "The requested operation is unavailable" });
            return true;
          }
          if (!["approved", "changes_requested", "commented"].includes(decision) || text.length > 64 * 1024) {
            invalid(response, "Review decision is invalid or body exceeds 64 KiB");
            return true;
          }
          const review = {
            review_id: `review_${randomSuffix()}`,
            version_id: versionId,
            decision,
            body: text,
            actor: { kind: "human", principal_id: "tok_mock" },
            created_at: new Date().toISOString(),
          };
          reviews.set(changeId, [...(reviews.get(changeId) ?? []), review]);
          bump();
          json(response, 200, review);
          return true;
        }
      }

      json(response, 404, { code: "not_found", message: "Resource was not found" });
      return true;
    },
  };
}
