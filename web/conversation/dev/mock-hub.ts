// In-memory mock of the conversation hub.
//
// Implements the endpoints and the event stream in
// `docs/conversation/decisions.md` §5 well enough to develop and test the
// client against: idempotent commands, per-conversation event cursors,
// heartbeats, a scripted coordinator that streams a reply in deltas, issue
// linking, a scripted runner lifecycle for linked conversations, and
// deliberate failure injection.
//
// It has two coordinator modes. `hub` is the transitional hub-side coordinator
// of the first slice: an unlinked message is answered in place. `runner` is the
// runner-dispatched coordinator of decisions.md §1 and §9: the message queues,
// execution reports `waiting_for_runner`, and the same `/__mock/runner/...`
// hooks that drive a linked attempt drive the coordinator attempt instead.
//
// It is a development and test double. It has no authorization, no storage
// and no provider; nothing here is a reference implementation of the hub.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { AddressInfo } from "node:net";
import { randomUUID } from "node:crypto";

import { createWorkMock } from "./mock-work.ts";
import type {
  Conversation,
  ConversationEvent,
  Execution,
  Message,
  Question,
  Receipt,
} from "../src/contracts/index.ts";

const ORGANIZATION = { id: "org_mock", name: "Mock organization" };
const ACTOR = {
  principal_id: "tok_mock",
  subject: "usr_mock",
  email: "operator@example.test",
  role: "owner",
};
const PROJECTS = [
  {
    id: "proj_alpha",
    name: "alpha",
    can_write: true,
    labels: ["bug", "chore", "goal:reliability"],
    priorities: ["High", "Normal", "Low"],
  },
  { id: "proj_beta", name: "beta", can_write: true },
  { id: "proj_readonly", name: "readonly", can_write: false },
];
const CSRF_TOKEN = "csrf_mock_token";
const API_BASE = `/api/v2/organizations/${ORGANIZATION.id}`;

// --- The hosted account surface (decisions.md §12) --------------------------
//
// §12 turns everything the hosted Templ pages did through forms into JSON
// under the same `/api/v2/organizations/:organization` base. The seeds below
// are this double's account fixture: shape for shape they follow
// `src/contracts/fixtures/account-*.json`, but with this file's own
// organization, actor and project ids, so the conversation half and the
// account half describe one hub rather than two that happen to share a port.

interface MemberRow {
  id: string;
  user_id: string;
  email: string;
  role: string;
  status: string;
  grants: Array<{ project_id: string; write: boolean; runner: boolean }>;
}

interface InvitationRow {
  id: string;
  email: string;
  role: string;
  created_at: string;
  expires_at: string;
}

interface ProjectRow {
  id: string;
  name: string;
  profile: string;
  can_write: boolean;
  can_manage_runners: boolean;
  labels?: string[];
  priorities?: string[];
}

interface IntegrationRow {
  profile: string;
  /** A JSON *string*: the hub marshals the counter with `json:",string"`. */
  revision: string;
  intake: string;
  projection: string;
  repository_enabled: boolean;
  repository?: string;
  authority: Record<string, string>;
}

interface ProgressRow {
  revision: string;
  repository: string;
  doctor: boolean;
  provider: boolean;
  artifacts: string;
  updated_at?: string;
}

interface ArtifactBindingRow {
  service_id: string;
  origin: string;
  mode: string;
  publisher_token_id?: string;
}

interface RunnerRow {
  runner_id: string;
  machine_id: string;
  display_name: string;
  tags: string[];
  state: string;
  capacity_limit: number;
  project_ids: string[];
  /** A plain number, unlike the integration and progress revisions. */
  revision: number;
  hostname: string;
  health: string;
  os: string;
  architecture: string;
  last_heartbeat_at: string;
}

interface RunnerEligibilityRow {
  runner: RunnerRow;
  /** Empty is what makes the wizard's third step ready. */
  exclusions: Array<{ code: string; message: string }>;
}

interface PolicyRow {
  policy: Record<string, unknown>;
  approved_by: string;
  approved_at: string;
}

interface SupportRow {
  actor: string;
  reason: string;
  expires_at: string;
}

/**
 * The other organization the actor belongs to. Switching is a redirect through
 * that organization's own origin rather than a state change here, which is why
 * a row carries a `public_url` at all.
 */
const OTHER_ORGANIZATION = {
  id: "org_second",
  name: "Second mock organization",
  public_url: "https://second.mock.test",
};

const MOCK_ORGANIZATIONS = [
  { ...ORGANIZATION, public_url: "https://mock.test", current: true },
  { ...OTHER_ORGANIZATION, current: false },
];

/**
 * `tracker.NativeState` as the bootstrap and `GET /projects` both carry it.
 * Todo is the only dispatchable lane and Done the only terminal one, which is
 * the shape every lane control is written against.
 */
const WORKFLOW_STATES = [
  { name: "Todo", terminal: false, dispatchable: true, transitions: ["In Progress"] },
  { name: "In Progress", terminal: false, dispatchable: false, transitions: ["Done"] },
  { name: "Done", terminal: true, dispatchable: false, transitions: [] },
];

/**
 * `onboarding.Evaluate` emits exactly these four, in this order, and the name
 * is the step's identity — there are no ids. The detail says what the step
 * needs, which does not change with whether it has it, so only `state` moves.
 */
const ONBOARDING_STEP_TEXT: ReadonlyArray<{ name: string; detail: string }> = [
  {
    name: "Repository configuration",
    detail:
      "Inspect detent.yaml and WORKFLOW.md on the customer host, then explicitly approve the resolved policy descriptor.",
  },
  {
    name: "Local validation",
    detail:
      "User-reported: run detent doctor and sign in to the selected provider on the execution host. Credentials stay local.",
  },
  {
    name: "Execution runner",
    detail:
      "Requires approved project access, matching tags and host selectors, a fresh heartbeat and available capacity.",
  },
  {
    name: "Artifact history",
    detail:
      "Choose local history or configure the customer S3-compatible service and independent gateway. A binding does not verify storage or promise offline access.",
  },
];

/**
 * The conversation half's projects given the account half's fields. The two
 * writable ones are native; the read-only one is GitHub-compatible, so both
 * intake profiles are on screen at once. Runner management follows write
 * access here: the mock has no member who can manage runners but not write.
 */
const PROJECT_PROFILES: Record<string, string> = {
  proj_alpha: "native",
  proj_beta: "native",
  proj_readonly: "github_compatible",
};

const SEED_PROJECTS: ProjectRow[] = PROJECTS.map((project) => ({
  ...project,
  profile: PROJECT_PROFILES[project.id] ?? "native",
  can_manage_runners: project.can_write,
}));

/** The roles `auth.ValidOrganizationRole` accepts. */
const ORGANIZATION_ROLES = ["owner", "admin", "member", "viewer"];

/**
 * Three members and one pending invitation. The owner is the signed-in actor,
 * so "members see only themselves" has a row to return, and the last-owner
 * refusal has a live list to be computed against rather than a hard-coded id.
 */
const SEED_MEMBERS: MemberRow[] = [
  {
    id: "mem_owner",
    user_id: ACTOR.subject,
    email: ACTOR.email,
    role: "owner",
    status: "active",
    grants: [{ project_id: "proj_alpha", write: true, runner: true }],
  },
  {
    id: "mem_admin",
    user_id: "usr_dana",
    email: "dana@example.test",
    role: "admin",
    status: "active",
    grants: [
      { project_id: "proj_alpha", write: true, runner: false },
      { project_id: "proj_beta", write: true, runner: false },
    ],
  },
  {
    id: "mem_viewer",
    user_id: "usr_sam",
    email: "sam@example.test",
    role: "viewer",
    status: "active",
    grants: [{ project_id: "proj_readonly", write: false, runner: false }],
  },
];

const SEED_INVITATIONS: InvitationRow[] = [
  {
    id: "inv_seed",
    email: "rae@example.test",
    role: "member",
    created_at: "2026-09-09T14:02:11Z",
    expires_at: "2026-09-10T14:02:11Z",
  },
];

/** The one token `POST /invitations/accept` recognises. Anything else is 404. */
const SEED_INVITE_TOKEN = "invite_ok";

/**
 * `ProjectIntegration.authority`: who owns each field once a project mirrors a
 * repository. The screen renders it read-only, so one seed serves every
 * project.
 */
const SEED_AUTHORITY: Record<string, string> = {
  title: "detent",
  body: "detent",
  discussion: "detent",
  dependencies: "detent",
  authors: "detent",
  workflow: "detent",
  labels: "detent",
  assignees: "detent",
  priority: "detent",
  source_timestamps: "source",
  scheduling: "detent",
  progress: "detent",
  repository_policy: "trusted_repository_revision",
  github_merge: "github_branch_protections_and_fresh_checks",
  native_approval: "detent_only",
};

const SEED_INTEGRATIONS: Record<string, IntegrationRow> = {
  proj_alpha: {
    profile: "native",
    revision: "7",
    intake: "manual",
    projection: "summary",
    repository_enabled: true,
    repository: "mockorg/alpha",
    authority: SEED_AUTHORITY,
  },
  proj_beta: {
    profile: "native",
    revision: "1",
    intake: "disabled",
    projection: "disabled",
    repository_enabled: false,
    authority: SEED_AUTHORITY,
  },
  proj_readonly: {
    profile: "github_compatible",
    revision: "3",
    intake: "manual",
    projection: "summary",
    repository_enabled: true,
    repository: "mockorg/readonly",
    authority: SEED_AUTHORITY,
  },
};

/** `policy.Approval`: the resolved descriptor a human approved by identity. */
const SEED_POLICY: PolicyRow = {
  policy: {
    schema: 1,
    policy_id: "pol_mock01",
    source_revision: "3f8f74e0",
    source_digest: "sha256:6ac1f0b7d2",
    config_digest: "sha256:1d90ae44c7",
    profile: "native",
    requirements: { required_tags: ["detent:mock"], runner_id: "", machine_id: "" },
    gates: {
      kind: "pull_request",
      plan_enabled: false,
      plan_review: "",
      plan_stop_digest: "",
      auto_promote: false,
      automated_review: "required",
      required_checks: 2,
      validator: true,
      security_audit: false,
      merge_method: "squash",
    },
  },
  approved_by: ACTOR.email,
  approved_at: "2026-09-08T09:12:44Z",
};

/**
 * `proj_alpha` is mid-wizard — policy approved and a runner eligible, local
 * validation and the artifact choice still open — so the stepper has a mixed
 * state to draw. `proj_beta` has nothing approved, which is what gives
 * `GET /policy` somewhere to answer 404.
 */
const SEED_PROGRESS: Record<string, ProgressRow> = {
  proj_alpha: {
    revision: "3",
    repository: "existing",
    doctor: true,
    provider: false,
    artifacts: "",
    updated_at: "2026-09-10T12:00:00Z",
  },
  proj_beta: { revision: "1", repository: "", doctor: false, provider: false, artifacts: "" },
  proj_readonly: {
    revision: "1",
    repository: "existing",
    doctor: true,
    provider: true,
    artifacts: "local",
  },
};

/**
 * `runnerauth.Eligibility` as the onboarding payload redacts it. One runner is
 * eligible and one is draining, so the wizard's third step is ready and the
 * reason the other cannot take work is still on screen.
 */
const SEED_RUNNERS: RunnerEligibilityRow[] = [
  {
    runner: {
      runner_id: "rnr_mock",
      machine_id: "mac_mock01",
      display_name: "Mock MacBook Pro",
      tags: ["detent:mock"],
      state: "enabled",
      capacity_limit: 2,
      project_ids: ["proj_alpha"],
      revision: 4,
      hostname: "mock-macbook.local",
      health: "healthy",
      os: "darwin",
      architecture: "arm64",
      last_heartbeat_at: "2026-09-10T12:04:31Z",
    },
    exclusions: [],
  },
  {
    runner: {
      runner_id: "rnr_mock_mini",
      machine_id: "mac_mock02",
      display_name: "mock-mini-1",
      tags: ["detent:mock-mini-1"],
      state: "paused",
      capacity_limit: 2,
      project_ids: ["proj_alpha"],
      revision: 2,
      hostname: "mock-mini-1.local",
      health: "stale",
      os: "darwin",
      architecture: "arm64",
      last_heartbeat_at: "2026-09-10T10:02:00Z",
    },
    exclusions: [{ code: "runner_draining", message: "Runner is paused and takes no new work" }],
  },
];

/**
 * `GET /fleet`. The host numbers and the runner's own are deliberately
 * different, because the host card draws two meters rather than one, and one
 * provider account is exhausted so the limited-availability branch is live.
 */
const SEED_FLEET = {
  runners: [
    {
      id: "rnr_mock",
      display_name: "Mock MacBook Pro",
      hostname: "mock-macbook.local",
      health: "healthy",
      state: "enabled",
      os: "darwin",
      architecture: "arm64",
      host_capacity: 2,
      host_used: 1,
      capacity_limit: 2,
      reported_capacity: 2,
      provider_capacity: [
        {
          provider: "codex",
          backend: "codex",
          account_alias: ACTOR.email,
          models: ["gpt-6-astra", "gpt-5.6-sol"],
          max_concurrent: 2,
          availability: "available",
          observed_at: "2026-09-10T12:04:31Z",
          used: 1,
          state: "ready",
          reason: "",
        },
        {
          provider: "claude",
          backend: "claude_code",
          account_alias: "mock-max",
          models: ["claude-fable-5-1", "claude-opus-5"],
          max_concurrent: 1,
          availability: "limited",
          observed_at: "2026-09-10T12:04:31Z",
          reset_at: "2026-09-10T17:00:00Z",
          used: 1,
          state: "exhausted",
          reason: "Five hour window is spent",
        },
      ],
      last_heartbeat_at: "2026-09-10T12:04:31Z",
      leases: [
        {
          lease_id: "lease_mock1",
          work_item_id: "wi_1001",
          title: "checklock: renew waits on healthy handoffs",
          project_id: "proj_alpha",
          expires_at: "2026-09-10T12:09:31Z",
        },
      ],
    },
    {
      id: "rnr_mock_mini",
      display_name: "mock-mini-1",
      hostname: "mock-mini-1.local",
      health: "stale",
      state: "paused",
      os: "darwin",
      architecture: "arm64",
      host_capacity: 2,
      host_used: 0,
      capacity_limit: 2,
      reported_capacity: 0,
      provider_capacity: [],
      last_heartbeat_at: "2026-09-10T10:02:00Z",
      leases: [],
    },
  ],
  usage: {
    window_ends_at: "2026-09-10T13:00:00Z",
    allowances: {
      members: { used: 3, limit: 10 },
      projects: { used: 3, limit: 10 },
      connected_runners: { used: 1, limit: 10 },
      concurrent_work: { used: 1, limit: 5 },
      api_mutations: { used: 11840, limit: 10000 },
      ingested_events: { used: 2044, limit: 10000 },
    },
  },
  spend: {
    today: 12.4,
    window: 118.12,
    currency: "USD",
    by_project: [
      { project_id: "proj_alpha", amount: 58.31 },
      { project_id: "proj_beta", amount: 34.06 },
    ],
    series: [
      { at: "2026-09-04T00:00:00Z", amount: 4.2 },
      { at: "2026-09-05T00:00:00Z", amount: 9.6 },
      { at: "2026-09-06T00:00:00Z", amount: 18.4 },
      { at: "2026-09-07T00:00:00Z", amount: 23.9 },
      { at: "2026-09-08T00:00:00Z", amount: 15.1 },
      { at: "2026-09-09T00:00:00Z", amount: 8.8 },
      { at: "2026-09-10T00:00:00Z", amount: 12.4 },
    ],
  },
};

