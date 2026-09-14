// Wire contracts for the hosted account surface.
//
// These schemas are the client half of `docs/conversation/decisions.md` §12:
// the extended `/app/bootstrap` payload and the organization, project,
// fleet, plan and billing endpoints under
// `/api/v2/organizations/:organization`. The fixtures under
// `src/contracts/fixtures/account-*.json` are the shared examples, bound to
// these schemas by `fixtures.ts`, so the Go side marshals the same files.
//
// Where §12 writes a field as `[...]` or names a Go report rather than a JSON
// shape, the struct below follows the Go type it names — `runnerauth.Runner`
// for a fleet row, `hubserver.HostedEntitlement` for the plan report,
// `hubserver.hostedBillingReport` for the billing report — and the deviations
// are recorded in README.md ("Contract ambiguities resolved").
import * as Schema from "effect/Schema";

import { ApiError } from "./conversation.ts";

// --- Roles and grants -------------------------------------------------------

/**
 * The hosted organization roles (`auth.ValidOrganizationRole`). `owner` and
 * `admin` manage the organization; `member` writes where a grant says so;
 * `viewer` reads only (decisions.md §10.11).
 */
export const OrganizationRole = Schema.Literals(["owner", "admin", "member", "viewer"]);
export type OrganizationRole = typeof OrganizationRole.Type;

/** True for the two roles §12 gates the management endpoints behind. */
export function canManageOrganization(role: string): boolean {
  return role === "owner" || role === "admin";
}

/** One project grant on a member: write access and runner management. */
export const ProjectGrant = Schema.Struct({
  project_id: Schema.String,
  write: Schema.Boolean,
  runner: Schema.Boolean,
});
export type ProjectGrant = typeof ProjectGrant.Type;

// --- Bootstrap --------------------------------------------------------------

/**
 * An organization the signed-in subject belongs to. `public_url` is the
 * origin that organization is served from, which is why switching is a
 * redirect through `/auth/oidc/start` rather than a state change here.
 */
export const BootstrapOrganization = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  public_url: Schema.String,
  current: Schema.Boolean,
});
export type BootstrapOrganization = typeof BootstrapOrganization.Type;

/**
 * The actor, extended with the two capability answers the account screens
 * need. `can_manage` is owner or admin; `can_manage_runners` is the fleet's
 * own grant, which a plain member can hold on some projects.
 */
export const AccountActor = Schema.Struct({
  principal_id: Schema.String,
  subject: Schema.String,
  email: Schema.String,
  role: Schema.String,
  can_manage: Schema.Boolean,
  can_manage_runners: Schema.Boolean,
});
export type AccountActor = typeof AccountActor.Type;

/** A workflow state, as `tracker.NativeState` marshals it. */
export const WorkflowState = Schema.Struct({
  name: Schema.String,
  terminal: Schema.Boolean,
  dispatchable: Schema.Boolean,
  transitions: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  operator_only: Schema.optional(Schema.Boolean),
});
export type WorkflowState = typeof WorkflowState.Type;

/**
 * A project as the extended bootstrap carries it. The conversation client's
 * own `BootstrapProject` fields (`labels`, `priorities`, `coordinator`) stay
 * optional here so one payload satisfies both schemas.
 */
export const AccountProject = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  profile: Schema.String,
  can_write: Schema.Boolean,
  can_manage_runners: Schema.Boolean,
  states: Schema.Array(WorkflowState),
  labels: Schema.optional(Schema.Array(Schema.String)),
  priorities: Schema.optional(Schema.Array(Schema.String)),
  coordinator: Schema.optional(Schema.Boolean),
});
export type AccountProject = typeof AccountProject.Type;

/**
 * An open support session. Its presence is what the organization banner reads:
 * somebody from Detent is acting as this organization, and the reader is
 * entitled to see who, why and until when (§12, "Plan, billing, support").
 */
export const SupportSession = Schema.Struct({
  actor: Schema.String,
  reason: Schema.String,
  expires_at: Schema.String,
});
export type SupportSession = typeof SupportSession.Type;

