// Foundations: the token, type, icon, layout and value rules of
// docs/design-system/foundations.md and patterns.md, rendered in a Light and a
// Dark frame. Token pages read `tokens.generated.json` (via `../tokens`) and
// paint each value through its CSS variable, so a swatch shows what the
// stylesheet really resolves to in that theme. Pages about rules (icons,
// breakpoints, formatting) render the real components and functions.
import {
  BellIcon,
  CheckIcon,
  ChevronDownIcon,
  CopyIcon,
  GitBranchIcon,
  PlusIcon,
  SearchIcon,
  SettingsIcon,
  TrashIcon,
} from "lucide-react";
import React from "react";

import { ThreadStatusLabel } from "../../../components/ThreadStatusIndicators.tsx";
import { resolveThreadStatusPill } from "../../../components/Sidebar.logic.ts";
import { formatShortcutLabel, DEFAULT_BINDINGS } from "../../../app/adapters/keybindings.ts";
import { formatDuration as formatResetDuration } from "../../../app/adapters/usageLimits.ts";
import { formatCount, formatPercent, formatTokens, formatUsd } from "../../../app/usage/usageFormat.ts";
import { PROVIDER_PRESENTATION } from "../../../app/usage/usageProviders.ts";
import { PROJECT_ICON_COLORS } from "../../../projectIconColors.ts";
import { formatDuration } from "../../../runtime/support/orchestrationTiming.ts";
import {
  formatChatTimestampTooltip,
  formatDayAwareTimestamp,
  formatRelativeTimeLabel,
  formatShortTimestamp,
} from "../../../timestampFormat.ts";
import { Badge } from "../../../components/ui/badge.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { Kbd, KbdGroup } from "../../../components/ui/kbd.tsx";
import { MiddleTruncate } from "../../../components/ui/middle-truncate.tsx";
import { TooltipProvider } from "../../../components/ui/tooltip.tsx";

import { Cell, Row, type GalleryDoc } from "../specimen";
import {
  displayValue,
  useFrameTheme,
  rootTokens,
  rootTokensNamed,
  tokenFile,
  valueIn,
  type Token,
} from "../tokens";
import { STATUS_THREADS } from "./compositionsA";

const SOURCE = "src/design-system/tokens.generated.json";

