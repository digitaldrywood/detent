import { describe, expect, it, vi } from "vitest";

import { loadChunk } from "../src/app/lib/chunkReload.ts";

function environment(stored: Record<string, string> = {}) {
  const values = new Map(Object.entries(stored));
  return {
    reload: vi.fn(),
    storage: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => void values.set(key, value),
    },
    values,
  };
}

const missing = new Error("Failed to fetch dynamically imported module: /static/app/conversation/chunks/router-AbCd1234.js");
const key = `tanstack_router_reload:${missing.message}`;

describe("loadChunk", () => {
  it("resolves the module when the import succeeds", async () => {
    const env = environment();
    await expect(loadChunk(() => Promise.resolve({ value: 1 }), env)).resolves.toEqual({ value: 1 });
    expect(env.reload).not.toHaveBeenCalled();
  });

  it("reloads once and stays pending when the chunk is missing", async () => {
    const env = environment();
    let settled = false;
    void loadChunk(() => Promise.reject(missing), env).finally(() => {
      settled = true;
    });
    await vi.waitFor(() => expect(env.reload).toHaveBeenCalledTimes(1));
    expect(env.values.get(key)).toBe("1");
    expect(settled).toBe(false);
  });

  it("rejects instead of reloading again after the guard is set", async () => {
    const env = environment({ [key]: "1" });
    await expect(loadChunk(() => Promise.reject(missing), env)).rejects.toBe(missing);
    expect(env.reload).not.toHaveBeenCalled();
  });

  it("rejects other failures without reloading", async () => {
    const env = environment();
    const failure = new Error("makeEntryRouter is not a function");
    await expect(loadChunk(() => Promise.reject(failure), env)).rejects.toBe(failure);
    expect(env.reload).not.toHaveBeenCalled();
  });
});
