export type RunnerAccessTier = "sandbox" | "native-trusted";

export const runnerAccessOptions = [
  { tier: "sandbox", label: "Sandbox", hint: "Agents run inside the backend sandbox; safer, needs sandbox support on the machine. Recommended for shared machines." },
  { tier: "native-trusted", label: "Full access", hint: "Agents run with your own permissions on the machine (native-trusted); use on a machine dedicated to this work." },
] as const;

export const spriteSandboxUnavailableReason = "Not available on Fly Sprites yet: the Sprite VM does not support the agent sandbox.";
