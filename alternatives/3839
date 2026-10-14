// Cross-references in message text (decisions.md §13.7, §14).
//
// The hub extracts references on accept and the message resource carries them
// with the exact label each one appeared as — `#123`, or `org/project#123`.
// The client's whole job is to turn those labels into links, and to leave the
// text alone where the resource carries none: a `#123` nobody resolved is a
// `#123`, not a guess at a URL.
import type { MessageReference } from "../../contracts/index.ts";

/** Escapes a string for use inside a regular expression. */
function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Escapes the characters that would otherwise end a markdown link. */
function escapeLinkText(value: string): string {
  return value.replace(/[[\]]/g, "\\$&");
}

/**
 * Rewrites each reference's label into a markdown link, leaving fenced and
 * inline code untouched — a `#123` inside a code span is part of the code, and
 * linkifying it would change what the message says.
 *
 * Returns the source unchanged when there is nothing to link, so an unmodified
 * message costs one array check.
 */
export function linkReferences(
  source: string,
  references: readonly MessageReference[] | undefined,
): string {
  if (references === undefined || references.length === 0) return source;
  // Longest label first: `org/project#12` must not be half-rewritten by `#12`.
  const ordered = [...references]
    .filter((reference) => reference.label.length > 0 && reference.url.length > 0)
    .sort((left, right) => right.label.length - left.label.length);
  if (ordered.length === 0) return source;

  // Split on code so the rewrite never enters it: fenced blocks first, then
  // inline spans. The odd segments of each split are the code.
  return splitOutside(source, /(```[\s\S]*?```|~~~[\s\S]*?~~~)/g, (block) =>
    splitOutside(block, /(`[^`\n]*`)/g, (text) => {
      let result = text;
      for (const reference of ordered) {
        const pattern = new RegExp(`(?<![\\w/#-])${escapeRegExp(reference.label)}(?![\\w-])`, "g");
        result = result.replace(
          pattern,
          () => `[${escapeLinkText(reference.label)}](${reference.url})`,
        );
      }
      return result;
    }),
  );
}

/** Applies `transform` to everything the capturing `pattern` does not match. */
function splitOutside(
  source: string,
  pattern: RegExp,
  transform: (segment: string) => string,
): string {
  return source
    .split(pattern)
    .map((segment, index) => (index % 2 === 1 ? segment : transform(segment)))
    .join("");
}

/** One run of a message's plain text: either literal, or a resolved link. */
export type ReferenceSegment =
  | { readonly kind: "text"; readonly text: string }
  | { readonly kind: "link"; readonly text: string; readonly url: string };

/**
 * Splits plain text into literal runs and reference links. Used where the text
 * is not markdown — a user's own message bubble — so the same `#123` is a link
 * whoever wrote it.
 */
export function referenceSegments(
  source: string,
  references: readonly MessageReference[] | undefined,
): readonly ReferenceSegment[] {
  const ordered = [...(references ?? [])]
    .filter((reference) => reference.label.length > 0 && reference.url.length > 0)
    .sort((left, right) => right.label.length - left.label.length);
  if (ordered.length === 0) return [{ kind: "text", text: source }];

  const pattern = new RegExp(
    `(?<![\\w/#-])(${ordered.map((reference) => escapeRegExp(reference.label)).join("|")})(?![\\w-])`,
    "g",
  );
  const byLabel = new Map(ordered.map((reference) => [reference.label, reference.url] as const));
  const segments: ReferenceSegment[] = [];
  let index = 0;
  for (const match of source.matchAll(pattern)) {
    const at = match.index ?? 0;
    if (at > index) segments.push({ kind: "text", text: source.slice(index, at) });
    const label = match[0];
    segments.push({ kind: "link", text: label, url: byLabel.get(label) ?? "" });
    index = at + label.length;
  }
  if (index < source.length) segments.push({ kind: "text", text: source.slice(index) });
  return segments;
}
