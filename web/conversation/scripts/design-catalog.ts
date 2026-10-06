/**
 * Validates the component catalog and generates the design-system contract docs.
 *
 *   tsx scripts/design-catalog.ts            validate, then write the docs
 *   tsx scripts/design-catalog.ts --check    validate, fail if the docs are stale
 *
 * The catalog (`src/design-system/catalog.json`) records every primitive in
 * `src/components/ui` and the key compositions built from them. Validation
 * proves each claim against the source: the file exists, its exports and cva
 * variants are what the entry says, and every primitive file has an entry.
 */
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, dirname, join, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import ts from "typescript";

export type EntryKind = "primitive" | "composition" | "surface";
export type EntryStatus = "available" | "proposed" | "exception";

export interface CatalogEntry {
  id: string;
  name: string;
  kind: EntryKind;
  group: string;
  status: EntryStatus;
  /** Relative to the client root (`web/conversation`); null when proposed. */
  source: string | null;
  /** Supplementary modules that belong to the same component (logic, helpers, styles). */
  files?: string[];
  exports: string[];
  variants: Record<string, string[]>;
  states: string[];
  keyboard: string;
  use: string;
  avoid: string;
  related: string[];
}

export interface Catalog {
  schema: 1;
  entries: CatalogEntry[];
}

export interface ValidationOptions {
  /** The client root that `source` paths resolve against. */
  clientRoot: string;
  /** The primitive directory every file of which must be catalogued. */
  uiDir?: string;
}

const KINDS: readonly EntryKind[] = ["primitive", "composition", "surface"];
const STATUSES: readonly EntryStatus[] = ["available", "proposed", "exception"];
const ID_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const UI_DIR = "src/components/ui";

function parse(file: string): ts.SourceFile {
  const kind = file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  return ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true, kind);
}

function hasExportModifier(node: ts.Node): boolean {
  return (
    ts.canHaveModifiers(node) &&
    (ts.getModifiers(node) ?? []).some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword)
  );
}

function hasDefaultModifier(node: ts.Node): boolean {
  return (
    ts.canHaveModifiers(node) &&
    (ts.getModifiers(node) ?? []).some((modifier) => modifier.kind === ts.SyntaxKind.DefaultKeyword)
  );
}

/**
 * Every name a module exports: declarations, export lists and aliases
 * (`export { A as B }`). A default export is reported as `default`.
 */
export function readExports(file: string): Set<string> {
  const names = new Set<string>();
  for (const statement of parse(file).statements) {
    if (ts.isExportDeclaration(statement) && statement.exportClause && ts.isNamedExports(statement.exportClause)) {
      for (const element of statement.exportClause.elements) names.add(element.name.text);
      continue;
    }
    if (ts.isExportAssignment(statement) && !statement.isExportEquals) {
      names.add("default");
      continue;
    }
    if (!hasExportModifier(statement)) continue;
    if (hasDefaultModifier(statement)) {
      names.add("default");
      continue;
    }
    if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) {
        if (ts.isIdentifier(declaration.name)) names.add(declaration.name.text);
      }
    } else if (
      (ts.isFunctionDeclaration(statement) ||
        ts.isClassDeclaration(statement) ||
        ts.isInterfaceDeclaration(statement) ||
        ts.isTypeAliasDeclaration(statement) ||
        ts.isEnumDeclaration(statement)) &&
      statement.name
    ) {
      names.add(statement.name.text);
    }
  }
  return names;
}

function propertyName(name: ts.PropertyName): string | null {
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) return name.text;
  return null;
}

function objectProperty(object: ts.ObjectLiteralExpression, key: string): ts.Expression | null {
  for (const property of object.properties) {
    if (ts.isPropertyAssignment(property) && propertyName(property.name) === key) return property.initializer;
  }
  return null;
}

