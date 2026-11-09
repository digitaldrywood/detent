// Every fixture decodes through its schema, and every fixture file is bound
// to one. The Go implementation reads the same directory, so an unmapped file
// here means the two sides have drifted.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import * as Schema from "effect/Schema";
import { describe, expect, it } from "vitest";

import { FIXTURE_SCHEMAS } from "./fixtures.ts";
import { WorkItemPage } from "./work.ts";

const directory = fileURLToPath(new URL("./fixtures", import.meta.url));
const files = readdirSync(directory)
  .filter((name) => name.endsWith(".json"))
  .toSorted();

describe("contract fixtures", () => {
  const boardDirectory = process.env.DETENT_TEST_BOARD_CONTRACT_DIR;
  it("decodes PostgreSQL board handler payloads", () => {
    if (!boardDirectory) {
      throw new Error("DETENT_TEST_BOARD_CONTRACT_DIR is required; run make app-test or make check-app");
    }
    const expected: string[] = [];
    for (const organization of ["org_first", "org_first_second"]) {
      for (const include of ["board", "work", "summary", ""]) {
        for (const role of ["", "reporter"]) {
          for (const limit of [1, 10, 200]) {
            expected.push(`${organization}-${role}-${include}-${limit}-.json`);
          }
        }
        expected.push(`${organization}--${include}--Backlog.json`);
      }
    }
    const payloads = readdirSync(boardDirectory).filter((name) => name.endsWith(".json")).toSorted();
    expect(payloads, "PostgreSQL board handler fixtures are missing or unexpected").toEqual(
      expected.flatMap((name) => [name, `operator-${name}`]).toSorted(),
    );
    for (const name of payloads) {
      const raw: unknown = JSON.parse(readFileSync(join(boardDirectory, name), "utf8"));
      expect(() => Schema.decodeUnknownSync(WorkItemPage)(raw), name).not.toThrow();
    }
  });

  it("binds every fixture file to a schema", () => {
    expect(files).toEqual(Object.keys(FIXTURE_SCHEMAS).toSorted());
  });

  it.each(files)("decodes %s", (name) => {
    const schema = FIXTURE_SCHEMAS[name];
    expect(schema, `no schema is bound to ${name}`).toBeDefined();
    const raw: unknown = JSON.parse(readFileSync(join(directory, name), "utf8"));
    expect(() => Schema.decodeUnknownSync(schema!)(raw)).not.toThrow();
  });

  it("rejects the legacy health detector actor kind", () => {
    expect(() =>
      Schema.decodeUnknownSync(FIXTURE_SCHEMAS["native-actors.json"]!)([
        { kind: "system", principal_id: "health_detector" },
      ]),
    ).toThrow();
  });

  it("decodes a Change review with observed attachment evidence", () => {
    const raw = JSON.parse(readFileSync(join(directory, "work-change-detail.json"), "utf8"));
    raw.reviews[0].validator = {
      criteria_evidence: [{ criterion: "Page is visible", kind: "attachment", reference: "page.png", behavior: "The capture shows the requested page" }],
    };
    expect(() => Schema.decodeUnknownSync(FIXTURE_SCHEMAS["work-change-detail.json"]!)(raw)).not.toThrow();
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
