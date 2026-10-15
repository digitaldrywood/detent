// Theme frames.
//
// Why iframes: the app keys dark mode on the document root. `global.css`
// declares every semantic token on `:root` and its dark values under
// `@variant dark`, i.e. `:root:is(.dark, .dark *)`, which only the `<html>`
// element can match. A `.dark` class on a wrapper div would flip the `dark:`
// utilities but not the tokens, so a scoped frame cannot be dark without
// copying the token block. And Base UI portals (menus, popovers, selects,
// dialogs, tooltips, toasts) mount on `document.body`, outside any wrapper.
//
// So each specimen renders in its own same-origin iframe of
// `/design-system/frame/$entryId/$specimenId?theme=…`, whose `<html>` carries
// the theme. The real stylesheet applies unchanged, portals land in that
// document's body and inherit its theme, `useTheme()` reads its `data-theme`
// (code highlighting follows), and a 390px frame is a real 390px viewport, so
// `sm:`/`md:` breakpoints and `useIsMobile` respond as on a phone.
import React from "react";

import { AnchoredToastProvider, ToastProvider } from "~/components/ui/toast";
import { hubPath } from "../../runtime/basePath.ts";

import { docFor } from "./registry";
import type { Specimen } from "./specimen";
import { FrameThemeContext } from "./tokens";

export type FrameTheme = "light" | "dark";

const MESSAGE = "detent-design-system-frame";

/** True in the static build (`npm run build:gallery`), which routes on the URL hash. */
export function isStaticGallery(): boolean {
  return import.meta.env.MODE === "gallery";
}

export function frameUrl(
  entryId: string,
  specimenId: string,
  theme: FrameTheme,
): string {
  const path = `/design-system/frame/${encodeURIComponent(entryId)}/${encodeURIComponent(specimenId)}?theme=${theme}`;
  // An iframe `src` of `#…` resolves against the gallery document itself.
  return isStaticGallery() ? `#${path}` : hubPath(path);
}

/** Puts the theme on the frame document's root, where the tokens are keyed. */
export function applyDocumentTheme(
  theme: FrameTheme,
  root: HTMLElement = document.documentElement,
): () => void {
  const hadDark = root.classList.contains("dark");
  const previous = root.dataset.theme;
  root.classList.toggle("dark", theme === "dark");
  root.dataset.theme = theme;
  return () => {
    root.classList.toggle("dark", hadDark);
    if (previous === undefined) delete root.dataset.theme;
    else root.dataset.theme = previous;
  };
}

class SpecimenBoundary extends React.Component<
  { readonly children: React.ReactNode },
  { readonly error: Error | null }
