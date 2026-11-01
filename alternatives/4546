import React from "react";
import { Link } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Dialog, DialogClose, DialogDescription, DialogFooter, DialogHeader,
  DialogPanel, DialogPopup, DialogTitle,
} from "../../components/ui/dialog.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { ToggleGroup, Toggle } from "../../components/ui/toggle-group.tsx";
import { ControlError } from "../account/controls.tsx";
import { newKey } from "../account/idempotency.ts";
import { useMutation } from "../account/useResource.ts";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { formatUsd } from "../usage/usageFormat.ts";
import type { PlatformTenantDetail, PlatformAICredits } from "./api.ts";
import { useEntryApi } from "./EntryScreens.tsx";
import { platformTime } from "./platformFormat.ts";
import { redirectPlatformSignIn } from "./usePlatformResource.ts";

type Adjustment = PlatformAICredits["history"][number];

function AdjustmentRow({ item, control }: { readonly item: Adjustment; readonly control?: React.ReactNode }) {
  return <SettingsRow
    title={<span className="flex flex-wrap items-center gap-2">
      {item.amount_micros > 0 ? "+" : ""}{formatUsd(item.amount_micros / 1_000_000, 6)}
      <Badge variant={item.kind === "complimentary" ? "info" : "outline"}>
        {item.kind === "complimentary" ? "Complimentary" : "Purchased"}
      </Badge>
    </span>}
    description={[platformTime(item.at), item.actor || (item.kind === "auto_fund" ? "Automatic top-up" : ""), item.reason].filter(Boolean).join(" · ")}
    control={control}
  />;
}

export function TenantCredits({ detail, canGrant, onGrant, history = false }: {
  readonly detail: PlatformTenantDetail;
  readonly canGrant: boolean;
  readonly onGrant: () => void;
  readonly history?: boolean;
}) {
  const credits = detail.ai_credits;
  const historyLink = <Button variant="link" size="sm" render={<Link
    to={"/platform/tenants/$organization" as never}
    params={{ organization: detail.organization.id } as never}
    search={{ section: "credits" } as never}
  />}>All adjustments</Button>;
  return <div className="flex min-w-0 flex-col gap-6">
    <SettingsSection title="AI credits" aria-label="AI credits" headerAction={canGrant ?
      <Button variant="outline" size="sm" onClick={onGrant}>Grant credits</Button> : null}>
      <SettingsRow title="Balance" description="Credits pay for Luna chat." control={
        detail.credit_balance_micros === null ? "Unavailable" : formatUsd(detail.credit_balance_micros / 1_000_000, 6)
      } />
      <SettingsRow title={credits ? `Spent in ${new Date(`${credits.month}-01T00:00:00Z`).toLocaleDateString("en-US", { month: "long", timeZone: "UTC" })}` : "Monthly spend"}
        control={credits ? formatUsd(credits.spent_micros / 1_000_000, 6) : "Unavailable"} />
      <SettingsRow title="Auto top-up" control={credits ? <Badge variant="outline">{credits.auto_enabled ? "On" : "Off"}</Badge> : "Unavailable"} />
      {!history && credits ? (credits.history[0] ? <AdjustmentRow item={credits.history[0]} control={historyLink} /> :
        <SettingsRow title="No adjustments yet." control={historyLink} />) : null}
    </SettingsSection>
    {history ? <SettingsSection title="Adjustment history" aria-label="Adjustment history">
      {!credits ? <SettingsRow title="History unavailable" /> : credits.history.length === 0 ?
        <SettingsRow title="No adjustments yet." /> : credits.history.map((item, index) => <AdjustmentRow key={`${item.at}-${index}`} item={item} />)}
    </SettingsSection> : null}
  </div>;
}

