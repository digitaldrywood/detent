import { useCallback } from "react";

import { readLocalApi } from "../localApi.ts";
import type { ScopedThreadRef } from "../environment/scoped.ts";
import { useConversationActionsValue, type ProjectActionId } from "../app/adapters/conversationActions.tsx";

type ThreadActionMenuId = "rename" | ProjectActionId;

export function useThreadActionMenu({
  threadRef,
  onStartRename,
}: {
  threadRef: ScopedThreadRef | null;
  projectCwd?: string | null;
  onStartRename: () => void;
}): { openMenu: (at: { x: number; y: number }) => void; closeMenu: () => void } {
  const { actions, run } = useConversationActionsValue();
  const openMenu = useCallback(
    (at: { x: number; y: number }) => {
      if (threadRef === null) return;
      const api = readLocalApi();
      if (!api) return;
      void api.contextMenu
        .show<ThreadActionMenuId>(
          [
            { id: "rename", label: "Rename", icon: "pencil" },
            ...actions.filter((action) => action.id.startsWith("copy-")).map((action) => ({
              id: action.id, label: action.name, icon: "copy" as const,
            })),
          ],
          at,
        )
        .then((action) => {
          if (action === "rename") {
            onStartRename();
            return;
          }
          const selected = actions.find((candidate) => candidate.id === action);
          if (selected !== undefined) run(selected);
        });
    },
    [actions, onStartRename, run, threadRef],
  );

  const closeMenu = useCallback(() => {
    const api = readLocalApi();
    void api?.contextMenu.close();
  }, []);

  return { openMenu, closeMenu };
}
