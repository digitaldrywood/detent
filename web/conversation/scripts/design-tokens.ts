/**
 * Extracts the design tokens the conversation client already defines and
 * publishes them as data. It never restates a value by hand: the owners stay
 *
 *   src/app/global.css  base tokens for both themes, sidebar and contrast roles
 *   src/app/index.css   fonts, primary and the sign-in surface
 *
 * and this script reads them, resolves `var()`/`color-mix()` chains against
 * the installed Tailwind palette, converts colours to approximate sRGB hex and
 * computes WCAG contrast for the key text-on-surface pairs.
 *
 * Outputs:
 *   src/design-system/tokens.generated.json
 *   generated tables between `<!-- tokens:NAME:start/end -->` markers in
 *   docs/design-system/foundations.md and docs/design-system/color-review.md
 *
 * Run:   npm run design:tokens
 * Check: npm run design:tokens:check   (exit 1 when any output is stale)
 */
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import type {
  Animation,
  ContrastResult,
  Keyframes,
  ReducedMotionRule,
  Theme,
  Token,
  TokenFile,
  TokenGroup,
  TokenScope,
  TokenValue,
} from "../src/design-system/tokens.ts";

// ---------------------------------------------------------------------------
// CSS parsing

export interface Declaration {
  name: string;
  value: string;
  file: string;
  line: number;
  scope: TokenScope;
  theme: Theme;
  /** `theme` for `@theme`, `theme-inline` for `@theme inline`, else `rule`. */
  layer: "theme" | "theme-inline" | "rule";
}

export interface Sheet {
  declarations: Declaration[];
  keyframes: Keyframes[];
  animations: Animation[];
  reducedMotion: ReducedMotionRule[];
  fontFaces: { family: string; owner: string }[];
}

interface Block {
  prelude: string;
  line: number;
  decls: { prop: string; value: string; line: number }[];
  children: Block[];
}

const collapse = (s: string) => s.replace(/\s+/g, " ").trim();

/** Blanks comments while keeping every newline, so line numbers stay true. */
const stripComments = (css: string) =>
  css.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));

function parseBlocks(css: string): Block {
  const root: Block = { prelude: "", line: 0, decls: [], children: [] };
  const stack: Block[] = [root];
  let buf = "";
  let bufLine = 0;
  let line = 1;
  let quote: string | null = null;
  let paren = 0;
  const top = () => stack[stack.length - 1] as Block;
  const flushDecl = () => {
    const text = buf.trim();
    buf = "";
    const colon = text.indexOf(":");
    if (!text || colon < 0 || text.startsWith("@")) return;
    top().decls.push({ prop: text.slice(0, colon).trim(), value: collapse(text.slice(colon + 1)), line: bufLine });
  };
  for (let i = 0; i < css.length; i++) {
    const ch = css[i] as string;
    if (!buf.trim() && !/\s/.test(ch)) bufLine = line;
    if (ch === "\n") line++;
    if (quote) {
      buf += ch;
      if (ch === quote && css[i - 1] !== "\\") quote = null;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      buf += ch;
      continue;
    }
    if (ch === "(") paren++;
    else if (ch === ")") paren--;
    // Braces and semicolons inside url(), var() and friends are not structure.
    if (paren > 0 || ch === ")") {
      buf += ch;
      continue;
    }
    if (ch === "{") {
      const block: Block = { prelude: collapse(buf), line: bufLine, decls: [], children: [] };
      top().children.push(block);
      stack.push(block);
      buf = "";
    } else if (ch === ";") {
      flushDecl();
    } else if (ch === "}") {
      flushDecl();
      if (stack.length > 1) stack.pop();
    } else {
      buf += ch;
    }
  }
  return root;
}

/** Maps a selector to the token scopes it declares, or null for ordinary rules. */
function selectorScopes(selector: string): { scopes: TokenScope[]; dark: boolean } | null {
  const scopes: TokenScope[] = [];
  let dark = false;
  for (const raw of selector.split(",")) {
    let part = raw.trim();
    if (part.startsWith(".dark ")) {
      dark = true;
      part = part.slice(".dark ".length).trim();
    }
    if (part === ":root") scopes.push("root");
    else if (part === "[data-app-sidebar]") scopes.push("sidebar");
    else if (part === ".detent-sign-in") scopes.push("sign-in");
    else return null;
  }
  return scopes.length ? { scopes, dark } : null;
}

interface WalkContext {
  scopes: TokenScope[] | null;
  theme: Theme;
  layer: Declaration["layer"];
  label: string[];
}

