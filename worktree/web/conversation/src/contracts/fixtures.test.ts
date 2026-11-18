// Every fixture decodes through its schema, and every fixture file is bound
// to one. The Go implementation reads the same directory, so an unmapped file
// here means the two sides have drifted.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import * as Schema from "effect/Schema";
import { describe, expect, it } from "vitest";

import { BoardViewWire, decodeBoardView } from "../app/work/lib/boardSchema.ts";
import { FIXTURE_SCHEMAS } from "./fixtures.ts";
import { OrganizationReports } from "./reports.ts";
import { HistoryPage, NativeBoardCard, WorkItemPage } from "./work.ts";

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
    const boards: string[] = [];
    for (const organization of ["org_first", "org_first_second"]) {
      for (const role of ["member", "reporter"]) {
        for (const read of ["", "-all", "-delta"]) boards.push(`work-board-${organization}-${role}${read}.json`);
      }
    }
    const payloads = readdirSync(boardDirectory).filter((name) => name.endsWith(".json")).toSorted();
    expect(payloads, "PostgreSQL board handler fixtures are missing or unexpected").toEqual(
      [...expected.flatMap((name) => [name, `operator-${name}`]), ...boards].toSorted(),
    );
    const kinds = new Set<string>();
    for (const name of payloads) {
      const raw: unknown = JSON.parse(readFileSync(join(boardDirectory, name), "utf8"));
      if (!boards.includes(name)) {
        expect(() => Schema.decodeUnknownSync(WorkItemPage)(raw), name).not.toThrow();
        continue;
      }
      const wire = Schema.decodeUnknownSync(BoardViewWire)(raw);
      expect(wire.cards.length, name).toBeGreaterThan(0);
      for (const card of wire.cards) expect(() => Schema.decodeUnknownSync(NativeBoardCard)(card), name).not.toThrow();
      const view = decodeBoardView(wire);
      expect(view.cards.length, name).toBe(wire.cards.length);
      for (const card of view.cards) kinds.add(card.issue.actor.kind);
    }
    expect([...kinds].toSorted()).toEqual(["human", "integration", "machine", "runner"]);
  });

  it("binds every fixture file to a schema", () => {
    expect(files).toEqual(Object.keys(FIXTURE_SCHEMAS).toSorted());
  });

  it.each(files)("decodes %s", (name) => {
    const schema = FIXTURE_SCHEMAS[name];
    expect(schema, `no schema is bound to ${name}`).toBeDefined();
    const raw: unknown = JSON.parse(readFileSync(join(directory, name), "utf8"));
    expect(() => Schema.decodeUnknownSync(schema!)(raw)).not.toThrow();
    if (name === "organizationReports.json") {
      const partial = JSON.parse(JSON.stringify(raw));
      partial.unavailable = ["rework_timeline_unavailable"];
      partial.current.worked = null;
      partial.current.reworked = null;
      partial.current.rework_percent = null;
      partial.projects[0].current.worked = null;
      partial.projects[0].current.reworked = null;
      partial.projects[0].current.rework_percent = null;
      const decoded = Schema.decodeUnknownSync(OrganizationReports)(partial);
      expect(decoded.current.worked).toBeNull();
      expect(decoded.current.reworked).toBeNull();
      expect(decoded.projects[0]!.current.worked).toBeNull();
      expect(decoded.projects[0]!.current.reworked).toBeNull();
      expect(decoded.projects[1]!.current.worked).toBe(40);
      expect(decoded.projects[1]!.current.reworked).toBe(3);
      expect(decoded.previous.worked).toBe(0);
    }
  });

  it("rejects an actor kind the hub never writes", () => {
    expect(() =>
      Schema.decodeUnknownSync(FIXTURE_SCHEMAS["native-actors.json"]!)([
        { kind: "robot", principal_id: "example" },
      ]),
    ).toThrow();
  });

  it("decodes a work item and a work list filed by the progress-check machine actor", () => {
    const actor = { kind: "machine", principal_id: "runner_example", display_name: "runner_example", agent: "progress-check" };
    const item = JSON.parse(readFileSync(join(directory, "work-item.json"), "utf8"));
    item.actor = actor;
    expect(Schema.decodeUnknownSync(FIXTURE_SCHEMAS["work-item.json"]!)(item)).toMatchObject({ actor });
    const list = JSON.parse(readFileSync(join(directory, "work-item-list.json"), "utf8"));
    list.items[1].actor = actor;
    expect(Schema.decodeUnknownSync(WorkItemPage)(list).items[1]).toMatchObject({ actor });
  });

  it("decodes history closed by the workspace service system actor", () => {
    const actor = { kind: "system", principal_id: "workspace_service" };
    const history = JSON.parse(readFileSync(join(directory, "work-history.json"), "utf8"));
    history.items[0].actor = actor;
    expect(Schema.decodeUnknownSync(HistoryPage)(history).items[0]).toMatchObject({ actor });
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
