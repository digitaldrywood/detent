import { Link } from "@tanstack/react-router";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import { Button } from "../../../components/ui/button.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { CatalogPlan, PlatformPlans } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { allowanceLabel } from "../../settings/Settings.tsx";
import { SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformAccess } from "../access.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { EmptyNote, Loadable, PlatformPage, day } from "../ui.tsx";

function PlanCard({ plan }: { readonly plan: CatalogPlan }): React.ReactElement {
  const allowances = Object.entries(plan.allowances).toSorted(([a], [b]) => a.localeCompare(b));
  return (
    <section
      aria-label={`${plan.id} version ${plan.version}`}
      className="flex flex-col gap-3 rounded-xl border border-border/60 bg-card/40 p-4"
    >
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <h3 className="text-sm font-medium">{plan.id}</h3>
          <p className="text-xs text-muted-foreground">
            Version {plan.version}, {plan.organizations} organization{plan.organizations === 1 ? "" : "s"}
          </p>
        </div>
        {plan.default ? (
          <Badge variant="secondary" size="sm">
            Default
          </Badge>
        ) : null}
      </div>
      <div className="flex flex-wrap gap-1">
        {plan.features.length === 0 ? (
          <span className="text-xs text-muted-foreground">No features</span>
        ) : (
          plan.features.map((feature) => (
            <Badge key={feature} variant="outline" size="sm">
              {feature.replaceAll("_", " ")}
            </Badge>
          ))
        )}
      </div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs">
        {allowances.map(([name, limit]) => (
          <div key={name} className="contents">
            <dt className="capitalize text-muted-foreground">{allowanceLabel(name)}</dt>
            <dd className="text-right tabular-nums">{limit.toLocaleString("en-US")}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

export function PlansPage(): React.ReactElement {
  const api = usePlatformApi();
  const access = usePlatformAccess();
  const plans = useResource<PlatformPlans>(() => api.plans(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Plans & grants" }]} proposed>
      <Loadable resource={plans} label="plans">
        {(value) => (
          <>
            <SettingsSection title="Plan catalog">
              {value.plans.length === 0 ? (
                <EmptyNote>No plans are configured. Add them under allocation.entitlements in the entry config.</EmptyNote>
              ) : (
                <div className="grid gap-3 p-3 sm:grid-cols-2 lg:grid-cols-3">
                  {value.plans.map((plan) => (
                    <PlanCard key={`${plan.id}@${plan.version}`} plan={plan} />
                  ))}
                </div>
              )}
            </SettingsSection>
            <SettingsSection
              title={`Complimentary grants (${value.grant_count})`}
            >
              {value.grant_count === 0 ? (
                <EmptyNote>No organization has a complimentary grant.</EmptyNote>
              ) : !access.canGrant ? (
                <EmptyNote>
                  {value.grant_count} organization{value.grant_count === 1 ? " has" : "s have"} a complimentary grant. Grant
                  details are limited to entitlement administrators.
                </EmptyNote>
              ) : (
                <Table aria-label="Complimentary grants">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Organization</TableHead>
                      <TableHead>Plan</TableHead>
                      <TableHead>Expires</TableHead>
                      <TableHead>Reason</TableHead>
                      <TableHead>Granted by</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {value.grants.map((grant) => {
                      const plan = `/platform/organizations/${encodeURIComponent(grant.organization.id)}/plan`;
                      return (
                        <TableRow key={grant.id}>
                          <TableCell>
                            <Link to={plan as never} className="font-medium hover:underline">
                              {grant.organization.name}
                            </Link>
                          </TableCell>
                          <TableCell>
                            <Badge variant="info">
                              {grant.plan.id} v{grant.plan.version}
                            </Badge>
                          </TableCell>
                          <TableCell>{day(grant.expires_at)}</TableCell>
                          <TableCell className="max-w-72 whitespace-normal text-muted-foreground">{grant.reason}</TableCell>
                          <TableCell>{grant.granted_by}</TableCell>
                          <TableCell className="text-right">
                            <Button size="xs" variant="outline" render={<Link to={plan as never} />}>
                              Manage
                            </Button>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </SettingsSection>
          </>
        )}
      </Loadable>
    </PlatformPage>
  );
}