/** The `variants` of every `cva(base, { variants })` call in a module, merged by variant name. */
export function readCvaVariants(file: string): Record<string, string[]> {
  const variants: Record<string, string[]> = {};
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "cva") {
      const config = node.arguments[1];
      if (config && ts.isObjectLiteralExpression(config)) {
        const map = objectProperty(config, "variants");
        if (map && ts.isObjectLiteralExpression(map)) {
          for (const property of map.properties) {
            if (!ts.isPropertyAssignment(property) || !ts.isObjectLiteralExpression(property.initializer)) continue;
            const key = propertyName(property.name);
            if (key === null) continue;
            const values = property.initializer.properties
              .map((value) => (value.name ? propertyName(value.name as ts.PropertyName) : null))
              .filter((value): value is string => value !== null);
            variants[key] = [...new Set([...(variants[key] ?? []), ...values])];
          }
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(parse(file));
  return variants;
}

function sameMembers(left: readonly string[], right: readonly string[]): boolean {
  const a = [...new Set(left)].sort();
  const b = [...new Set(right)].sort();
  return a.length === b.length && a.every((value, index) => value === b[index]);
}

function nonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

/** The catalogued modules of an entry: its source and any supplementary files. */
function entryFiles(entry: CatalogEntry): string[] {
  return entry.source === null ? [] : [entry.source, ...(entry.files ?? [])];
}

/** Export and variant failures of one entry against the modules that implement it. */
function checkModule(at: string, files: readonly string[], entry: CatalogEntry, label: string): string[] {
  const errors: string[] = [];
  const exported = new Set(files.flatMap((file) => [...readExports(file)]));
  for (const name of entry.exports) {
    if (!exported.has(name)) errors.push(`${at}: ${name} is not exported by ${label}`);
  }
  const cva: Record<string, string[]> = {};
  for (const file of files) {
    for (const [key, values] of Object.entries(readCvaVariants(file))) {
      cva[key] = [...new Set([...(cva[key] ?? []), ...values])];
    }
  }
  const declared = entry.variants ?? {};
  for (const [key, values] of Object.entries(cva)) {
    const listed = declared[key];
    if (!listed) errors.push(`${at}: variant ${key} (${values.join(", ")}) is defined by cva but not catalogued`);
    else if (!sameMembers(listed, values)) {
      errors.push(`${at}: variant ${key} lists ${listed.join(", ")} but cva defines ${values.join(", ")}`);
    }
  }
  // Prop-defined variants (string unions) are not cva; each value must at least appear in source.
  const text = files.map((file) => readFileSync(file, "utf8")).join("\n");
  for (const [key, values] of Object.entries(declared)) {
    if (key in cva) continue;
    for (const value of values) {
      if (!text.includes(`"${value}"`)) errors.push(`${at}: variant ${key}=${value} does not appear in ${label}`);
    }
  }
  return errors;
}

/** Every validation failure in the catalog; an empty list means the catalog is sound. */
export function validateCatalog(catalog: Catalog, options: ValidationOptions): string[] {
  const errors: string[] = [];
  const { clientRoot } = options;
  if (catalog.schema !== 1) errors.push(`catalog: unsupported schema ${String(catalog.schema)}`);
  if (!Array.isArray(catalog.entries)) return [...errors, "catalog: entries must be an array"];

  const ids = new Set<string>();
  const headings = new Set(catalog.entries.map((entry) => anchor(entry.group)));
  const names = new Set<string>();
  for (const entry of catalog.entries) {
    if (ids.has(entry.id)) errors.push(`${entry.id}: duplicate id`);
    ids.add(entry.id);
    // Names become headings in components.md; their anchors must be unique.
    const name = anchor(entry.name ?? "");
    if (names.has(name) || headings.has(name)) errors.push(`${entry.id}: name ${entry.name} repeats a heading`);
    names.add(name);
  }

  for (const entry of catalog.entries) {
    const at = entry.id;
    if (!ID_PATTERN.test(entry.id)) errors.push(`${at}: id must be kebab-case`);
    if (!nonEmptyString(entry.name)) errors.push(`${at}: name is required`);
    if (!KINDS.includes(entry.kind)) errors.push(`${at}: unknown kind ${String(entry.kind)}`);
    if (!STATUSES.includes(entry.status)) errors.push(`${at}: unknown status ${String(entry.status)}`);
    if (!nonEmptyString(entry.group)) errors.push(`${at}: group is required`);
    for (const field of ["keyboard", "use", "avoid"] as const) {
      if (!nonEmptyString(entry[field])) errors.push(`${at}: ${field} is required`);
    }
    if (!Array.isArray(entry.states)) errors.push(`${at}: states must be an array`);
    if (!Array.isArray(entry.exports)) errors.push(`${at}: exports must be an array`);
    for (const related of entry.related ?? []) {
      if (!catalog.entries.some((other) => other.id === related)) errors.push(`${at}: unknown related id ${related}`);
    }

    if (entry.status === "proposed") {
      if (entry.source !== null) errors.push(`${at}: a proposed entry has no source`);
      continue;
    }

    if (entry.source === null) {
      errors.push(`${at}: an ${entry.status} entry needs a source`);
      continue;
    }
    const missing = entryFiles(entry).filter((file) => !existsSync(join(clientRoot, file)));
    for (const file of missing) errors.push(`${at}: source ${file} does not exist`);
    if (missing.length > 0) continue;

    const files = entryFiles(entry).map((file) => join(clientRoot, file));
    errors.push(...checkModule(at, files, entry, entryFiles(entry).join(", ")));

  }

  const uiDir = join(clientRoot, options.uiDir ?? UI_DIR);
  if (existsSync(uiDir)) {
    const covered = new Set(catalog.entries.flatMap((entry) => entryFiles(entry)));
    for (const name of readdirSync(uiDir).sort()) {
      if (!/\.tsx?$/.test(name) || /\.test\.tsx?$/.test(name)) continue;
      const file = relative(clientRoot, join(uiDir, name)).split("\\").join("/");
      if (!covered.has(file)) errors.push(`catalog: ${file} is not covered by an entry`);
    }
  }
  return errors;
}

// Documentation ----------------------------------------------------------------

const KIND_LABEL: Record<EntryKind, string> = {
  primitive: "Primitive",
  composition: "Composition",
  surface: "Surface",
};
/** The group order of the catalog: first appearance wins. */
function groups(catalog: Catalog): string[] {
  return [...new Set(catalog.entries.map((entry) => entry.group))];
}

function code(value: string): string {
  return `\`${value}\``;
}

function sourceLink(path: string): string {
  return `[${path}](../../web/conversation/${path})`;
}

function importPath(source: string): string {
  return `~/${source.replace(/^src\//, "").replace(/\.tsx?$/, "")}`;
}

function importStatement(source: string, exports: readonly string[]): string {
  const named = exports.filter((name) => name !== "default");
  const parts: string[] = [];
  if (exports.includes("default")) parts.push(basename(source).replace(/\.tsx?$/, ""));
  if (named.length > 0) parts.push(`{ ${named.join(", ")} }`);
  return parts.length > 0 ? `import ${parts.join(", ")} from "${importPath(source)}";` : `import "${importPath(source)}";`;
}

function tableCell(value: string): string {
  return value.replace(/\|/g, "\\|").replace(/\n/g, " ");
}

function counts(catalog: Catalog): string {
  const by = <K extends keyof CatalogEntry>(key: K, values: readonly string[]) =>
    values
      .map((value) => `${catalog.entries.filter((entry) => entry[key] === value).length} ${value}`)
      .join(", ");
  return [
    `${catalog.entries.length} entries`,
    `by kind: ${by("kind", KINDS)}`,
    `by status: ${by("status", STATUSES)}`,
  ].join("; ");
}

const GENERATED_NOTE =
  "This document is generated from [`web/conversation/src/design-system/catalog.json`](../../web/conversation/src/design-system/catalog.json) by `npm run design:catalog` in `web/conversation`. Edit the catalog, not this file; `npm run design:catalog:check` fails when the two disagree.";

export function renderComponentsDoc(catalog: Catalog): string {
  const lines: string[] = [
    "# Component contracts",
    "",
    GENERATED_NOTE,
    "",
    "Each contract states what a component is for, how to import it, the variants its source defines, the interaction states it owns, its keyboard behaviour and what to use instead. The source file is the owner of the component's appearance and behaviour; features compose it and do not restyle it. A proposed entry names a component Detent does not have yet; listing one does not authorize building it.",
    "",
    `Catalog totals: ${counts(catalog)}.`,
    "",
  ];
  for (const group of groups(catalog)) {
    lines.push(`## ${group}`, "");
    for (const entry of catalog.entries.filter((item) => item.group === group)) {
      lines.push(`### ${entry.name}`, "");
      lines.push(entry.use, "");
      const facts: string[] = [];
      facts.push(`- Kind: ${KIND_LABEL[entry.kind]}; status: ${entry.status}; id: ${code(entry.id)}.`);
      if (entry.source !== null) {
        facts.push(`- Import: ${code(importStatement(entry.source, entry.exports))}`);
        const others = (entry.files ?? []).map(sourceLink);
        facts.push(`- Source: ${sourceLink(entry.source)}${others.length > 0 ? `, with ${others.join(", ")}` : ""}.`);
      } else {
        facts.push(`- Planned exports: ${entry.exports.map(code).join(", ") || "none listed"}.`);
      }
      const variants = Object.entries(entry.variants);
      facts.push(
        variants.length > 0
          ? `- Variants: ${variants.map(([key, values]) => `${code(key)}: ${values.map(code).join(", ")}`).join("; ")}.`
          : "- Variants: none.",
      );
      facts.push(`- States: ${entry.states.length > 0 ? entry.states.join(", ") : "none of its own"}.`);
      facts.push(`- Keyboard: ${entry.keyboard}`);
      facts.push(`- Avoid: ${entry.avoid}`);
      if (entry.related.length > 0) {
        const names = entry.related.map((id) => {
          const other = catalog.entries.find((item) => item.id === id);
          return other ? `[${other.name}](#${anchor(other.name)})` : code(id);
        });
        facts.push(`- Related: ${names.join(", ")}.`);
      }
      lines.push(...facts, "");
    }
  }
  return `${lines.join("\n").trimEnd()}\n`;
}

/** GitHub's heading anchor for a heading text. */
export function anchor(heading: string): string {
  return heading
    .toLowerCase()
    .replace(/[^a-z0-9 _-]/g, "")
    .replace(/ /g, "-");
}

export function renderElementCatalogDoc(catalog: Catalog): string {
  const lines: string[] = [
    "# Element catalog",
    "",
    GENERATED_NOTE,
    "",
    "One row per catalogued element. The [component contracts](components.md) hold the full contract for each. Status *available* means the component ships in Detent; *proposed* means it is planned and has no source yet; *exception* means a deliberate, documented departure from the shared contract.",
    "",
    `Totals: ${counts(catalog)}.`,
    "",
    "| Element | Group | Kind | Status | Source |",
    "| --- | --- | --- | --- | --- |",
  ];
  for (const group of groups(catalog)) {
    for (const entry of catalog.entries.filter((item) => item.group === group)) {
      lines.push(
        `| [${tableCell(entry.name)}](components.md#${anchor(entry.name)}) | ${tableCell(entry.group)} | ${entry.kind} | ${entry.status} | ${entry.source === null ? "—" : code(entry.source)} |`,
      );
    }
  }
  return `${lines.join("\n")}\n`;
}

// Command line -------------------------------------------------------------------

export const CLIENT_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
export const CATALOG_PATH = join(CLIENT_ROOT, "src/design-system/catalog.json");
export const DOCS_DIR = resolve(CLIENT_ROOT, "../../docs/design-system");

export function loadCatalog(path = CATALOG_PATH): Catalog {
  return JSON.parse(readFileSync(path, "utf8")) as Catalog;
}

/** The generated documents, keyed by file name within `docs/design-system`. */
export function renderDocs(catalog: Catalog): Map<string, string> {
  return new Map([
    ["components.md", renderComponentsDoc(catalog)],
    ["element-catalog.md", renderElementCatalogDoc(catalog)],
  ]);
}

/** Names of generated documents whose committed content differs from the catalog. */
export function staleDocs(catalog: Catalog, docsDir = DOCS_DIR): string[] {
  const stale: string[] = [];
  for (const [name, content] of renderDocs(catalog)) {
    const target = join(docsDir, name);
    if (!existsSync(target) || readFileSync(target, "utf8") !== content) stale.push(name);
  }
  return stale;
}

function main(argv: readonly string[]): number {
  const check = argv.includes("--check");
  const catalog = loadCatalog();
  const errors = validateCatalog(catalog, { clientRoot: CLIENT_ROOT });
  if (errors.length > 0) {
    for (const error of errors) console.error(`design-catalog: ${error}`);
    console.error(`design-catalog: ${errors.length} validation error(s)`);
    return 1;
  }
  if (check) {
    const stale = staleDocs(catalog);
    if (stale.length > 0) {
      console.error(`design-catalog: stale generated docs: ${stale.join(", ")}; run npm run design:catalog`);
      return 1;
    }
    console.log(`design-catalog: verified ${counts(catalog)}`);
    return 0;
  }
  for (const [name, content] of renderDocs(catalog)) writeFileSync(join(DOCS_DIR, name), content);
  console.log(`design-catalog: wrote ${[...renderDocs(catalog).keys()].join(", ")}; ${counts(catalog)}`);
  return 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  process.exitCode = main(process.argv.slice(2));
}
