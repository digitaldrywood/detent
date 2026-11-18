interface RawNode {
  type: string;
  value?: unknown;
  children?: RawNode[];
}

/**
 * A rehype plugin that escapes raw HTML instead of parsing it.
 *
 * The signature is `rehype-raw`'s — a unified plugin factory taking options
 * and returning a transformer — so the copied call site type-checks and runs
 * unchanged. The options are accepted and ignored.
 */
export default function rehypeRawDisabled(_options?: unknown): (tree: RawNode) => void {
  return function escapeRawNodes(tree: RawNode): void {
    visit(tree);
  };
}

function visit(node: RawNode): void {
  if (node.type === "raw") {
    // Same value, a type nothing will ever turn into markup.
    node.type = "text";
    node.value = typeof node.value === "string" ? node.value : "";
    return;
  }
  for (const child of node.children ?? []) visit(child);
}
