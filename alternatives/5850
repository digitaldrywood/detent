export type RunnerAccessTier = "sandbox" | "native-trusted";

export const runnerAccessOptions = [
  { tier: "sandbox", label: "Sandbox", hint: "Agents run inside the backend sandbox; safer, needs sandbox support on the machine. Recommended for shared machines." },
  { tier: "native-trusted", label: "Full access", hint: "Agents run with your own permissions on the machine (native-trusted); use on a machine dedicated to this work." },
] as const;

export const spriteSandboxUnavailableReason = "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.";

export function reportsSandbox(report: Readonly<Record<string, readonly string[]>> | undefined): boolean {
  const backends = Object.values(report ?? {});
  return backends.length > 0 && backends.every((tiers) => tiers.includes("sandbox"));
}