/**
 * `GET /usage?range=`. Decisions.md §17.5: one report per window, assembled
 * from the per-attempt usage the runners report. The numbers are generated
 * rather than listed so the 90-day window is as realistic as the 7-day one,
 * and the generator is deterministic (a small LCG seeded per bucket) so the
 * fixtures, the mock hub and a screenshot never disagree.
 *
 * The clock is pinned to the same instant the rest of this file uses, because
 * a fixture that moved every day would not be a fixture.
 */
const USAGE_NOW = Date.parse("2026-09-10T12:00:00Z");
const HOUR_MS = 60 * 60 * 1000;
const DAY_MS = 24 * HOUR_MS;

const USAGE_PROVIDERS = [
  { id: "codex", label: "Codex" },
  { id: "claude", label: "Claude Code" },
] as const;

/**
 * The six models the fleet actually dispatches, with the per-million prices
 * the hub's `usage.prices` table carries for them. `weight` is the share of
 * turns the coordinator routes to that model, which is what makes one model
 * dominate the breakdown the way a real fleet's does.
 */
const USAGE_MODELS = [
  { model: "gpt-6-astra", provider: "codex", weight: 0.42, input: 1.25, output: 10 },
  { model: "gpt-5.6-sol", provider: "codex", weight: 0.14, input: 0.6, output: 4.8 },
  { model: "gpt-5.6-terra", provider: "codex", weight: 0.07, input: 0.25, output: 2 },
  { model: "claude-opus-5", provider: "claude", weight: 0.16, input: 5, output: 25 },
  { model: "claude-fable-5-1", provider: "claude", weight: 0.13, input: 3, output: 15 },
  { model: "claude-sonnet-5", provider: "claude", weight: 0.08, input: 1, output: 5 },
] as const;

const USAGE_RANGES = {
  "24h": { buckets: 24, step: HOUR_MS, hourly: true },
  "7d": { buckets: 7, step: DAY_MS, hourly: false },
  "30d": { buckets: 30, step: DAY_MS, hourly: false },
  "90d": { buckets: 90, step: DAY_MS, hourly: false },
} as const;

export type UsageRangeKey = keyof typeof USAGE_RANGES;

/** A stable 0..1 from two integers. No state, so any bucket can be recomputed. */
function jitter(seed: number, salt: number): number {
  const value = Math.sin(seed * 12.9898 + salt * 78.233) * 43758.5453;
  return value - Math.floor(value);
}

function round(value: number, places: number): number {
  const scale = 10 ** places;
  return Math.round(value * scale) / scale;
}

/**
 * The activity multiplier for one bucket: a fleet that has been ramping up,
 * quieter at the weekend and overnight, with a plausible amount of noise.
 */
function intensity(index: number, count: number, hourly: boolean, startMs: number, step: number): number {
  const ramp = 0.35 + 0.65 * ((index + 1) / count);
  const at = new Date(startMs + index * step);
  const seasonal = hourly
    ? 0.25 + 0.75 * Math.max(0, Math.sin(((at.getUTCHours() - 5) / 24) * Math.PI * 2) + 0.35)
    : at.getUTCDay() === 0 || at.getUTCDay() === 6
      ? 0.3
      : 1;
  return ramp * seasonal * (0.65 + 0.7 * jitter(index, 7));
}

/**
 * One window's report. Every number below is derived from the same per-bucket,
 * per-model token counts, so the hero total, the provider rows, the chart, the
 * totals grid and both breakdowns are guaranteed to agree.
 */
export function usageReport(range: UsageRangeKey): Record<string, unknown> {
  const { buckets, step, hourly } = USAGE_RANGES[range];
  const endMs = hourly
    ? Math.floor(USAGE_NOW / HOUR_MS) * HOUR_MS
    : Date.parse(`${new Date(USAGE_NOW).toISOString().slice(0, 10)}T00:00:00Z`);
  const startMs = endMs - (buckets - 1) * step;
  const label = (ms: number) =>
    hourly ? new Date(ms).toISOString() : new Date(ms).toISOString().slice(0, 10);

  const modelTotals = new Map(
    USAGE_MODELS.map((model) => [
      model.model,
      { cost: 0, tokens: 0, cached: 0, uncached: 0, output: 0, sessions: 0 },
    ]),
  );
  const daily: {
    day: string;
    cost: number;
    tokens: number;
    by_provider: Record<string, number>;
  }[] = [];

  for (let index = 0; index < buckets; index += 1) {
    const scale = intensity(index, buckets, hourly, startMs, step);
    const byProvider: Record<string, number> = { codex: 0, claude: 0 };
    let bucketCost = 0;
    let bucketTokens = 0;
    for (const [modelIndex, model] of USAGE_MODELS.entries()) {
      // A turn's input is mostly a re-read of the thread, so most of it is
      // cached; the output is a fraction of it. The shape, not the numbers,
      // is what the Totals grid is showing.
      const turns = (hourly ? 3 : 34) * model.weight * scale * (0.6 + 0.8 * jitter(index, modelIndex + 11));
      const uncached = turns * 5_400;
      const cached = turns * 118_000;
      const output = turns * 2_900;
      const cost =
        ((uncached * model.input + cached * model.input * 0.1) / 1e6) +
        (output * model.output) / 1e6;
      const totals = modelTotals.get(model.model)!;
      totals.cost += cost;
      totals.tokens += uncached + cached + output;
      totals.cached += cached;
      totals.uncached += uncached;
      totals.output += output;
      totals.sessions += turns / 6;
      byProvider[model.provider] = (byProvider[model.provider] ?? 0) + cost;
      bucketCost += cost;
      bucketTokens += uncached + cached + output;
    }
    daily.push({
      day: label(startMs + index * step),
      cost: round(bucketCost, 2),
      tokens: Math.round(bucketTokens),
      by_provider: {
        codex: round(byProvider.codex ?? 0, 2),
        claude: round(byProvider.claude ?? 0, 2),
      },
    });
  }

  const cost = [...modelTotals.values()].reduce((sum, entry) => sum + entry.cost, 0);
  const tokens = [...modelTotals.values()].reduce((sum, entry) => sum + entry.tokens, 0);
  const sessions = Math.round(
    [...modelTotals.values()].reduce((sum, entry) => sum + entry.sessions, 0),
  );
  const cached = [...modelTotals.values()].reduce((sum, entry) => sum + entry.cached, 0);
  const uncached = [...modelTotals.values()].reduce((sum, entry) => sum + entry.uncached, 0);
  const output = [...modelTotals.values()].reduce((sum, entry) => sum + entry.output, 0);

  const providers = USAGE_PROVIDERS.map((provider) => {
    const models = USAGE_MODELS.filter((model) => model.provider === provider.id);
    const providerCost = models.reduce((sum, model) => sum + modelTotals.get(model.model)!.cost, 0);
    const providerTokens = models.reduce(
      (sum, model) => sum + modelTotals.get(model.model)!.tokens,
      0,
    );
    const providerSessions = models.reduce(
      (sum, model) => sum + modelTotals.get(model.model)!.sessions,
      0,
    );
    return {
      id: provider.id,
      label: provider.label,
      sessions: Math.round(providerSessions),
      cost: round(providerCost, 2),
      share: cost === 0 ? 0 : round(providerCost / cost, 4),
      tokens: Math.round(providerTokens),
    };
  });

  const byModel = USAGE_MODELS.map((model) => {
    const totals = modelTotals.get(model.model)!;
    return {
      model: model.model,
      provider: model.provider,
      cost: round(totals.cost, 2),
      share: cost === 0 ? 0 : round(totals.cost / cost, 4),
      tokens: Math.round(totals.tokens),
    };
  }).sort((left, right) => right.cost - left.cost);

  // The runners are the fleet's own two hosts, split by the share of the
  // window each of them was actually under lease for.
  const runnerSplit = [
    { id: "rnr_mock", display_name: "Mock MacBook Pro", share: 0.72, capacity: 2 },
    { id: "rnr_mock_mini", display_name: "mock-mini-1", share: 0.28, capacity: 2 },
  ];
  const windowSeconds = (buckets * step) / 1000;
  const runners = runnerSplit.map((runner) => ({
    id: runner.id,
    display_name: runner.display_name,
    sessions: Math.round(sessions * runner.share),
    tokens: Math.round(tokens * runner.share),
    cost: round(cost * runner.share, 2),
    busy_seconds: Math.round(windowSeconds * runner.share * 0.31),
    capacity_used: round(runner.share * 0.31 * runner.capacity, 2),
  }));

  return {
    range: { from: new Date(startMs).toISOString(), to: new Date(endMs + step).toISOString() },
    total: { cost: round(cost, 2), tokens: Math.round(tokens), sessions },
    providers,
    daily,
    totals: {
      processed: Math.round(tokens),
      cached_input: Math.round(cached),
      uncached_input: Math.round(uncached),
      output: Math.round(output),
      // What the cached input would have cost at the uncached price.
      cache_savings: round(
        USAGE_MODELS.reduce(
          (sum, model) => sum + (modelTotals.get(model.model)!.cached * model.input * 0.9) / 1e6,
          0,
        ),
        2,
      ),
    },
    breakdown: { by_model: byModel, by_day: daily },
    limits: {
      members: { used: 3, limit: 10 },
      projects: { used: 3, limit: 10 },
      connected_runners: { used: 1, limit: 10 },
      concurrent_work: { used: 1, limit: 5 },
      api_mutations: { used: 11840, limit: 10000 },
      ingested_events: { used: 2044, limit: 10000 },
    },
    runners,
    currency: "USD",
  };
}

/** An organization that has run nothing yet: every list empty, every total zero. */
export function emptyUsageReport(): Record<string, unknown> {
  return {
    range: { from: "2026-08-12T00:00:00Z", to: "2026-09-11T00:00:00Z" },
    total: { cost: 0, tokens: 0, sessions: 0 },
    providers: [],
    daily: [],
    totals: { processed: 0, cached_input: 0, uncached_input: 0, output: 0, cache_savings: 0 },
    breakdown: { by_model: [], by_day: [] },
    limits: {
      members: { used: 1, limit: 10 },
      projects: { used: 0, limit: 10 },
    },
    runners: [],
    currency: "USD",
  };
}

/** The plan summary the shell carries so the settings page can name it. */
const SEED_PLAN_SUMMARY = {
  id: "pilot_free",
  name: "pilot_free, version 1",
  source: "base",
  window_ends_at: "2026-09-10T13:00:00Z",
};

/** `GET /plan`: `hubserver.HostedEntitlement`, verbatim. */
const SEED_PLAN_REPORT = {
  organization_id: ORGANIZATION.id,
  base: { id: "pilot_free", version: 1 },
  effective_base: { id: "pilot_free", version: 1 },
  source: "base",
  revision: 4,
  features: ["collaboration", "native_execution", "github_integration"],
  allowances: {
    members: 10,
    projects: 10,
    repositories: 10,
    registered_runners: 10,
    connected_runners: 10,
    concurrent_work: 5,
    api_mutations: 10000,
    ingested_events: 10000,
    collaboration_bytes: 67108864,
    history_records: 10000,
  },
  grants: [
    {
      id: "grant_pilot",
      plan: { id: "pilot_plus", version: 2 },
      scope: ["hosted_artifacts"],
      starts_at: "2026-08-01T00:00:00Z",
      expires_at: "2026-10-01T00:00:00Z",
    },
  ],
  usage: {
    members: 3,
    projects: 3,
    repositories: 2,
    registered_runners: 2,
    connected_runners: 1,
    concurrent_work: 1,
    api_mutations: 11840,
    ingested_events: 2044,
    collaboration_bytes: 1048576,
    history_records: 512,
  },
  window_ends_at: "2026-09-10T13:00:00Z",
};

/**
 * `GET /billing`. `prices` is on the payload because the report alone cannot
 * render the screen: the hosted page read the configured prices from its own
 * config, and §12's "checkout buttons per configured price" needs them here.
 */
const SEED_BILLING = {
  organization_id: ORGANIZATION.id,
  state: {
    subscription: {
      subscription_id: "sub_mock01",
      price_id: "price_team_monthly",
      status: "active",
      invoice_id: "in_mock01",
      invoice_status: "paid",
      invoice_created_at: "2026-09-01T00:00:00Z",
      period_end: "2026-10-01T00:00:00Z",
      cancel_at_period_end: false,
      payment_hold: "",
    },
    status: "active",
    plan: { id: "pilot_team", version: 2 },
    paid_through: "2026-10-01T00:00:00Z",
    access_until: "2026-10-01T00:00:00Z",
    grace_until: "2026-10-08T00:00:00Z",
  },
  entitlement: SEED_PLAN_REPORT,
  reconciled_at: "2026-09-10T11:58:00Z",
  pending_events: 0,
  recent_audit: [
    {
      actor: ACTOR.email,
      action: "checkout_started",
      summary: "active price_team_monthly",
      at: "2026-09-01T00:00:00Z",
    },
  ],
  prices: [
    { id: "price_team_monthly", label: "Team, 40 per month" },
    { id: "price_team_yearly", label: "Team, 400 per year" },
  ],
};

/** An open support session: somebody from Detent acting as this organization. */
const SEED_SUPPORT: SupportRow = {
  actor: "support@detent.dev",
  reason: "Investigating a stuck lease reported in ticket 4471",
  expires_at: "2026-09-10T22:30:00Z",
};

/** The build the bootstrap's About row names. */
const HOSTED_VERSION = "v0.0.0-mock";

/**
 * Everything the account endpoints mutate. It is a second store beside the
 * conversation one: the two share the read-only switch and the idempotency
 * conventions and nothing else, and `/__mock/reset` restores both.
 */
