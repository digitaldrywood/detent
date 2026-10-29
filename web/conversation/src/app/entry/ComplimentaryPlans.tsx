import React from "react";

import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import {
  Tooltip,
  TooltipPopup,
  TooltipProvider,
  TooltipTrigger,
} from "../../components/ui/tooltip.tsx";
import {
  featureLabel,
  grantChoices,
  planDisplayName,
  planPrice,
  platformTime,
  readableKey,
  tenantPlanGrant,
} from "./platformFormat.ts";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import {
  Select,
  SelectGroup,
  SelectGroupLabel,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { ControlError } from "../account/controls.tsx";
import { newKey } from "../account/idempotency.ts";
import { redirectPlatformSignIn } from "./usePlatformResource.ts";
import { AccountError } from "../account/api.ts";
import type {
  EntitlementChange,
  EntitlementPlan,
  EntitlementGrant,
  OrganizationEntitlements,
  PlanReference,
  PlatformOrganization,
} from "./api.ts";
import { useEntryApi } from "./EntryScreens.tsx";

export const STALE_PLAN_MESSAGE =
  "Someone changed this organization's plan; reload and try again.";

function planKey(plan: PlanReference): string {
  return `${plan.id}@${plan.version}`;
}

function failureMessage(error: unknown): string {
  if (error instanceof AccountError) {
    return error.status === 409 || error.code === "revision_conflict"
      ? STALE_PLAN_MESSAGE
      : error.message;
  }
  return error instanceof Error ? error.message : String(error);
}

function useChange(
  organization: string,
  csrf: string,
  onChanged: () => Promise<void>,
): {
  readonly pending: boolean;
  readonly error: string | null;
  readonly setError: (message: string | null) => void;
  readonly submit: (change: EntitlementChange) => Promise<ChangeOutcome>;
} {
  const api = useEntryApi();
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const submit = React.useCallback(
    async (change: EntitlementChange): Promise<ChangeOutcome> => {
      setPending(true);
      setError(null);
      try {
        await api.changePlatformEntitlement({ organization, csrf, change });
        await onChanged();
        return "done";
      } catch (cause) {
        redirectPlatformSignIn(cause);
        setError(failureMessage(cause));
        if (cause instanceof AccountError && cause.status === 409) {
          await onChanged();
          return "conflict";
        }
        return "failed";
      } finally {
        setPending(false);
      }
    },
    [api, organization, csrf, onChanged],
  );
  return { pending, error, setError, submit };
}

/**
 * What a submission ended in. A conflict means the Hub applied nothing, so the
 * next attempt is a new command; any other failure may have been applied, so
 * the retry resends the same key and revision and the Hub replays it.
 */
type ChangeOutcome = "done" | "conflict" | "failed";

export function GrantComplimentaryDialog({
  organization,
  entitlements,
  csrf,
  onChanged,
  modelChoice = false,
  trigger,
}: {
  readonly organization: PlatformOrganization;
  readonly entitlements: OrganizationEntitlements;
  readonly csrf: string;
  readonly onChanged: () => Promise<void>;
  readonly modelChoice?: boolean;
  readonly trigger?: (onOpen: () => void, disabled: boolean) => React.ReactNode;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const options = grantChoices(entitlements);
  const choices = options.map((option) => option.plan);
  const [plan, setPlan] = React.useState("");
  const [expires, setExpires] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [key, setKey] = React.useState(newKey);
  const [retryRevision, setRetryRevision] = React.useState<number | null>(null);
  const change = useChange(organization.id, csrf, onChanged);
  const selected =
    choices.find((choice) => planKey(choice) === plan) ?? choices[0];
  const selectedOption = options.find((option) => option.plan === selected);
  const prefix = `grant-${modelChoice ? "model-choice-" : ""}${organization.id}`;

  const reset = (next: boolean) => {
    setOpen(next);
    if (next) {
      setPlan(choices[0] === undefined ? "" : planKey(choices[0]));
      setExpires("");
      setReason("");
      setKey(newKey());
      setRetryRevision(null);
      change.setError(null);
    }
  };

  const onSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    const selection = modelChoice
      ? { feature: "model_choice" as const }
      : selected === undefined
        ? null
        : { plan: { id: selected.id, version: selected.version } };
    if (selection === null) {
      change.setError("No other plan is configured to grant.");
      return;
    }
    if (reason.trim() === "") {
      change.setError("Give a reason for this grant.");
      return;
    }
    const revision = retryRevision ?? entitlements.revision;
    const outcome = await change.submit({
      action: "grant",
      idempotency_key: key,
      expected_revision: revision,
      ...selection,
      expires_at: expires === "" ? null : `${expires}T23:59:59Z`,
      reason: reason.trim(),
    });
    settle(outcome, revision);
  };

  const settle = (outcome: ChangeOutcome, revision: number) => {
    if (outcome === "done") setOpen(false);
    if (outcome === "failed") {
      setRetryRevision(revision);
      return;
    }
    setKey(newKey());
    setRetryRevision(null);
  };

  return (
    <Dialog open={open} onOpenChange={reset}>
      {trigger ? (
        trigger(
          () => reset(true),
          modelChoice
            ? (entitlements.features ?? []).includes("model_choice")
            : choices.length === 0,
        )
      ) : (
        <TooltipProvider>
          <Tooltip>
            <TooltipTrigger
              render={
                <span
                  tabIndex={
                    !modelChoice && choices.length === 0 ? 0 : undefined
                  }
                />
              }
            >
              <Button
                size="sm"
                variant="outline"
                onClick={() => reset(true)}
                disabled={
                  modelChoice
                    ? (entitlements.features ?? []).includes("model_choice")
                    : choices.length === 0
                }
              >
                {modelChoice ? "Grant model choice" : "Change plan"}
              </Button>
            </TooltipTrigger>
            {!modelChoice && choices.length === 0 ? (
              <TooltipPopup>No other plan is configured to grant.</TooltipPopup>
            ) : null}
          </Tooltip>
        </TooltipProvider>
      )}
      <DialogPopup>
        <form className="contents" onSubmit={onSubmit} noValidate>
          <DialogHeader>
            <DialogTitle>
              {modelChoice
                ? "Grant model choice"
                : "Grant a complimentary plan"}{" "}
              to {organization.name}
            </DialogTitle>
            <DialogDescription>
              {modelChoice
                ? "The organization can choose from models reported by its runners until the grant expires or is revoked."
                : "The organization gets every feature and allowance of the chosen plan until the grant expires or is revoked."}
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <div className="flex flex-col gap-4">
              {modelChoice ? null : (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={`${prefix}-plan`}>Plan</Label>
                  <Select
                    value={selected === undefined ? "" : planKey(selected)}
                    items={choices.map((choice) => ({
                      value: planKey(choice),
                      label: planDisplayName(choice, entitlements),
                    }))}
                    onValueChange={(next) => setPlan(next ?? "")}
                  >
                    <SelectTrigger id={`${prefix}-plan`} className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectPopup>
                      {(["plans", "other"] as const).map((group) => {
                        const members = options.filter(
                          (option) => option.group === group,
                        );
                        return members.length === 0 ? null : (
                          <SelectGroup key={group}>
                            <SelectGroupLabel>
                              {group === "plans" ? "Plans" : "Other"}
                            </SelectGroupLabel>
                            {members.map((option) => (
                              <SelectItem
                                key={planKey(option.plan)}
                                value={planKey(option.plan)}
                              >
                                <span className="flex items-center gap-2">
                                  <span className="min-w-0 flex-1 truncate">
                                    {planDisplayName(option.plan, entitlements)}
                                  </span>
                                  {option.current ? (
                                    <Badge variant="outline" size="sm">
                                      Current
                                    </Badge>
                                  ) : null}
                                  <span className="text-muted-foreground text-xs tabular-nums">
                                    {planPrice(option.plan)}
                                  </span>
                                </span>
                              </SelectItem>
                            ))}
                          </SelectGroup>
                        );
                      })}
                    </SelectPopup>
                  </Select>
                  {selectedOption === undefined ? null : (
                    <GrantPlanSummary
                      plan={selectedOption.plan}
                      current={selectedOption.current}
                    />
                  )}
                </div>
              )}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${prefix}-expires`}>Expires (optional)</Label>
                <Input
                  id={`${prefix}-expires`}
                  type="date"
                  value={expires}
                  onChange={(event) => setExpires(event.currentTarget.value)}
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${prefix}-reason`}>Reason</Label>
                <Textarea
                  id={`${prefix}-reason`}
                  required
                  maxLength={500}
                  value={reason}
                  onChange={(event) => setReason(event.currentTarget.value)}
                />
              </div>
              <ControlError message={change.error} />
            </div>
          </DialogPanel>
          <DialogFooter>
            <DialogClose render={<Button variant="outline">Cancel</Button>} />
            <Button type="submit" disabled={change.pending}>
              {change.pending ? "Granting…" : "Grant"}
            </Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

function GrantPlanSummary({
  plan,
  current,
}: {
  readonly plan: EntitlementPlan;
  readonly current: boolean;
}): React.ReactElement {
  const slots = plan.allowances?.concurrent_work;
  return (
    <dl
      aria-label="Selected plan"
      className="flex flex-wrap items-center gap-x-4 gap-y-1.5 pt-1 text-muted-foreground text-xs"
    >
      <div className="flex items-center gap-1">
        <dt className="sr-only">Price</dt>
        <dd className="font-medium text-foreground tabular-nums">
          {planPrice(plan)}
        </dd>
      </div>
      <div className="flex items-center gap-1">
        <dt className="sr-only">Runner slots</dt>
        <dd className="tabular-nums">
          {slots === undefined
            ? "Unlimited runner slots"
            : `${slots.toLocaleString("en-US")} runner ${slots === 1 ? "slot" : "slots"}`}
        </dd>
      </div>
      {current ? (
        <div>
          <dt className="sr-only">Status</dt>
          <dd>
            <Badge variant="outline" size="sm">
              Current plan
            </Badge>
          </dd>
        </div>
      ) : null}
      <div className="flex basis-full flex-wrap items-center gap-1">
        <dt className="sr-only">Features</dt>
        {(plan.features ?? []).length === 0 ? (
          <dd>No features</dd>
        ) : (
          (plan.features ?? []).map((feature) => (
            <dd key={feature}>
              <Badge variant="secondary" size="sm">
                {featureLabel(feature)}
              </Badge>
            </dd>
          ))
        )}
      </div>
    </dl>
  );
}

export function RevokeGrantDialog({
  organization,
  grant,
  revision,
  csrf,
  onChanged,
  planLabel,
}: {
  readonly organization: PlatformOrganization;
  readonly planLabel?: string;
  readonly grant: EntitlementGrant;
  readonly revision: number;
  readonly csrf: string;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const [reason, setReason] = React.useState("");
  const [key, setKey] = React.useState(newKey);
  const [retryRevision, setRetryRevision] = React.useState<number | null>(null);
  const change = useChange(organization.id, csrf, onChanged);
  const reasonId = `revoke-${organization.id}-${grant.id}-reason`;
  const modelChoice =
    (grant.scope ?? []).length === 1 && grant.scope?.[0] === "model_choice";

  const reset = (next: boolean) => {
    setOpen(next);
    if (next) {
      setReason("");
      setKey(newKey());
      setRetryRevision(null);
      change.setError(null);
    }
  };

  const onSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (reason.trim() === "") {
      change.setError("Give a reason for revoking this grant.");
      return;
    }
    const sent = retryRevision ?? revision;
    const outcome = await change.submit({
      action: "revoke",
      idempotency_key: key,
      expected_revision: sent,
      grant_id: grant.id,
      reason: reason.trim(),
    });
    if (outcome === "done") setOpen(false);
    if (outcome === "failed") {
      setRetryRevision(sent);
      return;
    }
    setKey(newKey());
    setRetryRevision(null);
  };

  return (
    <Dialog open={open} onOpenChange={reset}>
      <Button size="sm" variant="outline" onClick={() => reset(true)}>
        Revoke grant
      </Button>
      <DialogPopup>
        <form className="contents" onSubmit={onSubmit} noValidate>
          <DialogHeader>
            <DialogTitle>
              Revoke{" "}
              {modelChoice
                ? "model choice"
                : (planLabel ?? planDisplayName(grant.plan))}{" "}
              for {organization.name}?
            </DialogTitle>
            <DialogDescription>
              {modelChoice
                ? "This grant will no longer allow the organization to choose models."
                : "The organization returns to its base plan. Existing data stays; new work must fit the base allowances."}
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor={reasonId}>Reason</Label>
              <Textarea
                id={reasonId}
                required
                maxLength={500}
                value={reason}
                onChange={(event) => setReason(event.currentTarget.value)}
              />
              <ControlError message={change.error} />
            </div>
          </DialogPanel>
          <DialogFooter>
            <DialogClose render={<Button variant="outline">Keep it</Button>} />
            <Button
              type="submit"
              variant="destructive"
              disabled={change.pending}
            >
              {change.pending ? "Revoking…" : "Revoke grant"}
            </Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

function PlanAdministrationDialog({
  organization,
  value,
  csrf,
  onChanged,
  create = false,
}: {
  readonly organization: PlatformOrganization;
  readonly value: OrganizationEntitlements;
  readonly csrf: string;
  readonly onChanged: () => Promise<void>;
  readonly create?: boolean;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const [sourceKey, setSourceKey] = React.useState("");
  const [definition, setDefinition] = React.useState<EntitlementPlan | null>(
    null,
  );
  const [action, setAction] = React.useState<
    "base" | "subscription" | "end_subscription"
  >("base");
  const [expires, setExpires] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [key, setKey] = React.useState(newKey);
  const [retryRevision, setRetryRevision] = React.useState<number | null>(null);
  const change = useChange(organization.id, csrf, onChanged);
  const prefix = `${create ? "create" : "assign"}-plan-${organization.id}`;
  const title = create ? "Create plan version" : "Change base or subscription";
  const featureChoices = [
    ...new Set(value.plans.flatMap((plan) => plan.features ?? [])),
  ].sort();

  const choose = (selection: string) => {
    setSourceKey(selection);
    const source = value.plans.find((plan) => planKey(plan) === selection);
    if (source !== undefined) {
      const nextVersion =
        Math.max(
          ...value.plans
            .filter((plan) => plan.id === source.id)
            .map((plan) => plan.version),
        ) + 1;
      setDefinition({
        ...source,
        version: nextVersion,
        features: [...(source.features ?? [])],
        allowances: { ...(source.allowances ?? {}) },
      });
    }
  };
  const reset = (next: boolean) => {
    setOpen(next);
    if (next) {
      choose(value.plans[0] === undefined ? "" : planKey(value.plans[0]));
      setAction("base");
      setExpires("");
      setReason("");
      setKey(newKey());
      setRetryRevision(null);
      change.setError(null);
    }
  };
  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (reason.trim() === "") {
      change.setError("Give a reason for this plan change.");
      return;
    }
    const selected = value.plans.find((plan) => planKey(plan) === sourceKey);
    if (selected === undefined || definition === null) {
      change.setError("Choose an existing plan to continue.");
      return;
    }
    if (
      create &&
      (!/^[a-zA-Z0-9_-]{1,128}$/.test(definition.id) ||
        !Number.isSafeInteger(definition.version) ||
        definition.version < 1 ||
        Object.values(definition.allowances ?? {}).some(
          (limit) => !Number.isSafeInteger(limit) || limit < 0,
        ))
    ) {
      change.setError(
        "Give a plan ID, positive version and nonnegative whole-number allowances.",
      );
      return;
    }
    if (!create && action === "subscription" && expires === "") {
      change.setError("A subscription requires an expiry date.");
      return;
    }
    const revision = retryRevision ?? value.revision;
    const common = {
      idempotency_key: key,
      expected_revision: revision,
      reason: reason.trim(),
    };
    const command: EntitlementChange = create
      ? {
          ...common,
          action: "create_plan",
          definition,
          plan: { id: definition.id, version: definition.version },
        }
      : {
          ...common,
          action,
          plan: { id: selected.id, version: selected.version },
          expires_at: expires === "" ? null : `${expires}T23:59:59Z`,
        };
    const outcome = await change.submit(command);
    if (outcome === "done") setOpen(false);
    if (outcome === "failed") {
      setRetryRevision(revision);
    } else {
      setKey(newKey());
      setRetryRevision(null);
    }
  };
  return (
    <Dialog open={open} onOpenChange={reset}>
      <Button size="sm" variant="outline" onClick={() => reset(true)}>
        {title}
      </Button>
      <DialogPopup>
        <form className="contents" onSubmit={submit} noValidate>
          <DialogHeader>
            <DialogTitle>
              {title} for {organization.name}
            </DialogTitle>
            <DialogDescription>
              {create
                ? "Copy a plan and adjust its features and allowances. Existing versions and organization access stay unchanged until you assign or grant the new version."
                : "Base changes apply immediately. Stripe manages paid subscriptions; use complimentary grants for additional access."}
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <div className="flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${prefix}-source`}>
                  {create ? "Copy from" : "Plan"}
                </Label>
                <select
                  id={`${prefix}-source`}
                  value={sourceKey}
                  onChange={(event) => choose(event.currentTarget.value)}
                  className="h-9 rounded-lg border border-input bg-background px-2 text-sm"
                >
                  {value.plans.map((plan) => (
                    <option key={planKey(plan)} value={planKey(plan)}>
                      {planDisplayName(plan, value)}
                    </option>
                  ))}
                </select>
              </div>
              {create && definition !== null ? (
                <>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={`${prefix}-id`}>Plan ID</Label>
                      <Input
                        id={`${prefix}-id`}
                        value={definition.id}
                        maxLength={128}
                        onChange={(event) =>
                          setDefinition({
                            ...definition,
                            id: event.currentTarget.value,
                          })
                        }
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={`${prefix}-version`}>Version</Label>
                      <Input
                        id={`${prefix}-version`}
                        type="number"
                        min={1}
                        step={1}
                        value={definition.version}
                        onChange={(event) =>
                          setDefinition({
                            ...definition,
                            version: Number(event.currentTarget.value),
                          })
                        }
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={`${prefix}-name`}>Display name</Label>
                      <Input
                        id={`${prefix}-name`}
                        value={definition.name ?? ""}
                        maxLength={80}
                        onChange={(event) =>
                          setDefinition({
                            ...definition,
                            name: event.currentTarget.value,
                          })
                        }
                      />
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={`${prefix}-price`}>
                        Monthly price (USD cents)
                      </Label>
                      <Input
                        id={`${prefix}-price`}
                        type="number"
                        min={0}
                        step={1}
                        value={definition.monthly_usd_cents ?? ""}
                        onChange={(event) =>
                          setDefinition({
                            ...definition,
                            monthly_usd_cents:
                              event.currentTarget.value === ""
                                ? undefined
                                : Number(event.currentTarget.value),
                          })
                        }
                      />
                    </div>
                  </div>
                  <fieldset className="flex flex-col gap-2">
                    <legend className="mb-2 text-sm font-medium">
                      Features
                    </legend>
                    {featureChoices.map((feature) => (
                      <Label
                        key={feature}
                        htmlFor={`${prefix}-feature-${feature}`}
                        className="flex items-center gap-2"
                      >
                        <Checkbox
                          id={`${prefix}-feature-${feature}`}
                          checked={(definition.features ?? []).includes(
                            feature,
                          )}
                          onCheckedChange={(checked) =>
                            setDefinition({
                              ...definition,
                              features: checked
                                ? [...(definition.features ?? []), feature]
                                : (definition.features ?? []).filter(
                                    (item) => item !== feature,
                                  ),
                            })
                          }
                        />
                        {feature.replaceAll("_", " ")}
                      </Label>
                    ))}
                  </fieldset>
                  <fieldset className="grid gap-3 sm:grid-cols-2">
                    <legend className="mb-2 text-sm font-medium">
                      Allowances
                    </legend>
                    {Object.entries(definition.allowances ?? {})
                      .sort(([a], [b]) => a.localeCompare(b))
                      .map(([name, limit]) => (
                        <div key={name} className="flex flex-col gap-1.5">
                          <Label htmlFor={`${prefix}-allowance-${name}`}>
                            {name.replaceAll("_", " ")}
                          </Label>
                          <Input
                            id={`${prefix}-allowance-${name}`}
                            type="number"
                            min={0}
                            step={1}
                            value={limit}
                            onChange={(event) =>
                              setDefinition({
                                ...definition,
                                allowances: {
                                  ...definition.allowances,
                                  [name]: Number(event.currentTarget.value),
                                },
                              })
                            }
                          />
                        </div>
                      ))}
                  </fieldset>
                </>
              ) : null}
              {!create ? (
                <>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor={`${prefix}-action`}>Apply as</Label>
                    <select
                      id={`${prefix}-action`}
                      value={action}
                      onChange={(event) =>
                        setAction(event.currentTarget.value as typeof action)
                      }
                      className="h-9 rounded-lg border border-input bg-background px-2 text-sm"
                    >
                      <option value="base">Base plan</option>
                      {!organization.billing.available ? (
                        <>
                          <option value="subscription">Subscription</option>
                          <option value="end_subscription">
                            End subscription
                          </option>
                        </>
                      ) : null}
                    </select>
                  </div>
                  {action === "subscription" ? (
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={`${prefix}-expires`}>Expires</Label>
                      <Input
                        id={`${prefix}-expires`}
                        type="date"
                        value={expires}
                        onChange={(event) =>
                          setExpires(event.currentTarget.value)
                        }
                      />
                    </div>
                  ) : null}
                </>
              ) : null}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${prefix}-reason`}>Reason</Label>
                <Textarea
                  id={`${prefix}-reason`}
                  maxLength={500}
                  required
                  value={reason}
                  onChange={(event) => setReason(event.currentTarget.value)}
                />
              </div>
              <ControlError message={change.error} />
            </div>
          </DialogPanel>
          <DialogFooter>
            <DialogClose render={<Button variant="outline">Cancel</Button>} />
            <Button type="submit" disabled={change.pending}>
              {change.pending
                ? "Saving…"
                : create
                  ? "Create version"
                  : "Apply plan"}
            </Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

export function TenantPlan({
  organization,
  csrf,
  value,
  canGrant,
  onChanged,
  history = false,
}: {
  readonly organization: PlatformOrganization;
  readonly csrf: string;
  readonly value: OrganizationEntitlements;
  readonly canGrant: boolean;
  readonly onChanged: () => Promise<void>;
  readonly history?: boolean;
}): React.ReactElement {
  const current = tenantPlanGrant(value);
  const currentPlan = current?.plan ?? value.effective_base;
  const records = value.grant_history ?? value.grants;
  return (
    <SettingsSection
      title="Plan"
      aria-label={`Plan for ${organization.name}`}
      headerAction={
        canGrant ? (
          <div className="flex flex-wrap gap-2">
            <PlanAdministrationDialog
              organization={organization}
              value={value}
              csrf={csrf}
              onChanged={onChanged}
              create
            />
            <PlanAdministrationDialog
              organization={organization}
              value={value}
              csrf={csrf}
              onChanged={onChanged}
            />
            <GrantComplimentaryDialog
              organization={organization}
              entitlements={value}
              csrf={csrf}
              onChanged={onChanged}
            />
          </div>
        ) : null
      }
    >
      <SettingsRow
        title={
          <span className="flex flex-wrap items-center gap-2">
            {planDisplayName(currentPlan, value)}
            {current ? <Badge variant="info">Complimentary</Badge> : null}
          </span>
        }
        description={
          current
            ? `Granted ${platformTime(current.granted_at ?? current.starts_at)} by ${current.granted_by}. ${current.expires_at ? `Returns to ${planDisplayName(value.effective_base, value)} on ${platformTime(current.expires_at)}.` : "No expiry."}`
            : `Base plan: ${planDisplayName(value.base, value)}`
        }
        control={
          current && canGrant ? (
            <RevokeGrantDialog
              organization={organization}
              grant={current}
              planLabel={planDisplayName(current.plan, value)}
              revision={value.revision}
              csrf={csrf}
              onChanged={onChanged}
            />
          ) : null
        }
      />
      <SettingsRow
        title="Model choice"
        description="Lets the organization pick models for its runs."
        control={
          <>
            <Badge variant="outline">
              {(value.features ?? []).includes("model_choice") ? "On" : "Off"}
            </Badge>
            {canGrant ? (
              <GrantComplimentaryDialog
                organization={organization}
                entitlements={value}
                csrf={csrf}
                onChanged={onChanged}
                modelChoice
              />
            ) : null}
          </>
        }
      />
      <SettingsRow
        title="Stripe subscription"
        control={
          <span className="text-sm text-muted-foreground">
            {organization.billing.available
              ? readableKey(organization.billing.status ?? "None")
              : "Unavailable"}
            {organization.billing.customer_id ? (
              <>
                {" "}
                ·{" "}
                <a
                  className="text-link underline"
                  href={`https://dashboard.stripe.com/customers/${encodeURIComponent(organization.billing.customer_id)}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  Open Stripe customer
                </a>
              </>
            ) : null}
          </span>
        }
      />
      {history ? (
        <div className="p-4">
          {records.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No complimentary grants.
            </p>
          ) : (
            <Table aria-label={`Grant history for ${organization.name}`}>
              <TableHeader>
                <TableRow>
                  <TableHead>Plan</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Scope</TableHead>
                  <TableHead>Starts</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead>Reason</TableHead>
                  <TableHead>Granted by</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {records.map((grant) => (
                  <TableRow key={grant.id}>
                    <TableCell>
                      {grant.scope?.length === 1 &&
                      grant.scope[0] === "model_choice"
                        ? "Model choice"
                        : planDisplayName(grant.plan, value)}
                    </TableCell>
                    <TableCell>
                      {grant.revoked_at
                        ? "Revoked"
                        : !value.grants.some((active) => active.id === grant.id)
                          ? "Inactive"
                          : "Active"}
                    </TableCell>
                    <TableCell className="max-w-56 whitespace-normal">
                      {(grant.scope ?? []).map(readableKey).join(", ")}
                    </TableCell>
                    <TableCell>{platformTime(grant.starts_at)}</TableCell>
                    <TableCell>{platformTime(grant.expires_at)}</TableCell>
                    <TableCell className="max-w-56 whitespace-normal">
                      {grant.reason}
                    </TableCell>
                    <TableCell>{grant.granted_by}</TableCell>
                    <TableCell>
                      {canGrant &&
                      grant.id !== current?.id &&
                      value.grants.some((active) => active.id === grant.id) ? (
                        <RevokeGrantDialog
                          organization={organization}
                          grant={grant}
                          planLabel={planDisplayName(grant.plan, value)}
                          revision={value.revision}
                          csrf={csrf}
                          onChanged={onChanged}
                        />
                      ) : null}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      ) : null}
    </SettingsSection>
  );
}
