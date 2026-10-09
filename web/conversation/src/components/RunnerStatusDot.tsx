import type { FleetRunner } from "../contracts/account.ts";
import { cn } from "../lib/utils.ts";

export function RunnerStatusDot({
  runner,
  className,
}: {
  readonly runner?: Pick<
    FleetRunner,
    "state" | "health" | "claim_refusal_reason"
  >;
  readonly className?: string;
}) {
  const failed =
    runner?.state === "failed" ||
    (runner?.state === "active" && runner.health === "failed");
  const attention =
    runner?.state === "active" &&
    (runner.health === "needs_attention" || !!runner.claim_refusal_reason);
  const healthy =
    runner?.state === "active" &&
    !attention &&
    (runner.health === "healthy" || runner.health === "online");
  const label = failed
    ? "Failed"
    : runner?.state === "paused"
      ? "Paused"
      : attention
        ? "Needs attention"
        : healthy
          ? "Healthy"
          : "Offline";
  return (
    <span
      role="img"
      aria-label={label}
      title={label}
      className={cn("inline-flex shrink-0 items-center", className)}
    >
      <span
        aria-hidden="true"
        className={cn(
          "size-2 shrink-0 rounded-full",
          failed
            ? "bg-error"
            : attention
              ? "bg-warning"
              : healthy
                ? "bg-success motion-safe:animate-status-pulse"
                : "bg-muted-foreground",
        )}
      />
    </span>
  );
}
