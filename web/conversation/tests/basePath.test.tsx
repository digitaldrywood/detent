// @vitest-environment jsdom
import { createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it } from "vitest";

import { makeAccountApi } from "../src/app/account/api.ts";
import { checkForUpdates, resetUpdateCheckState } from "../src/app/adapters/detentUpdates.ts";
import { makeRouter } from "../src/app/router.tsx";
import bootstrapFixture from "../src/contracts/fixtures/bootstrap.json";
import {
  applyHubPaths,
  basePath,
  hubPath,
  normalizeBasePath,
  readBasePath,
  resetHubPaths,
  routerBasePath,
  signInPath,
  withoutBasePath,
} from "../src/runtime/basePath.ts";
import { loadBootstrap } from "../src/runtime/bootstrap.ts";

const SHARED = "/organizations/org_a";

function setShellMeta(base: string | null, signIn: string | null): void {
  document.head.innerHTML = "";
  for (const [name, content] of [
    ["detent-base-path", base],
    ["detent-sign-in-path", signIn],
  ] as const) {
    if (content === null) continue;
    const meta = document.createElement("meta");
    meta.setAttribute("name", name);
    meta.setAttribute("content", content);
    document.head.append(meta);
  }
  resetHubPaths();
}

function recordingFetch(respond: (url: string) => Response = () => new Response(null, { status: 404 })) {
  const urls: string[] = [];
  const fetchImpl = (async (input: RequestInfo | URL) => {
    const url = String(input);
    urls.push(url);
    return respond(url);
  }) as typeof globalThis.fetch;
  return { urls, fetchImpl };
}

afterEach(() => {
  document.head.innerHTML = "";
  resetHubPaths();
  resetUpdateCheckState();
});

describe("normalizeBasePath and readBasePath", () => {
  it.each([
    [null, ""],
    ["", ""],
    ["/", ""],
    [SHARED, SHARED],
    [`${SHARED}/`, SHARED],
    ["organizations/org_a", ""],
    ["//evil.example", ""],
  ])("%s normalizes to %s", (value, want) => {
    expect(normalizeBasePath(value)).toBe(want);
  });

  it("reads the shell meta tag and treats its absence as the origin root", () => {
    setShellMeta(SHARED, "/organizations");
    expect(readBasePath()).toBe(SHARED);
    expect(basePath()).toBe(SHARED);
    setShellMeta(null, null);
    expect(basePath()).toBe("");
    expect(readBasePath(undefined)).toBe("");
  });
});

describe("hubPath", () => {
  it.each([
    ["", "/app/bootstrap", "/app/bootstrap"],
    ["", "/logout", "/logout"],
    [SHARED, "/app/bootstrap", `${SHARED}/app/bootstrap`],
    [SHARED, "/app/updates", `${SHARED}/app/updates`],
    [SHARED, "/logout", `${SHARED}/logout`],
    [SHARED, "/chat/issues/wi_1", `${SHARED}/chat/issues/wi_1`],
    [SHARED, "/invite?token=x", `${SHARED}/invite?token=x`],
    [SHARED, `${SHARED}/chat`, `${SHARED}/chat`],
    [SHARED, SHARED, SHARED],
    [SHARED, "https://github.com/a/b/pull/1", "https://github.com/a/b/pull/1"],
    [SHARED, "//cdn.example/x", "//cdn.example/x"],
    [SHARED, "chat", "chat"],
  ])("base %j builds %s as %s", (base, path, want) => {
    expect(hubPath(path, base)).toBe(want);
  });

  it.each([
    ["", "/login", "/login"],
    [SHARED, `${SHARED}/support/org_7`, "/support/org_7"],
    [SHARED, SHARED, "/"],
    [SHARED, "/organizations/org_ab/chat", "/organizations/org_ab/chat"],
  ])("base %j strips %s to %s", (base, pathname, want) => {
    expect(withoutBasePath(pathname, base)).toBe(want);
  });

  it("gives the router `/` at the root and the base otherwise", () => {
    expect(routerBasePath("")).toBe("/");
    expect(routerBasePath(SHARED)).toBe(SHARED);
  });
});

describe("sign-in path", () => {
  it.each([
    [null, null, "/login"],
    ["", "/login", "/login"],
    [SHARED, "/organizations", "/organizations"],
    [SHARED, null, `${SHARED}/login`],
  ])("base %j and meta %j sign in at %s", (base, meta, want) => {
    setShellMeta(base, meta);
    expect(signInPath()).toBe(want);
  });

  it("follows the bootstrap over the shell", () => {
    setShellMeta("", "/login");
    applyHubPaths({ base_path: SHARED, sign_in_path: "/organizations" });
    expect(basePath()).toBe(SHARED);
    expect(signInPath()).toBe("/organizations");
  });
});

describe("bootstrap under a base", () => {
  it.each(["", SHARED])("base %j asks for the bootstrap under it and keeps the payload's", async (base) => {
    setShellMeta(base, base === "" ? "/login" : "/organizations");
    const { urls, fetchImpl } = recordingFetch((url) =>
      url.endsWith("/app/bootstrap")
        ? new Response(null, { status: 404 })
        : Response.json({ ...bootstrapFixture, base_path: base, sign_in_path: "/elsewhere" }),
    );
    const bootstrap = await loadBootstrap("", fetchImpl);
    expect(urls).toEqual([`${base}/app/bootstrap`, `${base}/chat/bootstrap`]);
    expect(bootstrap.api_base).toBe(bootstrapFixture.api_base);
    expect(basePath()).toBe(base);
    expect(signInPath()).toBe("/elsewhere");
  });

  it.each(["", SHARED])("base %j checks updates and signs out under it", async (base) => {
    setShellMeta(base, null);
    const updates = recordingFetch();
    await checkForUpdates(updates.fetchImpl);
    expect(updates.urls).toEqual([`${base}/app/updates`]);

    const logout = recordingFetch(() => new Response(null, { status: 204 }));
    const api = makeAccountApi({
      origin: "",
      apiBase: "/api/v2/organizations/org_a",
      csrfToken: "csrf",
      fetch: logout.fetchImpl,
    });
    await api.logout();
    await api.projects().catch(() => undefined);
    expect(logout.urls).toEqual([`${base}/logout`, "/api/v2/organizations/org_a/projects"]);
  });
});

describe("router basepath", () => {
  it.each(["", SHARED])("base %j resolves client routes inside it", async (base) => {
    const router = makeRouter(
      createMemoryHistory({ initialEntries: [`${base}/chat`] }),
      routerBasePath(base),
    );
    await router.load();
    expect(router.state.location.pathname).toBe("/chat");
    expect(router.buildLocation({ to: "/settings/$section", params: { section: "general" } }).href).toBe(
      `${base}/settings/general`,
    );
  });
});