export function parseStylesheet(css: string, file: string): Sheet {
  const sheet: Sheet = { declarations: [], keyframes: [], animations: [], reducedMotion: [], fontFaces: [] };
  const owner = (line: number) => `${file}:${line}`;

  const walk = (block: Block, ctx: WalkContext) => {
    for (const d of block.decls) {
      if (ctx.scopes && d.prop.startsWith("--")) {
        for (const scope of ctx.scopes) {
          sheet.declarations.push({ name: d.prop, value: d.value, file, line: d.line, scope, theme: ctx.theme, layer: ctx.layer });
        }
      }
      if (/^(animation|animation-duration|animation-timing-function|transition|transition-duration)$/.test(d.prop)) {
        sheet.animations.push({ selector: ctx.label.join(" › "), property: d.prop, value: d.value, owner: owner(d.line) });
      }
    }
    for (const child of block.children) {
      const p = child.prelude;
      const label = [...ctx.label, p.length > 90 ? `${p.slice(0, 87)}…` : p];
      if (p.startsWith("@theme")) {
        walk(child, { scopes: ["root"], theme: "light", layer: /\binline\b/.test(p) ? "theme-inline" : "theme", label });
      } else if (p === "@variant dark") {
        walk(child, { ...ctx, theme: "dark", label });
      } else if (p.startsWith("@layer")) {
        walk(child, { ...ctx, label: ctx.label });
      } else if (p.startsWith("@keyframes")) {
        sheet.keyframes.push({ name: p.slice("@keyframes".length).trim(), owner: owner(child.line) });
      } else if (p.startsWith("@font-face")) {
        const family = child.decls.find((d) => d.prop === "font-family")?.value.replace(/["']/g, "") ?? "";
        sheet.fontFaces.push({ family, owner: owner(child.line) });
      } else if (p.startsWith("@")) {
        if (p.startsWith("@media") && p.includes("prefers-reduced-motion")) {
          sheet.reducedMotion.push({
            query: p.slice("@media".length).trim(),
            owner: owner(child.line),
            within: ctx.label.join(" › ") || null,
          });
        }
        walk(child, { ...ctx, scopes: null, label });
      } else if (ctx.scopes === null && ctx.label.length === 0) {
        // A top-level rule (or one directly inside @layer) may declare tokens.
        const mapped = selectorScopes(p);
        walk(child, { scopes: mapped?.scopes ?? null, theme: mapped?.dark ? "dark" : ctx.theme, layer: "rule", label });
      } else {
        // Nested selectors (`&[data-variant]`, rules inside @media) declare no tokens.
        walk(child, { ...ctx, scopes: null, label });
      }
    }
  };

  walk(parseBlocks(stripComments(css)), { scopes: null, theme: "light", layer: "rule", label: [] });
  return sheet;
}

// ---------------------------------------------------------------------------
// Colour maths (CSS Color 4/5, written out with plain arithmetic)

/** Gamma-encoded sRGB, unclamped so out-of-gamut colours stay detectable. */
export interface Rgba {
  r: number;
  g: number;
  b: number;
  alpha: number;
}

const toLinear = (c: number) => {
  const a = Math.abs(c);
  return Math.sign(c) * (a <= 0.04045 ? a / 12.92 : ((a + 0.055) / 1.055) ** 2.4);
};
const toGamma = (c: number) => {
  const a = Math.abs(c);
  return Math.sign(c) * (a <= 0.0031308 ? 12.92 * a : 1.055 * a ** (1 / 2.4) - 0.055);
};

export function oklabToRgb(L: number, a: number, b: number, alpha = 1): Rgba {
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
  return {
    r: toGamma(4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s),
    g: toGamma(-1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s),
    b: toGamma(-0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s),
    alpha,
  };
}

export function rgbToOklab({ r, g, b }: Rgba): [number, number, number] {
  const [lr, lg, lb] = [toLinear(r), toLinear(g), toLinear(b)];
  const l = Math.cbrt(0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb);
  const m = Math.cbrt(0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb);
  const s = Math.cbrt(0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb);
  return [
    0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  ];
}

export function oklchToRgb(L: number, C: number, H: number, alpha = 1): Rgba {
  const h = (H * Math.PI) / 180;
  return oklabToRgb(L, C * Math.cos(h), C * Math.sin(h), alpha);
}

const clamp01 = (n: number) => Math.min(1, Math.max(0, n));

export function toHex(c: Rgba): string {
  const byte = (n: number) => Math.round(clamp01(n) * 255).toString(16).padStart(2, "0");
  const hex = `#${byte(c.r)}${byte(c.g)}${byte(c.b)}`;
  return c.alpha >= 0.9995 ? hex : `${hex}${byte(c.alpha)}`;
}

export const inGamut = (c: Rgba) => [c.r, c.g, c.b].every((n) => n >= -0.001 && n <= 1.001);

/** Splits function arguments at top-level commas. */
function splitArgs(s: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of s) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      out.push(cur.trim());
      cur = "";
    } else cur += ch;
  }
  if (cur.trim()) out.push(cur.trim());
  return out;
}

/** Returns the argument text of `name(...)` when `s` is exactly that call. */
function callArgs(s: string, name: string): string | null {
  if (!s.toLowerCase().startsWith(`${name}(`) || !s.endsWith(")")) return null;
  let depth = 0;
  for (let i = name.length; i < s.length; i++) {
    if (s[i] === "(") depth++;
    if (s[i] === ")" && --depth === 0) return i === s.length - 1 ? s.slice(name.length + 1, -1) : null;
  }
  return null;
}

