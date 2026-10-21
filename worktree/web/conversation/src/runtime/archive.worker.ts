import { ArchiveUnavailable, decodeArchive, type ArchiveDescriptor } from "./archiveDecode.ts";

self.onmessage = async (event: MessageEvent<{ url: string; descriptor: ArchiveDescriptor }>) => {
  try {
    const response = await fetch(event.data.url, { credentials: "omit", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer" });
    if (response.status === 403) {
      self.postMessage({ expired: true });
      return;
    }
    const data = await decodeArchive(response, event.data.descriptor);
    self.postMessage({ data });
  } catch (error) {
    self.postMessage({ error: error instanceof ArchiveUnavailable ? error.reason : "invalid archive" });
  }
};
