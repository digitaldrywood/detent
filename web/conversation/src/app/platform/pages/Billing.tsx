import { Link } from "@tanstack/react-router";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { PlatformBilling } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformAccess } from "../access.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, ReasonDialog, StateLabel, StatStrip, day } from "../ui.tsx";

const FAILING = new Set(["payment_failed", "grace", "disputed", "refunded", "unapproved_plan"]);

export function billingSummary(value: PlatformBilling) {
  return {
    customers: value.customers.length,
    paying: value.customers.filter((customer) => customer.status === "active" || customer.status === "trialing").length,
    failing: value.customers.filter((customer) => FAILING.has(customer.status)).length,
    quarantined: value.events.filter((event) => event.status === "quarantined").length,
    pending: value.events.filter((event) => event.status === "pending").length,
  };
}

function orgLink(organization: { readonly id: string; readonly name: string }): React.ReactElement {
  return (
    <Link
      to={`/platform/organizations/${encodeURIComponent(organization.id)}/billing` as never}
      className="font-medium hover:underline"
    >
      {organization.name}
    </Link>
  );
}

export function BillingPage(): React.ReactElement {
  const api = usePlatformApi();
  const access = usePlatformAccess();
  const billing = useResource<PlatformBilling>(() => api.platformBilling(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Billing" }]} proposed>
      <Loadable resource={billing} label="billing">
        {(value) => {
          if (value.mode === null) {
            return (
              <SettingsSection title="Stripe">
                <SettingsRow className="text-sm"
                  title="Billing is off"
                  description="No Stripe account is configured, so every organization runs on its plan and grants alone. Configure billing in the entry config to take payments."
                  control={<StateLabel state="idle" label="Off" />}
                />
              </SettingsSection>
            );
          }
          const summary = billingSummary(value);
          const attention = value.customers.filter((customer) => FAILING.has(customer.status));
          return (
            <>
              <StatStrip
                items={[
                  { label: "Customers", value: summary.customers },
                  { label: "Paying", value: summary.paying, tone: "ok" },
                  { label: "Payment problems", value: summary.failing, tone: summary.failing > 0 ? "error" : undefined },
                  { label: "Events waiting", value: summary.pending, tone: summary.pending > 0 ? "warn" : undefined },
                  { label: "Quarantined", value: summary.quarantined, tone: summary.quarantined > 0 ? "error" : undefined },
                ]}
              />
              <SettingsSection title="Stripe">
                <SettingsRow className="text-sm"
                  title="Mode"
                  description={value.mode === "test" ? "Test mode: no real card is charged." : "Live mode: real payments."}
                  control={<Badge variant={value.mode === "live" ? "success" : "warning"}>{value.mode}</Badge>}
                />
                <SettingsRow className="text-sm" title="Account" control={<span className="font-mono text-xs">{value.account_id ?? "—"}</span>} />
              </SettingsSection>
              <SettingsSection title={`Payment problems (${attention.length})`}>
                {attention.length === 0 ? (
                  <EmptyNote>Every paying organization is in good standing.</EmptyNote>
                ) : (
                  attention.map((customer) => (
                    <SettingsRow className="text-sm"
                      key={customer.customer_id}
                      title={orgLink(customer.organization)}
                      description={`Customer ${customer.customer_id}, paid through ${day(customer.paid_through)}.`}
                      control={<StateLabel state={customer.status} />}
                    />
                  ))
                )}
              </SettingsSection>
              <SettingsSection title="Customers">
                {value.customers.length === 0 ? (
                  <EmptyNote>No organization has started checkout yet.</EmptyNote>
                ) : (
                  <Table aria-label="Customers">
                    <TableHeader>
                      <TableRow>
                        <TableHead>Organization</TableHead>
                        <TableHead>Customer</TableHead>
                        <TableHead>Plan</TableHead>
                        <TableHead>Status</TableHead>
                        <TableHead>Paid through</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {value.customers.map((customer) => (
                        <TableRow key={customer.customer_id}>
                          <TableCell>{orgLink(customer.organization)}</TableCell>
                          <TableCell className="font-mono text-xs">{customer.customer_id}</TableCell>
                          <TableCell>{customer.plan ?? "—"}</TableCell>
                          <TableCell>
                            <StateLabel state={customer.status} />
                          </TableCell>
                          <TableCell>{day(customer.paid_through)}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </SettingsSection>
              <SettingsSection title="Webhook events">
                {value.events.length === 0 ? (
                  <EmptyNote>No Stripe events have arrived.</EmptyNote>
                ) : (
                  <Table aria-label="Webhook events">
                    <TableHeader>
                      <TableRow>
                        <TableHead>Event</TableHead>
                        <TableHead>Organization</TableHead>
                        <TableHead>Delivery</TableHead>
                        <TableHead>Received</TableHead>
                        <TableHead />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {value.events.map((event) => (
                        <TableRow key={event.event_id}>
                          <TableCell>
                            <div className="font-mono text-xs">{event.event_type}</div>
                            <div className="font-mono text-xs text-muted-foreground">{event.event_id}</div>
                          </TableCell>
                          <TableCell>{event.organization === null ? <span className="text-muted-foreground">Unmatched</span> : orgLink(event.organization)}</TableCell>
                          <TableCell>
                            <StateLabel state={event.status} />
                            {event.attempts > 1 ? <div className="text-muted-foreground">{event.attempts} attempts</div> : null}
                          </TableCell>
                          <TableCell>
                            <Ago at={event.received_at} />
                          </TableCell>
                          <TableCell className="text-right">
                            {event.status === "quarantined" ? (
                              <ReasonDialog
                                trigger="Redeliver"
                                title={`Redeliver ${event.event_type}?`}
                                description="The entry sends the stored event to the organization's Hub again. The Hub ignores it if it already applied it."
                                confirm="Redeliver event"
                                onConfirm={async (reason) => (await api.redeliverBillingEvent({ event: event.event_id, csrf: access.csrf, reason })).message}
                              />
                            ) : null}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </SettingsSection>
            </>
          );
        }}
      </Loadable>
    </PlatformPage>
  );
}
