import { ClaudeAI, GrokIcon, type Icon, OpenAI } from "./providerMarks.tsx";

type UsageProviderPresentation = {
  readonly label: string;
  readonly color: string;
  readonly mark: Icon;
};

/**
 * Exhaustive presentation for providers supported by the usage contract.
 * Declaration order is reused by every chart and table, so adding a provider
 * only requires its contract support and one entry here.
 */
export const PROVIDER_PRESENTATION: Readonly<Record<string, UsageProviderPresentation>> = {
  codex: {
    label: "Codex",
    color: "var(--contrast-foreground)",
    mark: OpenAI,
  },
  claude: {
    label: "Claude Code",
    color: "#d97757",
    mark: ClaudeAI,
  },
  grok: {
    label: "Grok Build",
    // Contrast-aware neutral between the Codex series and muted chart chrome.
    color: "color-mix(in oklab, var(--contrast-foreground) 72%, var(--background))",
    mark: GrokIcon,
  },
};

/** Stable provider reading order across charts, summaries, tables, and hover rows. */
export const PROVIDER_ORDER = Object.keys(PROVIDER_PRESENTATION);

/** A dot with no brand mark, for a provider id the table above does not carry. */
const UNKNOWN_MARK: Icon = () => null;

/**
 * What to draw for a provider id. Unknown ids take the muted chart colour and
 * their own id as the label, because the hub naming a provider this client has
 * never heard of is a reason to show it, not a reason to hide it.
 */
export function presentationFor(provider: string, label?: string): UsageProviderPresentation {
  const known = PROVIDER_PRESENTATION[provider];
  if (known !== undefined) return known;
  return {
    label: label ?? provider,
    color: "color-mix(in oklab, var(--contrast-foreground) 45%, var(--background))",
    mark: UNKNOWN_MARK,
  };
}

/** Providers with real activity, independent of the metric currently displayed. */
export function providersWithUsage(
  totals: readonly {
    readonly provider: string;
    readonly costUsd: number;
    readonly totalTokens: number;
  }[],
): readonly string[] {
  const active = totals
    .filter((entry) => entry.totalTokens > 0 || entry.costUsd > 0)
    .map((entry) => entry.provider);
  const known = PROVIDER_ORDER.filter((provider) => active.includes(provider));
  const rest = active.filter((provider) => !PROVIDER_ORDER.includes(provider));
  return [...known, ...new Set(rest)];
}