> {
  override state: { readonly error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  override render() {
    if (this.state.error === null) return this.props.children;
    return (
      <div
        role="alert"
        className="rounded-lg border border-destructive/40 p-3 text-destructive-foreground text-sm"
      >
        This specimen failed to render: {this.state.error.message}
      </div>
    );
  }
}

/** The providers a specimen may lean on, as the app shell supplies them. */
export function SpecimenProviders({
  children,
}: {
  readonly children: React.ReactNode;
}) {
  return (
    <ToastProvider>
      <AnchoredToastProvider>{children}</AnchoredToastProvider>
    </ToastProvider>
  );
}

/** One specimen, rendered for its frame (or for a test, without one). */
export function SpecimenStage({ specimen, theme }: { readonly specimen: Specimen; readonly theme?: FrameTheme }) {
  return (
    <FrameThemeContext.Provider value={theme ?? null}>
      <SpecimenProviders>
        <SpecimenBoundary>{specimen.render()}</SpecimenBoundary>
      </SpecimenProviders>
    </FrameThemeContext.Provider>
  );
}

function findSpecimen(entryId: string, specimenId: string): Specimen | null {
  const doc = docFor(entryId);
  if (doc === undefined || !("specimens" in doc)) return null;
  return doc.specimens.find((specimen) => specimen.id === specimenId) ?? null;
}

/** The frame document's page: theme on `<html>`, the specimen, its height reported up. */
export function FramePage({
  entryId,
  specimenId,
  theme,
}: {
  readonly entryId: string;
  readonly specimenId: string;
  readonly theme: FrameTheme;
}) {
  const specimen = findSpecimen(entryId, specimenId);
  const measured = React.useRef<HTMLDivElement>(null);

  React.useLayoutEffect(() => applyDocumentTheme(theme), [theme]);

  React.useEffect(() => {
    const node = measured.current;
    if (node === null || globalThis.parent === globalThis.window) return;
    const report = () => {
      globalThis.parent.postMessage(
        {
          type: MESSAGE,
          height: Math.ceil(node.getBoundingClientRect().height),
        },
        globalThis.location.origin,
      );
    };
    const observer = new ResizeObserver(report);
    observer.observe(node);
    report();
    return () => observer.disconnect();
  }, []);

  if (specimen === null) {
    return (
      <p className="p-4 text-muted-foreground text-sm">
        Unknown specimen {entryId}/{specimenId}.
      </p>
    );
  }
  const fixed = specimen.height !== undefined;
  return (
    <div
      className={fixed ? "h-full overflow-hidden" : "h-full overflow-auto"}
      data-design-system-frame={theme}
    >
      <div ref={measured} className={fixed ? "h-full" : "flow-root p-5"}>
        <SpecimenStage specimen={specimen} theme={theme} />
      </div>
    </div>
  );
}

let mockHub: Promise<boolean> | null = null;

/** Whether the dev server reaches a hub (the mock hub under `npm run dev:mock`). */
export function mockHubAvailable(): Promise<boolean> {
  if (isStaticGallery()) return Promise.resolve(false);
  mockHub ??= fetch(hubPath("/app/bootstrap"), { credentials: "same-origin" })
    .then((response) => response.ok)
    .catch(() => false);
  return mockHub;
}

function useMockHub(enabled: boolean): boolean | null {
  const [available, setAvailable] = React.useState<boolean | null>(null);
  React.useEffect(() => {
    if (!enabled) return;
    let live = true;
    void mockHubAvailable().then((value) => {
      if (live) setAvailable(value);
    });
    return () => {
      live = false;
    };
  }, [enabled]);
  return available;
}

/** Adds the dev server's `theme` flag, which the app applies before its first render. */
export function withTheme(path: string, theme: FrameTheme): string {
  return `${path}${path.includes("?") ? "&" : "?"}theme=${theme}`;
}

/** An app-route specimen: the real route in a frame, themed from outside. */
function AppRouteFrame({
  specimen,
  theme,
  width,
}: {
  readonly specimen: Specimen & {
    readonly appRoute: NonNullable<Specimen["appRoute"]>;
  };
  readonly theme: FrameTheme;
  readonly width: "full" | "narrow";
}) {
  const available = useMockHub(true);
  const ref = React.useRef<HTMLIFrameElement>(null);
  const route = specimen.appRoute;
  const height = specimen.height ?? 640;
  const onLoad = React.useCallback(() => {
    const win = ref.current?.contentWindow;
    const root = ref.current?.contentDocument?.documentElement;
    if (win === null || win === undefined || root === undefined) return;
    applyDocumentTheme(theme, root);
    if (route.setup !== undefined)
      win.setTimeout(() => route.setup?.(win), route.settle ?? 1200);
  }, [route, theme]);
  if (available !== true) {
    return (
      <div
        className="flex flex-col gap-1 rounded-xl border border-dashed border-border p-4 text-sm"
        style={{ width: width === "narrow" ? 390 : "100%" }}
        data-testid="design-system-needs-hub"
      >
        <p className="font-medium">
          {available === null
            ? "Looking for the mock hub…"
            : "Needs the mock hub"}
        </p>
        <p className="text-muted-foreground">
          This entry is the real{" "}
          <code className="font-mono text-xs">{route.path}</code> route, seeded
          by the mock hub. Run{" "}
          <code className="font-mono text-xs">npm run dev:mock</code> and
          reload.
        </p>
      </div>
    );
  }
  return (
    <iframe
      ref={ref}
      title={`${specimen.title} (${theme})`}
      src={hubPath(withTheme(route.path, theme))}
      onLoad={onLoad}
      className="block max-w-full rounded-xl border border-border bg-transparent"
      style={{
        height,
        width: width === "narrow" ? 390 : "100%",
        minWidth: width === "full" ? specimen.minWidth : undefined,
      }}
    />
  );
}

/** The parent page's side: an iframe sized to what its document reports. */
export function ThemeFrame({
  entryId,
  specimen,
  theme,
  width,
}: {
  readonly entryId: string;
  readonly specimen: Specimen;
  readonly theme: FrameTheme;
  readonly width: "full" | "narrow";
}) {
  const ref = React.useRef<HTMLIFrameElement>(null);
  const [reported, setReported] = React.useState<number | null>(null);

  React.useEffect(() => {
    if (specimen.height !== undefined || specimen.appRoute !== undefined)
      return;
    const onMessage = (event: MessageEvent) => {
      if (event.source === null || event.source !== ref.current?.contentWindow)
        return;
      const data = event.data as { type?: unknown; height?: unknown } | null;
      if (data?.type !== MESSAGE || typeof data.height !== "number") return;
      setReported(data.height);
    };
    globalThis.addEventListener("message", onMessage);
    return () => globalThis.removeEventListener("message", onMessage);
  }, [specimen.height]);

  const height =
    specimen.height ?? Math.max(specimen.minHeight ?? 0, reported ?? 96);
  const appRoute = specimen.appRoute;
  return (
    <div className="flex min-w-0 flex-col gap-1.5 overflow-x-auto" data-theme-frame={theme}>
      <span className="font-mono text-2xs text-muted-foreground">
        {theme === "light" ? "Light" : "Dark"}
        {width === "narrow" ? " · 390px" : ""}
        {appRoute !== undefined ? ` · app route ${appRoute.path}` : ""}
      </span>
      {appRoute !== undefined ? (
        <AppRouteFrame
          specimen={{ ...specimen, appRoute }}
          theme={theme}
          width={width}
        />
      ) : (
        <iframe
          ref={ref}
          title={`${specimen.title} (${theme})`}
          src={frameUrl(entryId, specimen.id, theme)}
          className="block rounded-xl border border-border bg-transparent"
          style={{
            // The border sits inside the box; add it so the document is not 2px short.
            height: height + 2,
            width: width === "narrow" ? 390 : "100%",
            minWidth: width === "full" ? specimen.minWidth : undefined,
          }}
        />
      )}
    </div>
  );
}
