// Complimentary plan grants in the platform console. Only entitlement
// administrators get this panel; the entry re-checks that on every request,
// reaches the tenant Hub with its own credential, and forwards the revision and
// idempotency key minted here.
import React from "react";

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
  EntitlementGrant,
  OrganizationEntitlements,
  PlanReference,
  PlatformOrganization,
} from "./api.ts";
import { useEntryApi } from "./EntryScreens.tsx";

export const STALE_PLAN_MESSAGE =
  "Someone changed this organization's plan; reload and try again.";

function planName(plan: PlanReference): string {
  return `${plan.id} v${plan.version}`;
}

function planKey(plan: PlanReference): string {
  return `${plan.id}@${plan.version}`;
}

function day(value: string | null): string {
  if (value === null) return "Never";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toISOString().slice(0, 10);
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
}: {
  readonly organization: PlatformOrganization;
  readonly entitlements: OrganizationEntitlements;
  readonly csrf: string;
  readonly onChanged: () => Promise<void>;
  readonly modelChoice?: boolean;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const choices = entitlements.plans.filter(
    (plan) => planKey(plan) !== planKey(entitlements.base),
  );
  const [plan, setPlan] = React.useState("");
  const [expires, setExpires] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [key, setKey] = React.useState(newKey);
  const [retryRevision, setRetryRevision] = React.useState<number | null>(null);
  const change = useChange(organization.id, csrf, onChanged);
  const selected =
    choices.find((choice) => planKey(choice) === plan) ?? choices[0];
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
      <Button
        size="sm"
        onClick={() => reset(true)}
        disabled={
          modelChoice && (entitlements.features ?? []).includes("model_choice")
        }
      >
        {modelChoice ? "Grant model choice" : "Grant complimentary plan"}
      </Button>
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
                  <select
                    id={`${prefix}-plan`}
                    value={selected === undefined ? "" : planKey(selected)}
                    onChange={(event) => setPlan(event.currentTarget.value)}
                    className="h-9 rounded-lg border border-input bg-background px-2 text-sm"
                  >
                    {choices.map((choice) => (
                      <option key={planKey(choice)} value={planKey(choice)}>
                        {planName(choice)}
                      </option>
                    ))}
                  </select>
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

export function RevokeGrantDialog({
  organization,
  grant,
  revision,
  csrf,
  onChanged,
}: {
  readonly organization: PlatformOrganization;
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
        Revoke
      </Button>
      <DialogPopup>
        <form className="contents" onSubmit={onSubmit} noValidate>
          <DialogHeader>
            <DialogTitle>
              Revoke {modelChoice ? "model choice" : planName(grant.plan)} for{" "}
              {organization.name}?
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

function effectivePlan(entitlements: OrganizationEntitlements): string {
  const plan = `${planName(entitlements.effective_base)} (${entitlements.source})`;
  const grants = entitlements.grants.length;
  if (grants === 0) return plan;
  return `${plan} + ${grants} complimentary grant${grants === 1 ? "" : "s"}`;
}

export function TenantPlan({
  organization,
  csrf,
  value,
  canGrant,
  onChanged,
}: {
  readonly organization: PlatformOrganization;
  readonly csrf: string;
  readonly value: OrganizationEntitlements;
  readonly canGrant: boolean;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  return (
    <section
      aria-label={`Plan for ${organization.name}`}
      className="border-t border-border pt-4"
    >
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex-1">
          <h3 className="text-sm font-medium">Plan</h3>
        </div>
        {!canGrant ? null : (
          <>
            <GrantComplimentaryDialog
              organization={organization}
              entitlements={value}
              csrf={csrf}
              onChanged={onChanged}
            />
            <GrantComplimentaryDialog
              organization={organization}
              entitlements={value}
              csrf={csrf}
              onChanged={onChanged}
              modelChoice
            />
          </>
        )}
      </div>
      <dl className="mt-3 grid gap-3 text-sm sm:grid-cols-2">
        <div>
          <dt className="text-muted-foreground">Base plan</dt>
          <dd className="font-medium">{planName(value.base)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">Effective plan</dt>
          <dd className="font-medium">{effectivePlan(value)}</dd>
        </div>
      </dl>
      <p className="mt-3 text-sm text-muted-foreground">
        Model choice:{" "}
        {(value.features ?? []).includes("model_choice")
          ? "Enabled"
          : "Disabled"}
      </p>
      {value.grants.length === 0 ? (
        <p className="mt-3 text-sm text-muted-foreground">
          No active complimentary grants.
        </p>
      ) : (
        <Table
          aria-label={`Active grants for ${organization.name}`}
          className="mt-3"
        >
          <TableHeader>
            <TableRow>
              <TableHead>Plan</TableHead>
              <TableHead>Scope</TableHead>
              <TableHead>Starts</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead>Reason</TableHead>
              <TableHead>Granted by</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {value.grants.map((grant) => (
              <TableRow key={grant.id}>
                <TableCell>
                  <Badge variant="info">
                    {grant.scope?.length === 1 &&
                    grant.scope[0] === "model_choice"
                      ? "Model choice"
                      : planName(grant.plan)}
                  </Badge>
                </TableCell>
                <TableCell className="max-w-56 whitespace-normal text-muted-foreground">
                  {(grant.scope ?? []).join(", ")}
                </TableCell>
                <TableCell>{day(grant.starts_at)}</TableCell>
                <TableCell>{day(grant.expires_at)}</TableCell>
                <TableCell className="max-w-56 whitespace-normal">
                  {grant.reason}
                </TableCell>
                <TableCell>{grant.granted_by}</TableCell>
                <TableCell>
                  {canGrant ? (
                    <RevokeGrantDialog
                      organization={organization}
                      grant={grant}
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
    </section>
  );
}