const num = (s: string, percentScale = 1) => {
  if (s === "none") return 0;
  return s.endsWith("%") ? (parseFloat(s) / 100) * percentScale : parseFloat(s);
};

function mix(space: string, a: Rgba, pa: number, b: Rgba, pb: number): Rgba | null {
  const wa = pa / (pa + pb);
  const wb = 1 - wa;
  const alpha = a.alpha * wa + b.alpha * wb;
  const multiplier = Math.min(1, pa + pb);
  if (alpha === 0) return { r: 0, g: 0, b: 0, alpha: 0 };
  let coords: (c: Rgba) => [number, number, number];
  let back: (v: [number, number, number], alpha: number) => Rgba;
  if (space === "srgb") {
    coords = (c) => [c.r, c.g, c.b];
    back = ([r, g, bb], al) => ({ r, g, b: bb, alpha: al });
  } else if (space === "srgb-linear") {
    coords = (c) => [toLinear(c.r), toLinear(c.g), toLinear(c.b)];
    back = ([r, g, bb], al) => ({ r: toGamma(r), g: toGamma(g), b: toGamma(bb), alpha: al });
  } else if (space === "oklab") {
    coords = rgbToOklab;
    back = ([L, aa, bb], al) => oklabToRgb(L, aa, bb, al);
  } else return null;
  const ca = coords(a);
  const cb = coords(b);
  // Premultiplied interpolation: a transparent side contributes no hue.
  const v = [0, 1, 2].map((i) => ((ca[i] as number) * a.alpha * wa + (cb[i] as number) * b.alpha * wb) / alpha) as [
    number,
    number,
    number,
  ];
  return back(v, alpha * multiplier);
}

/** Evaluates a fully substituted colour expression; null when it is not one. */
export function parseColor(input: string): Rgba | null {
  const s = collapse(input);
  const lower = s.toLowerCase();
  if (lower === "transparent") return { r: 0, g: 0, b: 0, alpha: 0 };
  if (lower === "white") return { r: 1, g: 1, b: 1, alpha: 1 };
  if (lower === "black") return { r: 0, g: 0, b: 0, alpha: 1 };
  const hex = /^#([0-9a-f]{3,8})$/i.exec(s)?.[1];
  if (hex && [3, 4, 6, 8].includes(hex.length)) {
    const full = hex.length <= 4 ? [...hex].map((c) => c + c).join("") : hex;
    const at = (i: number) => parseInt(full.slice(i, i + 2), 16) / 255;
    return { r: at(0), g: at(2), b: at(4), alpha: full.length === 8 ? at(6) : 1 };
  }
  const rgb = callArgs(s, "rgb") ?? callArgs(s, "rgba");
  if (rgb !== null) {
    const [main = "", alphaPart] = rgb.split("/");
    const parts = main.includes(",") ? splitArgs(main) : main.trim().split(/\s+/);
    const [r = "0", g = "0", b = "0", a4] = parts;
    const channel = (v: string) => (v.endsWith("%") ? parseFloat(v) / 100 : parseFloat(v) / 255);
    const alpha = alphaPart !== undefined ? num(alphaPart.trim()) : a4 !== undefined ? num(a4) : 1;
    return { r: channel(r), g: channel(g), b: channel(b), alpha };
  }
  const oklch = callArgs(s, "oklch");
  if (oklch !== null) {
    const [main = "", alphaPart] = oklch.split("/");
    const [l = "0", c = "0", h = "0"] = main.trim().split(/\s+/);
    return oklchToRgb(num(l), num(c, 0.4), num(h), alphaPart ? num(alphaPart.trim()) : 1);
  }
  const cm = callArgs(s, "color-mix");
  if (cm !== null) {
    const [method = "", first = "", second = ""] = splitArgs(cm);
    const space = /^in\s+([\w-]+)/.exec(method)?.[1];
    if (!space) return null;
    const side = (arg: string): [Rgba | null, number | null] => {
      const tail = /\s(-?[\d.]+%)$/.exec(arg);
      const head = /^(-?[\d.]+%)\s/.exec(arg);
      if (tail?.[1]) return [parseColor(arg.slice(0, -tail[1].length)), parseFloat(tail[1]) / 100];
      if (head?.[1]) return [parseColor(arg.slice(head[1].length)), parseFloat(head[1]) / 100];
      return [parseColor(arg), null];
    };
    const [a, pa0] = side(first);
    const [b, pb0] = side(second);
    if (!a || !b) return null;
    const pa = pa0 ?? (pb0 === null ? 0.5 : 1 - pb0);
    const pb = pb0 ?? 1 - pa;
    if (pa + pb <= 0) return null;
    return mix(space, a, pa, b, pb);
  }
  return null;
}

