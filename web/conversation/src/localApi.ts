import type { ContextMenuItem } from "./contracts/ui.ts";
import { dismissContextMenu, showContextMenuFallback } from "./contextMenuFallback.ts";

export interface LocalApi {
  readonly contextMenu: {
    show<T extends string>(
      items: readonly ContextMenuItem<T>[],
      position?: { x: number; y: number },
    ): Promise<T | null>;
    close(): Promise<void>;
  };

  readonly dialogs: {
    confirm(
      message: string,
      options?: { readonly variant?: "destructive" },
    ): Promise<boolean>;
  };

  readonly shell: {
    openExternal(url: string): Promise<void>;
  };
}

let cachedApi: LocalApi | undefined;

export function createLocalApi(): LocalApi {
  return {
    contextMenu: {
      show: async <T extends string>(
        items: readonly ContextMenuItem<T>[],
        position?: { x: number; y: number },
      ): Promise<T | null> => showContextMenuFallback(items, position),

      close: async () => {
        dismissContextMenu();
      },
    },
    dialogs: {
      confirm: async (message: string): Promise<boolean> =>
        typeof window.confirm === "function" ? window.confirm(message) : false,
    },
    shell: {
      openExternal: async (url: string): Promise<void> => {
        const opened = window.open(url, "_blank", "noopener,noreferrer");
        if (opened === null) {
          throw new Error("The link was blocked. Allow pop-ups for this site and try again.");
        }
      },
    },
  };
}

export function readLocalApi(): LocalApi | undefined {
  if (typeof window === "undefined") return undefined;
  if (cachedApi) return cachedApi;
  cachedApi = createLocalApi();
  return cachedApi;
}
