import type React from "react";

export type SidebarStageBackdropVariant = "nightly" | "dev";
export type EnvironmentIdentificationPillLabel = "Dev" | "Nightly";

export function resolveSidebarStageBackdropVariant(
  stageLabel: string,
  enabled = true,
): SidebarStageBackdropVariant | null {
  if (!enabled) return null;
  const normalized = stageLabel.trim().toLowerCase();
  if (normalized === "nightly") return "nightly";
  if (normalized === "dev") return "dev";
  return null;
}

export function resolveSidebarStageFocusRingOffsetClass(
  variant: SidebarStageBackdropVariant,
): string {
  return variant === "nightly"
    ? "focus-visible:ring-offset-(--stage-night-bottom)"
    : "focus-visible:ring-offset-(--stage-art-bottom)";
}

export function resolveEnvironmentIdentificationPillLabel(
  stageLabel: string,
): EnvironmentIdentificationPillLabel | null {
  const normalized = stageLabel.trim().toLowerCase();
  if (normalized === "dev") return "Dev";
  if (normalized === "nightly") return "Nightly";
  return null;
}

export function useEnvironmentStageLabel(): string {
  return "";
}

export function useSidebarStageBackdropVariant(
  _enabled = true,
): SidebarStageBackdropVariant | null {
  return null;
}

export function StageBackdropButtonArt(_props: {
  variant: SidebarStageBackdropVariant;
}): null {
  return null;
}

export function SidebarStageBackdrop(_props: {
  variant: SidebarStageBackdropVariant;
}): React.ReactElement | null {
  return null;
}