export function GrantAICreditsDialog({ detail, open, onOpenChange, onChanged, returnFocus }: {
  readonly detail: PlatformTenantDetail;
  readonly returnFocus: React.RefObject<HTMLElement | null>;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onChanged: (balance: number) => Promise<void>;
}) {
  const api = useEntryApi();
  const [direction, setDirection] = React.useState("add");
  const [amount, setAmount] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [validation, setValidation] = React.useState<string | null>(null);
  const command = React.useRef<{ body: string; key: string } | null>(null);
  const busy = React.useRef(false);
  const change = useMutation(api.adjustPlatformAICredits);
  React.useEffect(() => redirectPlatformSignIn(change.error), [change.error]);
  const numeric = Number(amount);
  const validAmount = /^\d+(?:\.\d{1,6})?$/.test(amount) && numeric > 0 && Number.isSafeInteger(Math.round(numeric * 1_000_000));
  const signed = direction === "remove" ? -numeric : numeric;
  const balance = detail.credit_balance_micros;
  const uncertain = change.error !== null && (change.error.status === 0 || change.error.status >= 500);
  const prefix = React.useId();
  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (busy.current || balance === null) return;
    if (!validAmount) { setValidation("Enter a positive dollar amount with at most six decimal places."); return; }
    if (!reason.trim()) { setValidation("Give a reason for this adjustment."); return; }
    setValidation(null);
    const payload = { amount_usd: `${direction === "remove" ? "-" : ""}${amount}`, reason: reason.trim() };
    const body = JSON.stringify(payload);
    if (command.current?.body !== body) command.current = { body, key: newKey() };
    busy.current = true;
    const result = await change.call({ organization: detail.organization.id, csrf: detail.csrf, change: { ...payload, idempotency_key: command.current.key } });
    if (result) {
      command.current = null;
      await onChanged(result.balance_micros);
      onOpenChange(false);
      setAmount(""); setReason(""); setDirection("add");
    }
    busy.current = false;
  };
  return <Dialog open={open} onOpenChange={(value) => { if (!busy.current) onOpenChange(value); }}>
    <DialogPopup finalFocus={returnFocus}>
      <form className="contents" onSubmit={(event) => void submit(event)} noValidate>
        <DialogHeader>
          <DialogTitle>Grant AI credits</DialogTitle>
          <DialogDescription>{detail.organization.name}. Applies immediately and is recorded in the audit log.</DialogDescription>
        </DialogHeader>
        <DialogPanel className="grid gap-4">
          <ToggleGroup aria-label="Adjustment direction" value={[direction]} disabled={change.pending}
            onValueChange={(values) => { if (values[0]) setDirection(values[0]); }}>
            <Toggle value="add">Add credits</Toggle><Toggle value="remove">Remove credits</Toggle>
          </ToggleGroup>
          <div className="grid gap-1.5">
            <Label htmlFor={`${prefix}-amount`}>Amount (USD)</Label>
            <div className="flex flex-wrap items-center gap-2">
              <Input id={`${prefix}-amount`} className="min-w-0 flex-1" inputMode="decimal" value={amount} disabled={change.pending}
                onChange={(event) => setAmount(event.target.value)} />
              {[10, 25, 100].map((value) => <Button key={value} type="button" variant="outline" disabled={change.pending}
                onClick={() => setAmount(`${value}.00`)}>${value}</Button>)}
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`${prefix}-reason`}>Reason</Label>
            <Textarea id={`${prefix}-reason`} required maxLength={500} value={reason} disabled={change.pending}
              onChange={(event) => setReason(event.target.value)} aria-describedby={`${prefix}-reason-help`} />
            <p id={`${prefix}-reason-help`} className="text-xs text-muted-foreground">Required. Shown in the tenant’s credit history.</p>
          </div>
          <SettingsSection title="Balance preview" hideTitle aria-label="Balance preview">
            <SettingsRow title="Current balance" control={balance === null ? "Unavailable" : formatUsd(balance / 1_000_000, 6)} />
            <SettingsRow title="After this change" control={balance === null || !validAmount ? "—" : formatUsd(balance / 1_000_000 + signed, 6)} />
          </SettingsSection>
          <ControlError message={validation ?? change.error?.message ?? null} />
          {uncertain ? <p className="text-sm text-muted-foreground">The result is unknown. Retry the same amount and reason to safely confirm this adjustment.</p> : null}
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" disabled={change.pending} />}>Cancel</DialogClose>
          <Button type="submit" variant={direction === "remove" ? "destructive" : "default"} disabled={change.pending || balance === null} aria-busy={change.pending}>
            {change.pending ? "Applying…" : `${direction === "remove" ? "Remove" : "Grant"} ${validAmount ? formatUsd(numeric, 6) : "credits"}`}
          </Button>
        </DialogFooter>
      </form>
    </DialogPopup>
  </Dialog>;
}