/** The plan summary the shell carries so the settings page can name it. */
export const PlanSummary = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  source: Schema.String,
  window_ends_at: Schema.String,
});
export type PlanSummary = typeof PlanSummary.Type;

/**
 * The client build the hub is serving: the hub binary's version, a content
 * hash of the bundle it serves, and when it started serving it. It rides along
 * with the update report as a second line, so a client that wants to know
 * whether the page itself is stale has it without a second request.
 */
export const ClientBuild = Schema.Struct({
  version: Schema.String,
  build: Schema.String,
  served_at: Schema.String,
});
export type ClientBuild = typeof ClientBuild.Type;

/** One enrolled runner, as the update report describes it. */
export const RunnerUpdate = Schema.Struct({
  runner_id: Schema.String,
  display_name: Schema.String,
  /** The Detent build its host reported; empty until one heartbeats. */
  version: Schema.String,
  online: Schema.Boolean,
  /** The hub's own comparison against `current`. */
  behind: Schema.Boolean,
});
export type RunnerUpdate = typeof RunnerUpdate.Type;

/**
 * `GET /app/updates`: what the sidebar footer's pill polls. `current` is the
 * version every runner is measured against and `source` names where it came
 * from — always "hub", the hub binary's own build, never a release feed.
 */
export const UpdatesReport = Schema.Struct({
  current: Schema.String,
  source: Schema.String,
  runners: Schema.Array(RunnerUpdate),
  behind_count: Schema.Number,
  client: ClientBuild,
});
export type UpdatesReport = typeof UpdatesReport.Type;

/**
 * `GET /app/bootstrap` (§12, "Serving"). It is a superset of the conversation
 * client's `Bootstrap`: the same payload decodes through both, so the shell
 * loads it once.
 */
export const AccountBootstrap = Schema.Struct({
  organization: Schema.Struct({ id: Schema.String, name: Schema.String }),
  organizations: Schema.Array(BootstrapOrganization),
  actor: AccountActor,
  projects: Schema.Array(AccountProject),
  support: Schema.NullOr(SupportSession),
  csrf_token: Schema.String,
  capabilities: Schema.Struct({
    coordinator: Schema.Boolean,
    attachments: Schema.Boolean,
  }),
  feature: Schema.Struct({ conversation: Schema.Boolean }),
  plan: Schema.NullOr(PlanSummary),
  api_base: Schema.String,
  /** The build the hub is serving, where it publishes one. The About row. */
  version: Schema.optional(Schema.String),
});
export type AccountBootstrap = typeof AccountBootstrap.Type;

// --- Members and invitations ------------------------------------------------

export const Member = Schema.Struct({
  id: Schema.String,
  user_id: Schema.String,
  email: Schema.String,
  role: Schema.String,
  /** `active` for a member who can sign in; anything else is disabled. */
  status: Schema.String,
  grants: Schema.Array(ProjectGrant),
});
export type Member = typeof Member.Type;

export const Invitation = Schema.Struct({
  id: Schema.String,
  email: Schema.String,
  role: Schema.String,
  created_at: Schema.String,
  expires_at: Schema.String,
});
export type Invitation = typeof Invitation.Type;

/** `GET /members`. A plain member sees only themselves and no invitations. */
export const MembersResponse = Schema.Struct({
  members: Schema.Array(Member),
  invitations: Schema.Array(Invitation),
});
export type MembersResponse = typeof MembersResponse.Type;

export const Organization = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  public_url: Schema.optional(Schema.String),
});
export type Organization = typeof Organization.Type;

/** `POST /api/v2/organizations` for a subject with no organization yet. */
export const CreateOrganizationResponse = Schema.Struct({
  organization: Organization,
  next: Schema.String,
});
export type CreateOrganizationResponse = typeof CreateOrganizationResponse.Type;

/**
 * `POST /invitations/accept` and `POST /switch` both answer with the one place
 * the browser has to go next: the client never guesses a destination, because
 * both end in a fresh sign-in against the organization that now owns the
 * session.
 */
