import React from "react";
import { ChevronLeftIcon, ChevronRightIcon, DownloadIcon, LockKeyholeIcon, RefreshCwIcon } from "lucide-react";
import { Button } from "../../components/ui/button.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { InputGroup, InputGroupAddon, InputGroupInput } from "../../components/ui/input-group.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import { Select, SelectTrigger, SelectValue, SelectPopup, SelectItem } from "../../components/ui/select.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import { Table, TableHeader, TableHead, TableBody, TableRow, TableCell } from "../../components/ui/table.tsx";
import { Tooltip, TooltipTrigger, TooltipPopup } from "../../components/ui/tooltip.tsx";
import type { AICredits, BillingReport } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi } from "../account/context.ts";
import { newKey } from "../account/idempotency.ts";
import { useMutation } from "../account/useResource.ts";
import { SettingsSection } from "./settingsLayout.tsx";

export function creditAmount(micros: number): string {
  const amount = Math.abs(micros);
  return `${micros < 0 ? "−" : ""}${amount > 0 && amount < 10_000 ? "<$0.01" : `$${(amount / 1_000_000).toFixed(2)}`}`;
}

export function BillingPlanStatus({ report }: { readonly report: BillingReport }): React.ReactElement {
  const plan = report.entitlement;
  const state = report.state;
  const complimentary = plan.source === "grant" || (["free", ""].includes(state.status) && !["free", "payment_pending"].includes(plan.effective_base.id));
  const date = (value?: string | null) => value && !value.startsWith("0001-") && Number.isFinite(Date.parse(value)) ? value : null;
  const grant = plan.grants?.find((item) => item.plan.id === plan.effective_base.id && !item.revoked_at && !item.scope?.length && Date.parse(item.starts_at) <= Date.now() && (!item.expires_at || Date.parse(item.expires_at) > Date.now()));
  const expiry = complimentary ? date(grant?.expires_at) : null;
  const failed = ["grace", "payment_failed"].includes(state.status);
  const renewal = date(state.subscription.cancel_at) ?? date(state.subscription.period_end) ?? date(state.paid_through);
  const end = expiry ?? (state.status === "trialing" ? date(state.access_until) : failed ? date(state.grace_until) ?? date(state.access_until) : renewal);
  const deadlineLabel = state.status === "trialing" ? "Trial ends" : failed ? "Access until" : state.status === "canceling" || state.subscription.cancel_at_period_end || date(state.subscription.cancel_at) ? "Cancels" : "Renews";
  const formatted = end === null ? null : new Date(end).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" });
  const price = complimentary ? 0 : plan.monthly_usd_cents;
  return <div className="min-w-0 space-y-1">
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <span className="font-medium">{plan.name || (plan.effective_base.id === "free" ? "Free plan" : "Current plan")}</span>
      {price == null ? null : <span className="text-muted-foreground">${(price / 100).toLocaleString(undefined, { maximumFractionDigits: 2 })}/mo</span>}
      {complimentary ? <Badge variant="outline">Complimentary</Badge> : null}
      {failed ? <Badge variant="error">Payment needed</Badge> : null}
      {state.status === "multiple_subscriptions" ? <Badge variant="warning">Billing needs attention</Badge> : null}
      {state.status === "trialing" ? <Badge variant="info">Trial</Badge> : null}
    </div>
    {formatted === null ? null : <p className="flex items-center gap-1.5 text-xs text-muted-foreground"><span className={`size-1.5 shrink-0 rounded-full ${failed ? "bg-destructive" : "bg-success"}`} aria-hidden />
      {expiry ? `Expires ${formatted} · ${Math.max(0, Math.ceil((Date.parse(expiry) - Date.now()) / 86400000))} days left` : `${deadlineLabel} ${formatted}`}
    </p>}
  </div>;
}

