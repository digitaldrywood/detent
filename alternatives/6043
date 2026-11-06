import type { ServerResponse } from "node:http";

import { createWorkMock } from "../dev/mock-work.ts";
import fleet from "../src/contracts/fixtures/account-fleet-empty.json";
import type { ConversationClient } from "../src/runtime/bootstrap.ts";
import type { FetchLike } from "../src/app/work/lib/workHttp.ts";

export function workPaginationFixture() {
  const projects = [{ id: "proj_alpha", name: "alpha" }, { id: "proj_beta", name: "beta" }];
  const apiBase = "/api/v2/organizations/org_mock";
  const work = createWorkMock({ apiBase, organizationId: "org_mock", projects });
  const requests: { url: URL; signal: AbortSignal | undefined }[] = [];
  let hold: { started: () => void; wait: Promise<void> } | undefined;
  const fetch: FetchLike = async (input, init) => {
    const url = new URL(input, "http://fixture.test");
    if (url.pathname.endsWith("/fleet")) return Response.json(fleet);
    requests.push({ url, signal: init?.signal ?? undefined });
    if (hold !== undefined && url.pathname.endsWith("/work-items") && url.searchParams.has("cursor")) {
      const deferred = hold;
      hold = undefined;
      deferred.started();
      await deferred.wait;
    }
    let status = 200;
    let body = "";
    const response = {
      writeHead: (value: number) => { status = value; },
      end: (value: string) => { body = value; },
    } as unknown as ServerResponse;
    const handled = await work.handle({
      url, response, method: init?.method ?? "GET",
      readBody: async () => JSON.parse(String(init?.body ?? "{}")),
    });
    return handled ? new Response(body, { status }) : Response.json({ code: "not_found", message: "Unauthorized project" }, { status: 404 });
  };
  const control = async (body: Record<string, unknown> = {}) => {
    await fetch("/__mock/work/pagination", { method: "POST", body: JSON.stringify(body) });
  };
  const deferPage = () => {
    let release!: () => void;
    let started!: () => void;
    const waiting = new Promise<void>((resolve) => { started = resolve; });
    const wait = new Promise<void>((resolve) => { release = resolve; });
    hold = { started, wait };
    return { waiting, release };
  };
  const client = {
    bootstrap: { projects, actor: { principal_id: "fixture-principal" }, organization: { id: "org_mock", name: "Fixture" } },
    http: { origin: "", apiBase, csrfToken: "fixture-csrf" },
  } as unknown as ConversationClient;
  return { client, fetch, control, requests, deferPage };
}
