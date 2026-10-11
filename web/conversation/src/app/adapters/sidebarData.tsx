import React from "react";

import type {
  BootstrapProject,
  Conversation,
  PreferenceChoices,
} from "../../contracts/index.ts";

import type { SidebarFinding } from "./sidebarFindings.ts";

export interface SidebarNavigation {
  /** The path of the current route, e.g. `/work/p/proj_1`. */
  readonly activePath: string;
  readonly onNavigate: (to: string) => void;
  /** The open change-request count, for the Pull requests row. */
  readonly changeCount?: number | null;
}

export interface SidebarShellData {
  readonly organizationName: string;
  readonly projects: readonly BootstrapProject[];
  readonly activeProjectId: string | null;
  readonly onProjectChange: (projectId: string) => void;
  readonly conversations: readonly Conversation[];
  /**
   * Hub search hits for the sidebar's current query. The copied sidebar
   * searches its own list by title (`Sidebar.logic.ts`), so these are merged
   * into the list it searches rather than rendered as a second section: a
   * conversation past the first page is then found by exactly the same rule.
   */
  readonly serverResults: readonly Conversation[];
  readonly activeConversationId: string | null;
  readonly onSelect: (conversation: Conversation) => void;
  readonly onNewChat: (shiftKey: boolean) => void;
  readonly onRename: (conversationId: string, title: string) => Promise<void>;
  readonly attention: ReadonlySet<string>;
  readonly findings?: readonly SidebarFinding[];
  /** The picker choices the hover card names the model from (§14). */
  readonly preferenceChoices?: PreferenceChoices | undefined;
  /** Navigation for the Work destination and the Browse group (A.1, A.6). */
  readonly navigation?: SidebarNavigation | undefined;
}

const SidebarDataContext = React.createContext<SidebarShellData | null>(null);

export const SidebarDataProvider = SidebarDataContext.Provider;

export function useSidebarData(): SidebarShellData | null {
  return React.use(SidebarDataContext);
}

let latest: SidebarShellData | null = null;

export function publishSidebarData(value: SidebarShellData | null): void {
  latest = value;
}

export function readSidebarData(): SidebarShellData | null {
  return latest;
}

const SIDEBAR_SEARCH_SELECTOR = 'aside.dc-side input[type="search"]';

/**
 * The sidebar's live search query, read from the copied component's own input.
 *
 * The copied `Sidebar.tsx` keeps `threadSearchQuery` in its own state and
 * offers no way to observe it, and the shell needs it for one thing: asking
 * the hub for conversations past the first page so the sidebar's own
 * title search can find them. Listening to the input it already renders is
 * how the shell learns the query without the component being edited
 * (decisions.md §16).
 *
 * The listener must not re-render during the same event dispatch. The search
 * box is a controlled input: a shell re-render triggered from a capture-phase
 * listener writes the *old* `threadSearchQuery` back into the DOM node before
 * React's own `onChange` has run, and the keystroke is swallowed. Reading the
 * value in a microtask puts the state update after the whole dispatch, which
 * is also why this listens on the bubble phase.
 */
export function useSidebarSearchQuery(): string {
  const [query, setQuery] = React.useState("");
  React.useEffect(() => {
    let disposed = false;
    const read = (event: Event): void => {
      const target = event.target;
      if (!(target instanceof HTMLInputElement)) return;
      if (!target.matches(SIDEBAR_SEARCH_SELECTOR) || target.getAttribute("role") === "searchbox") return;
      queueMicrotask(() => {
        if (!disposed) setQuery(target.value);
      });
    };
    document.addEventListener("input", read);
    return () => {
      disposed = true;
      document.removeEventListener("input", read);
    };
  }, []);
  return query;
}

/** Focuses that same box. The shell's `/` shortcut, which has no ref to it. */
export function focusSidebarSearch(): boolean {
  const input = document.querySelector<HTMLInputElement>(SIDEBAR_SEARCH_SELECTOR);
  if (input === null) return false;
  input.focus();
  input.select();
  return true;
}

interface WorkSidebarSearch {
  readonly query: string;
  readonly onChange: (query: string) => void;
}

const SidebarSearchContext = React.createContext<{
  readonly work: boolean;
  readonly search: WorkSidebarSearch | null;
  readonly setSearch: React.Dispatch<React.SetStateAction<WorkSidebarSearch | null>>;
} | null>(null);

export function SidebarSearchProvider({ work, children }: {
  readonly work: boolean;
  readonly children: React.ReactNode;
}): React.ReactElement {
  const [search, setSearch] = React.useState<WorkSidebarSearch | null>(null);
  const value = React.useMemo(() => ({ work, search, setSearch }), [work, search]);
  return <SidebarSearchContext.Provider value={value}>{children}</SidebarSearchContext.Provider>;
}

export function useWorkSidebarSearch(query: string, onChange: (query: string) => void): void {
  const setSearch = React.use(SidebarSearchContext)?.setSearch;
  React.useEffect(() => {
    setSearch?.({ query, onChange });
  }, [setSearch, query, onChange]);
  React.useEffect(() => () => setSearch?.(null), [setSearch]);
}

export function useSidebarSearchInput(enabled: boolean): React.ComponentProps<"input"> {
  const context = React.use(SidebarSearchContext);
  if (!enabled) return {};
  if (!context?.work) return { placeholder: "Search chats" };
  return {
    value: context.search?.query ?? "",
    placeholder: "Search issues",
    "aria-label": "Search issues",
    role: "searchbox",
    "aria-autocomplete": undefined,
    "aria-expanded": undefined,
    "aria-controls": undefined,
    "aria-activedescendant": undefined,
    onChange: (event) => context.search?.onChange(event.currentTarget.value),
    onKeyDown: (event) => {
      if (event.key === "Escape") context.search?.onChange("");
    },
  };
}

export function useSidebarThreadSearch(): readonly [string, React.Dispatch<React.SetStateAction<string>>] {
  const [query, setQuery] = React.useState("");
  const work = React.use(SidebarSearchContext)?.work === true;
  return [work ? "" : query, work ? () => {} : setQuery];
}