export function CreditSettings({ credits, report, refresh }: {
  readonly credits: AICredits;
  readonly report: BillingReport;
  readonly refresh: () => Promise<void>;
}): React.ReactElement {
  const api = useAccountApi();
  const [price, setPrice] = React.useState(credits.price_id || credits.packs[0]?.price_id || "");
  const [buyPrice, setBuyPrice] = React.useState(credits.packs[0]?.price_id || "");
  const [threshold, setThreshold] = React.useState((credits.threshold_cents / 100).toFixed(2));
  const [pages, setPages] = React.useState<readonly AICredits[]>([credits]);
  const [page, setPage] = React.useState(0);
  const generation = React.useRef(0);
  React.useLayoutEffect(() => {
    generation.current += 1;
    setPages([credits]);
    setPage(0);
    setPrice(credits.price_id || credits.packs[0]?.price_id || "");
    setThreshold((credits.threshold_cents / 100).toFixed(2));
    return () => { generation.current += 1; };
  }, [credits]);
  const purchaseKeys = React.useRef(new Map<string, string>());
  const buy = useMutation(async () => {
    let key = purchaseKeys.current.get(buyPrice);
    if (key === undefined) { key = newKey(); purchaseKeys.current.set(buyPrice, key); }
    const result = await api.creditCheckout({ price: buyPrice, key });
    globalThis.location?.assign(result.url);
  });
  const save = useMutation(async (enabled: boolean) => {
    await api.creditAutoFund({ enabled, price, threshold_cents: Math.round(Number(threshold) * 100) });
    await refresh();
  });
  const reload = useMutation(refresh);
  const range = report.chat_usage?.range;
  const current = pages[page] ?? credits;
  const next = useMutation(async () => {
    if (page + 1 < pages.length) { setPage(page + 1); return; }
    if (!current.next_cursor || !range) return;
    const mine = generation.current;
    const result = await api.creditTransactions({ cursor: current.next_cursor, from: range.from, to: range.to });
    if (mine !== generation.current) return;
    setPages((previous) => [...previous, result]);
    setPage((previous) => previous + 1);
  });
  const packLabel = (pack: AICredits["packs"][number]) => `${pack.label} · $${(pack.usd_cents / 100).toFixed(2)}`;
  const packSelect = (value: string, onChange: (value: string) => void, label: string, disabled = false) => <div className={label === "Buy credit pack" ? "min-w-0 flex-1 sm:w-44" : "w-full min-w-0 sm:w-36"}><Select value={value} onValueChange={(item) => onChange(String(item))} disabled={disabled}>
    <SelectTrigger size="sm" aria-label={label}><SelectValue>{credits.packs.find((pack) => pack.price_id === value) ? label === "Buy credit pack" ? packLabel(credits.packs.find((pack) => pack.price_id === value)!) : credits.packs.find((pack) => pack.price_id === value)!.label : "Choose pack"}</SelectValue></SelectTrigger>
    <SelectPopup>{credits.packs.map((pack) => <SelectItem key={pack.price_id} value={pack.price_id}>{packLabel(pack)}</SelectItem>)}</SelectPopup>
  </Select></div>;
  const locked = !credits.can_auto_fund;
  const dirty = price !== (credits.price_id || credits.packs[0]?.price_id || "") || Math.round(Number(threshold) * 100) !== credits.threshold_cents;
  const labels: Record<string, string> = { purchase: "Credit purchase", auto_fund: "Automatic top-up", complimentary: "Complimentary grant" };
  const usage = report.chat_usage;
  const total = current.total_rows ?? current.history.length;
  const dayLabel = (at: string) => {
    const today = new Date().toISOString().slice(0, 10);
    const yesterday = new Date(Date.now() - 86400000).toISOString().slice(0, 10);
    return at.slice(0, 10) === today ? "Today" : at.slice(0, 10) === yesterday ? "Yesterday" : new Date(at).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
  };
  return <>
    <SettingsSection id="settings-ai-credits" title="AI credits">
      <MetricTileGroup>
        <MetricTile size="compact" className="p-4" label="Balance" value={<span className="inline-flex flex-wrap items-center gap-1.5"><span title={`$${(credits.balance_micros / 1_000_000).toFixed(6)}`}>{creditAmount(credits.balance_micros)}</span><Button size="icon-micro" variant="ghost-muted" aria-label="Refresh balance" disabled={reload.pending} onClick={() => void reload.call()}><RefreshCwIcon className="size-3.5" /></Button></span>} detail={credits.in_flight ? <Badge variant="info">Top-up pending</Badge> : undefined} />
        <MetricTile size="compact" className="p-4" label="Charged this period" value={creditAmount(report.charged_ai_micros ?? 0)} />
        <MetricTile size="compact" className="p-4" title={usage ? `Input: ${usage.input.toLocaleString()} · Cached input: ${usage.cached_input.toLocaleString()} · Output: ${usage.output.toLocaleString()} · Reasoning output: ${usage.reasoning_output.toLocaleString()}` : undefined} label={`Tokens · ${usage?.turns.toLocaleString() ?? 0} turns`} value={usage?.tokens.toLocaleString(undefined, { notation: "compact", maximumFractionDigits: 2 }) ?? "0"} detail={usage?.unpriced_turns ? <Badge variant="outline">{usage.unpriced_turns} turns unpriced</Badge> : undefined} />
        <MetricTile size="compact" className="p-4" label="Auto top-up" value={credits.auto_enabled ? "On" : <span className="text-muted-foreground">Off</span>} />
      </MetricTileGroup>
      <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3 text-sm">
        <span className="font-medium">Buy credits</span>
        <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">{packSelect(buyPrice, setBuyPrice, "Buy credit pack", buy.pending || credits.packs.length === 0)}<Button size="sm" disabled={buy.pending || !buyPrice} onClick={() => void buy.call()}>{buy.pending ? "Opening…" : "Buy"}</Button></div>
      </div>
      <form className="grid grid-cols-2 items-center gap-3 px-4 py-3 text-sm sm:flex sm:flex-wrap sm:justify-between" onSubmit={(event) => { event.preventDefault(); void save.call(credits.auto_enabled); }}>
        <span className="flex items-center gap-2 font-medium">Auto top-up{credits.failure ? <Tooltip><TooltipTrigger render={<Button size="xs" variant="ghost" aria-label="Auto top-up needs attention" />}><Badge variant="error">Needs attention</Badge></TooltipTrigger><TooltipPopup>Your card was declined. Update the payment method in the billing portal, then turn auto top-up back on.</TooltipPopup></Tooltip> : null}</span>
        <div className="col-span-2 grid min-w-0 grid-cols-2 items-end gap-2 sm:flex sm:items-center">
          <div className="flex min-w-0 flex-col gap-1 sm:flex-row sm:items-center sm:gap-2">
            <label htmlFor="credit-threshold" className="text-muted-foreground">Below</label>
            <div className="sm:w-24"><InputGroup><InputGroupAddon>$</InputGroupAddon><InputGroupInput id="credit-threshold" aria-label="Top up below (USD)" size="sm" type="number" min="0.01" step="0.01" required disabled={locked || save.pending} value={threshold} onChange={(event) => setThreshold(event.target.value)} /></InputGroup></div>
          </div>
          <div className="flex min-w-0 flex-col gap-1 sm:flex-row sm:items-center sm:gap-2">
            <span className="text-muted-foreground">add</span>
            {packSelect(price, setPrice, "Auto top-up credit pack", locked || save.pending)}
          </div>
        </div>
        <div className="col-start-2 row-start-1 flex items-center justify-end gap-2">
          <Switch size="sm" aria-label="Auto top-up" checked={credits.auto_enabled} disabled={locked || save.pending} onCheckedChange={(enabled) => void save.call(enabled)} />
          {locked ? <Tooltip><TooltipTrigger render={<Button size="icon-sm" variant="ghost-muted" aria-label="Auto top-up locked" />}><LockKeyholeIcon className="size-3.5" /></TooltipTrigger><TooltipPopup>Buy credits or save a payment method in the billing portal first.</TooltipPopup></Tooltip> : null}
        </div>
        {dirty && !locked ? <Button type="submit" className="col-span-2" size="sm" variant="outline" disabled={save.pending}>{save.pending ? "Saving…" : "Save"}</Button> : null}
      </form>
      {new URLSearchParams(globalThis.location?.search ?? "").get("credits") === "returned" ? <p role="status" className="px-4 py-3 text-sm text-muted-foreground">Waiting for Stripe to confirm your credits.</p> : null}
      <ControlError message={save.error?.message ?? buy.error?.message ?? reload.error?.message ?? null} />
    </SettingsSection>
    <SettingsSection id="settings-credit-transactions" title="Transactions" headerAction={range ? <Button size="xs" variant="outline" render={<a href={api.creditExportURL(range.from, range.to)} download />}><DownloadIcon className="size-3.5" />Download CSV</Button> : null}>
      {current.history.length === 0 ? <p className="px-4 py-10 text-center text-sm text-muted-foreground">No credit transactions yet.</p> : <>
        <Table aria-label="Credit transactions">
          <TableHeader><TableRow><TableHead>Day</TableHead><TableHead>Activity</TableHead><TableHead className="text-right">Amount</TableHead></TableRow></TableHeader>
          <TableBody>{current.history.slice(0, 10).map((item, index) => <TableRow key={`${item.at}-${index}`}><TableCell title={item.at}>{dayLabel(item.at)}</TableCell><TableCell>{item.kind === "usage" ? `Chat usage · ${item.turns ?? 1} ${(item.turns ?? 1) === 1 ? "turn" : "turns"}` : labels[item.kind] ?? "Credit adjustment"}</TableCell><TableCell className={`text-right tabular-nums ${item.amount_micros > 0 ? "text-success-foreground" : ""}`} title={`$${(item.amount_micros / 1_000_000).toFixed(6)}`}>{item.amount_micros > 0 ? "+" : ""}{creditAmount(item.amount_micros)}</TableCell></TableRow>)}</TableBody>
        </Table>
        <div className="flex items-center justify-between gap-3 px-4 py-2 text-xs text-muted-foreground"><span>{page * 10 + 1}–{page * 10 + Math.min(10, current.history.length)} of {total} transactions</span><div className="flex gap-1"><Button size="icon-xs" variant="outline" aria-label="Previous transactions" disabled={page === 0 || next.pending} onClick={() => setPage(page - 1)}><ChevronLeftIcon className="size-3.5" /></Button><Button size="icon-xs" variant="outline" aria-label="Next transactions" disabled={next.pending || (page + 1 >= pages.length && !current.next_cursor)} onClick={() => void next.call()}><ChevronRightIcon className="size-3.5" /></Button></div></div>
      </>}
      <ControlError message={next.error?.message ?? null} />
    </SettingsSection>
  </>;
}
