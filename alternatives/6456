import { gzipSync } from "node:zlib";
import { type ArchiveDescriptor } from "../src/runtime/archiveDecode.ts";

export async function archiveFixture(data: unknown = { body: "Archived history" }, section = "issue") {
  const raw = JSON.stringify({ schema_version: 1, section_id: section, data });
  const bytes = new Uint8Array(gzipSync(raw));
  const sha256 = [...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes))].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  const descriptor: ArchiveDescriptor = {
    organization_id: "org_example", project_id: "prj_example", work_item_id: "wi_example",
    revision: "1", section_id: section, sha256, compressed_size: bytes.length,
    uncompressed_size: new TextEncoder().encode(raw).length, codec: "gzip", schema_version: 1,
    access_path: `/api/v2/organizations/org_example/projects/prj_example/work-items/wi_example/archives/${sha256}/access?section=${encodeURIComponent(section)}&revision=1`,
  };
  return { descriptor, bytes, response: () => new Response(bytes, { headers: { "Content-Type": "application/gzip" } }) };
}
