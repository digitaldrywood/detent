// Compositions that load their own data through the hub client, shown as the
// real app route. The dev server serves the route, the mock hub
// (`npm run dev:mock`) seeds it, and the frame puts the gallery's theme on the
// route's document. Their prop-driven parts have ordinary specimens.
import type { GalleryDoc, Specimen } from "../specimen";

const workItem = (number: number) => `wi_${number.toString(16).padStart(32, "0")}`;

/** An issue the mock hub seeds for `proj_alpha` (work item 3317), with history and a change. */
export const MOCK_ISSUE = workItem(3317);

/** The issue whose workspace the mock hub seeds as ready (work item 3301). */
export const MOCK_WORKSPACE_ISSUE = workItem(3301);

/** Opens the right panel through its own toggle, then the Files surface, as a reader would. */
function openRightPanel(win: Window): void {
  const doc = win.document;
  if (doc.querySelector('[aria-label="Maximize panel"]') === null) {
    doc.querySelector<HTMLButtonElement>('button[aria-label="Toggle right panel"]')?.click();
  }
  win.setTimeout(() => {
    const files = [...doc.querySelectorAll<HTMLButtonElement>('[aria-label="Open a surface"] button')].find((button) =>
      button.textContent?.includes("Files"),
    );
    files?.click();
  }, 800);
}

function routeSpecimen(id: string, title: string, note: string, appRoute: NonNullable<Specimen["appRoute"]>, height = 720): Specimen {
  return {
    id,
    title,
    note,
    height,
    // A desktop viewport, so the shell lays out with its sidebar inline.
    minWidth: 1100,
    appRoute,
    render: () => (
      <p className="text-muted-foreground text-sm">
        The real <code className="font-mono text-xs">{appRoute.path}</code> route, rendered by the dev server against
        the mock hub.
      </p>
    ),
  };
}

export const APP_ROUTES: Readonly<Record<string, GalleryDoc>> = {
  "command-palette": {
    meta: { name: "Command palette", kind: "composition", group: "Workspace", source: "src/app/components/CommandPalette.tsx" },
    specimens: [
      routeSpecimen(
        "open",
        "Open over the work board",
        "Open over the work board (⌘K, Ctrl+K elsewhere): actions, projects and issue search from the mock hub.",
        { path: "/work?palette=open" },
      ),
    ],
  },
  "right-panel-workspace": {
    meta: { name: "Right panel workspace", kind: "composition", group: "Panels", source: "src/app/components/RightPanel.tsx" },
    specimens: [
      routeSpecimen(
        "issue",
        "Open beside an issue",
        "The panel opened beside an issue (⌥⌘B), showing Files from the workspace session the mock hub seeds. At 980px and below it opens as a sheet.",
        { path: `/work/i/${MOCK_WORKSPACE_ISSUE}`, settle: 2500, setup: openRightPanel },
      ),
    ],
  },
  "settings-route": {
    meta: { name: "Settings route", kind: "composition", group: "Settings", source: "src/app/settings/Settings.tsx" },
    specimens: [
      routeSpecimen("general", "General settings", "The settings screen with its section navigation and rows, read from the mock account API.", {
        path: "/settings/general",
      }),
    ],
  },
  "work-board": {
    meta: { name: "Work board", kind: "composition", group: "Work", source: "src/app/work/WorkBoard.tsx" },
    specimens: [
      routeSpecimen("board", "The work board", "Every lane, subscribed to the mock hub's work items.", {
        path: "/work",
      }),
    ],
  },
  "issue-page": {
    meta: { name: "Issue page", kind: "composition", group: "Work", source: "src/app/work/IssuePage.tsx" },
    specimens: [
      routeSpecimen("issue", "An issue with history", "Description, activity, composer and properties of the first seeded issue.", {
        path: `/work/i/${MOCK_ISSUE}`,
      }),
    ],
  },
};
