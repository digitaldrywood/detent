export interface ArchiveDescriptor {
  readonly organization_id: string;
  readonly project_id: string;
  readonly work_item_id: string;
  readonly revision: string;
  readonly section_id: string;
  readonly sha256: string;
  readonly compressed_size: number;
  readonly uncompressed_size: number;
  readonly codec: string;
  readonly schema_version: number;
  readonly access_path: string;
}

export class ArchiveUnavailable extends Error {
  constructor(readonly reason: string) {
    super(`Archived detail is unavailable (${reason}).`);
  }
}

export function validateArchive(input: unknown): asserts input is ArchiveDescriptor {
  if (typeof input !== "object" || input === null) throw new ArchiveUnavailable("invalid descriptor");
  const value = input as ArchiveDescriptor;
  if (value.codec !== "gzip" || value.schema_version !== 1 || typeof value.sha256 !== "string" || !/^[a-f0-9]{64}$/.test(value.sha256) ||
    !Number.isSafeInteger(value.compressed_size) || value.compressed_size <= 0 || value.compressed_size > 20 * 1024 * 1024 ||
    !Number.isSafeInteger(value.uncompressed_size) || value.uncompressed_size <= 0 || value.uncompressed_size > 16 * 1024 * 1024 ||
    typeof value.revision !== "string" || !/^[1-9]\d*$/.test(value.revision) ||
    [value.organization_id, value.project_id, value.work_item_id, value.section_id, value.access_path].some((part) => typeof part !== "string" || part.length === 0)) {
    throw new ArchiveUnavailable("invalid descriptor");
  }
}

async function boundedBytes(stream: ReadableStream<Uint8Array>, limit: number): Promise<Uint8Array<ArrayBuffer>> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      size += next.value.byteLength;
      if (size > limit) throw new ArchiveUnavailable("size limit");
      chunks.push(next.value);
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
  if (size !== limit) throw new ArchiveUnavailable("size mismatch");
  const result = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
  return result;
}

export async function decodeArchive(response: Response, descriptor: ArchiveDescriptor): Promise<unknown> {
  validateArchive(descriptor);
  if (!response.ok || response.body === null) throw new ArchiveUnavailable(`HTTP ${response.status}`);
  const length = response.headers.get("Content-Length");
  if (length !== null && Number(length) !== descriptor.compressed_size) throw new ArchiveUnavailable("compressed size");
  if (response.headers.get("Content-Encoding")) throw new ArchiveUnavailable("unexpected content encoding");
  const compressed = await boundedBytes(response.body, descriptor.compressed_size);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", compressed));
  if ([...digest].map((byte) => byte.toString(16).padStart(2, "0")).join("") !== descriptor.sha256) throw new ArchiveUnavailable("checksum mismatch");
  const stream = new ReadableStream<BufferSource>({ start(controller) { controller.enqueue(compressed); controller.close(); } }).pipeThrough(new DecompressionStream("gzip"));
  const decoded = await boundedBytes(stream, descriptor.uncompressed_size);
  const payload: unknown = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(decoded));
  if (typeof payload !== "object" || payload === null || !("schema_version" in payload) || payload.schema_version !== 1 ||
    !("section_id" in payload) || payload.section_id !== descriptor.section_id || !("data" in payload)) throw new ArchiveUnavailable("schema mismatch");
  if ((descriptor.section_id === "issue" || descriptor.section_id.startsWith("revision::")) && typeof payload.data === "object" && payload.data !== null) {
    for (const key of ["dependencies", "blockers", "dependents", "related", "blocked_reason", "unblocks_reason"]) {
      if (key in payload.data) throw new ArchiveUnavailable("shared relationships");
    }
  }
  return payload.data;
}
