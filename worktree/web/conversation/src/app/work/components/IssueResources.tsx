import { ChevronDownIcon, FileDiffIcon, GitPullRequestIcon, MessageSquareIcon } from "lucide-react";
import React from "react";

import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "../../../components/ui/collapsible.tsx";

export interface ResourceRow {
  readonly key: string;
  readonly icon: "diff" | "pull-request" | "conversation";
  readonly label: string;
  readonly detail: string;
  readonly onOpen: () => void;
}

const ICONS = {
  diff: FileDiffIcon,
  "pull-request": GitPullRequestIcon,
  conversation: MessageSquareIcon,
} as const;

export function IssueResources({
  rows,
}: {
  readonly rows: readonly ResourceRow[];
}): React.ReactElement | null {
  if (rows.length === 0) return null;
  return (
    <Collapsible defaultOpen data-testid="issue-resources">
      <CollapsibleTrigger className="flex w-full items-center gap-1.5 rounded-sm text-left text-[13px] text-muted-foreground outline-none ring-ring hover:text-foreground focus-visible:ring-2">
        <ChevronDownIcon className="size-3" />
        Resources
      </CollapsibleTrigger>
      <CollapsiblePanel>
        <ul className="mt-2 flex flex-col gap-2">
          {rows.map((row) => {
            const Icon = ICONS[row.icon];
            return (
              <li key={row.key}>
                <button
                  type="button"
                  data-testid={`resource-${row.icon}`}
                  onClick={row.onOpen}
                  className="flex w-full cursor-pointer items-center gap-2.5 rounded-[var(--radius)] border border-border bg-card px-3 py-2.5 text-left text-[13px] outline-none ring-ring hover:border-input focus-visible:ring-2"
                >
                  <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 truncate">{row.label}</span>
                  <span className="ml-auto shrink-0 text-muted-foreground/70 text-xs">
                    {row.detail}
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      </CollapsiblePanel>
    </Collapsible>
  );
}
