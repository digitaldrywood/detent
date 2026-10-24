import { describe, expect, it } from "vitest";
import fixture from "../src/contracts/fixtures/work-item.json";
import { makeWorkHttp, WorkApiError } from "../src/app/work/lib/workHttp.ts";

function client(response: Response, calls: { url: string; init: RequestInit | undefined }[]) {
  return makeWorkHttp({ origin: "http://hub.test", apiBase: "/api/v2/organizations/org", csrfToken: "csrf",
    fetch: async (url, init) => { calls.push({ url, init }); return response; },
  });
}

describe("native issue archive", () => {
  it.each([true, false])("sends a revision and idempotency key for archived=%s", async (archived) => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    const http = client(new Response(JSON.stringify({ ...fixture, archived }), { status: 200 }), calls);
    const issue = await http.setArchived({ projectId: "p", itemId: "i", expectedRevision: "9", key: "archive-command", archived });
    expect(issue.archived).toBe(archived);
    expect(calls[0]?.url).toBe(`http://hub.test/api/v2/organizations/org/projects/p/work-items/i/${archived ? "archive" : "restore"}`);
    expect(calls[0]?.init?.method).toBe("POST");
    expect(JSON.parse(calls[0]?.init?.body as string)).toEqual({ expected_revision: "9", idempotency_key: "archive-command" });
  });

  it("fetches archived lists explicitly", async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    const http = client(new Response(JSON.stringify({ items: [] }), { status: 200 }), calls);
    await http.listWorkItems({ projectId: "p", archived: true, state: "Done" });
    const url = new URL(calls[0]!.url);
    expect(url.searchParams.get("archived")).toBe("true");
    expect(url.searchParams.get("state")).toBe("Done");
  });

  it("retains the contextual count when restore reaches the allowance", async () => {
    const http = client(new Response(JSON.stringify({ code: "allowance_exhausted", message: "Hosted unarchived_issues allowance reached (200 of 200). Existing data, reading, export and billing remain available.", resource: "unarchived_issues", allowance: 200, consumption: 200 }), { status: 429 }), []);
    try {
      await http.setArchived({ projectId: "p", itemId: "i", expectedRevision: "9", key: "restore", archived: false });
      throw new Error("Expected quota refusal");
    } catch (error) {
      expect(error).toBeInstanceOf(WorkApiError);
      expect((error as WorkApiError).message).toContain("200 of 200");
      expect((error as WorkApiError).status).toBe(429);
    }
  });
});
