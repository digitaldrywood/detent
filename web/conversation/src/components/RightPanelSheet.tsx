import { type ReactNode, useRef } from "react";

import { RIGHT_PANEL_SHEET_CLASS_NAME } from "../rightPanelLayout";
import { Sheet, SheetPopup } from "./ui/sheet";

export function RightPanelSheet(props: {
  animationDurationMs: number;
  children: ReactNode;
  open: boolean;

  label?: string;
  focusAsk?: boolean;
  onClose: () => void;
}) {
  const popup = useRef<HTMLDivElement>(null);
  return (
    <Sheet
      open={props.open}
      onOpenChange={(open) => {
        if (!open) {
          props.onClose();
        }
      }}
    >
      <SheetPopup
        ref={popup}
        initialFocus={() => props.focusAsk ? (popup.current?.querySelector<HTMLElement>("[data-testid=issue-ask-panel] [role=textbox]") ?? true) : true}
        aria-label={props.label ?? "Right panel"}
        transitionDurationMs={props.animationDurationMs}
        side="right"
        showCloseButton={false}
        keepMounted
        className={RIGHT_PANEL_SHEET_CLASS_NAME}
      >
        {props.children}
      </SheetPopup>
    </Sheet>
  );
}
