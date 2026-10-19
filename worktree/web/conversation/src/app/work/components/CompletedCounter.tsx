import { Link } from "@tanstack/react-router";
import { ChevronDownIcon } from "lucide-react";
import React from "react";

import { InlineButton } from "../../../components/ui/button.tsx";
import { Menu, MenuGroup, MenuGroupLabel, MenuItem, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from "../../../components/ui/menu.tsx";
import type { ReportsRange } from "../../../contracts/reports.ts";
import { cn } from "../../../lib/utils.ts";
import type { ReportsSearch } from "../../reports/model.ts";
import type { ShippedCount } from "../lib/model.ts";
import { COMPLETED_WINDOW_LABELS, COMPLETED_WINDOWS, DEFAULT_COMPLETED_WINDOW, type CompletedWindow } from "../lib/viewState.ts";

type ReportsLinkSearch = Pick<ReportsSearch, "range">;

const REPORTS_RANGE: Readonly<Record<CompletedWindow, ReportsRange>> = { "1h": "24h", "24h": "24h", "7d": "7d" };

export function CompletedCounter({
  count,
  cancelled = null,
  windows = null,
  completedWindow = DEFAULT_COMPLETED_WINDOW,
  onCompletedWindowChange,
  loading,
}: {
  count: number | null;
  cancelled?: number | null;
  windows?: Readonly<Record<string, ShippedCount>> | null;
  completedWindow?: CompletedWindow;
  onCompletedWindowChange: (window: CompletedWindow) => void;
  loading: boolean;
}): React.ReactElement {
  const label = COMPLETED_WINDOW_LABELS[completedWindow];
  const reportsSearch: ReportsLinkSearch = { range: REPORTS_RANGE[completedWindow] };
  return (
    <div
      aria-busy={loading}
      className={cn("shrink-0 text-muted-foreground text-xs", loading && "opacity-50")}
    >
      <Menu>
        <MenuTrigger
          render={<InlineButton tone="muted" className="min-h-6 whitespace-nowrap" />}
          data-testid="stat-completed"
          title={`Issues closed as Done in the last ${label} in the current filter scope. Choose the shipped window.`}
        >
          <span>
            <b className="font-semibold text-foreground tabular-nums">{count ?? "—"}</b>{" "}
            shipped · {completedWindow}
          </span>
          <ChevronDownIcon aria-hidden className="size-3" />
        </MenuTrigger>
        <MenuPopup align="start" className="w-64">
          <MenuGroup>
            <MenuGroupLabel>Shipped in the last</MenuGroupLabel>
            <MenuRadioGroup value={completedWindow} onValueChange={(value) => onCompletedWindowChange(value as CompletedWindow)}>
              {COMPLETED_WINDOWS.map((window) => (
                <MenuRadioItem key={window} value={window} density="touch" closeOnClick>
                  <span className="flex w-full items-center justify-between gap-4">
                    <span>{COMPLETED_WINDOW_LABELS[window]}</span>
                    <span className="text-muted-foreground text-xs tabular-nums" data-testid={`shipped-count-${window}`}>
                      {windows?.[window]?.shipped ?? "—"}
                    </span>
                  </span>
                </MenuRadioItem>
              ))}
            </MenuRadioGroup>
          </MenuGroup>
          <MenuSeparator />
          <MenuGroup>
            {cancelled === null ? null : (
              <MenuGroupLabel data-testid="shipped-cancelled">Plus {cancelled} cancelled in {label}</MenuGroupLabel>
            )}
            <MenuItem
              density="touch"
              className="text-primary data-highlighted:text-primary"
              render={<Link to="/reports" search={reportsSearch} />}
            >
              Open in Reports <span aria-hidden>→</span>
            </MenuItem>
          </MenuGroup>
        </MenuPopup>
      </Menu>
    </div>
  );
}
