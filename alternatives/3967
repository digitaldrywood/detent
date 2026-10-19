export const DRAFT_HERO_TRANSITION_ANIMATION_ID = "detent-draft-hero-transition";
export const DRAFT_HERO_TRANSITION_DURATION_MS = 180;
export const DRAFT_HERO_TRANSITION_EASING = "cubic-bezier(0.4, 0, 0.2, 1)";
export const NARROW_COMPOSER_VIEW_TRANSITION_NAME = "detent-narrow-composer";
export const NARROW_DRAFT_HEADLINE_VIEW_TRANSITION_NAME = "detent-narrow-draft-headline";

type ComposerViewTransition = {
  readonly finished: Promise<void>;
};

let activeNarrowComposerTransition: Promise<void> | null = null;

type ComposerViewTransitionDocument = Document & {
  startViewTransition?: (update: () => void | Promise<void>) => ComposerViewTransition;
};

export async function waitForDraftHeroTransition(): Promise<void> {
  const narrowComposerTransition = activeNarrowComposerTransition;
  if (typeof document === "undefined" || typeof document.getAnimations !== "function") {
    await narrowComposerTransition;
    return;
  }

  const activeTransitions = document
    .getAnimations()
    .filter((animation) => animation.id === DRAFT_HERO_TRANSITION_ANIMATION_ID);

  await Promise.all([
    narrowComposerTransition,
    ...activeTransitions.map(async (animation) => {
      try {
        await animation.finished;
      } catch {
        // A cancelled transition is already safe to hand off.
      }
    }),
  ]);
}

export async function runNarrowComposerTransition(
  update: () => void | Promise<void>,
): Promise<void> {
  if (typeof document === "undefined" || typeof window === "undefined") {
    await update();
    return;
  }

  const transitionDocument = document as ComposerViewTransitionDocument;
  const narrowViewport = window.matchMedia?.("(max-width: 767px)").matches ?? false;
  const prefersReducedMotion =
    window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
  if (!narrowViewport || prefersReducedMotion || !transitionDocument.startViewTransition) {
    await update();
    return;
  }

  let updateStarted = false;
  const runUpdate = async () => {
    if (updateStarted) return;
    updateStarted = true;
    await update();
  };
  let transitionFinished: Promise<void> | null = null;
  transitionDocument.documentElement.dataset.detentComposerRouteTransition = "true";
  try {
    const transition = transitionDocument.startViewTransition(runUpdate);
    transitionFinished = transition.finished.catch(() => undefined);
    activeNarrowComposerTransition = transitionFinished;
    try {
      await transition.finished;
    } catch {
      await runUpdate();
    }
  } catch {
    await runUpdate();
  } finally {
    if (activeNarrowComposerTransition === transitionFinished) {
      activeNarrowComposerTransition = null;
    }
    delete transitionDocument.documentElement.dataset.detentComposerRouteTransition;
  }
}
