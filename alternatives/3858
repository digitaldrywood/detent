import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { AICredits } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi } from "../account/context.ts";
import { newKey } from "../account/idempotency.ts";
import { useMutation } from "../account/useResource.ts";
import { SettingsRow, SettingsSection } from "./settingsLayout.tsx";

export function CreditSettings({
  credits,
  refresh,
}: {
  readonly credits: AICredits;
  readonly refresh: () => Promise<void>;
}): React.ReactElement {
  const api = useAccountApi();
  const returned =
    new URLSearchParams(globalThis.location?.search ?? "").get("credits") ===
    "returned";
  const [enabled, setEnabled] = React.useState(credits.auto_enabled);
  const [price, setPrice] = React.useState(
    credits.price_id || credits.packs[0]?.price_id || "",
  );
  const [threshold, setThreshold] = React.useState(
    String(credits.threshold_cents / 100),
  );
  React.useEffect(() => {
    setEnabled(credits.auto_enabled);
    setPrice(credits.price_id || credits.packs[0]?.price_id || "");
    setThreshold(String(credits.threshold_cents / 100));
  }, [credits.auto_enabled, credits.price_id, credits.threshold_cents]);
  const purchaseKeys = React.useRef(new Map<string, string>());
  const buy = useMutation(async (pack: string) => {
    let key = purchaseKeys.current.get(pack);
    if (key === undefined) {
      key = newKey();
      purchaseKeys.current.set(pack, key);
    }
    const result = await api.creditCheckout({ price: pack, key });
    globalThis.location?.assign(result.url);
  });
  const save = useMutation(async () => {
    await api.creditAutoFund({
      enabled,
      price,
      threshold_cents: Math.round(Number(threshold) * 100),
    });
    await refresh();
  });
  const labels: Record<string, string> = {
    purchase: "Credit purchase",
    auto_fund: "Automatic top-up",
    usage: "Chat usage",
  };
  return (
    <SettingsSection id="settings-ai-credits" title="AI credits">
      {returned ? (
        <SettingsRow
          title="Credit checkout returned"
          description="Credits arrive after Stripe confirms payment. Refresh the balance to check."
        />
      ) : null}
      <SettingsRow
        title="Credit balance"
        status={`$${(credits.balance_micros / 1_000_000).toFixed(6)} USD`}
        description="Purchased credits do not expire. No sponsored AI allowance is included."
        control={
          <Button size="sm" variant="outline" onClick={() => void refresh()}>
            Refresh balance
          </Button>
        }
      />
      {credits.failure === "" ? null : (
        <SettingsRow title="Auto-fund disabled" description={credits.failure} />
      )}
      {credits.in_flight ? (
        <SettingsRow
          title="Automatic top-up pending"
          description="Payment is awaiting confirmation. Your balance changes after Stripe confirms payment."
        />
      ) : null}
      {credits.packs.map((pack) => (
        <SettingsRow
          key={pack.price_id}
          title={`${pack.label} · $${(pack.usd_cents / 100).toFixed(2)} USD`}
          description="Stripe confirms the price. This purchase saves your payment method for optional auto-fund."
          control={
            <Button
              size="sm"
              disabled={buy.pending}
              onClick={() => void buy.call(pack.price_id)}
            >
              Buy credits
            </Button>
          }
        />
      ))}
      <SettingsRow
        title="Auto-fund"
        description="When enabled, charge the saved payment method for the selected pack whenever your balance drops below the threshold. A failed charge disables auto-fund."
      >
        <form
          className="flex flex-col gap-3 py-3"
          onSubmit={(event) => {
            event.preventDefault();
            void save.call();
          }}
        >
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={enabled}
              disabled={save.pending}
              onChange={(event) => setEnabled(event.target.checked)}
            />
            Enable auto-fund
          </label>
          {!credits.can_auto_fund ? (
            <p className="text-muted-foreground text-sm">
              Buy credits or save a payment method in the billing portal before
              enabling auto-fund.
            </p>
          ) : null}
          <label
            className="flex flex-col gap-1 text-sm"
            htmlFor="credit-threshold"
          >
            Top up below (USD)
            <input
              id="credit-threshold"
              type="number"
              min="0.01"
              step="0.01"
              required={enabled}
              disabled={!enabled || save.pending}
              value={threshold}
              onChange={(event) => setThreshold(event.target.value)}
              className="rounded-md border bg-background px-3 py-2"
            />
          </label>
          <div className="flex flex-col gap-1 text-sm">
            <label htmlFor="credit-pack">Credit pack</label>
            <select
              id="credit-pack"
              value={price}
              disabled={!enabled || save.pending}
              onChange={(event) => setPrice(event.target.value)}
              className="rounded-md border bg-background px-3 py-2"
            >
              {credits.packs.map((pack) => (
                <option key={pack.price_id} value={pack.price_id}>
                  {pack.label} · ${(pack.usd_cents / 100).toFixed(2)} USD
                </option>
              ))}
            </select>
          </div>
          <Button type="submit" size="sm" disabled={save.pending}>
            {save.pending ? "Saving…" : "Save auto-fund"}
          </Button>
          <ControlError
            message={save.error?.message ?? buy.error?.message ?? null}
          />
        </form>
      </SettingsRow>
      <SettingsRow title="Credit transaction history">
        {credits.history.length === 0 ? (
          <p className="py-3 text-muted-foreground text-sm">
            No credit transactions yet.
          </p>
        ) : (
          <ol className="space-y-2 py-3 text-sm">
            {credits.history.map((item, index) => (
              <li
                key={`${item.at}-${index}`}
                className="flex flex-wrap justify-between gap-2"
              >
                <span>
                  {labels[item.kind] ?? item.kind} ·{" "}
                  {item.at.slice(0, 19).replace("T", " ")} UTC
                </span>
                <span>${(item.amount_micros / 1_000_000).toFixed(6)} USD</span>
              </li>
            ))}
          </ol>
        )}
      </SettingsRow>
    </SettingsSection>
  );
}
