import { InfoIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
import {
  Popover,
  PopoverDescription,
  PopoverPopup,
  PopoverTitle,
  PopoverTrigger,
} from "../../components/ui/popover.tsx";

/** In-place help that can stay open for keyboard and touch readers. */
export function SettingsHelp({ label, children }: { readonly label: string; readonly children: string }) {
  return (
    <Popover>
      <PopoverTrigger
        openOnHover
        delay={200}
        render={
          <Button type="button" size="icon-micro" variant="ghost-muted" aria-label={`About ${label}`}>
            <InfoIcon aria-hidden="true" className="size-3.5" />
          </Button>
        }
      />
      <PopoverPopup side="top" align="start" className="w-80 max-w-[calc(100vw-2rem)]">
        <PopoverTitle className="mb-2 text-sm">{label}</PopoverTitle>
        <PopoverDescription className="text-xs leading-relaxed">{children}</PopoverDescription>
      </PopoverPopup>
    </Popover>
  );
}
