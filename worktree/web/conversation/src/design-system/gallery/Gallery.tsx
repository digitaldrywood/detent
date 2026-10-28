// The design-system gallery: a dev-only viewer of the component catalog that
// renders the real components (see `frame.tsx` for how themes are applied).
//
// The chrome itself is built from the app's primitives — Input, Badge,
// ToggleGroup, Button — so the gallery also dogfoods them.
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { MenuIcon, XIcon } from "lucide-react";
import React from "react";

import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/toggle-group";
import { cn } from "~/lib/utils";

import { groupEntries, type CatalogEntry, type CatalogStatus } from "./catalog";
import { FramePage, ThemeFrame, applyDocumentTheme, type FrameTheme } from "./frame";
import { docFor, galleryEntries } from "./registry";
import type { GalleryDoc, Specimen } from "./specimen";

export type ThemeView = "both" | "light" | "dark";
export type FrameWidth = "full" | "narrow";

const VIEW_KEY = "detent:design-system:view";
const WIDTH_KEY = "detent:design-system:width";

function readStored<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  try {
    const value = globalThis.localStorage?.getItem(key);
    return allowed.includes(value as T) ? (value as T) : fallback;
  } catch {
    return fallback;
  }
}

function writeStored(key: string, value: string): void {
  try {
    globalThis.localStorage?.setItem(key, value);
  } catch {
    // Storage is a convenience here; a blocked write changes nothing else.
  }
}

const STATUS_VARIANT: Record<CatalogStatus, "success" | "warning" | "outline"> = {
  available: "success",
  proposed: "outline",
  exception: "warning",
};

function EntryChips({ entry }: { readonly entry: CatalogEntry }) {
  if (entry.status === "available") return null;
  return (
    <Badge size="sm" variant={STATUS_VARIANT[entry.status]}>
      {entry.status}
    </Badge>
  );
}

