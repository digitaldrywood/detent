// Design tokens for the Foundations section, when the generated file exists.
//
// `src/design-system/tokens.generated.json` is produced by
// `scripts/design-tokens.ts`. The gallery only reads it, and tolerates both its
// absence and any of the shapes it may take while it settles: an array of
// tokens, or an object holding one under `tokens` or `entries`.

export interface TokenSwatch {
  readonly name: string;
  readonly group: string;
  readonly scope: string | null;
  readonly light: string | null;
  readonly dark: string | null;
}

const files = import.meta.glob<unknown>("../tokens.generated.json", { eager: true, import: "default" });

function str(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

/** A colour from a token value: a string, or an object carrying `hex`/`value`. */
function colour(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (value !== null && typeof value === "object") {
    const record = value as Record<string, unknown>;
    return str(record.hex) ?? str(record.value) ?? str(record.resolved);
  }
  return null;
}

export function normalizeTokens(raw: unknown): TokenSwatch[] {
  const rows = Array.isArray(raw)
    ? raw
    : raw !== null && typeof raw === "object"
      ? ((raw as Record<string, unknown>).tokens ?? (raw as Record<string, unknown>).entries)
      : null;
  if (!Array.isArray(rows)) return [];
  const tokens: TokenSwatch[] = [];
  for (const row of rows) {
    if (row === null || typeof row !== "object") continue;
    const item = row as Record<string, unknown>;
    const name = str(item.name);
    if (name === null) continue;
    const hex = item.hex;
    tokens.push({
      name,
      group: str(item.group) ?? "other",
      scope: str(item.scope),
      light: colour(item.light) ?? colour(hex !== null && typeof hex === "object" ? (hex as Record<string, unknown>).light : hex),
      dark: colour(item.dark) ?? colour(hex !== null && typeof hex === "object" ? (hex as Record<string, unknown>).dark : null),
    });
  }
  return tokens;
}

export function galleryTokens(): TokenSwatch[] {
  return normalizeTokens(Object.values(files)[0]);
}

/** Only values a browser paints as a colour are shown as swatches. */
export function isPaintable(value: string | null): value is string {
  if (value === null) return false;
  return /^(#|rgb|hsl|oklch|oklab|lab|lch|color\()/i.test(value.trim());
}
