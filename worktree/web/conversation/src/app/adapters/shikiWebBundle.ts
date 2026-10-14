// Shiki, narrowed to the web bundle.
//
// `@pierre/diffs` highlights code — both the diff hunks and every fenced block
// in `components/ChatMarkdown.tsx` — and it imports `createHighlighter`,
// `createJavaScriptRegexEngine` and `createOnigurumaEngine` from `shiki`.
//
// Shiki's root entry point is `bundle/full`: a map of every grammar it ships,
// around 200 of them, each behind its own dynamic import. That is the right
// default for a desktop app, and the wrong one for a bundle a browser fetches
// over the network — it committed 14 MB of generated grammar chunks to this
// repository, for languages a Detent conversation will never show.
//
// `shiki/bundle/web` is Shiki's own answer: the same API over the 79 languages
// a web client actually encounters, which covers everything Detent's runners
// write. It does not re-export the two engine factories, so they come straight
// from `shiki/engine/*` here and the three names `@pierre/diffs` asks for are
// all satisfied. The build aliases `shiki` to this module, so neither the
// vendored package nor any copied file changes.
//
// A language outside the web bundle falls back to `text` through
// `lib/syntaxHighlighting.ts`, which already has that path for a language
// Shiki does not know: the block renders unhighlighted rather than failing.
export {
  bundledLanguages,
  bundledThemes,
  createHighlighter,
  getSingletonHighlighter,
  codeToHast,
  codeToHtml,
  codeToTokens,
  codeToTokensBase,
  codeToTokensWithThemes,
  getLastGrammarState,
} from "shiki/bundle/web";

export { createJavaScriptRegexEngine } from "shiki/engine/javascript";
export { createOnigurumaEngine } from "shiki/engine/oniguruma";

export * from "shiki/core";