interface AccountState {
  organizations: Array<{ id: string; name: string; public_url: string; current: boolean }>;
  projects: ProjectRow[];
  members: MemberRow[];
  invitations: InvitationRow[];
  support: SupportRow | null;
  /** False once `POST /__mock/fleet {"spend": null}` has run. */
  spend: boolean;
  /** False once `POST /__mock/usage {"usage": null}` has run: §17.5's empty report. */
  usage: boolean;
  integrations: Map<string, IntegrationRow>;
  policies: Map<string, PolicyRow>;
  progress: Map<string, ProgressRow>;
  artifactServices: Map<string, ArtifactBindingRow[]>;
  runners: RunnerEligibilityRow[];
  /** Runner identities already registered; re-enrolling one is a collision. */
  enrolled: Set<string>;
  workItems: number;
  /**
   * Stored non-GET responses, keyed by method, path and idempotency key. A
   * retry replays the stored response; the same key with a different payload
   * is `idempotency_conflict` (§12).
   */
  mutations: Map<string, { payload: string; status: number; response: unknown }>;
}

/** A deep copy, so a reset cannot hand back state an earlier run mutated. */
function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function initialAccountState(): AccountState {
  return {
    organizations: clone(MOCK_ORGANIZATIONS),
    projects: clone(SEED_PROJECTS),
    members: clone(SEED_MEMBERS),
    invitations: clone(SEED_INVITATIONS),
    support: null,
    spend: true,
    usage: true,
    integrations: new Map(Object.entries(clone(SEED_INTEGRATIONS))),
    policies: new Map([["proj_alpha", clone(SEED_POLICY)]]),
    progress: new Map(Object.entries(clone(SEED_PROGRESS))),
    artifactServices: new Map(),
    runners: clone(SEED_RUNNERS),
    enrolled: new Set(SEED_RUNNERS.map((entry) => entry.runner.runner_id)),
    workItems: 0,
    mutations: new Map(),
  };
}

const RUNNER_REPLY = [
  "Reading the lease renewal path. ",
  "The handoff acknowledgement lands after the renewal returns. ",
  "Moving the renewal behind it now.",
];

const CANNED_REPLY = [
  "Looking at the lock renewal path now. ",
  "The renewal returns before the handoff completes, ",
  "so the lease can lapse under load. ",
  "I would move the renewal behind the handoff acknowledgement.",
];

interface StoredEvent {
  readonly seq: number;
  readonly event: ConversationEvent;
}

interface StoredConversation {
  conversation: Conversation;
  /** The issue this conversation was linked to, if any. */
  issue: { id: string; identifier: string; title: string; state: string } | null;
  /** Monotonic attempt counter behind the scripted runner. */
  attempts: number;
  /** The assistant message the scripted runner is streaming into. */
  runnerMessageId: string | null;
  messages: Message[];
  questions: Question[];
  events: StoredEvent[];
  receipts: Map<string, { payload: string; receipt: Receipt }>;
  /**
   * Attachments uploaded against this conversation and not yet sent
   * (decisions.md §17.1), keyed by id. A `message` command naming one moves it
   * onto the message; `DELETE` drops it.
   */
  attachments: Map<string, MockAttachment>;
  /** Upload idempotency keys already answered, so a retry returns the same id. */
  attachmentKeys: Map<string, string>;
  seq: number;
  messageSeq: number;
  subscribers: Set<Subscriber>;
}

/** The hub's own limits (decisions.md §17.1). */
const MAX_ATTACHMENT_BYTES = 20 * 1024 * 1024;
const MAX_ATTACHMENTS_PER_MESSAGE = 10;

/** `POST .../attachments` → 201, plus the `expires_at` the contract carries. */
interface MockAttachment {
  id: string;
  name: string;
  mime: string;
  size: number;
  url: string;
  expires_at: string;
}

interface Subscriber {
  readonly response: ServerResponse;
  /** Frames still to deliver before the injected drop fires. */
  dropAfter: number | null;
  delivered: number;
}

/**
 * `hub` answers an unlinked message in place (the transitional hub-side
 * coordinator). `runner` queues it for a runner-dispatched coordinator attempt.
 * `none` is a project with neither, so the bootstrap reports no coordinator.
 */
/**
 * The picker choices the composer offers (decisions.md §14). The hub builds
 * these from the enrolled runners' provider reports and policy; the mock
 * publishes a fixed set so the pickers have something to open.
 */
const PREFERENCE_CHOICES = {
  models: [
    { id: "auto", label: "Auto", default: false },
    { id: "gpt-6-astra", label: "Codex Astra", default: true },
    { id: "claude-opus-5", label: "Claude Opus 5", default: false },
  ],
  efforts: [
    { id: "auto", label: "Auto", default: false },
    { id: "low", label: "Low", default: true },
    { id: "medium", label: "Medium", default: false },
    { id: "high", label: "High", default: false },
  ],
  access: [
    { id: "auto", label: "Auto", default: false },
    { id: "read_only", label: "Read only", default: true },
    { id: "full", label: "Full access", default: false },
  ],
} as const;

export type CoordinatorMode = "hub" | "runner" | "none";

/**
 * Whether the signed-in account may write. `read_only` is a viewer with read
 * grants only: every project reports `can_write: false` and every mutation is
 * `403 forbidden` (decisions.md §10.11).
 */
export type AccountMode = "write" | "read_only";

export interface MockHubOptions {
  readonly port?: number;
  /** Delay between streamed deltas. Tests set 0. */
  readonly deltaDelayMs?: number;
  readonly heartbeatMs?: number;
  /** Default `hub`, or `MOCK_COORDINATOR` when it names a mode. */
  readonly coordinator?: CoordinatorMode;
  /** Default `write`, or `MOCK_ACCOUNT` when it names a mode. */
  readonly account?: AccountMode;
}

/** The enrollment request body, as `POST /runner-enrollments` received it. */
export interface MockEnrollmentRequest {
  readonly runner_id: string;
  readonly machine_id: string;
  readonly project_ids: readonly string[];
  readonly operations: readonly string[];
  readonly ttl_seconds: number;
}

export interface MockHub {
  readonly url: string;
  readonly close: () => Promise<void>;
  readonly server: Server;
  /**
   * The last enrollment this hub was asked for, or `null`. The 201 carries
   * only `{id, token, expires_at}`, so the projects and operations a caller
   * asked for are only assertable from the request it sent.
   */
  readonly lastEnrollment: () => MockEnrollmentRequest | null;
}

const now = () => new Date().toISOString();

function idleExecution(): Execution {
  return {
    status: "idle",
    attempt_id: null,
    run_id: null,
    runner_id: null,
    thread_id: null,
    turn_id: null,
    capabilities: { steer: false, interrupt: true, answer: true, continue: true },
    // No runner has bound, so there is nothing to have resumed from
    // (decisions.md §10.4).
    resume: "",
    error: null,
    updated_at: now(),
  };
}

function readCoordinatorMode(value: string | undefined | null): CoordinatorMode | null {
  return value === "hub" || value === "runner" || value === "none" ? value : null;
}

function readAccountMode(value: string | undefined | null): AccountMode | null {
  return value === "write" || value === "read_only" ? value : null;
}

