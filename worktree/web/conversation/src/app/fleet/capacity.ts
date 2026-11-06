import type { FleetRunner } from "../../contracts/account.ts";

function canTakeWork(entry: FleetRunner): boolean {
  const health = entry.capacity_health ?? entry.health;
  if (entry.state !== "active" || (health !== "online" && health !== "asleep") || entry.claim_refusal_reason) return false;
  return entry.provider_capacity.length === 0 || entry.provider_capacity.some((provider) => provider.state !== "exhausted" && (provider.max_concurrent === undefined || provider.max_concurrent === 0 || provider.used < provider.max_concurrent));
}

export function fleetHosts(runners: readonly FleetRunner[]) {
  const machines = new Map<string, FleetRunner[]>();
  for (const runner of runners) {
    const group = machines.get(runner.machine_id) ?? [];
    group.push(runner);
    machines.set(runner.machine_id, group);
  }
  return [...machines.values()].map((group) => {
    const runner = group[0]!;
    const leases = [...new Map(group.flatMap((entry) => entry.leases.map((lease) => [lease.lease_id, lease] as const))).values()];
    const used = Math.max(leases.length, ...group.map((entry) => entry.host_used));
    const assigned = Math.min(runner.host_capacity, group.reduce((count, entry) => count + Math.max(0, entry.capacity_limit), 0));
    const capacity = Math.max(used, assigned);
    const available = group.reduce((count, entry) => canTakeWork(entry) ? count + Math.max(0, Math.min(entry.capacity_limit, entry.reported_capacity) - entry.leases.length) : count, 0);
    const free = Math.min(Math.max(0, capacity - used), available);
    return { runner, runners: group, leases, used, capacity, free };
  });
}
