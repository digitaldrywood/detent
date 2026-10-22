import type { Onboarding, OnboardingStep, ProjectIntegration } from "../../contracts/account.ts";
import { ONBOARDING_STEPS } from "../../contracts/account.ts";

export function firstUnreadyStep(steps: readonly OnboardingStep[]): number {
  const index = steps.findIndex((step) => step.state !== "ready");
  return index === -1 ? Math.max(steps.length - 1, 0) : index;
}

export function orderedSteps(onboarding: Onboarding | undefined, integration?: ProjectIntegration): readonly OnboardingStep[] {
  const sent = onboarding?.steps ?? [];
  const evaluated = ONBOARDING_STEPS.map(
    (name) =>
      sent.find((step) => step.name === name) ?? {
        name,
        state: "action_required" as const,
        detail: "",
      },
  );
  return [{
    name: "Repository",
    state: integration?.checkout_repository || integration?.repository ? "ready" : "action_required",
    detail: "Bind the project repository before enrolling a runner. The runner uses its own Git credentials to prepare the checkout.",
  }, ...evaluated];
}
