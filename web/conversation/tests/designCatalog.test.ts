// The design catalog validator and its generated docs.
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  type Catalog,
  type CatalogEntry,
  CLIENT_ROOT,
  loadCatalog,
  staleDocs,
  validateCatalog,
} from "../scripts/design-catalog.ts";

const BUTTON = `import { cva } from "class-variance-authority";
const buttonVariants = cva("base", {
  variants: {
    variant: { default: "a", ghost: "b" },
    size: { default: "c", sm: "d" },
  },
});
function Button() {
  return null;
}
export { Button, buttonVariants };
`;

const LABEL = `export function Label() {
  return null;
}
`;

let root = "";
let client = "";

function write(path: string, text: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, text);
}

beforeAll(() => {
  root = mkdtempSync(join(tmpdir(), "design-catalog-"));
  client = join(root, "client");
  write(join(client, "src/components/ui/button.tsx"), BUTTON);
  write(join(client, "src/components/ui/label.tsx"), LABEL);
});

afterAll(() => {
  rmSync(root, { recursive: true, force: true });
});

function entry(overrides: Partial<CatalogEntry>): CatalogEntry {
  return {
    id: "button",
    name: "Button",
    kind: "primitive",
    group: "Actions",
    status: "available",
    source: "src/components/ui/button.tsx",
    exports: ["Button", "buttonVariants"],
    variants: { variant: ["default", "ghost"], size: ["default", "sm"] },
    states: ["hover"],
    keyboard: "Enter activates.",
    use: "Actions.",
    avoid: "Links.",
    related: [],
    ...overrides,
  };
}

const LABEL_ENTRY = entry({
  id: "label",
  name: "Label",
  group: "Forms",
  source: "src/components/ui/label.tsx",
  exports: ["Label"],
  variants: {},
});

function catalog(...entries: CatalogEntry[]): Catalog {
  return { schema: 1, entries };
}

describe("validateCatalog", () => {
  const cases: Array<{ name: string; catalog: () => Catalog; expected: string[] }> = [
    {
      name: "a sound catalog",
      catalog: () => catalog(entry({}), LABEL_ENTRY),
      expected: [],
    },
    {
      name: "duplicate id",
      catalog: () => catalog(entry({}), entry({ name: "Second button" }), LABEL_ENTRY),
      expected: ["button: duplicate id"],
    },
    {
      name: "missing source",
      catalog: () => catalog(entry({ source: "src/components/ui/missing.tsx" }), entry({ id: "b2", name: "B2" }), LABEL_ENTRY),
      expected: ["button: source src/components/ui/missing.tsx does not exist"],
    },
    {
      name: "missing export",
      catalog: () => catalog(entry({ exports: ["Button", "ButtonGroup"] }), LABEL_ENTRY),
      expected: ["button: ButtonGroup is not exported by src/components/ui/button.tsx"],
    },
    {
      name: "variant mismatch",
      catalog: () => catalog(entry({ variants: { variant: ["default", "outline"], size: ["default", "sm"] } }), LABEL_ENTRY),
      expected: ["button: variant variant lists default, outline but cva defines default, ghost"],
    },
    {
      name: "uncatalogued cva variant",
      catalog: () => catalog(entry({ variants: { variant: ["default", "ghost"] } }), LABEL_ENTRY),
      expected: ["button: variant size (default, sm) is defined by cva but not catalogued"],
    },
    {
      name: "uncovered ui file",
      catalog: () => catalog(entry({})),
      expected: ["catalog: src/components/ui/label.tsx is not covered by an entry"],
    },
    {
      name: "a proposed entry with a source",
      catalog: () => catalog(entry({}), LABEL_ENTRY, entry({ id: "radio", name: "Radio", status: "proposed" })),
      expected: ["radio: a proposed entry has no source"],
    },
    {
      name: "an unknown related id",
      catalog: () => catalog(entry({ related: ["missing"] }), LABEL_ENTRY),
      expected: ["button: unknown related id missing"],
    },
  ];

  for (const testCase of cases) {
    it(`reports ${testCase.name}`, () => {
      expect(validateCatalog(testCase.catalog(), { clientRoot: client })).toEqual(
        testCase.expected,
      );
    });
  }

});

describe("the committed catalog", () => {
  it("validates against the client source", () => {
    expect(validateCatalog(loadCatalog(), { clientRoot: CLIENT_ROOT })).toEqual([]);
  });

  it("matches the generated docs", () => {
    expect(staleDocs(loadCatalog())).toEqual([]);
  });
});
