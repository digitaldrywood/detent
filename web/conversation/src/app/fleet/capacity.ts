import type { FleetRunner } from "../../contracts/account.ts";

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
    const capacity = runner.host_capacity;
    const available = group.reduce((count, entry) => {
      if (entry.state !== "active" || (entry.health !== "online" && entry.health !== "asleep") || entry.claim_refusal_reason) return count;
      if (entry.provider_capacity.length > 0 && !entry.provider_capacity.some((provider) => provider.state !== "exhausted" && (provider.max_concurrent === undefined || provider.max_concurrent === 0 || provider.used < provider.max_concurrent))) return count;
      return count + Math.max(0, Math.min(entry.capacity_limit, entry.reported_capacity) - entry.leases.length);
    }, 0);
    const free = Math.min(Math.max(0, capacity - used), available);
    return { runner, leases, used, capacity, free };
  });
}
