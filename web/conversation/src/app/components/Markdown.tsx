import React from "react";

import ChatMarkdown from "../../components/ChatMarkdown.tsx";
import type { MessageReference } from "../../contracts/index.ts";
import { remarkDemoteHeadings } from "../adapters/remarkDemoteHeadings.ts";
import { linkReferences } from "../lib/references.ts";

const EXTRA_REMARK_PLUGINS = [remarkDemoteHeadings];

export function Markdown({
  source,
  className,
  references,
}: {
  source: string;
  className?: string;
  /**
   * Cross-references the hub resolved for this message (decisions.md §14).
   * Each one's label becomes a link; text with no resolved reference stays
   * exactly as it was written.
   */
  references?: readonly MessageReference[] | undefined;
}): React.ReactElement {
  const text = React.useMemo(() => linkReferences(source, references), [source, references]);
  return (
    <ChatMarkdown
      text={text}
      // No checkout, so no path for a relative file link to resolve against;
      // the copied component renders such a link as plain text.
      cwd={undefined}
      extraRemarkPlugins={EXTRA_REMARK_PLUGINS}
      {...(className === undefined ? {} : { className })}
    />
  );
}
