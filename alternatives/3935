/** True when the shell was served by the shared entry rather than an organization. */
export function isEntrySurface(source: Pick<Document, "querySelector"> | undefined = globalThis.document): boolean {
  return source?.querySelector('meta[name="detent-surface"]')?.getAttribute("content") === "entry";
}
