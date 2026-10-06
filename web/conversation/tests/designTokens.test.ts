import { existsSync, readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import {
  buildTokens,
  contrastRatio,
  generate,
  oklchToRgb,
  outputPath,
  parseColor,
  parseLength,
  parseStylesheet,
  replaceMarkers,
  toHex,
} from "../scripts/design-tokens.ts";
import type { Token, TokenScope } from "../src/design-system/tokens.ts";

/** Builds tokens from inline stylesheets, in cascade order. */
function build(css: string[]) {
  return buildTokens({
    sheets: css.map((text, i) => parseStylesheet(text, `sheet${i}.css`)),
    palette: new Map([
      ["--color-white", "#fff"],
      ["--color-black", "#000"],
    ]),
    tailwindVersion: "test",
  });
}

function find(tokens: Token[], name: string, scope: TokenScope = "root"): Token {
  const token = tokens.find((t) => t.name === name && t.scope === scope);
  if (!token) throw new Error(`missing ${scope} ${name}`);
  return token;
}

describe("design token parser", () => {
  it.each([
    {
      name: "follows a var() chain to its colour",
      css: [":root { --a: var(--b); --b: var(--c); --c: #ff0000; }"],
      token: "--a",
      light: "#ff0000",
      dark: "#ff0000",
    },
    {
      name: "uses a var() fallback when the variable is undeclared",
      css: [":root { --a: var(--missing, #00ff00); }"],
      token: "--a",
      light: "#00ff00",
      dark: "#00ff00",
    },
    {
      name: "resolves palette variables",
      css: [":root { --a: var(--color-white); }"],
      token: "--a",
      light: "#ffffff",
      dark: "#ffffff",
    },
    {
      name: "mixes in srgb",
      css: [":root { --a: color-mix(in srgb, #ffffff 50%, #000000); }"],
      token: "--a",
      light: "#808080",
      dark: "#808080",
    },
    {
      name: "mixes with transparent into a translucent colour",
      css: [":root { --a: color-mix(in srgb, #ff0000 25%, transparent); }"],
      token: "--a",
      light: "#ff000040",
      dark: "#ff000040",
    },
    {
      name: "expands Tailwind's --alpha()",
      css: [":root { --a: --alpha(var(--color-white) / 10%); }"],
      token: "--a",
      light: "#ffffff1a",
      dark: "#ffffff1a",
    },
    {
      name: "nests color-mix() through var()",
      css: [":root { --p: 100%; --a: color-mix(in oklab, color-mix(in oklab, #123456 var(--p), #ffffff), black 0%); }"],
      token: "--a",
      light: "#123456",
      dark: "#123456",
    },
    {
      name: "reads a dark override from @variant dark",
      css: [":root { --bg: #ffffff; @variant dark { --bg: #000000; } }"],
      token: "--bg",
      light: "#ffffff",
      dark: "#000000",
    },
    {
      name: "keeps the dark value when a later sheet overrides :root",
      css: [":root { --bg: #ffffff; @variant dark { --bg: #000000; } }", ":root { --bg: #eeeeee; }"],
      token: "--bg",
      light: "#eeeeee",
      dark: "#000000",
    },
    {
      name: "resolves references in the theme being built",
      css: [":root { --bg: #ffffff; --fg: var(--bg); @variant dark { --bg: #000000; } }"],
      token: "--fg",
      light: "#ffffff",
      dark: "#000000",
    },
    {
      name: "ignores comments",
      css: [":root { /* --a: #000000; */ --a: #0000ff; }"],
      token: "--a",
      light: "#0000ff",
      dark: "#0000ff",
    },
  ])("$name", ({ css, token, light, dark }) => {
    const t = find(build(css).tokens, token);
    expect(t.light?.hex).toBe(light);
    expect(t.dark?.hex).toBe(dark);
  });

  it("maps @theme inline colour aliases to utilities instead of tokens", () => {
    const { tokens } = build([
      "@theme inline { --color-fg: var(--contrast-fg); --color-card: var(--card); }",
      ":root { --fg: #111111; --contrast-fg: var(--fg); --card: #ffffff; }",
    ]);
    expect(tokens.some((t) => t.name.startsWith("--color-"))).toBe(false);
    expect(find(tokens, "--contrast-fg").utility).toBe("*-fg");
    expect(find(tokens, "--fg")).toMatchObject({ utility: "*-fg", utilityVia: "--contrast-fg" });
    expect(find(tokens, "--card").utility).toBe("*-card");
  });

  it("derives radius utilities and lengths from @theme inline calc()", () => {
    const { tokens } = build([":root { --radius: 0.625rem; }", "@theme inline { --radius-sm: calc(var(--radius) - 4px); }"]);
    expect(find(tokens, "--radius-sm")).toMatchObject({ utility: "rounded-sm", group: "radius" });
    expect(find(tokens, "--radius-sm").light?.px).toBe(6);
  });

  it("resolves a scoped override against its own scope", () => {
    const { tokens } = build([
      ":root { --a: #ffffff; --b: var(--a); --c: var(--a); }",
      "[data-app-sidebar] { --a: #000000; --b: var(--a); }",
    ]);
    expect(find(tokens, "--b", "sidebar").light?.hex).toBe("#000000");
    expect(find(tokens, "--b").light?.hex).toBe("#ffffff");
    expect(tokens.some((t) => t.name === "--c" && t.scope === "sidebar")).toBe(false);
  });

  it("records the owner line of the winning declaration", () => {
    const { tokens } = build([":root {\n  /* note */\n  --a: #fff;\n  @variant dark {\n    --a: #000;\n  }\n}"]);
    expect(find(tokens, "--a").light?.owner).toBe("sheet0.css:3");
    expect(find(tokens, "--a").dark?.owner).toBe("sheet0.css:5");
  });

  it("lists keyframes, reduced-motion queries and animations", () => {
    const file = build([
      "@keyframes blink { from { opacity: 1; } to { opacity: 0; } }\n" +
        "@media (prefers-reduced-motion: no-preference) { .x { animation: blink 1s infinite; } }",
    ]);
    expect(file.keyframes.map((k) => k.name)).toEqual(["blink"]);
    expect(file.reducedMotion[0]?.query).toBe("(prefers-reduced-motion: no-preference)");
    expect(file.animations[0]).toMatchObject({ property: "animation", value: "blink 1s infinite" });
  });
});

describe("colour conversion", () => {
  it.each([
    { oklch: [1, 0, 0], hex: "#ffffff" },
    { oklch: [0, 0, 0], hex: "#000000" },
    { oklch: [0.627955, 0.257683, 29.2339], hex: "#ff0000" },
    { oklch: [0.86644, 0.294827, 142.4953], hex: "#00ff00" },
    { oklch: [0.452014, 0.313214, 264.052], hex: "#0000ff" },
  ])("converts oklch($oklch) to $hex", ({ oklch, hex }) => {
    const [l = 0, c = 0, h = 0] = oklch;
    expect(toHex(oklchToRgb(l, c, h))).toBe(hex);
  });

  it.each([
    // Tailwind v4 publishes these sRGB equivalents for its palette.
    { css: "oklch(27.4% 0.006 286.033)", hex: "#27272a" },
    { css: "oklch(14.5% 0 none)", hex: "#0a0a0a" },
    { css: "oklch(55.2% 0.016 285.938)", hex: "#71717b" },
    { css: "rgb(38 56 78)", hex: "#26384e" },
    { css: "rgb(255 255 255 / 8%)", hex: "#ffffff14" },
    { css: "#fca5a5", hex: "#fca5a5" },
    { css: "transparent", hex: "#00000000" },
  ])("parses $css", ({ css, hex }) => {
    const color = parseColor(css);
    expect(color && toHex(color)).toBe(hex);
  });

  it.each([{ css: "currentcolor" }, { css: "url(x.svg)" }, { css: "color-mix(in hsl, red, blue)" }])(
    "returns null for $css",
    ({ css }) => {
      expect(parseColor(css)).toBeNull();
    },
  );

  it.each([
    { css: "0.625rem", px: 10 },
    { css: "calc(0.625rem + 12px)", px: 22 },
    { css: "calc((1rem - 2px) * 2)", px: 28 },
    { css: "100%", px: null },
    { css: "calc(env(safe-area-inset-left) + 0.75rem)", px: null },
  ])("measures $css", ({ css, px }) => {
    expect(parseLength(css)).toBe(px);
  });
});

describe("contrast", () => {
  it.each([
    { a: "#000000", b: "#ffffff", ratio: 21 },
    { a: "#ffffff", b: "#000000", ratio: 21 },
    { a: "#777777", b: "#ffffff", ratio: 4.48 },
    { a: "#767676", b: "#ffffff", ratio: 4.54 },
    { a: "#336699", b: "#336699", ratio: 1 },
  ])("rates $a on $b at $ratio:1", ({ a, b, ratio }) => {
    const fg = parseColor(a);
    const bg = parseColor(b);
    if (!fg || !bg) throw new Error("unparsed");
    expect(contrastRatio(fg, bg)).toBeCloseTo(ratio, 2);
  });
});

describe("generated outputs", () => {
  it("rejects an unknown marker", () => {
    expect(() => replaceMarkers("<!-- tokens:nope:start -->\n<!-- tokens:nope:end -->", {})).toThrow(/tokens:nope/);
  });

  it("match the current stylesheets", () => {
    const { json, docs } = generate();
    expect(existsSync(outputPath)).toBe(true);
    expect(readFileSync(outputPath, "utf8"), "run npm run design:tokens").toBe(json);
    expect(docs.size).toBe(2);
    for (const [path, content] of docs) {
      expect(readFileSync(path, "utf8"), `${path} is stale; run npm run design:tokens`).toBe(content);
    }
  });
});