export const NextResponse = Schema.Struct({ next: Schema.String });
export type NextResponse = typeof NextResponse.Type;

/** `POST /support/start`. */
export const SupportResponse = Schema.Struct({ support: SupportSession });
export type SupportResponse = typeof SupportResponse.Type;

// --- Project settings -------------------------------------------------------

/**
 * `GET/PUT {nativeBase}/integration` (`hubserver.ProjectIntegration`).
 *
 * `revision` is a JSON *string* on the wire (`json:"revision,string"`) and is
 * echoed back verbatim as `expected_revision`: the screen never parses it, so
 * a hub that widens the counter cannot break the round trip. A mismatch is the
 * `409 revision_conflict` the screen surfaces rather than overwriting somebody
 * else's edit — and in hosted mode the conflict carries no current revision,
 * so recovering means re-reading, never guessing.
 */
export const ProjectIntegration = Schema.Struct({
  profile: Schema.String,
  revision: Schema.String,
  intake: Schema.String,
  projection: Schema.String,
  repository_enabled: Schema.Boolean,
  repository: Schema.optional(Schema.String),
  authority: Schema.optional(Schema.Record(Schema.String, Schema.String)),
});
export type ProjectIntegration = typeof ProjectIntegration.Type;

/** The two values the hub accepts for each integration field. */
export const INTAKE_CHOICES = ["disabled", "manual"] as const;
export const PROJECTION_CHOICES = ["disabled", "summary"] as const;

export const PolicyRequirements = Schema.Struct({
  required_tags: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  runner_id: Schema.optional(Schema.String),
  machine_id: Schema.optional(Schema.String),
});
export type PolicyRequirements = typeof PolicyRequirements.Type;

export const PolicyGates = Schema.Struct({
  kind: Schema.String,
  plan_enabled: Schema.Boolean,
  plan_review: Schema.String,
  plan_stop_digest: Schema.String,
  auto_promote: Schema.Boolean,
  automated_review: Schema.String,
  required_checks: Schema.Number,
  validator: Schema.Boolean,
  security_audit: Schema.Boolean,
  merge_method: Schema.String,
});
export type PolicyGates = typeof PolicyGates.Type;

/** `policy.Descriptor`: the resolved policy a human approves by identity. */
export const PolicyDescriptor = Schema.Struct({
  schema: Schema.Number,
  policy_id: Schema.String,
  source_revision: Schema.String,
  source_digest: Schema.String,
  config_digest: Schema.String,
  profile: Schema.optional(Schema.String),
  requirements: PolicyRequirements,
  gates: PolicyGates,
});
export type PolicyDescriptor = typeof PolicyDescriptor.Type;

/** `GET {nativeBase}/policy` (`policy.Approval`). */
export const PolicyApproval = Schema.Struct({
  policy: PolicyDescriptor,
  approved_by: Schema.String,
  approved_at: Schema.String,
});
export type PolicyApproval = typeof PolicyApproval.Type;

// --- Onboarding (the first-run wizard) --------------------------------------

/**
 * `onboarding.Progress`. `revision` is a JSON string here too, and the `PUT`
 * response is this object alone rather than the whole project, so the wizard
 * takes the next revision from what it wrote (hosted-setup.js did the same by
 * reloading the page).
 */
export const OnboardingProgress = Schema.Struct({
  revision: Schema.String,
  repository: Schema.String,
  doctor: Schema.Boolean,
  provider: Schema.Boolean,
  artifacts: Schema.String,
  updated_at: Schema.optional(Schema.String),
});
export type OnboardingProgress = typeof OnboardingProgress.Type;

/** The two values `Progress.Validate` accepts, plus the unset one. */
export const REPOSITORY_CHOICES = ["", "existing", "generate"] as const;
export const ARTIFACT_CHOICES = ["", "local", "customer"] as const;

/**
 * One onboarding step. There are no step ids: `onboarding.Evaluate` names each
 * step and the name is its identity, and only two states exist. The wizard's
 * stepper reads exactly these (§12, "Projects and settings").
 */
