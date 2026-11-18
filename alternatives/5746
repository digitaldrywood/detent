interface MdastNode {
  type: string;
  depth?: number;
  children?: MdastNode[];
}

/** A remark plugin that pushes every heading one level deeper. */
export function remarkDemoteHeadings(): (tree: MdastNode) => void {
  return function demote(tree: MdastNode): void {
    visit(tree);
  };
}

function visit(node: MdastNode): void {
  if (node.type === "heading" && typeof node.depth === "number") {
    node.depth = Math.min(6, node.depth + 1);
  }
  for (const child of node.children ?? []) visit(child);
}
