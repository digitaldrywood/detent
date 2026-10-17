// A dynamic import whose chunk is missing after a deploy reloads the page
// once, with the same guard `lazyRouteComponent` uses for route chunks, so the
// shell picks up the new build instead of staying blank.

export interface ReloadEnvironment {
  readonly reload: () => void;
  readonly storage: Pick<Storage, "getItem" | "setItem"> | undefined;
}

function browserEnvironment(): ReloadEnvironment {
  return {
    reload: () => globalThis.location?.reload(),
    storage: typeof sessionStorage === "undefined" ? undefined : sessionStorage,
  };
}

/** The import failures browsers report for a module that no longer exists. */
export function isMissingChunk(error: unknown): error is Error {
  if (!(error instanceof Error)) return false;
  return (
    error.message.startsWith("Failed to fetch dynamically imported module") ||
    error.message.startsWith("error loading dynamically imported module") ||
    error.message.startsWith("Importing a module script failed")
  );
}

export function loadChunk<T>(importer: () => Promise<T>, environment: ReloadEnvironment = browserEnvironment()): Promise<T> {
  return importer().catch((error: unknown) => {
    if (isMissingChunk(error) && environment.storage !== undefined) {
      const key = `tanstack_router_reload:${error.message}`;
      if (environment.storage.getItem(key) === null) {
        environment.storage.setItem(key, "1");
        environment.reload();
        return new Promise<T>(() => {});
      }
    }
    throw error;
  });
}
