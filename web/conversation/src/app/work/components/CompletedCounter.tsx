import { ChevronDownIcon } from "lucide-react";
import React from "react";

import { InlineButton } from "../../../components/ui/button.tsx";
import { Menu, MenuGroup, MenuGroupLabel, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuTrigger } from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import { COMPLETED_WINDOWS, type CompletedWindow } from "../lib/viewState.ts";

export function CompletedCounter({
  count,
  completedWindow = "48h",
  onCompletedWindowChange,
  loading,
}: {
  count: number | null;
  completedWindow?: CompletedWindow;
  onCompletedWindowChange: (window: CompletedWindow) => void;
  loading: boolean;
}): React.ReactElement {
  return (
    <div
      aria-busy={loading}
      className={cn("shrink-0 text-muted-foreground text-xs", loading && "opacity-50")}
    >
      <Menu>
        <MenuTrigger
          render={<InlineButton tone="muted" className="min-h-6 whitespace-nowrap" />}
          data-testid="stat-completed"
          title={`${completedWindow === "all" ? "All items in terminal states" : `Items that entered terminal states in the last ${completedWindow}`} in the current filter scope, including cancelled and custom terminal states. Choose the completed window.`}
        >
          <span>
            <b className="font-semibold text-foreground tabular-nums">{count ?? "—"}</b>{" "}
            completed · {completedWindow === "all" ? "All time" : completedWindow}
          </span>
          <ChevronDownIcon aria-hidden className="size-3" />
        </MenuTrigger>
        <MenuPopup align="start" className="w-44">
          <MenuGroup>
            <MenuGroupLabel>Completed window</MenuGroupLabel>
            <MenuRadioGroup value={completedWindow} onValueChange={(value) => onCompletedWindowChange(value as CompletedWindow)}>
              {COMPLETED_WINDOWS.map((window) => (
                <MenuRadioItem key={window} value={window} closeOnClick>
                  {window === "48h" ? "48 hours" : window === "7d" ? "7 days" : window === "14d" ? "14 days" : "All time"}
                </MenuRadioItem>
              ))}
            </MenuRadioGroup>
          </MenuGroup>
        </MenuPopup>
      </Menu>
    </div>
  );
}
