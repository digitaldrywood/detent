import React from "react";
import type { SavedView, SavedViewInput, SavedViewSetup } from "./savedViewSchema.ts";
export type { SavedView, SavedViewInput, SavedViewSetup } from "./savedViewSchema.ts";
import { serializeViewState } from "./viewState.ts";
import { useWorkHttp } from "./useWork.ts";

interface SavedViews {
  readonly currentSetup: SavedViewSetup | null;
  readonly setCurrentSetup: (setup: SavedViewSetup | null) => void;
  readonly views: readonly SavedView[];
  readonly loading: boolean;
  readonly error: string | null;
  readonly reload: () => void;
  readonly save: (input: SavedViewInput, id?: string) => Promise<SavedView>;
  readonly favorite: (view: SavedView, favorite: boolean) => Promise<void>;
  readonly remove: (view: SavedView) => Promise<void>;
}
const Context = React.createContext<SavedViews | null>(null);
export function useSavedViews(): SavedViews | null {
  return React.useContext(Context);
}
export function savedViewPath(view: SavedView): string {
  return `/work/views/${encodeURIComponent(view.id)}`;
}

export function SavedViewsProvider({
  children,
}: {
  children: React.ReactNode;
}): React.ReactElement {
  const http = useWorkHttp();
  const [currentSetup, updateSetup] = React.useState<SavedViewSetup | null>(null);
  const setCurrentSetup = React.useCallback((next: SavedViewSetup | null) => updateSetup((current) =>
    current !== null && next !== null && current.project_id === next.project_id &&
    JSON.stringify(current.project_ids) === JSON.stringify(next.project_ids) &&
    serializeViewState(current.state) === serializeViewState(next.state) ? current : next), []);
  const [views, setViews] = React.useState<readonly SavedView[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [generation, reload] = React.useReducer(
    (value: number) => value + 1,
    0,
  );
  React.useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setViews([]);
    void http
      .listSavedViews(controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        setViews(result.views);
        setError(null);
        setLoading(false);
      })
      .catch(() => {
        if (controller.signal.aborted) return;
        setError("Saved views could not be loaded.");
        setLoading(false);
      });
    return () => controller.abort();
  }, [http, generation]);
  const value: SavedViews = {
    currentSetup,
    setCurrentSetup,
    views,
    loading,
    error,
    reload,
    save: async (input, id) => {
      const saved = await http.saveView(input, id);
      setViews((current) => [
        ...current.filter((view) => view.id !== saved.id),
        saved,
      ]);
      return saved;
    },
    favorite: async (view, favorite) => {
      const saved = await http.favoriteView(view.id, favorite);
      setViews((current) =>
        current.map((item) => (item.id === saved.id ? saved : item)),
      );
    },
    remove: async (view) => {
      await http.deleteView(view.id);
      setViews((current) => current.filter((item) => item.id !== view.id));
    },
  };
  return <Context.Provider value={value}>{children}</Context.Provider>;
}
