import React from "react";
import type { FleetRunner } from "../../contracts/account.ts";

export function RunnerCapacity({ runner, detailed = false }: { readonly runner: FleetRunner; readonly detailed?: boolean }): React.ReactElement {
  const capacity = runner.capacity_configuration;
  const effective = capacity?.effective;
  const applied = capacity?.applied;
  return <div className="space-y-1 text-xs text-muted-foreground">
    <p>Requested total: {capacity?.desired ?? runner.capacity_limit} slots</p>
    {detailed ? <p>{applied ? `Applied configuration: ${applied.local_limit} local · ${applied.client_limit} client · ${applied.runtime_limit} runtime slots` : "Applied configuration: not reported"}</p> : null}
    <p>{effective == null ? "Effective total: unknown" : `Effective total: ${effective} slots`}{capacity && capacity.status !== "applied" ? " · Pending or constrained" : ""}</p>
    <p>Shared machine: {runner.host_used} of {runner.host_capacity} slots in use{detailed ? " across runner identities" : ""}</p>
    {detailed ? capacity?.limits.filter((limit) => limit.constraint && (limit.limit == null || limit.limit < capacity.desired)).map((limit) => <p key={limit.owner} className="break-words">{limit.owner.replaceAll("_", " ")}{limit.limit == null ? "" : ` (${limit.limit} slots)`}: {limit.constraint}</p>) : null}
  </div>;
}
