import { EllipsisIcon } from "lucide-react";
import React from "react";

import {
  Menu,
  MenuGroup,
  MenuGroupLabel,
  MenuItem,
  MenuPopup,
  MenuTrigger,
} from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import type { WorkItemView } from "../lib/model.ts";

// The popup renders in a portal above the board: a lane body scrolls and
// clips, so a menu inside it lost its last lanes and scrolled the card above
// out of view.
export function LaneMenu({
  item,
  lanes,
  onMove,
  className,
}: {
  item: WorkItemView;
  lanes: readonly string[];
  onMove: (item: WorkItemView, toState: string) => void;
  className?: string;
}): React.ReactElement | null {
  const labelId = React.useId();
  if (lanes.length === 0) return null;

  return (
    <Menu>
      <MenuTrigger
        aria-label={`Move ${item.title}`}
        data-testid="lane-menu-trigger"
        className={cn(
          "inline-flex size-5 shrink-0 cursor-pointer items-center justify-center rounded-sm text-muted-foreground outline-none ring-ring hover:bg-accent hover:text-foreground focus-visible:ring-2",
          className,
        )}
      >
        <EllipsisIcon className="size-3.5" />
      </MenuTrigger>
      {/* Base UI names the menu after its trigger ("Move <title>"); the menu
          is the list of destinations, so its name is its own heading. */}
      <MenuPopup align="end" sideOffset={4} aria-labelledby={labelId} className="w-44">
        <MenuGroup aria-labelledby={labelId}>
          <MenuGroupLabel id={labelId}>Move to</MenuGroupLabel>
          {lanes.map((lane) => (
            <MenuItem
              key={lane}
              data-testid={`lane-menu-item-${lane}`}
              onClick={() => onMove(item, lane)}
            >
              {lane}
            </MenuItem>
          ))}
        </MenuGroup>
      </MenuPopup>
    </Menu>
  );
}