export const OnboardingStepState = Schema.Literals(["ready", "action_required"]);
export type OnboardingStepState = typeof OnboardingStepState.Type;

export const OnboardingStep = Schema.Struct({
  name: Schema.String,
  state: OnboardingStepState,
  detail: Schema.String,
});
export type OnboardingStep = typeof OnboardingStep.Type;

/** The four steps `Evaluate()` emits, in its order. */
export const ONBOARDING_STEPS = [
  "Repository configuration",
  "Local validation",
  "Execution runner",
  "Artifact history",
] as const;

export const ArtifactBinding = Schema.Struct({
  service_id: Schema.String,
  origin: Schema.String,
  mode: Schema.String,
  hosted_opt_in: Schema.optional(Schema.Boolean),
  publisher_token_id: Schema.optional(Schema.String),
});
export type ArtifactBinding = typeof ArtifactBinding.Type;

export const RunnerExclusion = Schema.Struct({
  code: Schema.String,
  message: Schema.String,
});
export type RunnerExclusion = typeof RunnerExclusion.Type;

/**
 * `runnerauth.Eligibility` as the onboarding payload redacts it: the runner
 * with its leases and provider capacity stripped, plus the reasons it cannot
 * take this project's work. An empty `exclusions` is what makes step three
 * ready.
 */
export const RunnerEligibility = Schema.Struct({
  runner: Schema.Struct({
    runner_id: Schema.String,
    machine_id: Schema.optional(Schema.String),
    display_name: Schema.String,
    tags: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
    state: Schema.String,
    capacity_limit: Schema.Number,
    project_ids: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
    revision: Schema.Number,
    hostname: Schema.optional(Schema.String),
    health: Schema.optional(Schema.String),
    os: Schema.optional(Schema.String),
    architecture: Schema.optional(Schema.String),
    last_heartbeat_at: Schema.optional(Schema.String),
  }),
  exclusions: Schema.optional(Schema.NullOr(Schema.Array(RunnerExclusion))),
});
export type RunnerEligibility = typeof RunnerEligibility.Type;

/** `GET {nativeBase}/onboarding` (`onboarding.Project`). */
export const Onboarding = Schema.Struct({
  latest_run: Schema.optional(Schema.String),
  progress: OnboardingProgress,
  policy: Schema.optional(Schema.NullOr(PolicyApproval)),
  runners: Schema.NullOr(Schema.Array(RunnerEligibility)),
  artifact_services: Schema.NullOr(Schema.Array(ArtifactBinding)),
  steps: Schema.Array(OnboardingStep),
  ready: Schema.Boolean,
});
export type Onboarding = typeof Onboarding.Type;

/** `POST {organization}/runner-enrollments`: the one-time token. */
export const RunnerEnrollment = Schema.Struct({
  id: Schema.String,
  token: Schema.String,
  expires_at: Schema.String,
});
export type RunnerEnrollment = typeof RunnerEnrollment.Type;

// --- Projects ---------------------------------------------------------------

/**
 * `GET /projects` carries the readiness summary rather than the whole
 * onboarding project: the list needs "is this set up" and "what is left", and
 * the wizard reads the full `GET {nativeBase}/onboarding` when it opens.
 * `OnboardingStep` is defined with the rest of the onboarding shapes below.
 */
export const ProjectOnboardingSummary = Schema.Struct({
  ready: Schema.Boolean,
  steps: Schema.Array(OnboardingStep),
});
export type ProjectOnboardingSummary = typeof ProjectOnboardingSummary.Type;

export const ProjectSummary = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  profile: Schema.String,
  states: Schema.Array(WorkflowState),
  can_write: Schema.Boolean,
  can_manage_runners: Schema.Boolean,
  onboarding: ProjectOnboardingSummary,
});
export type ProjectSummary = typeof ProjectSummary.Type;

export const ProjectsResponse = Schema.Array(ProjectSummary);
export type ProjectsResponse = typeof ProjectsResponse.Type;

// --- Fleet and spend --------------------------------------------------------