/** Source-over composite in gamma-encoded sRGB, as browsers paint. */
export function composite(top: Rgba, under: Rgba): Rgba {
  const a = top.alpha + under.alpha * (1 - top.alpha);
  if (a === 0) return { r: 0, g: 0, b: 0, alpha: 0 };
  const ch = (t: number, u: number) => (t * top.alpha + u * under.alpha * (1 - top.alpha)) / a;
  return { r: ch(top.r, under.r), g: ch(top.g, under.g), b: ch(top.b, under.b), alpha: a };
}

export function relativeLuminance(c: Rgba): number {
  const [r, g, b] = [c.r, c.g, c.b].map((v) => toLinear(clamp01(v))) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrastRatio(a: Rgba, b: Rgba): number {
  const [hi, lo] = [relativeLuminance(a), relativeLuminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

// ---------------------------------------------------------------------------
// Lengths

/** Evaluates px/rem lengths and simple calc() arithmetic; null otherwise. */
export function parseLength(input: string): number | null {
  let s = collapse(input);
  const inner = callArgs(s, "calc");
  if (inner !== null) s = inner;
  if (!/^[\d.\s+\-*/()a-z]+$/i.test(s)) return null;
  const tokens = s.match(/-?\d*\.?\d+(?:px|rem)?|[+\-*/()]/g);
  if (!tokens || tokens.join("") !== s.replace(/\s+/g, "")) return null;
  let i = 0;
  const value = (t: string) => (t.endsWith("rem") ? parseFloat(t) * 16 : parseFloat(t));
  const primary = (): number => {
    const t = tokens[i++];
    if (t === "(") {
      const v = sum();
      i++;
      return v;
    }
    if (t === undefined || Number.isNaN(parseFloat(t))) throw new Error("length");
    return value(t);
  };
  const product = (): number => {
    let v = primary();
    while (tokens[i] === "*" || tokens[i] === "/") v = tokens[i++] === "*" ? v * primary() : v / primary();
    return v;
  };
  const sum = (): number => {
    let v = product();
    while (tokens[i] === "+" || tokens[i] === "-") v = tokens[i++] === "+" ? v + product() : v - product();
    return v;
  };
  try {
    const v = sum();
    return i === tokens.length && /px|rem/.test(s) ? Math.round(v * 1000) / 1000 : null;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Token model

const GROUP_RULES: [TokenGroup, RegExp][] = [
  ["font", /^--(font|text)-/],
  ["motion", /^--(animate|ease)-/],
  ["radius", /^--(radius|control-radius)/],
  ["layout", /-(inset|gap|height|width|top|left|right)$|^--workspace-|-control-size$|-size$/],
  ["sidebar", /^--(contrast-)?sidebar/],
  ["message", /^--(contrast-)?message-/],
  ["code", /^--code-/],
  ["terminal", /^--terminal-/],
  ["feedback", /^--(error|warning|info|success|update|tool-error|destructive)/],
  ["color-role", /^--(primary|appearance-|color-)/],
  ["border", /^--(contrast-)?(border|input|ring|toolbar-border)$/],
  ["text", /foreground$|^--(contrast-)?(placeholder|secondary-label|icon-muted)$/],
  ["surface", /./],
];

export const groupOf = (name: string): TokenGroup =>
  (GROUP_RULES.find(([, re]) => re.test(name)) as [TokenGroup, RegExp])[0];

const isAlias = (d: Declaration) => d.layer === "theme-inline" && d.name.startsWith("--color-") && /^var\(--[\w-]+\)$/.test(d.value);

const SCOPE_PARENT: Record<TokenScope, TokenScope | null> = { root: null, sidebar: "root", "sign-in": "root" };

/** Finds the matching `)` for the `(` at `open`. */
function closing(s: string, open: number): number {
  let depth = 0;
  for (let i = open; i < s.length; i++) {
    if (s[i] === "(") depth++;
    if (s[i] === ")" && --depth === 0) return i;
  }
  return -1;
}

export interface BuildInput {
  sheets: Sheet[];
  /** Tailwind's default theme variables, the bottom of every lookup. */
  palette: Map<string, string>;
  tailwindVersion: string;
}

export function buildTokens(input: BuildInput): TokenFile {
  const decls = input.sheets.flatMap((s) => s.declarations);
  const aliases = decls.filter(isAlias);
  const tokenDecls = decls.filter((d) => !isAlias(d));

  // Last declaration wins within a theme. A dark declaration is a
  // `:is(.dark …)` variant with higher specificity, so a later light override
  // (index.css) does not replace it.
  const byKey = new Map<string, { light?: Declaration; dark?: Declaration }>();
  for (const d of tokenDecls) {
    const key = `${d.scope}|${d.name}`;
    const entry = byKey.get(key) ?? {};
    entry[d.theme] = d;
    byKey.set(key, entry);
  }
  const aliasValue = new Map(aliases.map((d) => [d.name, d.value]));

  const find = (name: string, scope: TokenScope, theme: Theme): { value: string; scope: TokenScope } | null => {
    const entry = byKey.get(`${scope}|${name}`);
    const d = (theme === "dark" ? entry?.dark : undefined) ?? entry?.light;
    if (d) return { value: d.value, scope };
    const parent = SCOPE_PARENT[scope];
    if (parent) return find(name, parent, theme);
    const v = aliasValue.get(name) ?? input.palette.get(name);
    return v === undefined ? null : { value: v, scope: "root" };
  };

  const substitute = (text: string, scope: TokenScope, theme: Theme, seen: Set<string>): string => {
    let out = text;
    for (let at = out.indexOf("var("); at >= 0; at = out.indexOf("var(", at)) {
      const end = closing(out, at + 3);
      if (end < 0) break;
      const [ref = "", ...fallback] = splitArgs(out.slice(at + 4, end));
      const found = seen.has(ref) ? null : find(ref, scope, theme);
      let replacement: string;
      if (found) replacement = substitute(found.value, found.scope, theme, new Set([...seen, ref]));
      else if (fallback.length) replacement = substitute(fallback.join(", "), scope, theme, seen);
      else {
        at = end + 1;
        continue;
      }
      out = out.slice(0, at) + replacement + out.slice(end + 1);
      at += replacement.length;
    }
    // Tailwind's --alpha(color / n%) compiles to an oklab mix with transparent.
    for (let at = out.indexOf("--alpha("); at >= 0; at = out.indexOf("--alpha(")) {
      const end = closing(out, at + 7);
      const inner = out.slice(at + 8, end);
      const slash = inner.lastIndexOf("/");
      out = `${out.slice(0, at)}color-mix(in oklab, ${inner.slice(0, slash).trim()} ${inner.slice(slash + 1).trim()}, transparent)${out.slice(end + 1)}`;
    }
    return collapse(out);
  };

  const valueOf = (d: Declaration, scope: TokenScope, theme: Theme): TokenValue => {
    const resolved = substitute(d.value, scope, theme, new Set([d.name]));
    const color = parseColor(resolved);
    const px = color ? null : parseLength(resolved);
    return {
      value: d.value,
      resolved,
      hex: color ? toHex(color) : null,
      inGamut: color ? inGamut(color) : null,
      px,
      owner: `${d.file}:${d.line}`,
    };
  };

  const utilities = new Map<string, { utility: string; via?: string }>();
  for (const a of aliases) {
    const target = (/^var\((--[\w-]+)\)$/.exec(a.value) as RegExpExecArray)[1] as string;
    const utility = `*-${a.name.slice("--color-".length)}`;
    utilities.set(target, { utility });
    if (target.startsWith("--contrast-")) {
      const base = `--${target.slice("--contrast-".length)}`;
      if (!utilities.has(base)) utilities.set(base, { utility, via: target });
    }
  }
  const utilityFor = (name: string, layer: Declaration["layer"]): { utility: string; via?: string } | null => {
    if (layer !== "rule") {
      if (name.startsWith("--radius-")) return { utility: `rounded-${name.slice("--radius-".length)}` };
      if (name.startsWith("--animate-")) return { utility: name.slice(2) };
      if (name.startsWith("--font-")) return { utility: name.slice(2) };
      if (name.startsWith("--text-") && !name.includes("--line-height")) return { utility: name.slice(2) };
      if (name.startsWith("--color-")) return { utility: `*-${name.slice("--color-".length)}` };
      if (name.startsWith("--ease-")) return { utility: name.slice(2) };
    }
    return utilities.get(name) ?? null;
  };

  const tokens: Token[] = [];
  for (const [key, entry] of byKey) {
    const [scope, name] = key.split("|") as [TokenScope, string];
    const first = (entry.light ?? entry.dark) as Declaration;
    const light = entry.light ? valueOf(entry.light, scope, "light") : null;
    const darkDecl = entry.dark ?? entry.light;
    const dark = darkDecl ? valueOf(darkDecl, scope, "dark") : null;
    const u = utilityFor(name, first.layer);
    tokens.push({
      name,
      scope,
      group: groupOf(name),
      utility: u?.utility ?? null,
      ...(u?.via ? { utilityVia: u.via } : {}),
      themed: entry.dark !== undefined,
      light,
      dark,
    });
  }
  const scopeOrder: TokenScope[] = ["root", "sidebar", "sign-in"];
  tokens.sort((a, b) => scopeOrder.indexOf(a.scope) - scopeOrder.indexOf(b.scope) || a.name.localeCompare(b.name));

  const keyframes: Keyframes[] = input.sheets.flatMap((s) => s.keyframes);

  const colorOf = (name: string, scope: TokenScope, theme: Theme): Rgba | null => {
    const found = find(name, scope, theme);
    return found ? parseColor(substitute(found.value, found.scope, theme, new Set([name]))) : null;
  };

  const contrast: ContrastResult[] = [];
  for (const pair of CONTRAST_PAIRS) {
    for (const theme of ["light", "dark"] as Theme[]) {
      const fg = colorOf(pair.fg, pair.scope, theme);
      const bg = colorOf(pair.bg, pair.scope, theme);
      const base = colorOf(pair.over ?? "--background", pair.scope, theme);
      if (!fg || !bg || !base) continue;
      const surface = bg.alpha < 1 ? composite(bg, base) : bg;
      const text = fg.alpha < 1 ? composite(fg, surface) : fg;
      const ratio = Math.round(contrastRatio(text, surface) * 100) / 100;
      const min = pair.kind === "text" ? 4.5 : 3;
      contrast.push({
        label: pair.label,
        scope: pair.scope,
        theme,
        kind: pair.kind,
        fg: pair.fg,
        bg: pair.bg,
        fgHex: toHex(text),
        bgHex: toHex(surface),
        ratio,
        rating: ratio >= min ? "pass" : pair.kind === "text" && ratio >= 3 ? "large-only" : "fail",
      });
    }
  }

  const unresolved = tokens.flatMap((t) => {
    const runtime = (v: TokenValue | null) => (v && /\b(env|var)\(/.test(v.resolved) ? v.resolved : null);
    const [light, dark] = [runtime(t.light), runtime(t.dark)];
    if (light && light === dark) return [`${t.name} (${t.scope}): ${light}`];
    return [
      ...(light ? [`${t.name} (${t.scope}, light): ${light}`] : []),
      ...(dark ? [`${t.name} (${t.scope}, dark): ${dark}`] : []),
    ];
  });

  return {
    $comment: "Generated by web/conversation/scripts/design-tokens.ts from src/app/global.css and src/app/index.css. Do not edit.",
    sources: {
      owners: input.sheets.length ? [...new Set(input.sheets.flatMap((s) => s.declarations.map((d) => d.file)))] : [],
      tailwindcss: input.tailwindVersion,
    },
    tokens,
    keyframes,
    animations: input.sheets.flatMap((s) => s.animations),
    reducedMotion: input.sheets.flatMap((s) => s.reducedMotion),
    fontFaces: input.sheets.flatMap((s) => s.fontFaces),
    contrast,
    unresolved: [...new Set(unresolved)],
  };
}

interface ContrastPair {
  label: string;
  fg: string;
  bg: string;
  /** What a translucent `bg` sits on; defaults to the scope's --background. */
  over?: string;
  scope: TokenScope;
  kind: "text" | "ui";
}

const pairs = (scope: TokenScope, list: Omit<ContrastPair, "scope" | "kind">[], kind: ContrastPair["kind"] = "text") =>
  list.map((p) => ({ ...p, scope, kind }));

/** Text uses the `--contrast-*` value its Tailwind utility renders. */
export const CONTRAST_PAIRS: ContrastPair[] = [
  ...pairs("root", [
    { label: "foreground on background", fg: "--contrast-foreground", bg: "--background" },
    { label: "card-foreground on card", fg: "--contrast-card-foreground", bg: "--card" },
    { label: "popover-foreground on popover", fg: "--contrast-popover-foreground", bg: "--popover" },
    { label: "muted-foreground on background", fg: "--contrast-muted-foreground", bg: "--background" },
    { label: "muted-foreground on card", fg: "--contrast-muted-foreground", bg: "--card" },
    { label: "muted-foreground on muted", fg: "--contrast-muted-foreground", bg: "--muted" },
    { label: "placeholder on background", fg: "--contrast-placeholder", bg: "--background" },
    { label: "secondary-foreground on secondary", fg: "--contrast-secondary-foreground", bg: "--secondary" },
    { label: "accent-foreground on accent", fg: "--contrast-accent-foreground", bg: "--accent" },
    { label: "primary-foreground on primary", fg: "--primary-foreground", bg: "--primary" },
    { label: "message-foreground on message", fg: "--contrast-message-foreground", bg: "--message-surface" },
    { label: "message-action-foreground on message-action", fg: "--message-action-foreground", bg: "--message-action" },
    { label: "error-foreground on background", fg: "--error-foreground", bg: "--background" },
    { label: "error-foreground on error-surface", fg: "--error-foreground", bg: "--error-surface" },
    { label: "warning-foreground on background", fg: "--warning-foreground", bg: "--background" },
    { label: "warning-foreground on warning-surface", fg: "--warning-foreground", bg: "--warning-surface" },
    { label: "success-foreground on background", fg: "--success-foreground", bg: "--background" },
    { label: "info-foreground on background", fg: "--info-foreground", bg: "--background" },
    { label: "update-foreground on update-surface", fg: "--update-foreground", bg: "--update-surface" },
    { label: "code-foreground on code-background", fg: "--code-foreground", bg: "--code-background" },
    { label: "terminal-foreground on terminal-background", fg: "--terminal-foreground", bg: "--terminal-background" },
    { label: "sidebar-foreground on sidebar (portaled)", fg: "--contrast-sidebar-foreground", bg: "--sidebar" },
    { label: "sidebar-muted-foreground on sidebar (portaled)", fg: "--contrast-sidebar-muted-foreground", bg: "--sidebar" },
  ]),
  ...pairs("sidebar", [
    { label: "sidebar-foreground on sidebar", fg: "--contrast-sidebar-foreground", bg: "--sidebar" },
    { label: "sidebar-muted-foreground on sidebar", fg: "--contrast-sidebar-muted-foreground", bg: "--sidebar" },
    { label: "sidebar-foreground on row hover", fg: "--contrast-sidebar-foreground", bg: "--sidebar-row-hover", over: "--sidebar" },
    { label: "sidebar-foreground on row active", fg: "--contrast-sidebar-foreground", bg: "--sidebar-row-active", over: "--sidebar" },
    { label: "sidebar-foreground on row selected", fg: "--contrast-sidebar-foreground", bg: "--sidebar-row-selected", over: "--sidebar" },
    { label: "sidebar-muted-foreground on row active", fg: "--contrast-sidebar-muted-foreground", bg: "--sidebar-row-active", over: "--sidebar" },
  ]),
  // --sidebar-icon-color is declared on :root only, so the app sidebar inherits
  // the root-computed colour and paints it on its own surface.
  ...pairs("root", [{ label: "sidebar-icon-color on sidebar (portaled)", fg: "--sidebar-icon-color", bg: "--sidebar" }], "ui"),
  ...pairs("sidebar", [{ label: "sidebar-icon-color on sidebar", fg: "--sidebar-icon-color", bg: "--sidebar" }], "ui"),
  ...pairs("sign-in", [
    { label: "foreground on background", fg: "--contrast-foreground", bg: "--background" },
    { label: "muted-foreground on card", fg: "--contrast-muted-foreground", bg: "--card" },
    { label: "primary-foreground on primary", fg: "--primary-foreground", bg: "--primary" },
  ]),
];

// ---------------------------------------------------------------------------
// Docs

const code = (s: string | null | undefined) => (s ? `\`${s.replace(/\|/g, "\\|")}\`` : "—");

function table(head: string[], rows: string[][]): string {
  return [`| ${head.join(" | ")} |`, `| ${head.map(() => "---").join(" | ")} |`, ...rows.map((r) => `| ${r.join(" | ")} |`)].join(
    "\n",
  );
}

const shown = (v: TokenValue | null) => {
  if (!v) return "—";
  if (v.hex) return `${code(v.hex)}${v.inGamut === false ? "†" : ""}`;
  if (v.px !== null) return code(`${v.px}px`);
  return code(v.resolved.length > 60 ? v.value : v.resolved);
};

const ownerOf = (t: Token) => {
  const l = t.light?.owner;
  const d = t.dark?.owner;
  return l && d && l !== d ? `${code(l)}, dark ${code(d)}` : code(l ?? d);
};

const COLOR_GROUPS: TokenGroup[] = ["color-role", "surface", "text", "border", "feedback", "message", "code", "terminal"];

export function renderSections(file: TokenFile): Record<string, string> {
  const root = file.tokens.filter((t) => t.scope === "root");
  const tokenRows = (list: Token[], withGroup = false) =>
    list.map((t) => [
      code(t.name),
      ...(withGroup ? [t.group] : []),
      code(t.utility),
      shown(t.light),
      t.themed ? shown(t.dark) : "same",
      ownerOf(t),
    ]);
  const head = (withGroup = false) => ["Token", ...(withGroup ? ["Group"] : []), "Utility", "Light", "Dark", "Owner"];
  const colorList = root.filter((t) => COLOR_GROUPS.includes(t.group) && (t.light?.hex || t.dark?.hex));

  const summary = table(
    ["Scope", "Tokens", "With a dark value", "Colours"],
    (["root", "sidebar", "sign-in"] as TokenScope[]).map((scope) => {
      const list = file.tokens.filter((t) => t.scope === scope);
      const themed = list.filter((t) => t.themed).length;
      const colours = list.filter((t) => t.light?.hex || t.dark?.hex).length;
      return [code(scope), String(list.length), String(themed), String(colours)];
    }),
  );

  const colors = COLOR_GROUPS.map((g) => {
    const list = colorList.filter((t) => t.group === g);
    return list.length ? `**${g}**\n\n${table(head(), tokenRows(list))}` : "";
  })
    .filter(Boolean)
    .join("\n\n");

  const sidebarScoped = file.tokens.filter((t) => t.scope === "sidebar");
  const sidebarTable = [
    "`:root` sidebar roles (portaled sheets and settings navigation):",
    "",
    table(head(), tokenRows(root.filter((t) => t.group === "sidebar"))),
    "",
    "`[data-app-sidebar]` overrides (the app sidebar itself):",
    "",
    table(head(true), tokenRows(sidebarScoped, true)),
  ].join("\n");

  const fonts = [
    table(head(), tokenRows(root.filter((t) => t.group === "font"))),
    "",
    table(["Font face", "Owner"], file.fontFaces.map((f) => [f.family, code(f.owner)])),
  ].join("\n");

  const radius = table(head(), tokenRows(root.filter((t) => t.group === "radius")));
  const layout = table(head(), tokenRows(root.filter((t) => t.group === "layout")));
  const motion = [
    table(head(), tokenRows(root.filter((t) => t.group === "motion"))),
    "",
    table(["Keyframes", "Owner"], file.keyframes.map((k) => [code(k.name), code(k.owner)])),
    "",
    table(
      ["Where", "Property", "Value", "Owner"],
      file.animations.map((a) => [code(a.selector), a.property, code(a.value), code(a.owner)]),
    ),
    "",
    table(["Reduced-motion query", "Inside", "Owner"], file.reducedMotion.map((r) => [code(r.query), code(r.within), code(r.owner)])),
  ].join("\n");

  const contrastRows = (scope: TokenScope) => {
    const labels = [...new Set(file.contrast.filter((c) => c.scope === scope).map((c) => c.label))];
    return labels.map((label) => {
      const at = (theme: Theme) => file.contrast.find((c) => c.scope === scope && c.label === label && c.theme === theme);
      const cell = (c: ContrastResult | undefined) =>
        c ? `${c.ratio.toFixed(2)} ${c.rating === "pass" ? "pass" : c.rating === "large-only" ? "**large only**" : "**fail**"}` : "—";
      const l = at("light");
      const d = at("dark");
      return [label, l?.kind === "ui" ? "3:1 (UI)" : "4.5:1", l ? `${code(l.fgHex)} on ${code(l.bgHex)}` : "—", cell(l), d ? `${code(d.fgHex)} on ${code(d.bgHex)}` : "—", cell(d)];
    });
  };
  const contrastHead = ["Pair", "Target", "Light colours", "Light", "Dark colours", "Dark"];
  const contrast = [
    "Workspace (`:root`):",
    "",
    table(contrastHead, contrastRows("root")),
    "",
    "App sidebar (`[data-app-sidebar]`):",
    "",
    table(contrastHead, contrastRows("sidebar")),
    "",
    "Public sign-in exception (`.detent-sign-in`):",
    "",
    table(contrastHead, contrastRows("sign-in")),
  ].join("\n");

  const unresolved = file.unresolved.length ? file.unresolved.map((u) => `- ${code(u)}`).join("\n") : "None.";

  return {
    summary,
    colors,
    sidebar: sidebarTable,
    fonts,
    radius,
    layout,
    motion,
    contrast,
    unresolved,
  };
}

/** Rewrites every `<!-- tokens:NAME:start -->…<!-- tokens:NAME:end -->` region. */
export function replaceMarkers(doc: string, sections: Record<string, string>): string {
  return doc.replace(
    /<!-- tokens:([\w-]+):start -->[\s\S]*?<!-- tokens:\1:end -->/g,
    (_whole, name: string) => {
      const body = sections[name];
      if (body === undefined) throw new Error(`unknown marker tokens:${name}`);
      return `<!-- tokens:${name}:start -->\n${body}\n<!-- tokens:${name}:end -->`;
    },
  );
}

// ---------------------------------------------------------------------------
// Main

const here = dirname(fileURLToPath(import.meta.url));
export const appRoot = resolve(here, "..");
const repoRoot = resolve(appRoot, "../..");
export const outputPath = join(appRoot, "src/design-system/tokens.generated.json");
const docPaths = ["docs/design-system/foundations.md", "docs/design-system/color-review.md"].map((p) => join(repoRoot, p));

const OWNERS = ["src/app/global.css", "src/app/index.css"];

export function generate(): { json: string; docs: Map<string, string> } {
  const sheets = OWNERS.map((p) => parseStylesheet(readFileSync(join(appRoot, p), "utf8"), p));
  const tailwindDir = join(appRoot, "node_modules/tailwindcss");
  const paletteSheet = parseStylesheet(readFileSync(join(tailwindDir, "theme.css"), "utf8"), "tailwindcss/theme.css");
  const palette = new Map(paletteSheet.declarations.map((d) => [d.name, d.value]));
  const tailwindVersion = (JSON.parse(readFileSync(join(tailwindDir, "package.json"), "utf8")) as { version: string }).version;
  const file = buildTokens({ sheets, palette, tailwindVersion });
  const sections = renderSections(file);
  const docs = new Map<string, string>();
  for (const path of docPaths) {
    if (existsSync(path)) docs.set(path, replaceMarkers(readFileSync(path, "utf8"), sections));
  }
  return { json: `${JSON.stringify(file, null, 2)}\n`, docs };
}

function main() {
  const check = process.argv.includes("--check");
  const { json, docs } = generate();
  const outputs = new Map([[outputPath, json], ...docs]);
  const stale: string[] = [];
  for (const [path, content] of outputs) {
    const current = existsSync(path) ? readFileSync(path, "utf8") : "";
    if (current === content) continue;
    if (check) stale.push(relative(process.cwd(), path));
    else {
      writeFileSync(path, content);
      console.log(`wrote ${relative(process.cwd(), path)}`);
    }
  }
  if (stale.length) {
    console.error(`design tokens are stale; run npm run design:tokens:\n  ${stale.join("\n  ")}`);
    process.exit(1);
  }
  if (check) console.log("design tokens are current");
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) main();
