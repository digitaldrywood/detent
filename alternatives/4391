/**
 * Typed access to the design tokens the client already defines.
 *
 * `tokens.generated.json` is produced by `scripts/design-tokens.ts` from the
 * owners `src/app/global.css` (base tokens) and `src/app/index.css` (fonts,
 * primary and the sign-in surface). This module only reads it; to change a value, edit the owning
 * stylesheet and run `npm run design:tokens`.
 */
import generated from "./tokens.generated.json";

export type Theme = "light" | "dark";

/** `root` is `:root`, `sidebar` is `[data-app-sidebar]`, `sign-in` is `.detent-sign-in`. */
export type TokenScope = "root" | "sidebar" | "sign-in";

export type TokenGroup =
  | "color-role"
  | "surface"
  | "text"
  | "border"
  | "feedback"
  | "sidebar"
  | "message"
  | "code"
  | "terminal"
  | "radius"
  | "layout"
  | "motion"
  | "elevation"
  | "layer"
  | "breakpoint"
  | "font";

export interface TokenValue {
  /** The declaration as written in the owner file. */
  value: string;
  /** The declaration with every statically known `var()` substituted. */
  resolved: string;
  /** Approximate sRGB hex (`#rrggbbaa` when translucent), for colours. */
  hex: string | null;
  /** False when the colour lies outside sRGB and the hex is clipped. */
  inGamut: boolean | null;
  /** Lengths in CSS pixels at a 16px root. */
  px: number | null;
  /** `file:line` of the winning declaration. */
  owner: string;
}

export interface Token {
  name: string;
  scope: TokenScope;
  group: TokenGroup;
  /** The Tailwind utility it backs; `*-name` means any colour utility (`bg-`, `text-`, `border-`…). */
  utility: string | null;
  /** Set when the utility reads the `--contrast-*` variant of this token. */
  utilityVia?: string;
  /**
   * True when the token resolves to a different value in dark mode, either
   * through its own dark declaration or through a themed token it references.
   */
  themed: boolean;
  light: TokenValue | null;
  dark: TokenValue | null;
}

export interface Keyframes {
  name: string;
  owner: string;
}

export interface Animation {
  selector: string;
  property: string;
  value: string;
  owner: string;
}

export interface ReducedMotionRule {
  query: string;
  owner: string;
  within: string | null;
}

export interface ContrastResult {
  label: string;
  scope: TokenScope;
  theme: Theme;
  /** `text` targets 4.5:1 (WCAG 1.4.3); `ui` targets 3:1 (WCAG 1.4.11). */
  kind: "text" | "ui";
  fg: string;
  bg: string;
  /** The painted colours after compositing translucent layers. */
  fgHex: string;
  bgHex: string;
  ratio: number;
  rating: "pass" | "large-only" | "fail";
}

export interface TokenFile {
  $comment: string;
  sources: { owners: string[]; tailwindcss: string };
  tokens: Token[];
  keyframes: Keyframes[];
  animations: Animation[];
  reducedMotion: ReducedMotionRule[];
  fontFaces: { family: string; owner: string }[];
  contrast: ContrastResult[];
  /** Values that depend on runtime input (env(), undeclared variables). */
  unresolved: string[];
}

export const tokenFile = generated as TokenFile;

export const tokens: readonly Token[] = tokenFile.tokens;

/** Looks a token up by its custom property name, in `:root` unless a scope is given. */
export function token(name: string, scope: TokenScope = "root"): Token | undefined {
  return tokens.find((t) => t.name === name && t.scope === scope);
}

export function tokensInGroup(group: TokenGroup, scope: TokenScope = "root"): Token[] {
  return tokens.filter((t) => t.group === group && t.scope === scope);
}

/** The value a token takes in a theme; untouched tokens share the light value. */
export function tokenValue(t: Token, theme: Theme): TokenValue | null {
  return theme === "dark" ? (t.dark ?? t.light) : (t.light ?? t.dark);
}

export const contrast: readonly ContrastResult[] = tokenFile.contrast;