/** One provider account's capacity on a runner (`providercapacity.View`). */
export const ProviderCapacity = Schema.Struct({
  provider: Schema.String,
  backend: Schema.String,
  account_alias: Schema.String,
  shared_account_alias: Schema.optional(Schema.String),
  models: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  max_concurrent: Schema.Number,
  availability: Schema.String,
  observed_at: Schema.String,
  reset_at: Schema.optional(Schema.String),
  used: Schema.Number,
  state: Schema.String,
  reason: Schema.optional(Schema.String),
});
export type ProviderCapacity = typeof ProviderCapacity.Type;

export const RunnerLease = Schema.Struct({
  lease_id: Schema.String,
  work_item_id: Schema.String,
  title: Schema.String,
  project_id: Schema.String,
  expires_at: Schema.String,
});
export type RunnerLease = typeof RunnerLease.Type;

/**
 * One row of `GET /fleet`. `host_capacity`/`host_used` are the shared machine's
 * numbers and `capacity_limit`/`reported_capacity` the runner's own, which is
 * why the host card draws two meters rather than one.
 */
export const FleetRunner = Schema.Struct({
  id: Schema.String,
  display_name: Schema.String,
  hostname: Schema.String,
  health: Schema.String,
  state: Schema.String,
  os: Schema.String,
  architecture: Schema.String,
  /**
   * The Detent build this runner's host reported. Compared against the
   * response's `current` to draw the update state the footer's pill points at.
   * Optional so a hub that does not publish it yet still decodes.
   */
  version: Schema.optional(Schema.String),
  host_capacity: Schema.Number,
  host_used: Schema.Number,
  capacity_limit: Schema.Number,
  reported_capacity: Schema.Number,
  provider_capacity: Schema.Array(ProviderCapacity),
  last_heartbeat_at: Schema.String,
  leases: Schema.Array(RunnerLease),
});
export type FleetRunner = typeof FleetRunner.Type;

/** One allowance: what the window consumed, and what the plan allows. */
export const Allowance = Schema.Struct({
  used: Schema.Number,
  limit: Schema.Number,
});
export type Allowance = typeof Allowance.Type;

export const FleetUsage = Schema.Struct({
  window_ends_at: Schema.String,
  allowances: Schema.Record(Schema.String, Allowance),
});
export type FleetUsage = typeof FleetUsage.Type;

export const ProjectSpend = Schema.Struct({
  project_id: Schema.String,
  amount: Schema.Number,
});
export type ProjectSpend = typeof ProjectSpend.Type;

/**
 * Spend is nullable on purpose: an organization with no metering configured
 * has no number, and the screen says so rather than drawing a zero that looks
 * like a measurement.
 */
export const Spend = Schema.Struct({
  today: Schema.Number,
  window: Schema.Number,
  by_project: Schema.Array(ProjectSpend),
  /** Points for the chart, newest last, where the hub has a series. */
  series: Schema.optional(
    Schema.Array(Schema.Struct({ at: Schema.String, amount: Schema.Number })),
  ),
  currency: Schema.optional(Schema.String),
});
export type Spend = typeof Spend.Type;

export const FleetResponse = Schema.Struct({
  runners: Schema.Array(FleetRunner),
  usage: FleetUsage,
  spend: Schema.NullOr(Spend),
  /**
   * The Detent build this hub runs, which is the version a runner is expected
   * to be on: the hub and the runner are the same binary. Optional so a hub
   * that does not publish it yet still decodes.
   */
  current: Schema.optional(Schema.String),
});
export type FleetResponse = typeof FleetResponse.Type;

// --- Plan and billing -------------------------------------------------------

export const PlanReference = Schema.Struct({
  id: Schema.String,
  version: Schema.Number,
});
export type PlanReference = typeof PlanReference.Type;

export const PlanGrant = Schema.Struct({
  id: Schema.String,
  plan: PlanReference,
  scope: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  starts_at: Schema.String,
  expires_at: Schema.optional(Schema.NullOr(Schema.String)),
  revoked_at: Schema.optional(Schema.NullOr(Schema.String)),
});
export type PlanGrant = typeof PlanGrant.Type;

