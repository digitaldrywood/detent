import { useCallback } from "react";

import { readLocalApi } from "../localApi.ts";
import type { ScopedThreadRef } from "../environment/scoped.ts";
import { toastManager } from "../components/ui/toast.tsx";

type ThreadActionMenuId = "rename" | "copy-link";

export function useThreadActionMenu({
  threadRef,
  onStartRename,
}: {
  threadRef: ScopedThreadRef | null;
  projectCwd?: string | null;
  onStartRename: () => void;
}): { openMenu: (at: { x: number; y: number }) => void; closeMenu: () => void } {
  const openMenu = useCallback(
    (at: { x: number; y: number }) => {
      if (threadRef === null) return;
      const api = readLocalApi();
      if (!api) return;
      void api.contextMenu
        .show<ThreadActionMenuId>(
          [
            { id: "rename", label: "Rename", icon: "pencil" },
            { id: "copy-link", label: "Copy link", icon: "copy" },
          ],
          at,
        )
        .then((action) => {
          if (action === "rename") {
            onStartRename();
            return;
          }
          if (action === "copy-link") {
            const href = globalThis.location?.href;
            if (href === undefined) return;
            void globalThis.navigator?.clipboard
              ?.writeText(href)
              .then(() => toastManager.add({ type: "success", title: "Link copied" }))
              .catch(() =>
                toastManager.add({ type: "error", title: "Could not copy the link" }),
              );
          }
        });
    },
    [onStartRename, threadRef],
  );

  const closeMenu = useCallback(() => {
    const api = readLocalApi();
    void api?.contextMenu.close();
  }, []);

  return { openMenu, closeMenu };
}
