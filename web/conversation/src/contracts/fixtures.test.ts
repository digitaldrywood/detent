// Every fixture decodes through its schema, and every fixture file is bound
// to one. The Go implementation reads the same directory, so an unmapped file
// here means the two sides have drifted.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import * as Schema from "effect/Schema";
import { describe, expect, it } from "vitest";

import { FIXTURE_SCHEMAS } from "./fixtures.ts";

const directory = fileURLToPath(new URL("./fixtures", import.meta.url));
const files = readdirSync(directory)
  .filter((name) => name.endsWith(".json"))
  .toSorted();

describe("contract fixtures", () => {
  it("binds every fixture file to a schema", () => {
    expect(files).toEqual(Object.keys(FIXTURE_SCHEMAS).toSorted());
  });

  it.each(files)("decodes %s", (name) => {
    const schema = FIXTURE_SCHEMAS[name];
    expect(schema, `no schema is bound to ${name}`).toBeDefined();
    const raw: unknown = JSON.parse(readFileSync(join(directory, name), "utf8"));
    expect(() => Schema.decodeUnknownSync(schema!)(raw)).not.toThrow();
  });

  it("rejects a payload with an unknown enumeration member", () => {
    const raw = JSON.parse(
      readFileSync(join(directory, "conversation.json"), "utf8"),
    ) as Record<string, unknown>;
    expect(() =>
      Schema.decodeUnknownSync(FIXTURE_SCHEMAS["conversation.json"]!)({
        ...raw,
        visibility: "public",
      }),
    ).toThrow();
  });
});