/** `GET /plan`: `hubserver.HostedEntitlement`, verbatim. */
export const PlanReport = Schema.Struct({
  organization_id: Schema.String,
  base: PlanReference,
  effective_base: PlanReference,
  source: Schema.String,
  revision: Schema.Number,
  features: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
  allowances: Schema.Record(Schema.String, Schema.Number),
  grants: Schema.optional(Schema.NullOr(Schema.Array(PlanGrant))),
  usage: Schema.Record(Schema.String, Schema.Number),
  window_ends_at: Schema.String,
});
export type PlanReport = typeof PlanReport.Type;

export const BillingSubscription = Schema.Struct({
  subscription_id: Schema.String,
  price_id: Schema.String,
  status: Schema.String,
  invoice_id: Schema.optional(Schema.String),
  invoice_status: Schema.optional(Schema.String),
  invoice_created_at: Schema.optional(Schema.String),
  period_end: Schema.optional(Schema.String),
  trial_end: Schema.optional(Schema.String),
  cancel_at: Schema.optional(Schema.String),
  cancel_at_period_end: Schema.optional(Schema.Boolean),
  payment_hold: Schema.optional(Schema.String),
});
export type BillingSubscription = typeof BillingSubscription.Type;

export const BillingState = Schema.Struct({
  subscription: BillingSubscription,
  status: Schema.String,
  plan: PlanReference,
  paid_through: Schema.String,
  access_until: Schema.String,
  grace_until: Schema.String,
});
export type BillingState = typeof BillingState.Type;

export const BillingAudit = Schema.Struct({
  actor: Schema.String,
  action: Schema.String,
  summary: Schema.String,
  at: Schema.String,
});
export type BillingAudit = typeof BillingAudit.Type;

/** One price the hub is configured to sell, and the button that buys it. */
export const BillingPrice = Schema.Struct({
  id: Schema.String,
  label: Schema.String,
});
export type BillingPrice = typeof BillingPrice.Type;

/**
 * `GET /billing`: `hubserver.hostedBillingReport` plus `prices`. The report
 * alone cannot render the screen — the hosted Templ page read the configured
 * prices from `HostedBillingConfig` rather than from the report — so §12's
 * "checkout buttons per configured price" needs the list on the payload.
 */
export const BillingReport = Schema.Struct({
  organization_id: Schema.String,
  state: BillingState,
  entitlement: PlanReport,
  reconciled_at: Schema.String,
  pending_events: Schema.Number,
  recent_audit: Schema.optional(Schema.NullOr(Schema.Array(BillingAudit))),
  prices: Schema.Array(BillingPrice),
  /** False while impersonating: §12 refuses billing to a support session. */
  can_checkout: Schema.optional(Schema.Boolean),
});
export type BillingReport = typeof BillingReport.Type;

/** `POST /billing/checkout` and `POST /billing/portal`. */
export const CheckoutResponse = Schema.Struct({ url: Schema.String });
export type CheckoutResponse = typeof CheckoutResponse.Type;

// --- Decoders ---------------------------------------------------------------

export const decodeAccountBootstrap = Schema.decodeUnknownSync(AccountBootstrap);
export const decodeMembers = Schema.decodeUnknownSync(MembersResponse);
export const decodeMember = Schema.decodeUnknownSync(Member);
export const decodeInvitation = Schema.decodeUnknownSync(Invitation);
export const decodeProjects = Schema.decodeUnknownSync(ProjectsResponse);
export const decodeFleet = Schema.decodeUnknownSync(FleetResponse);
export const decodePlan = Schema.decodeUnknownSync(PlanReport);
export const decodeBilling = Schema.decodeUnknownSync(BillingReport);
export const decodeCheckout = Schema.decodeUnknownSync(CheckoutResponse);
export const decodeOnboarding = Schema.decodeUnknownSync(Onboarding);
export const decodeIntegration = Schema.decodeUnknownSync(ProjectIntegration);
export const decodePolicy = Schema.decodeUnknownSync(PolicyApproval);

export { ApiError };
