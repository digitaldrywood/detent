// Idempotency keys for the first-run wizard, persisted the way
// `static/js/hosted-setup.js` persisted them.
//
// The rule the hosted page established and this port keeps: a key belongs to
// one (request path, request body) pair. It is minted on the first attempt,
// reused by every retry of the identical body — so a request whose outcome the
// browser never learned cannot create a second thing — and deleted once the
// hub has answered. Editing a field is a different command and mints a new
// key. Storage is `sessionStorage`, not `localStorage`: a key that outlived
// the tab would replay against a session that no longer exists, and the hub
// scopes its replay table by hosted session id anyway.
export const SETUP_KEY_PREFIX = "detent-setup:";

/**
 * The body fingerprint. SHA-256 where the browser exposes it, as the hosted
 * page used, and a stable string hash where it does not (a jsdom test, an
 * insecure origin): the fingerprint only has to be a function of the body, and
 * both sides of a retry compute it the same way in the same browser.
 */
export async function fingerprint(body: unknown): Promise<string> {
  const text = JSON.stringify(body) ?? "";
  const subtle = globalThis.crypto?.subtle;
  if (subtle !== undefined) {
    const digest = await subtle.digest("SHA-256", new TextEncoder().encode(text));
    return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join(
      "",
    );
  }
  // FNV-1a, 32 bit, hex. Not a cryptographic digest and not used as one.
  let hash = 0x811c9dc5;
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}

function read(name: string): string | null {
  try {
    return globalThis.sessionStorage?.getItem(name) ?? null;
  } catch {
    return null;
  }
}

function write(name: string, value: string): void {
  try {
    globalThis.sessionStorage?.setItem(name, value);
  } catch {
    // A blocked store costs deduplication, not correctness: the hub still
    // refuses a second identical command under the same key, and a fresh key
    // is a fresh command the user asked for.
  }
}

function remove(name: string): void {
  try {
    globalThis.sessionStorage?.removeItem(name);
  } catch {
    // See `write`.
  }
}

export function newKey(): string {
  const random = globalThis.crypto?.randomUUID?.();
  return random ?? `key_${Date.now()}_${Math.random().toString(16).slice(2)}`;
}

export interface SetupKey {
  /** The value sent as `idempotency_key`. */
  readonly key: string;
  /** The `sessionStorage` name, so the caller can retire it on success. */
  readonly storageKey: string;
}

/** Mints or recovers the key for one (path, body) pair. */
export async function setupKey(path: string, body: unknown): Promise<SetupKey> {
  const storageKey = `${SETUP_KEY_PREFIX}${path}:${await fingerprint(body)}`;
  const existing = read(storageKey);
  if (existing !== null && existing.length > 0) return { key: existing, storageKey };
  const key = newKey();
  write(storageKey, key);
  return { key, storageKey };
}

/**
 * Retires a key once the hub answered. It is deliberately not called on a
 * failure: a retry of the identical body has to reuse the key, which is the
 * whole point of persisting it.
 */
export function retireKey(storageKey: string): void {
  remove(storageKey);
}

/** Every wizard key, for a test or a "start over" that means it. */
export function clearSetupKeys(): void {
  try {
    const store = globalThis.sessionStorage;
    if (store === undefined) return;
    const names: string[] = [];
    for (let index = 0; index < store.length; index += 1) {
      const name = store.key(index);
      if (name !== null && name.startsWith(SETUP_KEY_PREFIX)) names.push(name);
    }
    for (const name of names) store.removeItem(name);
  } catch {
    // See `write`.
  }
}