function Nav({
  entries,
  activeId,
  onNavigate,
  onClose,
}: {
  readonly entries: readonly CatalogEntry[];
  readonly activeId: string | null;
  readonly onNavigate: () => void;
  /** Set while the navigation is open as a drawer below md. */
  readonly onClose?: (() => void) | undefined;
}) {
  const [query, setQuery] = React.useState("");
  const groups = groupEntries(entries, query);
  return (
    <nav aria-label="Design system" className="flex h-full min-h-0 flex-col">
      <div className="flex flex-col gap-3 border-b border-border p-3">
        <div className="flex items-center justify-between gap-2">
          <Link
            to="/design-system"
            onClick={onNavigate}
            activeOptions={{ exact: true }}
            className="font-semibold text-foreground text-sm"
            data-testid="design-system-home"
          >
            Detent design system
          </Link>
          {onClose !== undefined ? (
            <Button className="md:hidden" variant="ghost" size="icon-sm" aria-label="Close navigation" onClick={onClose}>
              <XIcon aria-hidden />
            </Button>
          ) : null}
        </div>
        <Input
          type="search"
          size="sm"
          placeholder="Search components"
          aria-label="Search components"
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {groups.length === 0 ? (
          <p className="px-2 py-4 text-muted-foreground text-sm">No component matches “{query}”.</p>
        ) : null}
        {groups.map((group) => (
          <section key={group.key} className="mb-3" aria-label={`${group.kind} ${group.group}`}>
            <h2 className="px-2 pb-1 font-medium text-2xs text-muted-foreground uppercase tracking-wide">
              {group.group}
              {group.kind !== "foundation" ? (
                <span className="ms-1 font-normal normal-case tracking-normal">· {group.kind}</span>
              ) : null}
            </h2>
            <ul className="flex flex-col gap-px">
              {group.entries.map((entry) => (
                <li key={entry.id}>
                  <Link
                    to="/design-system/$entryId"
                    params={{ entryId: entry.id }}
                    onClick={onNavigate}
                    aria-current={entry.id === activeId ? "page" : undefined}
                    className={cn(
                      "flex items-center justify-between gap-2 rounded-md px-2 py-1 text-sm text-foreground/80 hover:bg-accent hover:text-foreground",
                      "aria-[current=page]:bg-accent aria-[current=page]:font-medium aria-[current=page]:text-foreground",
                    )}
                  >
                    <span className="truncate">{entry.name}</span>
                    <EntryChips entry={entry} />
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </nav>
  );
}

function Controls({
  view,
  width,
  onView,
  onWidth,
}: {
  readonly view: ThemeView;
  readonly width: FrameWidth;
  readonly onView: (view: ThemeView) => void;
  readonly onWidth: (width: FrameWidth) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <ToggleGroup
        aria-label="Theme"
        value={[view]}
        onValueChange={(values) => {
          const next = values[0] as ThemeView | undefined;
          if (next !== undefined) onView(next);
        }}
      >
        <ToggleGroupItem value="both">Light + Dark</ToggleGroupItem>
        <ToggleGroupItem value="light">Light</ToggleGroupItem>
        <ToggleGroupItem value="dark">Dark</ToggleGroupItem>
      </ToggleGroup>
      <ToggleGroup
        aria-label="Frame width"
        value={[width]}
        onValueChange={(values) => {
          const next = values[0] as FrameWidth | undefined;
          if (next !== undefined) onWidth(next);
        }}
      >
        <ToggleGroupItem value="full">Full width</ToggleGroupItem>
        <ToggleGroupItem value="narrow">390px</ToggleGroupItem>
      </ToggleGroup>
    </div>
  );
}

function Field({ label, children }: { readonly label: string; readonly children: React.ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-2xs text-muted-foreground uppercase tracking-wide">{label}</dt>
      <dd className="m-0 min-w-0 text-foreground/90 text-sm">{children}</dd>
    </div>
  );
}

/**
 * An app-route specimen shows one live route at a time, with its own theme
 * switch. Two copies of the app in one page share its local storage and
 * answer each other's writes without end, so they are never mounted together.
 */
function AppRouteThemes({
  entryId,
  specimen,
  width,
}: {
  readonly entryId: string;
  readonly specimen: Specimen;
  readonly width: FrameWidth;
}) {
  const [theme, setTheme] = React.useState<FrameTheme>("dark");
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <ToggleGroup
        aria-label={`${specimen.title}: theme`}
        value={[theme]}
        onValueChange={(values) => {
          const next = values[0] as FrameTheme | undefined;
          if (next !== undefined) setTheme(next);
        }}
      >
        <ToggleGroupItem value="light">Light</ToggleGroupItem>
        <ToggleGroupItem value="dark">Dark</ToggleGroupItem>
      </ToggleGroup>
      <ThemeFrame key={`${entryId}/${specimen.id}/${theme}`} entryId={entryId} specimen={specimen} theme={theme} width={width} />
    </div>
  );
}

function EntryPage({
  entry,
  doc,
  view,
  width,
}: {
  readonly entry: CatalogEntry;
  readonly doc: GalleryDoc | undefined;
  readonly view: ThemeView;
  readonly width: FrameWidth;
}) {
  const themes: FrameTheme[] = view === "both" ? ["light", "dark"] : [view];
  return (
    <article className="flex flex-col gap-8" data-testid="design-system-entry" data-entry-id={entry.id}>
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="font-semibold text-2xl text-foreground">{entry.name}</h1>
          <Badge variant="outline">{entry.kind}</Badge>
          <EntryChips entry={entry} />
        </div>
        <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
          <Field label="Source">
            <code className="break-all font-mono text-xs">{entry.source ?? "—"}</code>
          </Field>
          {entry.use !== null ? <Field label="Use">{entry.use}</Field> : null}
          {entry.avoid !== null ? <Field label="Avoid">{entry.avoid}</Field> : null}
          {entry.exports.length > 0 ? (
            <Field label="Exports">
              <code className="break-words font-mono text-xs">{entry.exports.join(", ")}</code>
            </Field>
          ) : null}
          {entry.states.length > 0 ? <Field label="States">{entry.states.join(", ")}</Field> : null}
          {entry.keyboard !== null ? <Field label="Keyboard">{entry.keyboard}</Field> : null}
          {entry.related.length > 0 ? (
            <Field label="Related">
              <span className="flex flex-wrap gap-1.5">
                {entry.related.map((id) => (
                  <Link key={id} to="/design-system/$entryId" params={{ entryId: id }} className="text-primary hover:underline">
                    {id}
                  </Link>
                ))}
              </span>
            </Field>
          ) : null}
        </dl>
      </header>
      {doc === undefined ? (
        <p className="text-muted-foreground text-sm" data-testid="design-system-no-specimen">
          {entry.status === "available"
            ? "No specimen yet."
            : `This entry is ${entry.status}; nothing to render until it is available.`}
        </p>
      ) : "excluded" in doc ? (
        <div className="rounded-lg border border-dashed border-border p-4 text-sm" data-testid="design-system-excluded">
          <p className="font-medium">Cannot be rendered</p>
          <p className="mt-1 text-muted-foreground">{doc.excluded}</p>
        </div>
      ) : (
        doc.specimens.map((specimen) => (
          <section key={specimen.id} aria-label={specimen.title} className="flex flex-col gap-2">
            <h2 className="font-medium text-base text-foreground">{specimen.title}</h2>
            {specimen.note !== undefined ? <p className="max-w-[72ch] text-muted-foreground text-sm">{specimen.note}</p> : null}
            <div
              className={cn(
                "grid gap-4",
                themes.length === 2 &&
                  (width === "narrow"
                    ? "md:grid-cols-[repeat(2,max-content)]"
                    : specimen.minWidth === undefined && "2xl:grid-cols-2"),
              )}
            >
              {specimen.appRoute !== undefined && themes.length === 2 ? (
                <AppRouteThemes entryId={entry.id} specimen={specimen} width={width} />
              ) : (
                themes.map((theme) => (
                  <ThemeFrame
                    key={`${entry.id}/${specimen.id}/${theme}`}
                    entryId={entry.id}
                    specimen={specimen}
                    theme={theme}
                    width={width}
                  />
                ))
              )}
            </div>
          </section>
        ))
      )}
    </article>
  );
}

function Overview({ entries }: { readonly entries: readonly CatalogEntry[] }) {
  const available = entries.filter((entry) => entry.status === "available");
  const withSpecimens = available.filter((entry) => {
    const doc = docFor(entry.id);
    return doc !== undefined && "specimens" in doc;
  });
  const excluded = available.flatMap((entry) => {
    const doc = docFor(entry.id);
    return doc !== undefined && "excluded" in doc ? [{ entry, reason: doc.excluded }] : [];
  });
  const foundations = entries.filter((entry) => entry.kind === "foundation");
  const components = available.filter((entry) => entry.kind !== "foundation");
  const componentsWithSpecimens = withSpecimens.filter((entry) => entry.kind !== "foundation");
  const appRoutes = components.filter((entry) => {
    const doc = docFor(entry.id);
    return doc !== undefined && "specimens" in doc && doc.specimens.some((specimen) => specimen.appRoute !== undefined);
  });
  return (
    <article className="flex max-w-4xl flex-col gap-8" data-testid="design-system-overview">
      <header className="flex flex-col gap-2">
        <h1 className="font-semibold text-2xl">Detent design system</h1>
        <p className="text-muted-foreground text-sm">
          The source of truth for Detent's Cloud UI. Every specimen is the real component from{" "}
          <code className="font-mono text-xs">src/components</code> or the app, rendered with synthetic data in a Light
          and a Dark document. Nothing here is restyled. The rules are in{" "}
          <code className="font-mono text-xs">docs/design-system</code>.
        </p>
        <p className="text-sm">
          {componentsWithSpecimens.length} of {components.length} catalogued components have specimens
          {appRoutes.length > 0 ? `, ${appRoutes.length} of them as the real app route (these need the mock hub)` : ""}
          {excluded.length > 0 ? `; ${excluded.length} cannot be rendered` : ""}.
        </p>
      </header>
      <section aria-label="Foundations" className="flex flex-col gap-2">
        <h2 className="font-semibold text-lg">Foundations</h2>
        <ul className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {foundations.map((entry) => (
            <li key={entry.id}>
              <Link
                to="/design-system/$entryId"
                params={{ entryId: entry.id }}
                className="flex flex-col gap-0.5 rounded-lg border border-border p-3 hover:bg-accent"
              >
                <span className="font-medium text-sm">{entry.name}</span>
                <code className="truncate font-mono text-2xs text-muted-foreground">{entry.source}</code>
              </Link>
            </li>
          ))}
        </ul>
      </section>
      {excluded.length > 0 ? (
        <section aria-label="Cannot be rendered" className="flex flex-col gap-2">
          <h2 className="font-semibold text-lg">Cannot be rendered</h2>
          <ul className="flex flex-col gap-1 text-sm">
            {excluded.map(({ entry, reason }) => (
              <li key={entry.id}>
                <span className="font-medium">{entry.name}</span> — <span className="text-muted-foreground">{reason}</span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </article>
  );
}

/**
 * The chrome follows the view: Light shows light chrome, Dark and side by side
 * dark. The frames carry their own theme regardless.
 */
function useChromeTheme(view: ThemeView): void {
  React.useLayoutEffect(() => applyDocumentTheme(view === "light" ? "light" : "dark"), [view]);
}

export function Gallery({ entryId }: { readonly entryId: string | null }) {
  const entries = React.useMemo(() => galleryEntries(), []);
  const [view, setView] = React.useState<ThemeView>(() => readStored(VIEW_KEY, ["both", "light", "dark"], "both"));
  const [width, setWidth] = React.useState<FrameWidth>(() => readStored(WIDTH_KEY, ["full", "narrow"], "full"));
  const [navOpen, setNavOpen] = React.useState(false);
  useChromeTheme(view);

  const entry = entryId === null ? null : (entries.find((candidate) => candidate.id === entryId) ?? null);

  React.useEffect(() => {
    if (!navOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setNavOpen(false);
    };
    globalThis.addEventListener("keydown", onKey);
    return () => globalThis.removeEventListener("keydown", onKey);
  }, [navOpen]);

  return (
    <div className="flex h-full min-h-0 bg-background text-foreground" data-testid="design-system-gallery">
      {navOpen ? (
        <div
          aria-hidden
          className="fixed inset-0 z-40 bg-black/40 md:hidden"
          data-testid="design-system-nav-backdrop"
          onClick={() => setNavOpen(false)}
        />
      ) : null}
      <aside
        className={cn(
          "w-72 shrink-0 border-r border-border bg-sidebar max-md:fixed max-md:inset-y-0 max-md:left-0 max-md:z-50 max-md:max-w-[calc(100vw-3rem)]",
          navOpen ? "max-md:block" : "max-md:hidden",
        )}
      >
        <Nav
          entries={entries}
          activeId={entryId}
          onNavigate={() => setNavOpen(false)}
          onClose={navOpen ? () => setNavOpen(false) : undefined}
        />
      </aside>
      <main className="min-w-0 flex-1 overflow-y-auto overflow-x-hidden">
        <div className="sticky top-0 z-30 flex flex-wrap items-center gap-2 border-b border-border bg-background/90 px-4 py-2 backdrop-blur md:px-8">
          <Button
            className="md:hidden"
            variant="ghost"
            size="icon-sm"
            aria-label={navOpen ? "Close navigation" : "Open navigation"}
            onClick={() => setNavOpen((open) => !open)}
          >
            {navOpen ? <XIcon /> : <MenuIcon />}
          </Button>
          <Controls
            view={view}
            width={width}
            onView={(next) => {
              setView(next);
              writeStored(VIEW_KEY, next);
            }}
            onWidth={(next) => {
              setWidth(next);
              writeStored(WIDTH_KEY, next);
            }}
          />
        </div>
        <div className="px-4 py-6 md:px-8">
          {entryId === null ? (
            <Overview entries={entries} />
          ) : entry === null ? (
            <p className="text-muted-foreground text-sm">No catalog entry “{entryId}”.</p>
          ) : (
            <EntryPage entry={entry} doc={docFor(entry.id)} view={view} width={width} />
          )}
        </div>
      </main>
    </div>
  );
}

// --- Route components (lazy-loaded from `app/router.tsx`) --------------------

export function GalleryRoute() {
  const params = useParams({ strict: false }) as { entryId?: string };
  return <Gallery entryId={params.entryId ?? null} />;
}

export function FrameRoute() {
  const params = useParams({ strict: false }) as { entryId?: string; specimenId?: string };
  const search = useSearch({ strict: false }) as { theme?: unknown };
  return (
    <FramePage
      entryId={params.entryId ?? ""}
      specimenId={params.specimenId ?? ""}
      theme={search.theme === "light" ? "light" : "dark"}
    />
  );
}
