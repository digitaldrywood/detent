import React from "react";
import { createPortal } from "react-dom";

export const PlatformHeadingContext = React.createContext<React.Dispatch<React.SetStateAction<React.ReactNode>>>(() => undefined);

export const PlatformActionsContext = React.createContext<HTMLDivElement | null>(null);

export function PlatformPageActions({ children }: { readonly children: React.ReactNode }): React.ReactNode {
  const target = React.useContext(PlatformActionsContext);
  return target === null ? null : createPortal(children, target);
}