export function startMockHub(options: MockHubOptions = {}): Promise<MockHub> {
  const deltaDelayMs = options.deltaDelayMs ?? 25;
  const heartbeatMs = options.heartbeatMs ?? 15_000;
  // Sticky, because it is a property of the fleet the hub is talking to rather
  // than of one request: whoever flips it flips it for every later call.
  let coordinatorMode: CoordinatorMode =
    options.coordinator ?? readCoordinatorMode(process.env.MOCK_COORDINATOR) ?? "hub";
  let accountMode: AccountMode =
    options.account ?? readAccountMode(process.env.MOCK_ACCOUNT) ?? "write";
  const store = new Map<string, StoredConversation>();
  const issues = new Map<string, { id: string; identifier: string; title: string; state: string }>();
  // Creating a conversation is idempotent by actor and key (decisions.md
  // §10.2). The stored response is returned verbatim; a different payload
  // under the same key is `idempotency_conflict`.
  const creates = new Map<string, { payload: string; status: number; response: unknown }>();

  // Failure injection, driven by the `/__mock/*` control endpoints.
  const injected = {
    queueFullOnce: false,
    unknownOnce: false,
    dropAfterFrames: null as number | null,
    expireCursorsBelow: 0,
    revokeAccess: false,
    serverErrorOnce: false,
    /** Refuses the next attachment upload, for the failed-upload path. */
    uploadFailsOnce: false,
  };

  function conversationOrNull(id: string): StoredConversation | null {
    return store.get(id) ?? null;
  }

  function emit(entry: StoredConversation, event: ConversationEvent): number {
    entry.seq += 1;
    const seq = entry.seq;
    entry.events.push({ seq, event });
    entry.conversation = { ...entry.conversation, event_seq: seq };
    for (const subscriber of [...entry.subscribers]) {
      writeFrame(entry, subscriber, seq, event);
    }
    return seq;
  }

  function writeFrame(
    entry: StoredConversation,
    subscriber: Subscriber,
    seq: number | null,
    event: ConversationEvent,
  ): void {
    if (subscriber.response.writableEnded) {
      entry.subscribers.delete(subscriber);
      return;
    }
    const id = seq === null ? "" : `id: ${seq}\n`;
    subscriber.response.write(`${id}event: ${event.type}\ndata: ${JSON.stringify(event.data)}\n\n`);
    subscriber.delivered += 1;
    if (subscriber.dropAfter !== null && subscriber.delivered >= subscriber.dropAfter) {
      entry.subscribers.delete(subscriber);
      subscriber.response.destroy();
    }
    if (event.type === "closed") {
      entry.subscribers.delete(subscriber);
      subscriber.response.end();
    }
  }

  function touch(entry: StoredConversation, patch: Partial<Conversation> = {}): void {
    entry.conversation = {
      ...entry.conversation,
      ...patch,
      revision: entry.conversation.revision + 1,
      updated_at: now(),
    };
    emit(entry, { type: "conversation.updated", data: entry.conversation });
  }

  function setExecution(entry: StoredConversation, execution: Partial<Execution>): void {
    const next: Execution = {
      ...entry.conversation.execution,
      ...execution,
      updated_at: now(),
    };
    entry.conversation = { ...entry.conversation, execution: next };
    emit(entry, { type: "execution.updated", data: next });
  }

  function addMessage(
    entry: StoredConversation,
    message: Omit<Message, "seq" | "references"> & { references?: Message["references"] },
  ): Message {
    entry.messageSeq += 1;
    // The hub extracts references on accept and the resource always carries
    // the array (decisions.md §14); the mock resolves the `#123` and `conv_…`
    // tokens it can actually see.
    const stored: Message = {
      ...message,
      references: message.references ?? extractReferences(entry, message.text),
      seq: entry.messageSeq,
    };
    entry.messages.push(stored);
    // The whole history, not the loaded page: the handoff form states this
    // number before it shares any of it (decisions.md §10.5). The next
    // `touch` publishes it; a count is not on its own worth a revision.
    entry.conversation = { ...entry.conversation, message_count: entry.messages.length };
    emit(entry, { type: "message.accepted", data: stored });
    return stored;
  }

  /**
   * The mock's half of the hub's reference extraction (decisions.md §14):
   * `#123`, `project#123`, `org/project#123` and a conversation id, resolved
   * against what this mock actually has.
   */
  function extractReferences(entry: StoredConversation, text: string): Message["references"] {
    const found: Array<{ kind: "issue" | "conversation"; id: string; label: string; url: string }> =
      [];
    const seen = new Set<string>();
    const issuePattern =
      /(^|[^0-9A-Za-z_/#-])((?:[0-9A-Za-z][0-9A-Za-z._-]*\/)?(?:[0-9A-Za-z][0-9A-Za-z._-]*)?#[0-9]{1,10})/g;
    for (const match of text.matchAll(issuePattern)) {
      const label = match[2] ?? "";
      const number = label.slice(label.lastIndexOf("#") + 1);
      const issue = [...issues.values()].find((candidate) =>
        candidate.identifier.endsWith(`#${number}`),
      );
      if (issue === undefined || seen.has(label)) continue;
      seen.add(label);
      found.push({ kind: "issue", id: issue.id, label, url: `/work/i/${issue.id}` });
    }
    for (const match of text.matchAll(/\bconv_[0-9a-f]{32}\b/g)) {
      const label = match[0];
      if (seen.has(label) || !store.has(label) || label === entry.conversation.id) continue;
      seen.add(label);
      found.push({ kind: "conversation", id: label, label, url: `/chat/c/${label}` });
    }
    return found;
  }

  function updateMessage(entry: StoredConversation, id: string, patch: Partial<Message>): void {
    const index = entry.messages.findIndex((message) => message.id === id);
    if (index < 0) return;
    const next = { ...entry.messages[index]!, ...patch, updated_at: now() };
    entry.messages[index] = next;
    emit(entry, { type: "message.updated", data: next });
  }

  /**
   * A `role: system`, `kind: status` message is what a runner's `item` turn
   * event becomes (decisions.md §9.3); the hub-side coordinator writes the same
   * card as `role: assistant`. Both are history, and the client renders the
   * card off `kind` and `data`, never off the role.
   */
  function addStatusMessage(
    entry: StoredConversation,
    text: string,
    data: Record<string, unknown>,
    from: "coordinator" | "runner" = "coordinator",
  ): Message {
    return addMessage(entry, {
      id: `msg_${randomUUID().replace(/-/g, "").slice(0, 12)}`,
      conversation_id: entry.conversation.id,
      role: from === "runner" ? "system" : "assistant",
      kind: "status",
      text,
      data,
      delivery: "completed",
      attempt_id: entry.conversation.execution.attempt_id,
      turn_id: entry.conversation.execution.turn_id,
      provider_item_id: null,
      actor:
        from === "runner"
          ? { kind: "runner", principal_id: "tok_runner" }
          : { kind: "coordinator", principal_id: "tok_hub" },
      command_key: null,
      created_at: now(),
      updated_at: now(),
    });
  }

  /** Streams a canned coordinator reply as deltas, then a final message. */
  async function runCoordinatorTurn(entry: StoredConversation): Promise<void> {
    setExecution(entry, { status: "running", turn_id: `turn_${entry.seq}` });
    const assistant = addMessage(entry, {
      id: `msg_${randomUUID().replace(/-/g, "").slice(0, 12)}`,
      conversation_id: entry.conversation.id,
      role: "assistant",
      kind: "text",
      text: "",
      data: {},
      delivery: "responding",
      attempt_id: null,
      turn_id: entry.conversation.execution.turn_id,
      provider_item_id: null,
      actor: { kind: "coordinator", principal_id: "tok_hub" },
      command_key: null,
      created_at: now(),
      updated_at: now(),
    });
    let text = "";
    for (const [index, chunk] of CANNED_REPLY.entries()) {
      if (deltaDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, deltaDelayMs));
      if (!store.has(entry.conversation.id)) return;
      text += chunk;
      // The stored message accumulates too, without an update event: a client
      // that re-snapshots mid-turn gets what has been written so far, and the
      // deltas after its cursor continue from there (decisions.md §5).
      const stored = entry.messages.findIndex((message) => message.id === assistant.id);
      if (stored >= 0) entry.messages[stored] = { ...entry.messages[stored]!, text };
      emit(entry, {
        type: "message.delta",
        data: { message_id: assistant.id, seq: index + 1, text: chunk },
      });
    }
    updateMessage(entry, assistant.id, { text, delivery: "completed" });
    setExecution(entry, { status: "completed", turn_id: null });
    touch(entry, { last_message_at: now() });
  }

  // --- The scripted runner ------------------------------------------------
  //
  // A linked conversation is driven from the test hooks under
  // `/__mock/runner/<conversation>/...` rather than by a real attempt. The
  // transitions are the ones decisions.md §5 names, in the order the hub would
  // emit them, so the client can be exercised against every execution status
  // without a runner, a lease or a provider.

  /**
   * Streams the runner's reply into one assistant message, left open. On an
   * unlinked chat the runner is holding a coordinator work item, so the turn is
   * attributed to the coordinator role it is running (decisions.md §9.4).
   */
  async function streamRunnerTurn(entry: StoredConversation): Promise<void> {
    const coordinating = entry.conversation.work_item_id === null;
    const assistant = addMessage(entry, {
      id: `msg_${randomUUID().replace(/-/g, "").slice(0, 12)}`,
      conversation_id: entry.conversation.id,
      role: "assistant",
      kind: "text",
      text: "",
      data: {},
      delivery: "responding",
      attempt_id: entry.conversation.execution.attempt_id,
      turn_id: entry.conversation.execution.turn_id,
      provider_item_id: null,
      actor: coordinating
        ? { kind: "coordinator", principal_id: "tok_runner" }
        : { kind: "runner", principal_id: "tok_runner" },
      command_key: null,
      created_at: now(),
      updated_at: now(),
    });
    entry.runnerMessageId = assistant.id;
    let text = "";
    for (const [index, chunk] of RUNNER_REPLY.entries()) {
      if (deltaDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, deltaDelayMs));
      if (!store.has(entry.conversation.id)) return;
      text += chunk;
      const stored = entry.messages.findIndex((message) => message.id === assistant.id);
      if (stored >= 0) entry.messages[stored] = { ...entry.messages[stored]!, text };
      emit(entry, {
        type: "message.delta",
        data: { message_id: assistant.id, seq: index + 1, text: chunk },
      });
    }
  }

  async function startRunner(
    entry: StoredConversation,
    resume: "thread" | "transcript" = "thread",
  ): Promise<void> {
    entry.attempts += 1;
    const attemptId = `att_${entry.attempts}`;
    setExecution(entry, {
      status: "starting",
      attempt_id: attemptId,
      run_id: `run_${entry.attempts}`,
      runner_id: "rnr_mock",
      thread_id: `thr_${entry.attempts}`,
      turn_id: `turn_${entry.attempts}`,
      capabilities: { steer: true, interrupt: true, answer: true, continue: true },
      resume,
      error: null,
    });
    setExecution(entry, { status: "running" });
    // Transcript recovery is visible: the execution says where the context
    // came from and the history carries a status message saying it in words
    // (decisions.md §10.4).
    if (resume === "transcript") {
      addStatusMessage(
        entry,
        `Provider history was not available on this runner; continuing from a transcript of the last ${Math.min(entry.messages.length, 20)} messages`,
        {},
        "runner",
      );
    }
    // The messages queued before the attempt bound are what it answers: they
    // reach it through the controls poll, so they leave `queued` behind
    // (decisions.md §9.1).
    for (const message of entry.messages) {
      if (message.role !== "user" || message.delivery !== "queued") continue;
      updateMessage(entry, message.id, { delivery: "sent", attempt_id: attemptId });
    }
    await streamRunnerTurn(entry);
  }

  function openRunnerQuestion(entry: StoredConversation): Question {
    const anchor = addStatusMessage(entry, "The runner needs your input.", {});
    const question: Question = {
      id: `q_${randomUUID().replace(/-/g, "").slice(0, 8)}`,
      conversation_id: entry.conversation.id,
      message_id: anchor.id,
      status: "pending",
      owner: {
        attempt_id: entry.conversation.execution.attempt_id,
        turn_id: entry.conversation.execution.turn_id,
      },
      questions: [
        {
          id: "renewal-window",
          header: "Renewal window",
          question: "Which renewal window should the lock use?",
          options: [
            { label: "Half the lease", description: "Renew at 50 percent of the lease." },
            { label: "Fixed 20 seconds", description: "Renew on a fixed cadence." },
          ],
          free_text: true,
        },
      ],
      answers: {},
      answered_by: null,
      expires_at: null,
      created_at: now(),
      updated_at: now(),
    };
    entry.questions.push(question);
    setExecution(entry, { status: "waiting_input" });
    emit(entry, { type: "question.opened", data: question });
    return question;
  }

  /**
   * The worker unbound, lost its lease, or the hub restarted: every control it
   * was handed can no longer be established, so it becomes `unknown` — for
   * text messages too — and stays there until the user retries
   * (decisions.md §10.3). A `queued` control that was never handed out stays
   * `queued`.
   */
  function loseRunner(entry: StoredConversation): void {
    for (const message of entry.messages) {
      if (message.role !== "user") continue;
      if (message.delivery !== "sending" && message.delivery !== "sent") continue;
      updateMessage(entry, message.id, { delivery: "unknown" });
    }
    if (entry.runnerMessageId !== null) {
      const open = entry.messages.find((candidate) => candidate.id === entry.runnerMessageId);
      updateMessage(entry, entry.runnerMessageId, {
        text: open?.text ?? "",
        delivery: "unknown",
      });
      entry.runnerMessageId = null;
    }
    setExecution(entry, { status: "unknown", turn_id: null });
  }

  function completeRunner(entry: StoredConversation): void {
    if (entry.runnerMessageId !== null) {
      const message = entry.messages.find(
        (candidate) => candidate.id === entry.runnerMessageId,
      );
      updateMessage(entry, entry.runnerMessageId, {
        text: message?.text ?? "",
        delivery: "completed",
      });
      entry.runnerMessageId = null;
    }
    setExecution(entry, { status: "completed", turn_id: null });
    // `propose_issue` posts an `item` event carrying `data.proposal` and creates
    // nothing (decisions.md §9.4). It arrives as a `role: system` status
    // message, which is the same card the hub-side coordinator writes.
    if (entry.conversation.work_item_id === null && wantsIssue(entry)) {
      addStatusMessage(
        entry,
        "This looks like work for a tracked issue.",
        { proposal: proposalFor(entry) },
        "runner",
      );
    }
    touch(entry, { last_message_at: now() });
  }

  /** True when the reader asked for an issue and no proposal has been posted. */
  function wantsIssue(entry: StoredConversation): boolean {
    if (entry.messages.some((message) => "proposal" in message.data)) return false;
    return entry.messages.some(
      (message) => message.role === "user" && /create an issue/i.test(message.text),
    );
  }

  function proposalFor(entry: StoredConversation): Record<string, unknown> {
    const asked = entry.messages.find(
      (message) => message.role === "user" && /create an issue/i.test(message.text),
    );
    const text = asked?.text ?? entry.conversation.title;
    return {
      project_id: entry.conversation.project_id,
      title: text.slice(0, 80),
      objective: `${text}\n\nRaised from this conversation.`,
    };
  }

  function updateQuestion(
    entry: StoredConversation,
    id: string,
    patch: Partial<Question>,
  ): Question | null {
    const index = entry.questions.findIndex((question) => question.id === id);
    if (index < 0) return null;
    const next: Question = { ...entry.questions[index]!, ...patch, updated_at: now() };
    entry.questions[index] = next;
    emit(entry, { type: "question.updated", data: next });
    return next;
  }

  function createConversation(projectId: string, title: string): StoredConversation {
    const id = `conv_${randomUUID().replace(/-/g, "")}`;
    const conversation: Conversation = {
      id,
      organization_id: ORGANIZATION.id,
      project_id: projectId,
      title,
      visibility: "private",
      status: "active",
      work_item_id: null,
      linked_at: null,
      work_item: null,
      owner: { principal_id: ACTOR.principal_id, subject: ACTOR.subject },
      execution: idleExecution(),
      preferences: { model: "auto", reasoning_effort: "auto", access: "auto" },
      revision: 1,
      event_seq: 0,
      message_count: 0,
      created_at: now(),
      updated_at: now(),
      last_message_at: null,
    };
    const entry: StoredConversation = {
      conversation,
      issue: null,
      attempts: 0,
      runnerMessageId: null,
      messages: [],
      questions: [],
      events: [],
      receipts: new Map(),
      attachments: new Map(),
      attachmentKeys: new Map(),
      seq: 0,
      messageSeq: 0,
      subscribers: new Set(),
    };
    store.set(id, entry);
    return entry;
  }

  function acceptCommand(
    entry: StoredConversation,
    body: Record<string, unknown>,
  ): { status: number; payload: unknown } {
    const key = String(body.key ?? "");
    const serialized = JSON.stringify(body);
    const existing = entry.receipts.get(key);
    if (existing !== undefined) {
      if (existing.payload !== serialized) {
        return {
          status: 409,
          payload: {
            code: "idempotency_conflict",
            message: "That key was used with a different payload.",
          },
        };
      }
      return { status: 200, payload: existing.receipt };
    }
    if (injected.queueFullOnce) {
      injected.queueFullOnce = false;
      return {
        status: 503,
        payload: { code: "queue_full", message: "The control queue is full. Retry later." },
      };
    }
    if (injected.unknownOnce) {
      injected.unknownOnce = false;
      const receipt: Receipt = {
        key,
        kind: String(body.kind ?? "message"),
        status: "unknown",
        message_id: null,
        question_id: null,
        error: { code: "invalid", message: "Delivery could not be established." },
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt });
      emit(entry, { type: "command.receipt", data: receipt });
      return { status: 200, payload: receipt };
    }

    const kind = String(body.kind ?? "message");
    const expected = (body.expected ?? {}) as { attempt_id?: string | null };
    const current = entry.conversation.execution.attempt_id;

    // The owner generation is re-validated at acceptance. A mismatch is
    // `stale_execution`, never a silent redirect to the attempt that replaced
    // it (decisions.md §2).
    if (
      kind !== "message" &&
      expected.attempt_id != null &&
      expected.attempt_id !== current
    ) {
      return {
        status: 409,
        payload: {
          code: "stale_execution",
          message: "The attempt this control targeted is no longer current.",
          details: { expected_attempt_id: expected.attempt_id, current_attempt_id: current },
        },
      };
    }

    // `retry` re-queues one message the hub already stored. It never creates a
    // second message: the same id goes back to `queued` (or `saved` on the
    // hub-side coordinator path), `message.updated` says so, and the receipt
    // for the retry key is `queued` (decisions.md §10.3).
    if (kind === "retry") {
      const messageId = String(body.message_id ?? "");
      const target = entry.messages.find((candidate) => candidate.id === messageId);
      if (
        target === undefined ||
        !["unknown", "failed", "rejected"].includes(target.delivery)
      ) {
        return {
          status: 422,
          payload: {
            code: "invalid_request",
            message:
              "Only a message whose delivery is unknown, failed or rejected can be retried.",
          },
        };
      }
      const requeued =
        entry.conversation.work_item_id === null && coordinatorMode === "hub"
          ? "saved"
          : "queued";
      updateMessage(entry, messageId, { delivery: requeued });
      const retryReceipt: Receipt = {
        key,
        kind,
        status: "queued",
        message_id: messageId,
        question_id: null,
        error: null,
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt: retryReceipt });
      emit(entry, { type: "command.receipt", data: retryReceipt });
      return { status: 200, payload: retryReceipt };
    }

    if (kind === "answer") {
      const questionId = String(body.question_id ?? "");
      const question = entry.questions.find((candidate) => candidate.id === questionId);
      if (question === undefined) {
        return { status: 404, payload: { code: "not_found", message: "No such question." } };
      }
      if (question.status === "answered") {
        return {
          status: 409,
          payload: {
            code: "question_already_answered",
            message: "That question was already answered.",
            details: { answered_by: question.answered_by },
          },
        };
      }
      const answers = (body.answers ?? {}) as Record<string, string[]>;
      updateQuestion(entry, questionId, {
        status: "answered",
        answers,
        answered_by: ACTOR.principal_id,
      });
      addMessage(entry, {
        id: `msg_${randomUUID().replace(/-/g, "").slice(0, 12)}`,
        conversation_id: entry.conversation.id,
        role: "user",
        kind: "answer",
        text: Object.values(answers).flat().join(", "),
        data: { question_id: questionId },
        delivery: "sent",
        attempt_id: current,
        turn_id: entry.conversation.execution.turn_id,
        provider_item_id: null,
        actor: { kind: "human", principal_id: ACTOR.principal_id },
        command_key: key,
        created_at: now(),
        updated_at: now(),
      });
      // The runner resumes on an authorized answer; answers have no provider
      // acknowledgement, so the receipt stops at `sent`.
      setExecution(entry, { status: "running" });
      const receipt: Receipt = {
        key,
        kind,
        status: "sent",
        message_id: null,
        question_id: questionId,
        error: null,
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt });
      emit(entry, { type: "command.receipt", data: receipt });
      return { status: 200, payload: receipt };
    }

    if (kind === "interrupt" && entry.conversation.work_item_id !== null) {
      setExecution(entry, { status: "interrupting" });
      const receipt: Receipt = {
        key,
        kind,
        status: "sent",
        message_id: null,
        question_id: null,
        error: null,
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt });
      emit(entry, { type: "command.receipt", data: receipt });
      setExecution(entry, { status: "interrupted", turn_id: null });
      if (entry.runnerMessageId !== null) {
        const message = entry.messages.find(
          (candidate) => candidate.id === entry.runnerMessageId,
        );
        updateMessage(entry, entry.runnerMessageId, {
          text: message?.text ?? "",
          delivery: "interrupted",
        });
        entry.runnerMessageId = null;
      }
      return { status: 200, payload: receipt };
    }

    if (kind === "continue" && entry.conversation.work_item_id !== null) {
      // `continue` records intent and reports `waiting_for_runner`; the hub
      // never starts a runner itself (decisions.md §5).
      setExecution(entry, { status: "waiting_for_runner", turn_id: null });
      const receipt: Receipt = {
        key,
        kind,
        status: "sent",
        message_id: null,
        question_id: null,
        error: null,
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt });
      emit(entry, { type: "command.receipt", data: receipt });
      return { status: 200, payload: receipt };
    }

    const linked = entry.conversation.work_item_id !== null;
    const dispatched = !linked && coordinatorMode === "runner";

    // `cancel` on an unlinked chat is delivered as an interrupt to the runner
    // holding the coordinator work item; the client-facing kind stays `cancel`
    // (decisions.md §9.1). The ladder is the interrupt one: `interrupting`
    // while it is in flight, then `interrupted`.
    if (kind === "cancel" && !linked) {
      setExecution(entry, { status: "interrupting" });
      const cancelReceipt: Receipt = {
        key,
        kind,
        status: "sent",
        message_id: null,
        question_id: null,
        error: null,
        updated_at: now(),
      };
      entry.receipts.set(key, { payload: serialized, receipt: cancelReceipt });
      emit(entry, { type: "command.receipt", data: cancelReceipt });
      if (entry.runnerMessageId !== null) {
        const open = entry.messages.find(
          (candidate) => candidate.id === entry.runnerMessageId,
        );
        updateMessage(entry, entry.runnerMessageId, {
          text: open?.text ?? "",
          delivery: "interrupted",
        });
        entry.runnerMessageId = null;
      }
      setExecution(entry, { status: "interrupted", turn_id: null });
      return { status: 200, payload: cancelReceipt };
    }

    // In a linked conversation with a running attempt that supports steering,
    // a message joins the current turn; without one it waits for the next
    // attempt to read it (decisions.md §5). A runner-dispatched coordinator
    // chat is the same shape: the message is `queued` until a runner claims the
    // coordinator work item and reads it off the controls poll (§9.1).
    const steering =
      linked &&
      entry.conversation.execution.status === "running" &&
      entry.conversation.execution.capabilities.steer;
    const messageDelivery = linked
      ? steering
        ? "delivered"
        : "queued"
      : dispatched
        ? "queued"
        : "delivered";

    // §17.1: a `message` command names attachments already uploaded against
    // this conversation. Unknown or already-sent ids are refused rather than
    // dropped, so a client that loses track of them hears about it.
    const named = Array.isArray(body.attachments) ? (body.attachments as string[]) : [];
    if (named.length > 0 && kind !== "message") {
      return {
        status: 422,
        payload: { code: "invalid_request", message: "Only a message carries attachments." },
      };
    }
    if (named.length > MAX_ATTACHMENTS_PER_MESSAGE) {
      return {
        status: 422,
        payload: {
          code: "invalid_request",
          message: `A message carries at most ${MAX_ATTACHMENTS_PER_MESSAGE} attachments.`,
        },
      };
    }
    const bound: MockAttachment[] = [];
    for (const id of named) {
      const attachment = entry.attachments.get(String(id));
      if (attachment === undefined) {
        return {
          status: 422,
          payload: { code: "invalid_request", message: "No such unsent attachment." },
        };
      }
      bound.push(attachment);
    }

    let messageId: string | null = null;
    if (kind === "message" || kind === "continue" || kind === "answer") {
      const message = addMessage(entry, {
        id: `msg_${randomUUID().replace(/-/g, "").slice(0, 12)}`,
        conversation_id: entry.conversation.id,
        role: "user",
        kind: kind === "message" ? "text" : (kind as Message["kind"]),
        text: String(body.text ?? ""),
        data: {},
        delivery: messageDelivery,
        attempt_id: entry.conversation.execution.attempt_id,
        turn_id: entry.conversation.execution.turn_id,
        provider_item_id: null,
        actor: { kind: "human", principal_id: ACTOR.principal_id },
        command_key: key,
        ...(bound.length === 0
          ? {}
          : {
              attachments: bound.map(({ id, name, mime, size, url }) => ({
                id,
                name,
                mime,
                size,
                url,
              })),
            }),
        created_at: now(),
        updated_at: now(),
      });
      messageId = message.id;
      for (const attachment of bound) entry.attachments.delete(attachment.id);
      touch(entry, { last_message_at: now() });
    }
    const receipt: Receipt = {
      key,
      kind,
      status:
        kind === "cancel" || kind === "interrupt" ? "sent" : (messageDelivery as Receipt["status"]),
      message_id: messageId,
      question_id: kind === "answer" ? String(body.question_id ?? "") : null,
      error: null,
      updated_at: now(),
    };
    entry.receipts.set(key, { payload: serialized, receipt });
    emit(entry, { type: "command.receipt", data: receipt });
    if (dispatched && (kind === "message" || kind === "continue")) {
      // The hub creates or reuses the coordinator work item and waits: it never
      // starts a runner itself. Until one binds, the chat reports
      // `waiting_for_runner` exactly as a linked issue with no attempt does
      // (decisions.md §9.1). An attempt that is already bound keeps its state;
      // the queued message reaches it through the controls poll.
      const bound =
        entry.conversation.execution.attempt_id !== null &&
        (entry.conversation.execution.status === "starting" ||
          entry.conversation.execution.status === "running" ||
          entry.conversation.execution.status === "waiting_input");
      if (!bound) {
        setExecution(entry, {
          status: "waiting_for_runner",
          turn_id: null,
          capabilities: { steer: false, interrupt: true, answer: true, continue: true },
        });
      }
    }
    if (!linked && !dispatched && (kind === "message" || kind === "continue")) {
      const text = String(body.text ?? "");
      const proposes = /create an issue/i.test(text);
      void runCoordinatorTurn(entry).then(() => {
        if (!proposes || !store.has(entry.conversation.id)) return;
        // The coordinator offering a handoff: a status message whose data the
        // client renders as a proposal card with the form prefilled (U06).
        addStatusMessage(entry, "This looks like work for a tracked issue.", {
          proposal: {
            project_id: entry.conversation.project_id,
            title: text.slice(0, 80),
            objective: `${text}\n\nRaised from this conversation.`,
          },
        });
      });
    }
    if (kind === "interrupt") {
      setExecution(entry, { status: "interrupted", turn_id: null });
    }
    return { status: 200, payload: receipt };
  }

  function listConversations(projectId: string | null, query: URLSearchParams) {
    const search = (query.get("q") ?? "").trim().toLowerCase();
    // `settled=true|false` filters the shelf (decisions.md §14); absent means
    // everything, because the sidebar groups them itself.
    const settled = query.get("settled");
    const conversations = [...store.values()]
      .map((entry) => entry.conversation)
      .filter((conversation) => projectId === null || conversation.project_id === projectId)
      .filter(
        (conversation) =>
          settled === null || (conversation.status === "settled") === (settled === "true"),
      )
      .filter(
        (conversation) => search === "" || conversation.title.toLowerCase().includes(search),
      )
      .toSorted(
        (left, right) =>
          Date.parse(right.last_message_at ?? right.updated_at) -
          Date.parse(left.last_message_at ?? left.updated_at),
      );
    return { conversations, next_cursor: null };
  }

  function openEventStream(
    entry: StoredConversation,
    request: IncomingMessage,
    response: ServerResponse,
    query: URLSearchParams,
  ): void {
    const after = Number(query.get("after") ?? "0");
    response.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
      Connection: "keep-alive",
    });
    const dropAfter = query.has("drop")
      ? Number(query.get("drop"))
      : injected.dropAfterFrames;
    injected.dropAfterFrames = null;
    const subscriber: Subscriber = { response, dropAfter, delivered: 0 };

    if (injected.revokeAccess || query.get("fail") === "access_revoked") {
      injected.revokeAccess = false;
      writeFrame(entry, subscriber, null, { type: "closed", data: { reason: "access_revoked" } });
      return;
    }
    if (injected.serverErrorOnce || query.get("fail") === "server_error") {
      // One shot: the client backs off, comes back with the cursor it holds,
      // and this stream then behaves (decisions.md §5, §10 client review).
      injected.serverErrorOnce = false;
      writeFrame(entry, subscriber, null, { type: "closed", data: { reason: "server_error" } });
      return;
    }
    if (
      query.get("fail") === "cursor_expired" ||
      (injected.expireCursorsBelow > 0 && after > 0 && after < injected.expireCursorsBelow)
    ) {
      // One shot: the client re-snapshots and resubscribes with a fresh cursor,
      // which must then be accepted or the two would loop forever.
      injected.expireCursorsBelow = 0;
      writeFrame(entry, subscriber, null, { type: "closed", data: { reason: "cursor_expired" } });
      return;
    }

    entry.subscribers.add(subscriber);
    for (const stored of entry.events) {
      if (stored.seq <= after) continue;
      writeFrame(entry, subscriber, stored.seq, stored.event);
      if (response.writableEnded || response.destroyed) return;
    }
    const heartbeat = setInterval(() => {
      if (response.writableEnded) {
        clearInterval(heartbeat);
        return;
      }
      writeFrame(entry, subscriber, null, {
        type: "heartbeat",
        data: { seq: entry.seq },
      });
    }, heartbeatMs);
    heartbeat.unref?.();
    request.on("close", () => {
      clearInterval(heartbeat);
      entry.subscribers.delete(subscriber);
    });
  }

  async function readBody(request: IncomingMessage): Promise<Record<string, unknown>> {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(chunk as Buffer);
    const raw = Buffer.concat(chunks).toString("utf8");
    if (raw.length === 0) return {};
    try {
      return JSON.parse(raw) as Record<string, unknown>;
    } catch {
      return {};
    }
  }

  /**
   * The one multipart body this hub parses: `POST .../attachments`
   * (decisions.md §17.1, fields `file` and `idempotency_key`). Enough of RFC
   * 7578 to read what the browser's `FormData` sends and nothing more.
   */
  async function readUpload(
    request: IncomingMessage,
  ): Promise<{ name: string; mime: string; bytes: Buffer; key: string } | null> {
    const type = request.headers["content-type"] ?? "";
    const boundary = /boundary=(?:"([^"]+)"|([^;]+))/.exec(type);
    if (boundary === null) return null;
    const marker = `--${boundary[1] ?? boundary[2] ?? ""}`;
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(chunk as Buffer);
    const raw = Buffer.concat(chunks);
    const parts = raw.toString("binary").split(marker);
    let file: { name: string; mime: string; bytes: Buffer } | null = null;
    let key = "";
    for (const part of parts) {
      const split = part.indexOf("\r\n\r\n");
      if (split < 0) continue;
      const headers = part.slice(0, split);
      // The trailing CRLF before the next boundary is not part of the body.
      const body = part.slice(split + 4, part.length - 2);
      const name = /name="([^"]*)"/.exec(headers)?.[1] ?? "";
      if (name === "idempotency_key") {
        key = Buffer.from(body, "binary").toString("utf8").trim();
        continue;
      }
      if (name !== "file") continue;
      file = {
        name: /filename="([^"]*)"/.exec(headers)?.[1] ?? "file",
        mime: /content-type:\s*([^\r\n]+)/i.exec(headers)?.[1]?.trim() ?? "",
        bytes: Buffer.from(body, "binary"),
      };
    }
    if (file === null || key === "") return null;
    return { ...file, key };
  }

  /**
   * A read-only viewer's mutation is refused (decisions.md §10.11). The client
   * hides every control that would send one, so reaching this is a defect the
   * tests are entitled to see rather than a path a reader can walk into.
   */
  function refuseReadOnly(response: ServerResponse): boolean {
    if (accountMode !== "read_only") return false;
    json(response, 403, {
      code: "forbidden",
      message: "You can read this chat but not send messages.",
    });
    return true;
  }

  function json(response: ServerResponse, status: number, payload: unknown): void {
    const body = JSON.stringify(payload);
    response.writeHead(status, {
      "Content-Type": "application/json",
      "Content-Length": Buffer.byteLength(body),
    });
    response.end(body);
  }

  const work = createWorkMock({
    apiBase: API_BASE,
    organizationId: ORGANIZATION.id,
    projects: PROJECTS.map((project) => ({ id: project.id, name: project.name })),
  });

  // --- The hosted account surface (decisions.md §12) ------------------------
  //
  // The organization, project, fleet, plan and billing endpoints. They share
  // this hub's read-only switch and its idempotency conventions, and they
  // decline every path they do not own so the conversation routes below still
  // see `${API_BASE}/projects/:project/conversations/...`.

  let account = initialAccountState();
  // Kept outside `account` on purpose: it is test cover for what was sent, not
  // hub state, so `POST /__mock/reset` leaves it alone.
  let lastEnrollment: MockEnrollmentRequest | null = null;

  /** A 204 and the other body-less answers; `json` would write a body. */
  function noContent(response: ServerResponse, status = 204): void {
    response.writeHead(status);
    response.end();
  }

  function notFound(response: ServerResponse, message: string): void {
    json(response, 404, { code: "not_found", message });
  }

  function forbidden(response: ServerResponse, message: string): void {
    json(response, 403, { code: "forbidden", message });
  }

  function invalidRequest(response: ServerResponse, message: string): void {
    json(response, 422, { code: "invalid_request", message });
  }

  /**
   * A lost update. Hosted mode redacts the current revision, so recovering
   * means re-reading rather than retrying with a guess: the body carries no
   * number at all, and a client that invents one is wrong.
   */
  const REVISION_CONFLICT = {
    code: "revision_conflict",
    message: "Somebody else changed this first.",
  };

  interface MutationOutcome {
    readonly status: number;
    readonly payload: unknown;
    /** False for an outcome that must not be replayed under this key. */
    readonly store?: boolean;
  }

  /**
   * Every non-GET in §12 carries an idempotency key of at most 128 bytes. A
   * repeat of the same key with the same payload replays the stored response;
   * the same key with a different payload is `idempotency_conflict`, which is
   * why a retry has to reuse its key and a new intent has to mint one. Only a
   * successful outcome is stored: a refusal is the state of the world at that
   * moment, not an answer worth freezing.
   */
  function runMutation(
    response: ServerResponse,
    method: string,
    path: string,
    body: Record<string, unknown>,
    produce: () => MutationOutcome,
  ): void {
    const key = typeof body.idempotency_key === "string" ? body.idempotency_key : "";
    if (key.length === 0 || Buffer.byteLength(key, "utf8") > 128) {
      invalidRequest(response, "An idempotency key of at most 128 bytes is required.");
      return;
    }
    const slot = `${method} ${path} ${key}`;
    const serialized = JSON.stringify(body);
    const stored = account.mutations.get(slot);
    if (stored !== undefined) {
      if (stored.payload !== serialized) {
        json(response, 409, {
          code: "idempotency_conflict",
          message: "That key was used with a different payload.",
        });
        return;
      }
      if (stored.status === 204) {
        noContent(response);
        return;
      }
      json(response, stored.status, stored.response);
      return;
    }
    const outcome = produce();
    if (outcome.status < 400 && outcome.store !== false) {
      account.mutations.set(slot, {
        payload: serialized,
        status: outcome.status,
        response: outcome.payload,
      });
    }
    if (outcome.status === 204) {
      noContent(response);
      return;
    }
    json(response, outcome.status, outcome.payload);
  }

  function accountProject(projectId: string): ProjectRow | null {
    return account.projects.find((project) => project.id === projectId) ?? null;
  }

  /**
   * Whether removing or demoting this member would leave the organization
   * without an owner. Computed against the live list rather than pinned to a
   * seeded id, so the refusal lifts the moment a second owner exists.
   */
  function isLastOwner(member: MemberRow): boolean {
    if (member.role !== "owner") return false;
    return account.members.filter((candidate) => candidate.role === "owner").length === 1;
  }

  const LAST_OWNER = {
    code: "last_owner",
    message: "An organization keeps at least one owner.",
  };

  /**
   * `onboarding.Evaluate`, recomputed on every read rather than stored: the
   * wizard's stepper is a view of the live state, so a policy approved in
   * another tab moves the step without anything having written a step.
   */
  function evaluateOnboarding(projectId: string): {
    steps: Array<{ name: string; state: string; detail: string }>;
    ready: boolean;
  } {
    const policy = account.policies.get(projectId) ?? null;
    const progress = account.progress.get(projectId);
    const services = account.artifactServices.get(projectId) ?? [];
    const artifacts = progress?.artifacts ?? "";
    const met = [
      policy !== null,
      progress?.doctor === true && progress.provider === true,
      policy !== null && account.runners.some((entry) => entry.exclusions.length === 0),
      artifacts === "local" || (artifacts === "customer" && services.length > 0),
    ];
    return {
      steps: ONBOARDING_STEP_TEXT.map((step, index) => ({
        name: step.name,
        state: met[index] === true ? "ready" : "action_required",
        detail: step.detail,
      })),
      ready: met.every((value) => value),
    };
  }

  /** A project as `GET /projects` and the extended bootstrap carry it. */
  function projectSummary(project: ProjectRow): Record<string, unknown> {
    const reader = accountMode === "read_only";
    const evaluated = evaluateOnboarding(project.id);
    return {
      id: project.id,
      name: project.name,
      profile: project.profile,
      states: WORKFLOW_STATES,
      can_write: reader ? false : project.can_write,
      can_manage_runners: reader ? false : project.can_manage_runners,
      onboarding: { ready: evaluated.ready, steps: evaluated.steps },
    };
  }

  /**
   * `GET /app/bootstrap` (§12, "Serving"), and `/chat/bootstrap` as its
   * documented alias: one payload, because the narrower conversation schema
   * ignores the fields it does not know and the shell then loads it once.
   */
  function accountBootstrap(): Record<string, unknown> {
    const coordinator = coordinatorMode !== "none";
    const reader = accountMode === "read_only";
    return {
      organization: ORGANIZATION,
      organizations: account.organizations,
      actor: { ...ACTOR, can_manage: !reader, can_manage_runners: !reader },
      projects: account.projects.map((project) => ({
        ...project,
        ...(reader ? { can_write: false, can_manage_runners: false } : {}),
        states: WORKFLOW_STATES,
        coordinator,
      })),
      support: account.support,
      csrf_token: CSRF_TOKEN,
      capabilities: { coordinator, attachments: false },
      api_base: API_BASE,
      preferences: PREFERENCE_CHOICES,
      feature: { conversation: true },
      plan: SEED_PLAN_SUMMARY,
      version: HOSTED_VERSION,
    };
  }

  /**
   * The organization, fleet, plan and billing routes. Returns false for a path
   * it does not own, which is how the conversation routes keep theirs.
   */
  async function handleAccountRoute(
    request: IncomingMessage,
    response: ServerResponse,
    url: URL,
    method: string,
  ): Promise<boolean> {
    const path = url.pathname;

    // The two account routes outside the organization base: one ends the
    // session, the other is what a subject with no organization yet calls.
    if (path === "/logout" && method === "POST") {
      noContent(response);
      return true;
    }
    if (path === "/api/v2/organizations" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const name = String(body.name ?? "").trim();
        if (name.length === 0) {
          return {
            status: 422,
            payload: { code: "invalid_request", message: "An organization needs a name." },
          };
        }
        const id = `org_${randomUUID().replace(/-/g, "").slice(0, 8)}`;
        return {
          status: 201,
          payload: {
            organization: { id, name, public_url: `https://${id}.mock.test` },
            // Creating one does not sign you into it: the session belongs to
            // the organization that issued it, so the browser goes back
            // through the identity provider.
            next: "/auth/oidc/start",
          },
        };
      });
      return true;
    }

    if (!path.startsWith(`${API_BASE}/`)) return false;
    const tail = path.slice(API_BASE.length + 1);
    const segments = tail.split("/").map((segment) => decodeURIComponent(segment));

    if (tail === "members" && method === "GET") {
      if (accountMode === "read_only") {
        // §12: a plain member sees only themselves, and pending invitations
        // are an owner's business rather than a directory.
        json(response, 200, {
          members: account.members.filter((member) => member.user_id === ACTOR.subject),
          invitations: [],
        });
        return true;
      }
      json(response, 200, { members: account.members, invitations: account.invitations });
      return true;
    }

    if (tail === "members/invitations" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const email = String(body.email ?? "");
        const role = String(body.role ?? "");
        if (!email.includes("@")) {
          return {
            status: 422,
            payload: { code: "invalid_request", message: "That is not an email address." },
          };
        }
        if (!ORGANIZATION_ROLES.includes(role)) {
          return {
            status: 422,
            payload: {
              code: "invalid_request",
              message: "role must be owner, admin, member or viewer.",
            },
          };
        }
        const invitation: InvitationRow = {
          id: `inv_${randomUUID().replace(/-/g, "").slice(0, 8)}`,
          email,
          role,
          created_at: now(),
          expires_at: new Date(Date.now() + 86_400_000).toISOString(),
        };
        account.invitations.push(invitation);
        return { status: 201, payload: clone(invitation) };
      });
      return true;
    }

    if (
      segments.length === 3 &&
      segments[0] === "members" &&
      segments[1] === "invitations" &&
      method === "DELETE"
    ) {
      const id = segments[2] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        if (!account.invitations.some((invitation) => invitation.id === id)) {
          return { status: 404, payload: { code: "not_found", message: "No such invitation." } };
        }
        account.invitations = account.invitations.filter((invitation) => invitation.id !== id);
        return { status: 204, payload: null };
      });
      return true;
    }

    if (segments.length === 2 && segments[0] === "members" && method === "DELETE") {
      const id = segments[1] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const member = account.members.find((candidate) => candidate.id === id);
        if (member === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such member." } };
        }
        if (isLastOwner(member)) return { status: 409, payload: LAST_OWNER };
        account.members = account.members.filter((candidate) => candidate.id !== id);
        return { status: 204, payload: null };
      });
      return true;
    }

    if (
      segments.length === 3 &&
      segments[0] === "members" &&
      segments[2] === "role" &&
      method === "PUT"
    ) {
      const id = segments[1] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const member = account.members.find((candidate) => candidate.id === id);
        if (member === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such member." } };
        }
        const role = String(body.role ?? "");
        if (!ORGANIZATION_ROLES.includes(role)) {
          return {
            status: 422,
            payload: {
              code: "invalid_request",
              message: "role must be owner, admin, member or viewer.",
            },
          };
        }
        // Demoting the last owner is the same refusal as removing them: the
        // organization would have nobody who can restore it.
        if (role !== "owner" && isLastOwner(member)) {
          return { status: 409, payload: LAST_OWNER };
        }
        member.role = role;
        return { status: 200, payload: clone(member) };
      });
      return true;
    }

    if (
      segments.length === 3 &&
      segments[0] === "members" &&
      segments[2] === "grants" &&
      method === "PUT"
    ) {
      const id = segments[1] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const member = account.members.find((candidate) => candidate.id === id);
        if (member === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such member." } };
        }
        const projectId = String(body.project_id ?? "");
        if (accountProject(projectId) === null) {
          return { status: 404, payload: { code: "not_found", message: "No such project." } };
        }
        if (body.revoke === true) {
          member.grants = member.grants.filter((grant) => grant.project_id !== projectId);
          return { status: 200, payload: clone(member) };
        }
        const grant = {
          project_id: projectId,
          write: body.write === true,
          runner: body.runner === true,
        };
        const index = member.grants.findIndex((candidate) => candidate.project_id === projectId);
        if (index < 0) member.grants.push(grant);
        else member.grants[index] = grant;
        return { status: 200, payload: clone(member) };
      });
      return true;
    }

    if (tail === "invitations/accept" && method === "POST") {
      const body = await readBody(request);
      runMutation(response, method, path, body, () => {
        if (String(body.token ?? "") !== SEED_INVITE_TOKEN) {
          return {
            status: 404,
            payload: { code: "not_found", message: "That invitation is not valid any more." },
          };
        }
        return { status: 200, payload: { next: "/auth/oidc/start" } };
      });
      return true;
    }

    if (tail === "switch" && method === "POST") {
      const body = await readBody(request);
      runMutation(response, method, path, body, () => {
        const target = account.organizations.find(
          (organization) => organization.id === String(body.organization ?? ""),
        );
        if (target === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such organization." } };
        }
        // Switching is a redirect through the other organization's own origin:
        // the session belongs to the organization that issued it.
        return { status: 200, payload: { next: `${target.public_url}/auth/oidc/start` } };
      });
      return true;
    }

    if (tail === "support/start" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        account.support = clone(SEED_SUPPORT);
        return { status: 200, payload: { support: clone(SEED_SUPPORT) } };
      });
      return true;
    }

    if (tail === "projects" && method === "GET") {
      json(response, 200, account.projects.map((project) => projectSummary(project)));
      return true;
    }

    if (tail === "projects" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const name = String(body.name ?? "").trim();
        if (name.length === 0) {
          return {
            status: 422,
            payload: { code: "invalid_request", message: "A project needs a name." },
          };
        }
        // `grant_access` is the creating admin taking a grant on their own new
        // project; without it they can see it and not write it.
        const granted = body.grant_access !== false;
        const project: ProjectRow = {
          id: `proj_${randomUUID().replace(/-/g, "").slice(0, 8)}`,
          name,
          profile: "native",
          can_write: granted,
          can_manage_runners: granted,
        };
        account.projects.push(project);
        account.integrations.set(project.id, {
          profile: "native",
          revision: "1",
          intake: "disabled",
          projection: "disabled",
          repository_enabled: false,
          authority: clone(SEED_AUTHORITY),
        });
        account.progress.set(project.id, {
          revision: "1",
          repository: "",
          doctor: false,
          provider: false,
          artifacts: "",
        });
        return { status: 201, payload: projectSummary(project) };
      });
      return true;
    }

    if (tail === "fleet" && method === "GET") {
      // Any member reads the fleet. Spend is nullable on purpose: an
      // organization with no metering has no number, and the screen says so
      // rather than drawing a zero that looks like a measurement.
      json(response, 200, {
        ...clone(SEED_FLEET),
        spend: account.spend ? clone(SEED_FLEET.spend) : null,
      });
      return true;
    }

    if (segments[0] === "usage" && segments.length === 1 && method === "GET") {
      // §17.5. Any member reads usage: it is a report, and there is nothing on
      // it a reader who can load the organization may not see. An unknown
      // range is refused rather than silently answered for a different window.
      const range = url.searchParams.get("range") ?? "30d";
      if (range !== "24h" && range !== "7d" && range !== "30d" && range !== "90d") {
        invalidRequest(response, "range must be one of 24h, 7d, 30d, 90d.");
        return true;
      }
      json(response, 200, account.usage ? usageReport(range) : emptyUsageReport());
      return true;
    }

    if (tail === "plan" && method === "GET") {
      if (accountMode === "read_only") {
        forbidden(response, "Only an owner or an admin can see the plan.");
        return true;
      }
      json(response, 200, SEED_PLAN_REPORT);
      return true;
    }

    if (tail === "billing" && method === "GET") {
      if (accountMode === "read_only") {
        forbidden(response, "Only an owner can see billing.");
        return true;
      }
      if (account.support !== null) {
        // Somebody from Detent is acting as this organization. Reading a
        // customer's payment details is not part of that.
        forbidden(response, "Billing is not available during a support session.");
        return true;
      }
      json(response, 200, { ...SEED_BILLING, can_checkout: true });
      return true;
    }

    if (tail === "billing/checkout" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const price = String(body.price ?? "");
        if (!SEED_BILLING.prices.some((candidate) => candidate.id === price)) {
          return {
            status: 404,
            payload: { code: "not_found", message: "That price is not configured." },
          };
        }
        return { status: 200, payload: { url: `https://checkout.stripe.test/c/pay/${price}` } };
      });
      return true;
    }

    if (tail === "billing/portal" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => ({
        status: 200,
        payload: { url: "https://billing.stripe.test/p/session/mock" },
      }));
      return true;
    }

    if (tail === "runner-enrollments" && method === "POST") {
      // No idempotency key: enrollment is not a native mutation, and the
      // collision below is what makes a retry safe instead.
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      const runnerId = String(body.runner_id ?? "");
      const projectIds = Array.isArray(body.project_ids)
        ? body.project_ids.map((id) => String(id))
        : [];
      const operations = Array.isArray(body.operations)
        ? body.operations.map((operation) => String(operation))
        : [];
      lastEnrollment = {
        runner_id: runnerId,
        machine_id: String(body.machine_id ?? ""),
        project_ids: projectIds,
        operations,
        ttl_seconds: Number(body.ttl_seconds ?? 0),
      };
      if (runnerId.length === 0) {
        invalidRequest(response, "runner_id is required.");
        return true;
      }
      // The hub refuses an enrollment that names no project, a project outside
      // the organization, or the same project twice (`createRunnerEnrollment`).
      if (
        projectIds.length === 0 ||
        projectIds.some(
          (id, index) =>
            projectIds.indexOf(id) !== index ||
            !account.projects.some((project) => project.id === id),
        )
      ) {
        invalidRequest(
          response,
          "Enrollment projects must be unique and belong to the organization",
        );
        return true;
      }
      if (account.enrolled.has(runnerId)) {
        json(response, 409, {
          code: "identity_collision",
          message:
            "Identity is already registered; create a fresh host identity and enroll it explicitly",
        });
        return true;
      }
      account.enrolled.add(runnerId);
      const ttl = Number(body.ttl_seconds ?? 900);
      json(response, 201, {
        id: `enr_${randomUUID().replace(/-/g, "").slice(0, 8)}`,
        token: `det_enroll_${randomUUID().replace(/-/g, "").slice(0, 16)}`,
        expires_at: new Date(Date.now() + (Number.isFinite(ttl) ? ttl : 900) * 1000).toISOString(),
      });
      return true;
    }

    if (tail === "runners" && method === "GET") {
      // A bare array, not an envelope: the runner routing endpoints predate
      // the hosted JSON API and keep the shapes they already had.
      json(response, 200, account.runners.map((entry) => entry.runner));
      return true;
    }

    if (
      segments.length === 3 &&
      segments[0] === "runners" &&
      segments[2] === "routing" &&
      method === "PUT"
    ) {
      const runnerId = segments[1] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      const entry = account.runners.find((candidate) => candidate.runner.runner_id === runnerId);
      if (entry === undefined) {
        notFound(response, "No such runner.");
        return true;
      }
      // A number here, where the integration and progress revisions are
      // strings: this endpoint marshals its counter plainly.
      if (Number(body.expected_revision) !== entry.runner.revision) {
        json(response, 409, REVISION_CONFLICT);
        return true;
      }
      entry.runner = {
        ...entry.runner,
        display_name: String(body.display_name ?? entry.runner.display_name),
        tags: Array.isArray(body.tags) ? body.tags.map((tag) => String(tag)) : entry.runner.tags,
        state: String(body.state ?? entry.runner.state),
        capacity_limit: Number(body.capacity_limit ?? entry.runner.capacity_limit),
        project_ids: Array.isArray(body.project_ids)
          ? body.project_ids.map((id) => String(id))
          : entry.runner.project_ids,
        revision: entry.runner.revision + 1,
      };
      json(response, 200, clone(entry.runner));
      return true;
    }

    if (segments[0] === "projects" && segments.length >= 3) {
      const projectId = segments[1] ?? "";
      const rest = segments.slice(2);
      // The conversation routes own this prefix; everything else under a
      // project is a settings or wizard endpoint.
      if (rest[0] === "conversations") return false;
      if (accountProject(projectId) === null) {
        notFound(response, "No such project.");
        return true;
      }
      return handleProjectRoute(request, response, method, path, projectId, rest);
    }

    return false;
  }

  /** The project settings and first-run wizard endpoints under `nativeBase`. */
  async function handleProjectRoute(
    request: IncomingMessage,
    response: ServerResponse,
    method: string,
    path: string,
    projectId: string,
    rest: string[],
  ): Promise<boolean> {
    const tail = rest.join("/");

    if (tail === "integration" && method === "GET") {
      json(response, 200, account.integrations.get(projectId));
      return true;
    }

    if (tail === "integration" && method === "PUT") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const current = account.integrations.get(projectId);
        if (current === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such project." } };
        }
        if (String(body.expected_revision ?? "") !== current.revision) {
          return { status: 409, payload: REVISION_CONFLICT };
        }
        const next: IntegrationRow = {
          ...current,
          intake: String(body.intake ?? current.intake),
          projection: String(body.projection ?? current.projection),
          repository_enabled: body.repository_enabled === true,
          revision: String(Number(current.revision) + 1),
        };
        account.integrations.set(projectId, next);
        return { status: 200, payload: clone(next) };
      });
      return true;
    }

    if (tail === "policy" && method === "GET") {
      const approval = account.policies.get(projectId);
      if (approval === undefined) {
        // Nothing approved yet. The wizard reads that as "step one is still
        // open", not as a broken endpoint.
        notFound(response, "No policy has been approved for this project.");
        return true;
      }
      json(response, 200, approval);
      return true;
    }

    // `PUT /policy` and `PUT /onboarding/policy` are the same approval through
    // two doors. Neither is a native mutation, so neither carries a key: the
    // descriptor's identity is what makes a repeat safe.
    if ((tail === "policy" || tail === "onboarding/policy") && method === "PUT") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      const descriptor = body.policy;
      if (descriptor === null || typeof descriptor !== "object") {
        invalidRequest(response, "A policy descriptor is required.");
        return true;
      }
      const expected = String(body.expected_policy_id ?? "");
      const current = account.policies.get(projectId) ?? null;
      const currentId = current === null ? "" : String(current.policy.policy_id ?? "");
      // Approval is by identity: naming the approval you are replacing and
      // naming a different one are not the same intent. An empty
      // `expected_policy_id` is "I saw none", which the first approval sends.
      if (expected.length > 0 && expected !== currentId) {
        json(response, 409, REVISION_CONFLICT);
        return true;
      }
      const approval: PolicyRow = {
        policy: descriptor as Record<string, unknown>,
        approved_by: ACTOR.email,
        approved_at: now(),
      };
      account.policies.set(projectId, approval);
      json(response, 200, clone(approval));
      return true;
    }

    if (tail === "onboarding" && method === "GET") {
      const evaluated = evaluateOnboarding(projectId);
      json(response, 200, {
        latest_run: "succeeded",
        progress: account.progress.get(projectId),
        policy: account.policies.get(projectId) ?? null,
        runners: account.runners,
        artifact_services: account.artifactServices.get(projectId) ?? [],
        steps: evaluated.steps,
        ready: evaluated.ready,
      });
      return true;
    }

    if (tail === "onboarding" && method === "PUT") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const current = account.progress.get(projectId);
        if (current === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such project." } };
        }
        const incoming = (body.progress ?? {}) as Record<string, unknown>;
        const repository = String(incoming.repository ?? "");
        const artifacts = String(incoming.artifacts ?? "");
        if (!["", "existing", "generate"].includes(repository)) {
          return {
            status: 422,
            payload: {
              code: "invalid_request",
              message: "repository must be empty, existing or generate.",
            },
          };
        }
        if (!["", "local", "customer"].includes(artifacts)) {
          return {
            status: 422,
            payload: {
              code: "invalid_request",
              message: "artifacts must be empty, local or customer.",
            },
          };
        }
        if (String(incoming.revision ?? "") !== current.revision) {
          return { status: 409, payload: REVISION_CONFLICT };
        }
        const next: ProgressRow = {
          revision: String(Number(current.revision) + 1),
          repository,
          doctor: incoming.doctor === true,
          provider: incoming.provider === true,
          artifacts,
          updated_at: now(),
        };
        account.progress.set(projectId, next);
        // The stored progress alone, not the whole project: the wizard takes
        // its next revision from what it wrote rather than re-reading.
        return { status: 200, payload: clone(next) };
      });
      return true;
    }

    if (tail === "onboarding/repository" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const current = account.integrations.get(projectId);
        if (current === undefined) {
          return { status: 404, payload: { code: "not_found", message: "No such project." } };
        }
        const repository = String(body.repository ?? "").trim();
        if (!/^[^/\s]+\/[^/\s]+$/.test(repository)) {
          return {
            status: 422,
            payload: { code: "invalid_request", message: "A repository is owner/name." },
          };
        }
        // Already bound to exactly this repository: the wizard's retry is a
        // no-op, and a no-op must not spend the key it was retried with.
        if ((current.repository ?? "").toLowerCase() === repository.toLowerCase()) {
          return { status: 200, payload: clone(current), store: false };
        }
        if (String(body.expected_revision ?? "") !== current.revision) {
          return { status: 409, payload: REVISION_CONFLICT };
        }
        const next: IntegrationRow = {
          ...current,
          repository,
          repository_enabled: true,
          revision: String(Number(current.revision) + 1),
        };
        account.integrations.set(projectId, next);
        return { status: 200, payload: clone(next) };
      });
      return true;
    }

    if (rest.length === 3 && rest[0] === "onboarding" && rest[1] === "artifact-services" && method === "PUT") {
      // No idempotency key: the binding is keyed by service id, so writing it
      // twice is writing the same row twice.
      const service = rest[2] ?? "";
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      if (service !== String(body.service_id ?? "")) {
        invalidRequest(response, "The service id in the path and in the body disagree.");
        return true;
      }
      const publisher = String(body.publisher_token_id ?? "");
      if (publisher.length === 0) {
        invalidRequest(response, "A publisher token is required.");
        return true;
      }
      const binding: ArtifactBindingRow = {
        service_id: service,
        origin: String(body.origin ?? ""),
        mode: String(body.mode ?? "customer"),
        publisher_token_id: publisher,
      };
      const bound = account.artifactServices.get(projectId) ?? [];
      const index = bound.findIndex((candidate) => candidate.service_id === service);
      if (index < 0) bound.push(binding);
      else bound[index] = binding;
      account.artifactServices.set(projectId, bound);
      json(response, 200, clone(binding));
      return true;
    }

    if (tail === "work-items" && method === "POST") {
      const body = await readBody(request);
      if (refuseReadOnly(response)) return true;
      runMutation(response, method, path, body, () => {
        const stateName = String(body.state ?? "");
        const state = WORKFLOW_STATES.find((candidate) => candidate.name === stateName);
        if (state === undefined) {
          return {
            status: 422,
            payload: { code: "invalid_request", message: "Workflow state does not exist" },
          };
        }
        account.workItems += 1;
        const number = 1000 + account.workItems;
        const project = accountProject(projectId);
        // 200, not 201: the native mutation path answers with the stored
        // record rather than a creation, and the wizard reads it that way.
        return {
          status: 200,
          payload: {
            organization_id: ORGANIZATION.id,
            project_id: projectId,
            work_item_id: `wi_${number}`,
            number,
            revision: "1",
            profile: project?.profile ?? "native",
            title: String(body.title ?? ""),
            body: String(body.body ?? ""),
            state: state.name,
            terminal: state.terminal,
            labels: Array.isArray(body.labels) ? body.labels.map((label) => String(label)) : [],
            assignees: Array.isArray(body.assignees)
              ? body.assignees.map((assignee) => String(assignee))
              : [],
            actor: { kind: "human", principal_id: ACTOR.principal_id },
            created_at: now(),
            updated_at: now(),
            dependencies: [],
            blockers: [],
            external_references: [],
          },
        };
      });
      return true;
    }

    notFound(response, "No such endpoint.");
    return true;
  }

  const server = createServer((request, response) => {
    void (async () => {
      const url = new URL(request.url ?? "/", "http://mock.local");
      const path = url.pathname;
      const method = request.method ?? "GET";

      // The wizard's first issue. `dev/mock-work.ts` owns the native work API
      // but reads `.../work-items` only, so a `POST` falls through its item
      // lookup and answers `not_found`; §12's create goes through the native
      // mutation path with the account surface's idempotency handling, so that
      // one route is claimed before the work mock sees it. Delete this branch
      // the day the work mock grows a create of its own.
      if (
        method === "POST" &&
        new RegExp(`^${API_BASE}/projects/[^/]+/work-items$`).test(path) &&
        (await handleAccountRoute(request, response, url, method))
      ) {
        return;
      }

      // The native work API, the hosted activity stream, and their own
      // `/__mock/work/*` controls (`dev/mock-work.ts`). It answers only what it
      // owns and returns false for everything else, so every route below is
      // unchanged. It runs first because its controls share the `/__mock/`
      // prefix with the conversation mock's, which answers unknown ones itself.
      if (await work.handle({ response, url, method, readBody: () => readBody(request) })) {
        return;
      }

      if (path.startsWith("/__mock/")) {
        const body = await readBody(request);
        const runnerMatch = path.match(
          /^\/__mock\/runner\/([^/]+)\/(start|question|complete|lose)$/,
        );
        if (runnerMatch !== null) {
          const target = conversationOrNull(decodeURIComponent(runnerMatch[1] ?? ""));
          if (target === null) {
            json(response, 404, { code: "not_found", message: "No such conversation." });
            return;
          }
          if (target.conversation.work_item_id === null && coordinatorMode !== "runner") {
            json(response, 422, {
              code: "invalid",
              message:
                "Only a linked conversation has a runner while the hub answers chats itself.",
            });
            return;
          }
          switch (runnerMatch[2]) {
            case "start":
              await startRunner(
                target,
                String(body.resume ?? "thread") === "transcript" ? "transcript" : "thread",
              );
              json(response, 200, { execution: target.conversation.execution });
              return;
            case "question": {
              const question = openRunnerQuestion(target);
              json(response, 200, { question });
              return;
            }
            case "lose":
              loseRunner(target);
              json(response, 200, { execution: target.conversation.execution });
              return;
            default:
              completeRunner(target);
              json(response, 200, { execution: target.conversation.execution });
              return;
          }
        }
        switch (path) {
          case "/__mock/queue-full":
            injected.queueFullOnce = true;
            break;
          case "/__mock/unknown-outcome":
            injected.unknownOnce = true;
            break;
          case "/__mock/drop-stream":
            injected.dropAfterFrames = Number(body.after ?? 1);
            break;
          case "/__mock/drop-open-streams":
            for (const entry of store.values()) {
              for (const subscriber of entry.subscribers) subscriber.response.destroy();
              entry.subscribers.clear();
            }
            break;
          case "/__mock/expire-cursors":
            injected.expireCursorsBelow = Number(body.below ?? Number.MAX_SAFE_INTEGER);
            break;
          case "/__mock/revoke-access":
            injected.revokeAccess = true;
            break;
          case "/__mock/server-error":
            injected.serverErrorOnce = true;
            break;
          case "/__mock/fail-upload":
            injected.uploadFailsOnce = true;
            break;
          case "/__mock/bad-frame":
            // A frame with a known event name and a body the client's schema
            // rejects. A hub that ships a field this client does not know
            // looks exactly like this.
            for (const entry of store.values()) {
              entry.seq += 1;
              const seq = entry.seq;
              const raw = { type: "message.updated", data: { nope: true } } as unknown;
              entry.events.push({ seq, event: raw as ConversationEvent });
              for (const subscriber of [...entry.subscribers]) {
                writeFrame(entry, subscriber, seq, raw as ConversationEvent);
              }
            }
            break;
          case "/__mock/coordinator": {
            const mode = readCoordinatorMode(String(body.mode ?? ""));
            if (mode === null) {
              json(response, 422, {
                code: "invalid_request",
                message: "mode must be hub, runner or none.",
              });
              return;
            }
            coordinatorMode = mode;
            break;
          }
          case "/__mock/account": {
            const mode = readAccountMode(String(body.mode ?? ""));
            if (mode === null) {
              json(response, 422, {
                code: "invalid_request",
                message: "mode must be write or read_only.",
              });
              return;
            }
            accountMode = mode;
            break;
          }
          case "/__mock/support":
            // Opens or closes an impersonated support session. The bootstrap
            // reports it and billing refuses while it is open (§12).
            account.support = body.open === false ? null : clone(SEED_SUPPORT);
            break;
          case "/__mock/fleet":
            // `{"spend": null}` is the organization with no metering: the
            // screen has to say it has no number rather than draw a zero.
            account.spend = body.spend !== null;
            break;
          case "/__mock/usage":
            // `{"usage": null}` is the organization that has run nothing yet,
            // which is the usage page's empty state rather than an error.
            account.usage = body.usage !== null;
            break;
          case "/__mock/revision-drift": {
            // Somebody else's write, landing between the client's read and its
            // own write. It is the only way to provoke a `revision_conflict`
            // deterministically from the outside.
            const resource = String(body.resource ?? "");
            if (resource === "integration") {
              for (const [id, value] of account.integrations) {
                account.integrations.set(id, {
                  ...value,
                  revision: String(Number(value.revision) + 1),
                });
              }
            } else if (resource === "onboarding") {
              for (const [id, value] of account.progress) {
                account.progress.set(id, { ...value, revision: String(Number(value.revision) + 1) });
              }
            } else if (resource === "runner") {
              for (const entry of account.runners) {
                entry.runner = { ...entry.runner, revision: entry.runner.revision + 1 };
              }
            } else {
              json(response, 422, {
                code: "invalid_request",
                message: "resource must be integration, onboarding or runner.",
              });
              return;
            }
            break;
          }
          // The hub settles a conversation itself (decisions.md §14); a test
          // needs a way to make that happen on demand.
          case "/__mock/settle": {
            const target = store.get(String(body["conversation"] ?? ""));
            if (target === undefined) {
              json(response, 404, { code: "not_found", message: "No such conversation." });
              return;
            }
            touch(target, { status: body["settled"] === false ? "active" : "settled" });
            break;
          }
          case "/__mock/reset":
            coordinatorMode =
              options.coordinator ?? readCoordinatorMode(process.env.MOCK_COORDINATOR) ?? "hub";
            accountMode = options.account ?? readAccountMode(process.env.MOCK_ACCOUNT) ?? "write";
            store.clear();
            issues.clear();
            creates.clear();
            injected.queueFullOnce = false;
            injected.unknownOnce = false;
            injected.dropAfterFrames = null;
            injected.expireCursorsBelow = 0;
            injected.revokeAccess = false;
            injected.serverErrorOnce = false;
            injected.uploadFailsOnce = false;
            account = initialAccountState();
            break;
          default:
            json(response, 404, { code: "not_found", message: "Unknown control endpoint." });
            return;
        }
        json(response, 200, { ok: true });
        return;
      }

      if (path === "/app/bootstrap" || path === "/chat/bootstrap") {
        // `?coordinator=runner|hub|none` flips the mode from the browser, so a
        // dev session can watch a chat queue for a runner without a restart.
        const requested = readCoordinatorMode(url.searchParams.get("coordinator"));
        if (requested !== null) coordinatorMode = requested;
        // `?account=read_only` is the same switch for the viewer role: every
        // project reports `can_write: false` (decisions.md §10.11).
        const requestedAccount = readAccountMode(url.searchParams.get("account"));
        if (requestedAccount !== null) accountMode = requestedAccount;
        // One payload for both paths. `/chat/bootstrap` is the documented
        // alias of `/app/bootstrap` (decisions.md §12, "Serving"), and the
        // narrower conversation schema ignores the account fields, so a client
        // that has not moved yet reads exactly what it always did.
        json(response, 200, accountBootstrap());
        return;
      }

      if (path === `${API_BASE}/conversations` && method === "GET") {
        json(response, 200, listConversations(null, url.searchParams));
        return;
      }

      // The hosted account surface (decisions.md §12). It answers first and
      // declines anything it does not own, so the conversation routes below
      // still see `${API_BASE}/projects/:project/conversations/...`.
      if (await handleAccountRoute(request, response, url, method)) return;

      const projectMatch = path.match(
        new RegExp(`^${API_BASE}/projects/([^/]+)/conversations(?:/([^/]+))?(?:/(.+))?$`),
      );
      if (projectMatch === null) {
        json(response, 404, { code: "not_found", message: "No such endpoint." });
        return;
      }
      const [, rawProject, rawConversation, action] = projectMatch;
      const projectId = decodeURIComponent(rawProject ?? "");

      if (rawConversation === undefined) {
        if (method === "GET") {
          json(response, 200, listConversations(projectId, url.searchParams));
          return;
        }
        if (method === "POST") {
          const body = await readBody(request);
          if (refuseReadOnly(response)) return;
          // Creating a conversation is idempotent by actor and key: the key is
          // mandatory, at most 128 bytes, and a retry returns the stored
          // response rather than a second conversation (decisions.md §10.2).
          const createKey = typeof body.key === "string" ? body.key : "";
          if (createKey.length === 0 || Buffer.byteLength(createKey, "utf8") > 128) {
            json(response, 422, {
              code: "invalid_request",
              message: "key is required and must be at most 128 bytes.",
            });
            return;
          }
          const serializedCreate = JSON.stringify(body);
          const storedCreate = creates.get(createKey);
          if (storedCreate !== undefined) {
            if (storedCreate.payload !== serializedCreate) {
              json(response, 409, {
                code: "idempotency_conflict",
                message: "That key was used with a different payload.",
              });
              return;
            }
            json(response, storedCreate.status, storedCreate.response);
            return;
          }
          const first = body.first_message as { key: string; text: string } | undefined;
          const entry = createConversation(
            projectId,
            typeof body.title === "string" && body.title.length > 0
              ? body.title
              : (first?.text ?? "New chat").slice(0, 60),
          );
          let receipt: Receipt | undefined;
          if (first !== undefined) {
            const outcome = acceptCommand(entry, {
              key: first.key,
              kind: "message",
              text: first.text,
            });
            if (outcome.status !== 200) {
              store.delete(entry.conversation.id);
              json(response, outcome.status, outcome.payload);
              return;
            }
            receipt = outcome.payload as Receipt;
          }
          const created = {
            conversation: entry.conversation,
            ...(receipt === undefined ? {} : { receipt }),
          };
          creates.set(createKey, {
            payload: serializedCreate,
            status: 201,
            response: created,
          });
          json(response, 201, created);
          return;
        }
      }

      const entry = conversationOrNull(decodeURIComponent(rawConversation ?? ""));
      if (entry === null) {
        json(response, 404, { code: "not_found", message: "No such conversation." });
        return;
      }

      if (action === undefined && method === "GET") {
        const page = entry.messages.slice(-40);
        json(response, 200, {
          conversation: entry.conversation,
          messages: page,
          // Answered questions stay in the snapshot: a second tab has to see
          // the resolution rather than an empty card it could answer again.
          questions: entry.questions,
          cursor: entry.seq,
          has_more: page.length < entry.messages.length,
        });
        return;
      }

      if (action === undefined && method === "PATCH") {
        const body = await readBody(request);
        // `PATCH` carries a title, a preference triple, or both
        // (decisions.md §14). Anything absent keeps the value it had.
        const preferences = body.preferences as Record<string, unknown> | undefined;
        touch(entry, {
          title: String(body.title ?? entry.conversation.title),
          ...(preferences === undefined
            ? {}
            : {
                preferences: {
                  model: String(preferences["model"] ?? "auto"),
                  reasoning_effort: String(preferences["reasoning_effort"] ?? "auto"),
                  access: String(preferences["access"] ?? "auto"),
                },
              }),
        });
        json(response, 200, entry.conversation);
        return;
      }

      if (action === "messages" && method === "GET") {
        const before = Number(url.searchParams.get("before") ?? "0");
        const limit = Number(url.searchParams.get("limit") ?? "50");
        const older = entry.messages.filter((message) => message.seq < before);
        const page = older.slice(Math.max(0, older.length - limit));
        json(response, 200, {
          messages: page,
          next_cursor: page.length < older.length ? String(page[0]?.seq ?? "") : null,
        });
        return;
      }

      if (action === "events" && method === "GET") {
        openEventStream(entry, request, response, url.searchParams);
        return;
      }

      // `POST {nativeBase}/conversations/:id/attachments` (decisions.md §17.1).
      if (action === "attachments" && method === "POST") {
        if (refuseReadOnly(response)) return;
        const upload = await readUpload(request);
        if (upload === null) {
          json(response, 422, {
            code: "invalid_request",
            message: "A multipart upload with a file part and an idempotency key is required.",
          });
          return;
        }
        const seen = entry.attachmentKeys.get(upload.key);
        if (seen !== undefined) {
          const stored = entry.attachments.get(seen);
          if (stored !== undefined) {
            json(response, 201, stored);
            return;
          }
        }
        if (upload.bytes.length > MAX_ATTACHMENT_BYTES) {
          json(response, 413, {
            code: "invalid_request",
            message: `A file must be at most ${MAX_ATTACHMENT_BYTES} bytes`,
          });
          return;
        }
        if (url.searchParams.get("fail") === "upload" || injected.uploadFailsOnce) {
          injected.uploadFailsOnce = false;
          json(response, 500, {
            code: "internal",
            message: "The attachment could not be stored.",
          });
          return;
        }
        const id = `att_${randomUUID().replace(/-/g, "").slice(0, 12)}`;
        const attachment: MockAttachment = {
          id,
          name: upload.name,
          mime: upload.mime,
          size: upload.bytes.length,
          url: `${API_BASE}/projects/${entry.conversation.project_id}/conversations/${entry.conversation.id}/attachments/${id}`,
          expires_at: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
        };
        entry.attachments.set(id, attachment);
        entry.attachmentKeys.set(upload.key, id);
        json(response, 201, attachment);
        return;
      }

      if (action !== undefined && action.startsWith("attachments/") && method === "DELETE") {
        if (refuseReadOnly(response)) return;
        const id = decodeURIComponent(action.slice("attachments/".length));
        if (!entry.attachments.delete(id)) {
          json(response, 404, { code: "not_found", message: "No such unsent attachment." });
          return;
        }
        response.writeHead(204);
        response.end();
        return;
      }

      if (action === "commands" && method === "POST") {
        const body = await readBody(request);
        if (refuseReadOnly(response)) return;
        if (url.searchParams.get("fail") === "queue_full") {
          json(response, 503, {
            code: "queue_full",
            message: "The control queue is full. Retry later.",
          });
          return;
        }
        const outcome = acceptCommand(entry, body);
        json(response, outcome.status, outcome.payload);
        return;
      }

      if (action === "link" && method === "POST") {
        const body = await readBody(request);
        if (refuseReadOnly(response)) return;
        const linkKey = String(body.key ?? "");
        const storedLink = entry.receipts.get(linkKey);
        if (storedLink !== undefined) {
          // The link key is an idempotency key like any other: the same key
          // with the same payload returns the stored result, so a retry after
          // an uncertain outcome cannot create a second issue.
          if (storedLink.payload !== JSON.stringify(body)) {
            json(response, 409, {
              code: "idempotency_conflict",
              message: "That key was used with a different payload.",
            });
            return;
          }
          json(response, 200, {
            conversation: entry.conversation,
            issue: entry.issue,
            scheduling: { lane: entry.issue?.state ?? "Todo", runner_bound: false },
            next: { state: entry.issue?.state ?? "Todo", priority: null, dispatch: "later" },
          });
          return;
        }
        if (body.share_history !== true) {
          json(response, 422, {
            code: "share_history_required",
            message: "Linking shares the whole history. Confirm before linking.",
          });
          return;
        }
        if (entry.conversation.work_item_id !== null) {
          json(response, 409, {
            code: "conversation_already_linked",
            message: "This conversation is already linked.",
            details: { existing_conversation_id: entry.conversation.id },
          });
          return;
        }
        const issueBody = (body.issue ?? {}) as {
          title?: string;
          labels?: string[];
          priority?: string;
          state?: string;
        };
        // The next step (decisions.md §13.8, §14): the lane the issue lands
        // in, and whether it is dispatched now or left to be picked up.
        const nextStep = (body.next ?? {}) as {
          state?: string;
          /** The native rank, 0-3 (decisions.md §14). */
          priority?: number;
          dispatch?: string;
        };
        const lane = nextStep.state ?? issueBody.state ?? "Todo";
        const number = issues.size + 1000;
        const id = `wi_${number}`;
        const issue = {
          id,
          identifier: `mock#${number}`,
          title: issueBody.title ?? entry.conversation.title,
          state: lane,
        };
        issues.set(id, issue);
        entry.issue = issue;
        entry.receipts.set(linkKey, {
          payload: JSON.stringify(body),
          receipt: {
            key: linkKey,
            kind: "link",
            status: "delivered",
            message_id: null,
            question_id: null,
            error: null,
            updated_at: now(),
          },
        });
        touch(entry, {
          work_item_id: id,
          linked_at: now(),
          visibility: "shared",
          work_item: { ...issue, lane, runner_bound: false },
        });
        // Linking does not schedule: the issue waits for a runner, and
        // `continue` is the only control a conversation with no attempt has
        // (decisions.md §3 rule 6).
        setExecution(entry, {
          status: "waiting_for_runner",
          capabilities: { steer: false, interrupt: false, answer: false, continue: true },
        });
        // The result card is a message, so it is history: a second tab and a
        // reload both see it without a second endpoint.
        addStatusMessage(entry, `Linked to ${issue.identifier}.`, {
          issue: {
            ...issue,
            lane,
            runner_bound: false,
            ...(issueBody.labels === undefined ? {} : { labels: issueBody.labels }),
            ...(issueBody.priority === undefined ? {} : { priority: issueBody.priority }),
          },
        });
        json(response, 200, {
          conversation: entry.conversation,
          issue,
          scheduling: { lane, runner_bound: false },
          // The next step the mock applied, echoed the way the hub echoes it.
          next: {
            state: lane,
            priority: typeof nextStep.priority === "number" ? nextStep.priority : null,
            dispatch: nextStep.dispatch === "now" ? "now" : "later",
          },
        });
        return;
      }

      // Settled replaces Archive (decisions.md §13.9): the hub settles a
      // conversation on its own, so there is no action here. `__mock/settle`
      // below is a test hook, not a product endpoint.


      json(response, 404, { code: "not_found", message: "No such endpoint." });
    })().catch((cause: unknown) => {
      json(response, 500, { code: "invalid", message: String(cause) });
    });
  });

  return new Promise((resolve) => {
    server.listen(options.port ?? 0, "127.0.0.1", () => {
      const address = server.address() as AddressInfo;
      resolve({
        url: `http://127.0.0.1:${address.port}`,
        server,
        lastEnrollment: () => lastEnrollment,
        close: () =>
          new Promise<void>((done) => {
            for (const entry of store.values()) {
              for (const subscriber of entry.subscribers) subscriber.response.destroy();
              entry.subscribers.clear();
            }
            work.close();
            server.closeAllConnections?.();
            server.close(() => done());
          }),
      });
    });
  });
}

const isEntryPoint =
  process.argv[1] !== undefined && import.meta.url.endsWith(process.argv[1].split("/").pop() ?? "");

if (isEntryPoint) {
  const port = Number(process.env.MOCK_HUB_PORT ?? "4100");
  const mode = readCoordinatorMode(process.env.MOCK_COORDINATOR) ?? "hub";
  const account = readAccountMode(process.env.MOCK_ACCOUNT) ?? "write";
  void startMockHub({ port, coordinator: mode, account }).then((hub) => {
    process.stdout.write(
      `Mock conversation hub listening on ${hub.url} (coordinator: ${mode}, account: ${account})\n`,
    );
  });
}
