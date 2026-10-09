import { Tooltip, TooltipPopup, TooltipTrigger } from "./ui/tooltip.tsx";

export function StageProgress({
  steps,
  currentStep,
  progress,
  tone,
  label,
  description,
}: {
  readonly steps: readonly string[];
  readonly currentStep: number;
  readonly progress: number;
  readonly tone: "working" | "success" | "warning" | "error" | "muted";
  readonly label: string;
  readonly description: string;
}) {
  const fill = {
    working: "bg-sky-500 dark:bg-sky-300/80",
    success: "bg-success",
    warning: "bg-warning",
    error: "bg-error",
    muted: "bg-muted-foreground",
  }[tone];
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <div
            role="img"
            tabIndex={0}
            aria-label={label}
            className="flex gap-1 rounded-xs outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
        }
      >
        {steps.map((step, index) => (
          <span
            key={step}
            aria-hidden="true"
            className="h-1 min-w-0 flex-1 overflow-hidden rounded-full bg-muted"
          >
            <span
              className={`block h-full rounded-full ${index < currentStep ? "bg-muted-foreground/60" : fill}`}
              style={{
                width: `${index < currentStep ? 100 : index === currentStep ? Math.max(0, Math.min(1, progress)) * 100 : 0}%`,
              }}
            />
          </span>
        ))}
      </TooltipTrigger>
      <TooltipPopup>{description}</TooltipPopup>
    </Tooltip>
  );
}
