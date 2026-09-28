import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import * as Schema from "effect/Schema";
import { describe, expect, it } from "vitest";

import { PLATFORM_FIXTURE_SCHEMAS } from "./platformFixtures.ts";

const directory = fileURLToPath(new URL("./fixtures/platform", import.meta.url));
const files = readdirSync(directory)
  .filter((name) => name.endsWith(".json"))
  .toSorted();

describe("platform fixtures", () => {
  it("binds every fixture file to a schema", () => {
    expect(files).toEqual(Object.keys(PLATFORM_FIXTURE_SCHEMAS).toSorted());
  });

  it.each(files)("decodes %s", (name) => {
    const raw: unknown = JSON.parse(readFileSync(join(directory, name), "utf8"));
    expect(() => Schema.decodeUnknownSync(PLATFORM_FIXTURE_SCHEMAS[name]!)(raw)).not.toThrow();
  });
});