/** A small two-column definition table, in the frame's theme. */
function Table({ head, rows }: { readonly head: readonly string[]; readonly rows: readonly (readonly React.ReactNode[])[] }) {
  return (
    <div className="max-w-full overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">
        <thead>
          <tr>
            {head.map((cell) => (
              <th key={cell} scope="col" className="border-border border-b px-2 py-1.5 font-medium text-muted-foreground">
                {cell}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, index) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: static rows
            <tr key={index} className="border-border border-b last:border-b-0">
              {row.map((cell, column) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: static cells
                <td key={column} className="px-2 py-1.5 align-top">
                  {cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const mono = (text: string) => <code className="font-mono text-2xs">{text}</code>;

// --- Colour ---------------------------------------------------------------------

function Swatch({ token }: { readonly token: Token }) {
  const value = valueIn(token, useFrameTheme());
  return (
    <li className="flex min-w-0 items-center gap-2">
      <span
        aria-hidden
        className="size-8 shrink-0 rounded-md border border-border"
        style={{ background: `var(${token.name})` }}
      />
      <span className="flex min-w-0 flex-col">
        <code className="truncate font-mono text-xs">{token.name}</code>
        <span className="truncate font-mono text-2xs text-muted-foreground">
          {displayValue(value)}
          {token.utility !== null ? ` · ${token.utility}` : ""}
        </span>
      </span>
    </li>
  );
}

function Swatches({ tokens }: { readonly tokens: readonly Token[] }) {
  return (
    <ul className="grid gap-x-4 gap-y-2.5 sm:grid-cols-2 xl:grid-cols-3">
      {tokens
        .filter((token) => token.light?.hex !== null || token.dark?.hex !== null)
        .map((token) => (
          <Swatch key={`${token.scope}${token.name}`} token={token} />
        ))}
    </ul>
  );
}

const color: GalleryDoc = {
  meta: { name: "Colour", kind: "foundation", group: "Foundations", source: SOURCE },
  specimens: [
    {
      id: "roles",
      title: "Surface, text and boundary roles",
      note: "Each swatch paints its variable in this frame's theme; the label is the generated value and the utility key.",
      render: () => <Swatches tokens={rootTokens("color-role", "surface", "text", "border")} />,
    },
    {
      id: "feedback",
      title: "Feedback roles",
      note: "Outcome colours. A brand action (primary) and a successful outcome (success) are different roles.",
      render: () => <Swatches tokens={rootTokens("feedback")} />,
    },
    {
      id: "renderers",
      title: "Message, code and terminal roles",
      render: () => <Swatches tokens={rootTokens("message", "code", "terminal")} />,
    },
    {
      id: "sidebar",
      title: "Sidebar roles",
      note: "Painted inside [data-app-sidebar], where the sidebar scope redeclares them.",
      render: () => (
        <div data-app-sidebar className="rounded-lg bg-sidebar p-3 text-sidebar-foreground">
          <Swatches
            tokens={tokenFile.tokens.filter(
              (token) => (token.scope === "root" && token.group === "sidebar") || token.scope === "sidebar",
            )}
          />
        </div>
      ),
    },
  ],
};

// --- Categorical ----------------------------------------------------------------

const categorical: GalleryDoc = {
  meta: { name: "Categorical palette", kind: "foundation", group: "Foundations", source: "src/projectIconColors.ts" },
  specimens: [
    {
      id: "identity",
      title: "Identity hues",
      note: "The one categorical set: 18 hues for things a person tells apart by colour (projects, accounts). Swatch at 500; text at 600 light and 400 dark.",
      render: () => (
        <ul className="grid grid-cols-3 gap-x-4 gap-y-3 sm:grid-cols-6">
          {PROJECT_ICON_COLORS.map((entry) => (
            <li key={entry.value} className="flex flex-col items-start gap-1">
              <span aria-hidden className={`size-6 rounded-md ${entry.swatchClassName}`} />
              <span className={`font-medium text-xs ${entry.className}`}>{entry.label}</span>
            </li>
          ))}
        </ul>
      ),
    },
    {
      id: "series",
      title: "Chart series",
      note: "One stable colour per provider, in declaration order, from usageProviders.ts. Breakdown segments are neutral mixes of foreground, never a provider colour.",
      render: () => (
        <div className="flex flex-col gap-4">
          <Row>
            {Object.entries(PROVIDER_PRESENTATION).map(([id, provider]) => (
              <Cell key={id} label={id}>
                <span className="flex items-center gap-2 text-sm">
                  <span aria-hidden className="h-3 w-8 rounded-sm" style={{ background: provider.color }} />
                  {provider.label}
                </span>
              </Cell>
            ))}
          </Row>
          <Row>
            {[100, 72, 60, 44, 30].map((share) => (
              <Cell key={share} label={`ink ${share}%`}>
                <span
                  aria-hidden
                  className="h-3 w-12 rounded-sm"
                  style={{ background: `color-mix(in oklab, var(--contrast-foreground) ${share}%, var(--background))` }}
                />
              </Cell>
            ))}
          </Row>
        </div>
      ),
    },
    {
      id: "not-status",
      title: "Status colours are a separate set",
      note: "Outcome and state use the semantic roles, never an identity hue; a project coloured green is not a success.",
      render: () => (
        <Row>
          <Badge variant="success">Completed</Badge>
          <Badge variant="info">Queued</Badge>
          <Badge variant="warning">Needs review</Badge>
          <Badge variant="error">Failed</Badge>
        </Row>
      ),
    },
  ],
};

// --- Type -----------------------------------------------------------------------

const TYPE_ROLES: readonly { role: string; classes: string; sample: React.ReactNode }[] = [
  { role: "Page or onboarding headline", classes: "text-2xl font-semibold", sample: <p className="font-semibold text-2xl">Connect a repository</p> },
  { role: "Dialog or sheet title", classes: "text-xl font-semibold leading-none", sample: <p className="font-semibold text-xl leading-none">Delete project</p> },
  { role: "Section heading", classes: "text-sm font-medium", sample: <p className="font-medium text-sm">Notifications</p> },
  { role: "Body, labels, controls", classes: "text-base sm:text-sm", sample: <p className="text-base sm:text-sm">The runner picks up queued work every minute.</p> },
  { role: "Supporting text", classes: "text-sm text-muted-foreground", sample: <p className="text-muted-foreground text-sm">Applies to every project in this organization.</p> },
  { role: "Compact controls, metadata, tooltips", classes: "text-xs", sample: <p className="text-xs">Updated 5m ago · 3 files</p> },
  { role: "Dense annotation, counters", classes: "text-2xs tabular-nums", sample: <p className="text-2xs tabular-nums">+128 −42</p> },
  { role: "Eyebrow and status label", classes: "text-3xs font-semibold uppercase tracking-widest", sample: <p className="font-semibold text-3xs uppercase tracking-widest">Recent</p> },
  { role: "Icon badge overlay only", classes: "text-4xs / text-5xs", sample: <p className="text-4xs">2</p> },
  { role: "Code, paths, SHAs", classes: "font-mono text-xs", sample: <p className="font-mono text-xs">src/app/main.tsx · 9dd7335</p> },
];

const type: GalleryDoc = {
  meta: { name: "Type", kind: "foundation", group: "Foundations", source: "src/app/global.css" },
  specimens: [
    {
      id: "roles",
      title: "Type roles",
      note: "One role, one treatment. Controls are 16px below sm (no zoom on focus) and 14px from sm.",
      render: () => <Table head={["Role", "Classes", "Sample"]} rows={TYPE_ROLES.map((row) => [row.role, mono(row.classes), row.sample])} />,
    },
    {
      id: "scale",
      title: "Declared sizes",
      note: "The dense sizes Detent declares below text-xs, from the generated tokens, each at its own line height.",
      render: () => (
        <Table
          head={["Token", "Size", "Sample"]}
          rows={rootTokensNamed("--text-")
            .filter((token) => !token.name.includes("--line-height"))
            .map((token) => [
              mono(token.name),
              displayValue(token.light),
              <span
                key={token.name}
                style={{ fontSize: `var(${token.name})`, lineHeight: `var(${token.name}--line-height)` }}
              >
                Detent runs the agents
              </span>,
            ])}
        />
      ),
    },
    {
      id: "families",
      title: "Families and weights",
      render: () => (
        <div className="flex flex-col gap-2 text-sm">
          <p className="font-sans">font-sans — system UI stack, Geist fallback</p>
          <p className="font-mono">font-mono — ui-monospace stack, Geist Mono fallback</p>
          <p>
            <span className="font-normal">Regular for prose</span> · <span className="font-medium">medium for labels and actions</span> ·{" "}
            <span className="font-semibold">semibold for titles</span>
          </p>
          <p className="tabular-nums">tabular-nums: 1,204 · 98.6% · 00:42</p>
        </div>
      ),
    },
  ],
};

// --- Icons ----------------------------------------------------------------------

const ICON_SIZES: readonly { cls: string; px: number; use: string; icon: React.ReactNode }[] = [
  { cls: "size-3", px: 12, use: "micro controls, Kbd, chevrons in dense rows", icon: <GitBranchIcon aria-hidden className="size-3" /> },
  { cls: "size-3.5", px: 14, use: "compact controls, badges, inline metadata", icon: <GitBranchIcon aria-hidden className="size-3.5" /> },
  { cls: "size-4", px: 16, use: "default controls, menu and sidebar rows, alerts", icon: <GitBranchIcon aria-hidden className="size-4" /> },
  { cls: "size-4.5", px: 18, use: "default controls below sm (touch)", icon: <GitBranchIcon aria-hidden className="size-4.5" /> },
  { cls: "size-5", px: 20, use: "xl controls, provider marks", icon: <GitBranchIcon aria-hidden className="size-5" /> },
];

const icons: GalleryDoc = {
  meta: { name: "Icons", kind: "foundation", group: "Foundations", source: "lucide-react" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes",
      note: "Lucide at its default 2px stroke. Controls size their own icons; set a size only on an icon outside a control.",
      render: () => <Table head={["Class", "px", "Use", "Icon"]} rows={ICON_SIZES.map((row) => [mono(row.cls), row.px, row.use, row.icon])} />,
    },
    {
      id: "in-controls",
      title: "In controls: size and gap follow the control",
      note: "Leading icon, then the label. The trailing chevron of a picker is the last child.",
      render: () => (
        <div className="flex flex-col gap-3">
          <Row>
            <Cell label="default">
              <Button>
                <PlusIcon aria-hidden />
                New issue
              </Button>
            </Cell>
            <Cell label="sm">
              <Button size="sm" variant="outline">
                <CopyIcon aria-hidden />
                Copy
              </Button>
            </Cell>
            <Cell label="compact">
              <Button size="compact" variant="outline">
                <GitBranchIcon aria-hidden />
                main
                <ChevronDownIcon aria-hidden />
              </Button>
            </Cell>
            <Cell label="xs">
              <Button size="xs" variant="ghost">
                <SearchIcon aria-hidden />
                Search
              </Button>
            </Cell>
            <Cell label="micro">
              <Button size="micro" variant="ghost">
                <CheckIcon aria-hidden />
                Done
              </Button>
            </Cell>
          </Row>
          <Row>
            <Cell label="icon">
              <Button size="icon" variant="ghost" aria-label="Settings">
                <SettingsIcon aria-hidden />
              </Button>
            </Cell>
            <Cell label="icon-sm">
              <Button size="icon-sm" variant="ghost" aria-label="Notifications">
                <BellIcon aria-hidden />
              </Button>
            </Cell>
            <Cell label="icon-xs">
              <Button size="icon-xs" variant="ghost" aria-label="Delete">
                <TrashIcon aria-hidden />
              </Button>
            </Cell>
            <Cell label="badge">
              <Badge variant="outline">
                <GitBranchIcon aria-hidden />
                feat/design
              </Badge>
            </Cell>
          </Row>
        </div>
      ),
    },
    {
      id: "stroke",
      title: "Stroke",
      note: "2px everywhere. A 12px glyph that reads thin beside text may take 2.25; nothing else overrides the stroke.",
      render: () => (
        <Row>
          <Cell label="size-3 · 2">
            <CheckIcon aria-hidden className="size-3" />
          </Cell>
          <Cell label="size-3 · 2.25">
            <CheckIcon aria-hidden className="size-3" strokeWidth={2.25} />
          </Cell>
          <Cell label="size-4 · 2">
            <CheckIcon aria-hidden className="size-4" />
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Spacing and layout ---------------------------------------------------------

const SPACING_STEPS: readonly { cls: string; px: number; use: string }[] = [
  { cls: "gap-0.5", px: 2, use: "stacked label and value" },
  { cls: "gap-1", px: 4, use: "icon and label in compact controls" },
  { cls: "gap-1.5", px: 6, use: "icon and label; field label and input" },
  { cls: "gap-2", px: 8, use: "control groups" },
  { cls: "gap-3", px: 12, use: "toolbar and content rows; narrow gutter" },
  { cls: "gap-4", px: 16, use: "section insets (p-4)" },
  { cls: "gap-6", px: 24, use: "sections of a page" },
  { cls: "gap-8", px: 32, use: "major page regions" },
];

const spacing: GalleryDoc = {
  meta: { name: "Spacing and layout", kind: "foundation", group: "Foundations", source: SOURCE },
  specimens: [
    {
      id: "scale",
      title: "Spacing by purpose",
      note: "Tailwind's 4px step (--spacing: 0.25rem). Choose the step by purpose, not by eye.",
      render: () => (
        <Table
          head={["Step", "px", "Use", ""]}
          rows={SPACING_STEPS.map((step) => [
            mono(step.cls),
            step.px,
            step.use,
            <span key={step.cls} aria-hidden className="block h-3 rounded-sm bg-primary/60" style={{ width: step.px }} />,
          ])}
        />
      ),
    },
    {
      id: "layout-tokens",
      title: "Layout tokens",
      note: "Widths, heights and gutters the stylesheets declare. Values with env() resolve at runtime.",
      render: () => (
        <Table
          head={["Token", "Light", "Dark"]}
          rows={rootTokens("layout").map((token) => [mono(token.name), displayValue(token.light), displayValue(token.dark)])}
        />
      ),
    },
    {
      id: "page",
      title: "Page template",
      note: "Header and content share one gutter: 12px, 20px from sm. Readable 896px (max-w-4xl), wide 1024px, expanded 1152px.",
      render: () => (
        <div className="overflow-hidden rounded-lg border border-border">
          <div className="flex h-[52px] items-center border-border border-b px-3 text-sm font-medium sm:px-5">Page title</div>
          <div className="px-3 py-6 sm:px-5">
            <div className="mx-auto flex max-w-4xl flex-col gap-6">
              <section className="flex flex-col gap-2">
                <h3 className="font-medium text-sm">Section</h3>
                <div className="h-16 rounded-md bg-muted" />
              </section>
              <section className="flex flex-col gap-2">
                <h3 className="font-medium text-sm">Section</h3>
                <div className="h-16 rounded-md bg-muted" />
              </section>
            </div>
          </div>
        </div>
      ),
    },
  ],
};

// --- Radius ---------------------------------------------------------------------

const radius: GalleryDoc = {
  meta: { name: "Radius", kind: "foundation", group: "Foundations", source: SOURCE },
  specimens: [
    {
      id: "scale",
      title: "Radius scale",
      render: () => (
        <Row>
          {rootTokens("radius").map((token) => (
            <Cell key={token.name} label={`${token.utility ?? token.name} · ${displayValue(token.light)}`}>
              <span aria-hidden className="block size-12 border border-border bg-muted" style={{ borderRadius: `var(${token.name})` }} />
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

// --- Elevation ------------------------------------------------------------------

const elevation: GalleryDoc = {
  meta: { name: "Elevation", kind: "foundation", group: "Foundations", source: SOURCE },
  specimens: [
    {
      id: "tokens",
      title: "Shadow tokens",
      note: "Declared shadows, painted through their variables.",
      render: () => (
        <Row className="gap-8 p-4">
          {rootTokensNamed("--shadow-", "--inset-shadow-").map((token) => (
            <Cell key={token.name} label={token.name}>
              <span
                aria-hidden
                className="block h-14 w-24 rounded-lg bg-card"
                style={{ boxShadow: token.name.startsWith("--inset-") ? `inset var(${token.name})` : `var(${token.name})` }}
              />
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "surfaces",
      title: "Surfaces by elevation",
      note: "Flat structure uses borders; floating surfaces use their primitive's glass and shadow.",
      minHeight: 220,
      render: () => (
        <Row className="gap-8 p-4">
          <Cell label="flat: border">
            <span aria-hidden className="block h-16 w-28 rounded-lg border border-border bg-background" />
          </Cell>
          <Cell label="card">
            <span aria-hidden className="block h-16 w-28 rounded-xl border border-border bg-card shadow-xs/5" />
          </Cell>
          <Cell label="dropdown-glass">
            <span aria-hidden className="dropdown-glass block h-16 w-28 rounded-lg border border-border" />
          </Cell>
          <Cell label="dialog-glass">
            <span aria-hidden className="dialog-glass block h-16 w-28 rounded-2xl border border-border" />
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Layers ---------------------------------------------------------------------

const LAYERS: readonly [string, string][] = [
  ["z-10", "In-flow overlays; the fixed desktop sidebar"],
  ["z-20", "Sidebar resize rail"],
  ["z-30", "Marks overlaid on an icon (provider badge)"],
  ["z-40", "Drop overlays, docked composer, minimap"],
  ["z-(--z-sheet) · 46", "Sheets (sidebar, right panel)"],
  ["z-50", "Dialog and alert-dialog backdrop and popup; command palette"],
  ["z-[60]", "Media viewer over a dialog"],
  ["z-100", "Toasts"],
  ["z-[130]", "Menu, select, combobox, autocomplete and popover positioners"],
  ["z-[140]", "Tooltip and preview-card positioners"],
];

const layers: GalleryDoc = {
  meta: { name: "Layers", kind: "foundation", group: "Foundations", source: "src/components/ui" },
  specimens: [
    {
      id: "ladder",
      title: "The layer ladder",
      note: "Bottom to top. Each layer belongs to a primitive; a feature never sets a z-index of its own.",
      render: () => (
        <Table
          head={["Layer", "Surface", "Token value"]}
          rows={LAYERS.map(([layer, surface]) => [
            mono(layer),
            surface,
            layer.includes("--z-sheet") ? displayValue(rootTokensNamed("--z-sheet")[0]?.light ?? null) : "",
          ])}
        />
      ),
    },
  ],
};

// --- Motion ---------------------------------------------------------------------

const motion: GalleryDoc = {
  meta: { name: "Motion", kind: "foundation", group: "Foundations", source: SOURCE },
  specimens: [
    {
      id: "tokens",
      title: "Easing, duration and animation tokens",
      render: () => (
        <Table
          head={["Token", "Value"]}
          rows={rootTokens("motion").map((token) => [mono(token.name), mono(displayValue(token.light))])}
        />
      ),
    },
    {
      id: "indicators",
      title: "Stepped indicator animations",
      note: "Duty-cycled: they step between a few states. Gated by motion-safe; still under reduced motion.",
      render: () => (
        <Row>
          <Cell label="animate-skeleton">
            <span aria-hidden className="block h-3 w-24 rounded bg-muted motion-safe:animate-skeleton" />
          </Cell>
          <Cell label="animate-status-pulse">
            <span aria-hidden className="block size-2 rounded-full bg-sky-500 motion-safe:animate-status-pulse" />
          </Cell>
          <Cell label="animate-status-ping">
            <span className="relative flex size-3 items-center justify-center">
              <span aria-hidden className="absolute inset-0 rounded-full bg-warning/60 motion-safe:animate-status-ping" />
              <span aria-hidden className="relative size-2 rounded-full bg-warning" />
            </span>
          </Cell>
        </Row>
      ),
    },
    {
      id: "transitions",
      title: "Control transitions",
      note: "Hover these: colours change over 150ms ease-out; nothing moves or resizes on hover.",
      render: () => (
        <Row>
          <Button variant="outline">Outline</Button>
          <Button variant="ghost">Ghost</Button>
          <Button>Primary</Button>
        </Row>
      ),
    },
  ],
};

// --- Breakpoints ----------------------------------------------------------------

const BREAKPOINTS: readonly [string, string, string][] = [
  ["max-[320px]", "(max-width: 320px)", "Composer banners and badges take their narrowest form"],
  ["max-[400px]", "(max-width: 400px)", "Composer banners and badges compact"],
  ["sm", "(min-width: 640px)", "Controls step down to 32px and text-sm; gutters widen to 20px; dialogs stop docking to the bottom"],
  ["md", "(min-width: 768px)", "The sidebar is inline (below: a sheet); status labels appear beside their dots"],
  ["980px", "(max-width: 980px)", "At or below: the right panel opens as a sheet"],
  ["lg", "(min-width: 1024px)", "—"],
  ["xl", "(min-width: 1280px)", "—"],
  ["2xl", "(min-width: 1536px)", "—"],
  ["3xl", "(min-width: 1600px)", "useMediaQuery only"],
  ["4xl", "(min-width: 2000px)", "useMediaQuery only"],
  ["pointer-coarse", "(pointer: coarse)", "Hit areas extend to 44×44px; the prompt is at least 16px"],
];

function BreakpointTable() {
  const [, rerender] = React.useReducer((count: number) => count + 1, 0);
  React.useEffect(() => {
    globalThis.addEventListener?.("resize", rerender);
    return () => globalThis.removeEventListener?.("resize", rerender);
  }, []);
  const matches = (query: string) => globalThis.matchMedia?.(query).matches ?? false;
  return (
    <div className="flex flex-col gap-2">
      <p className="text-muted-foreground text-xs tabular-nums">This frame is {globalThis.innerWidth}px wide.</p>
      <Table
        head={["Name", "Query", "What changes", "Here"]}
        rows={BREAKPOINTS.map(([name, query, change]) => [
          mono(name),
          mono(query),
          change,
          matches(query) ? <Badge key={name} size="sm" variant="success">applies</Badge> : "",
        ])}
      />
    </div>
  );
}

const breakpoints: GalleryDoc = {
  meta: { name: "Breakpoints", kind: "foundation", group: "Foundations", source: "src/hooks/useMediaQuery.ts" },
  specimens: [
    {
      id: "table",
      title: "Breakpoints and what changes at each",
      note: "Viewport queries change the shell; container queries (@container) change a component inside a panel of unknown width. Switch to 390px to compare.",
      render: () => <BreakpointTable />,
    },
  ],
};

// --- Status indicators ----------------------------------------------------------

const status: GalleryDoc = {
  meta: { name: "Status indicators", kind: "foundation", group: "Foundations", source: "src/components/ThreadStatusIndicators.tsx" },
  specimens: [
    {
      id: "thread",
      title: "Thread status: dot and label",
      note: "A 6px dot with a text-3xs label (label from md); compact rows show the dot alone with a tooltip and an accessible name. Working pulses.",
      minHeight: 140,
      render: () => (
        <TooltipProvider>
          <Row>
            {Object.entries(STATUS_THREADS).map(([label, thread]) => {
              const pill = resolveThreadStatusPill({ thread });
              return pill === null ? null : (
                <Cell key={label} label={label}>
                  <span className="flex items-center gap-3">
                    <ThreadStatusLabel status={pill} />
                    <ThreadStatusLabel status={pill} compact />
                  </span>
                </Cell>
              );
            })}
          </Row>
        </TooltipProvider>
      ),
    },
    {
      id: "semantic",
      title: "Connection and outcome dots",
      note: "An 8px dot in a 12px box, from the semantic roles. Always paired with text or an accessible name.",
      render: () => (
        <Row>
          {(
            [
              ["Connected", "bg-success", false],
              ["Reconnecting", "bg-warning", true],
              ["Failed", "bg-destructive", false],
              ["Offline", "bg-muted-foreground/40", false],
            ] as const
          ).map(([label, fill, ping]) => (
            <Cell key={label} label={fill}>
              <span className="flex items-center gap-1.5 text-xs">
                <span className="relative flex size-3 items-center justify-center" role="img" aria-label={label}>
                  {ping ? <span aria-hidden className="absolute inset-0 rounded-full bg-warning/60 motion-safe:animate-status-ping" /> : null}
                  <span aria-hidden className={`relative size-2 rounded-full ${fill}`} />
                </span>
                {label}
              </span>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

// --- Shortcuts and formatting ---------------------------------------------------

const SHORTCUT_COMMANDS = ["commandPalette.toggle", "sidebar.toggle", "rightPanel.toggle", "diff.toggle", "chat.new", "thread.stop", "issue.status"] as const;

const shortcuts: GalleryDoc = {
  meta: { name: "Keyboard shortcuts", kind: "foundation", group: "Foundations", source: "src/app/adapters/keybindings.ts" },
  specimens: [
    {
      id: "display",
      title: "Display on macOS and elsewhere",
      note: "formatShortcutLabel: glyphs in ⌃⌥⇧⌘ order with no separator on macOS; words joined by + elsewhere.",
      render: () => (
        <Table
          head={["Command", "macOS", "Windows and Linux"]}
          rows={SHORTCUT_COMMANDS.map((command) => [
            mono(command),
            <Kbd key="mac">{formatShortcutLabel(DEFAULT_BINDINGS[command], "MacIntel")}</Kbd>,
            <Kbd key="other">{formatShortcutLabel(DEFAULT_BINDINGS[command], "Win32")}</Kbd>,
          ])}
        />
      ),
    },
    {
      id: "surfaces",
      title: "Where shortcuts appear",
      note: "In a tooltip or accessible label as “Label (⌘K)”, at the end of a menu row, and in the command palette. Sequences use KbdGroup.",
      render: () => (
        <Row>
          <Cell label="Kbd">
            <Kbd>⌘K</Kbd>
          </Cell>
          <Cell label="KbdGroup">
            <KbdGroup>
              <Kbd>⇧</Kbd>
              <Kbd>⌘</Kbd>
              <Kbd>O</Kbd>
            </KbdGroup>
          </Cell>
          <Cell label="single key (not while typing)">
            <Kbd>S</Kbd>
          </Cell>
        </Row>
      ),
    },
  ],
};

const NOW = Date.UTC(2026, 9, 6, 14, 30);
const iso = (offsetMs: number) => new Date(NOW - offsetMs).toISOString();

const formatting: GalleryDoc = {
  meta: { name: "Formatting values", kind: "foundation", group: "Foundations", source: "src/timestampFormat.ts" },
  specimens: [
    {
      id: "values",
      title: "Times, durations, counts and costs",
      note: "Live output of the shared formatters; never format these by hand.",
      render: () => (
        <Table
          head={["Value", "Function", "Output"]}
          rows={[
            ["Clock time", mono("formatShortTimestamp"), formatShortTimestamp(iso(0), "locale")],
            ["Clock time, 24-hour", mono("formatShortTimestamp"), formatShortTimestamp(iso(0), "24-hour")],
            ["Day-aware", mono("formatDayAwareTimestamp"), formatDayAwareTimestamp(iso(26 * 3_600_000), "locale", NOW)],
            ["Tooltip", mono("formatChatTimestampTooltip"), formatChatTimestampTooltip(iso(0), "24-hour")],
            ["Relative", mono("formatRelativeTimeLabel"), formatRelativeTimeLabel(new Date(Date.now() - 5 * 60_000).toISOString())],
            ["Run duration", mono("formatDuration"), [450, 3_200, 63_000, 3_725_000].map(formatDuration).join(" · ")],
            ["Time to reset", mono("usageLimits.formatDuration"), [720_000, 7_980_000, 273_600_000].map(formatResetDuration).join(" · ")],
            ["Count", mono("formatCount"), formatCount(12_345)],
            ["Tokens", mono("formatTokens"), [804_000, 76_700_000].map(formatTokens).join(" · ")],
            ["Cost", mono("formatUsd"), formatUsd(1234.5)],
            ["Share", mono("formatPercent"), [0.123, 0.0004].map((share) => formatPercent(share)).join(" · ")],
            ["Commit", mono("sha.slice(0, 7)"), <span key="sha" className="font-mono">9dd7335</span>],
            [
              "Path or branch",
              mono("MiddleTruncate"),
              <span key="path" className="block w-40">
                <MiddleTruncate value="web/conversation/src/design-system/gallery/Gallery.tsx" />
              </span>,
            ],
          ]}
        />
      ),
    },
  ],
};

export const FOUNDATIONS: Readonly<Record<string, GalleryDoc>> = {
  "foundations-color": color,
  "foundations-categorical": categorical,
  "foundations-type": type,
  "foundations-icons": icons,
  "foundations-spacing": spacing,
  "foundations-radius": radius,
  "foundations-elevation": elevation,
  "foundations-layers": layers,
  "foundations-motion": motion,
  "foundations-breakpoints": breakpoints,
  "foundations-status": status,
  "foundations-shortcuts": shortcuts,
  "foundations-formatting": formatting,
};
