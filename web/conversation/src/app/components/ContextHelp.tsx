import { CircleHelpIcon } from "lucide-react";
import React from "react";

import { Popover, PopoverPopup, PopoverTrigger } from "../../components/ui/popover.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";

/** Read by hover or keyboard focus; click/tap pins the same help in a popover. */
export function ContextHelp({ label, children }: {
  readonly label: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  const [pinned, setPinned] = React.useState(false);
  const [preview, setPreview] = React.useState(false);
  React.useEffect(() => {
    if (!preview || pinned) return;
    // Hover help can be open while focus is on an input in the enrollment
    // dialog. Dismiss that help before Escape reaches the surrounding dialog.
    const dismiss = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.stopPropagation();
      setPreview(false);
    };
    document.addEventListener("keydown", dismiss, true);
    return () => document.removeEventListener("keydown", dismiss, true);
  }, [preview, pinned]);
  const popupClass = "w-72 max-w-[calc(100vw-2rem)] text-pretty text-left text-xs leading-relaxed";
  return (
    <Tooltip disabled={pinned} open={preview && !pinned} onOpenChange={setPreview}>
      <Popover open={pinned} onOpenChange={setPinned}>
        <TooltipTrigger
          render={
            <PopoverTrigger
              type="button"
              aria-label={`Help for ${label}`}
              className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
            />
          }
        >
          <CircleHelpIcon aria-hidden="true" className="size-4" />
        </TooltipTrigger>
        <TooltipPopup aria-label={`Help for ${label}`} className={popupClass}>
          {children}
        </TooltipPopup>
        <PopoverPopup
          aria-label={`Help for ${label}`}
          initialFocus={false}
          finalFocus={false}
          tooltipStyle
          className={popupClass}
        >
          {children}
        </PopoverPopup>
      </Popover>
    </Tooltip>
  );
}
