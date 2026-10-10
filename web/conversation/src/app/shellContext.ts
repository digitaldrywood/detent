import React from "react";
import type { ShellState } from "./App.tsx";

export const ShellContext = React.createContext<ShellState | null>(null);

export function useOptionalShell(): ShellState | null {
  return React.useContext(ShellContext);
}
