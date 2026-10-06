export const BOARD_CACHE_VERSION = 1;
export const BOARD_CACHE_LIMIT = 12;

export interface BoardDiskRecord {
  readonly key: string;
  readonly account: string;
  readonly version: number;
  readonly asOf: number;
  readonly entries: unknown;
}

let database: Promise<IDBDatabase | null> | undefined;

function open(): Promise<IDBDatabase | null> {
  database ??= new Promise((resolve) => {
    let unavailable = false;
    const fail = () => { unavailable = true; resolve(null); };
    try {
      if (globalThis.indexedDB === undefined) return resolve(null);
      const request = globalThis.indexedDB.open("detent-work-reads", BOARD_CACHE_VERSION);
      request.onupgradeneeded = () => {
        const db = request.result;
        for (const name of Array.from(db.objectStoreNames)) db.deleteObjectStore(name);
        db.createObjectStore("boards", { keyPath: "key" });
      };
      request.onsuccess = () => {
        if (unavailable) { request.result.close(); return; }
        request.result.onversionchange = () => { request.result.close(); database = undefined; };
        resolve(request.result);
      };
      request.onerror = fail;
      request.onblocked = fail;
    } catch {
      resolve(null);
    }
  });
  return database;
}

export async function readBoardDisk(key: string): Promise<BoardDiskRecord | null> {
  try {
    const db = await open();
    if (db === null) return null;
    return await new Promise((resolve) => {
      const transaction = db.transaction("boards", "readonly");
      const request = transaction.objectStore("boards").get(key);
      request.onsuccess = () => resolve(request.result ?? null);
      request.onerror = () => resolve(null);
      transaction.onabort = () => resolve(null);
    });
  } catch {
    return null;
  }
}

let writes = Promise.resolve();

export function updateBoardDisk(account: string | null, record?: BoardDiskRecord, valid: () => boolean = () => true): Promise<void> {
  writes = writes.then(async () => {
    try {
      const db = await open();
      if (db === null || !valid()) return;
      await new Promise<void>((resolve) => {
        const transaction = db.transaction("boards", "readwrite");
        const store = transaction.objectStore("boards");
        const request = store.getAll();
        request.onsuccess = () => {
          if (!valid()) return;
          const records = (request.result as BoardDiskRecord[]).filter((candidate) => {
            if (candidate.account === account && candidate.version === BOARD_CACHE_VERSION) return true;
            store.delete(candidate.key);
            return false;
          });
          if (record !== undefined) {
            store.put(record);
            records.push(record);
          }
          const newest = [...new Map(records.map((candidate) => [candidate.key, candidate])).values()]
            .sort((a, b) => b.asOf - a.asOf);
          for (const candidate of newest.slice(BOARD_CACHE_LIMIT)) store.delete(candidate.key);
        };
        transaction.oncomplete = () => resolve();
        transaction.onabort = () => resolve();
        transaction.onerror = () => resolve();
      });
    } catch {
      return;
    }
  });
  return writes;
}
