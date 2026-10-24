// The organization conversation list is only requested when the hub's
// bootstrap reports the conversation feature on; with it off the route
// answers 404, so asking for it on every board load is wasted work.
import { afterEach, describe, expect, it } from "vitest";

import { makeHarness, type Harness } from "./harness.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

describe("conversation list feature gate", () => {
  it.each([
    { enabled: true, requested: true },
    { enabled: false, requested: false },
  ])("conversation feature $enabled requests the list: $requested", async ({ enabled, requested }) => {
    const urls: string[] = [];
    const recording: typeof globalThis.fetch = (input, init) => {
      urls.push(input instanceof Request ? input.url : String(input));
      return globalThis.fetch(input, init);
    };
    harness = await makeHarness({
      fetch: recording,
      bootstrap: (bootstrap) => ({ ...bootstrap, feature: { conversation: enabled } }),
    });
    const listAtom = harness.client.list.stateAtom("hub");
    harness.mount(listAtom);
    const settled = await harness.waitFor(listAtom, (value) => value.status === "live", "a live list");
    await harness.run(harness.client.effects.refreshList());

    const listRequests = urls.filter((url) => new URL(url).pathname.endsWith("/conversations"));
    expect(listRequests.length > 0).toBe(requested);
    if (!enabled) expect(settled.conversations).toHaveLength(0);
  });
});
