// Design tokens for the Foundations pages.
//
// `src/design-system/tokens.generated.json` is produced by
// `scripts/design-tokens.ts`; `../tokens.ts` types it. The gallery only reads
// it: a value changes in its owning stylesheet, never here.
import React from "react";

import { tokenFile, type Theme, type Token, type TokenGroup, type TokenValue } from "../tokens";

export { tokenFile };
export type { Theme, Token, TokenGroup, TokenValue };

/** The `:root` tokens of the given groups, in the generated order. */
export function rootTokens(...groups: readonly TokenGroup[]): Token[] {
  return tokenFile.tokens.filter((token) => token.scope === "root" && groups.includes(token.group));
}

/** The `:root` tokens whose name starts with one of the prefixes. */
export function rootTokensNamed(...prefixes: readonly string[]): Token[] {
  return tokenFile.tokens.filter(
    (token) => token.scope === "root" && prefixes.some((prefix) => token.name.startsWith(prefix)),
  );
}

/** The value a token takes in a theme. */
export function valueIn(token: Token, theme: Theme): TokenValue | null {
  return theme === "dark" ? (token.dark ?? token.light) : (token.light ?? token.dark);
}

/** The theme of the frame a specimen renders in, when a frame provides one. */
export const FrameThemeContext = React.createContext<Theme | null>(null);

/** The theme the current document renders in (a frame sets it on `<html>`). */
export function documentTheme(): Theme {
  return globalThis.document?.documentElement.dataset.theme === "light" ? "light" : "dark";
}

/** The frame's theme, read from its context so it is right on the first render. */
export function useFrameTheme(): Theme {
  return React.useContext(FrameThemeContext) ?? documentTheme();
}

/** Only values a browser paints as a colour are shown as swatches. */
export function isPaintable(value: string | null | undefined): value is string {
  if (value === null || value === undefined) return false;
  return /^(#|rgb|hsl|oklch|oklab|lab|lch|color\()/i.test(value.trim());
}

/** A value for display: the hex for colours, px for lengths, else the declaration. */
export function displayValue(value: TokenValue | null): string {
  if (value === null) return "—";
  if (value.hex !== null) return value.hex;
  if (value.px !== null) return `${value.px}px`;
  return value.resolved;
}
