// Layout and data display: scroll area, separator, group, collapsible, table, kbd.
import { ChevronDownIcon, ChevronRightIcon, GitCommitIcon, GitPullRequestIcon, PlayIcon } from "lucide-react";

import { Button } from "~/components/ui/button";
import { Collapsible, CollapsiblePanel, CollapsibleTrigger } from "~/components/ui/collapsible";
import { Group, GroupSeparator, GroupText } from "~/components/ui/group";
import { Kbd, KbdGroup } from "~/components/ui/kbd";
import { ScrollArea } from "~/components/ui/scroll-area";
import { Separator } from "~/components/ui/separator";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";

import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

const FILES = Array.from({ length: 30 }, (_, index) => `src/app/work/components/File${index + 1}.tsx`);

export const scrollArea: GalleryDoc = {
  meta: { name: "Scroll area", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "vertical",
      title: "Vertical, with and without scroll fade",
      render: () => (
        <Row>
          {[false, true].map((fade) => (
            <Cell key={String(fade)} label={fade ? "scrollFade" : "default"}>
              <div className="h-48 w-64 rounded-lg border">
                <ScrollArea scrollFade={fade}>
                  <ul className="p-2 font-mono text-xs">
                    {FILES.map((file) => (
                      <li key={file} className="truncate py-1">
                        {file}
                      </li>
                    ))}
                  </ul>
                </ScrollArea>
              </div>
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "horizontal",
      title: "Horizontal overflow",
      render: () => (
        <div className="h-20 w-72 max-w-full rounded-lg border">
          <ScrollArea scrollFade>
            <div className="w-[640px] p-3 font-mono text-xs">{LONG_LABEL} — {LONG_LABEL}</div>
          </ScrollArea>
        </div>
      ),
    },
  ],
};

export const separator: GalleryDoc = {
  meta: { name: "Separator", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "orientations",
      title: "Horizontal and vertical",
      render: () => (
        <div className="flex w-72 max-w-full flex-col gap-3 text-sm">
          <span>Above</span>
          <Separator />
          <div className="flex h-5 items-center gap-3">
            <span>Board</span>
            <Separator orientation="vertical" />
            <span>List</span>
            <Separator orientation="vertical" />
            <span>Changes</span>
          </div>
        </div>
      ),
    },
  ],
};

export const group: GalleryDoc = {
  meta: { name: "Group", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "buttons",
      title: "Joined buttons, text, separator",
      render: () => (
        <div className="flex flex-col gap-4">
          <Cell label="outline + separator">
            <Group aria-label="Git actions">
              <Button variant="outline" size="sm">
                <GitCommitIcon />
                Commit
              </Button>
              <GroupSeparator />
              <Button variant="outline" size="sm">
                <GitPullRequestIcon />
                Open PR
              </Button>
              <GroupSeparator />
              <Button variant="outline" size="icon-sm" aria-label="More">
                <ChevronDownIcon />
              </Button>
            </Group>
          </Cell>
          <Cell label="with GroupText">
            <Group aria-label="Run script">
              <GroupText>npm run</GroupText>
              <Button variant="outline" size="sm">
                <PlayIcon />
                test
              </Button>
            </Group>
          </Cell>
          <Cell label="vertical">
            <Group orientation="vertical" aria-label="Vertical">
              <Button variant="outline" size="sm">
                Top
              </Button>
              <Button variant="outline" size="sm">
                Middle
              </Button>
              <Button variant="outline" size="sm">
                Bottom
              </Button>
            </Group>
          </Cell>
        </div>
      ),
    },
  ],
};

export const collapsible: GalleryDoc = {
  meta: { name: "Collapsible", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "states",
      title: "Closed and open",
      render: () => (
        <div className="flex w-80 max-w-full flex-col gap-3">
          {[false, true].map((open) => (
            <Collapsible key={String(open)} defaultOpen={open} className="rounded-lg border p-2">
              <CollapsibleTrigger className="group flex w-full items-center gap-1.5 text-sm">
                <ChevronRightIcon className="size-4 transition-transform group-data-[panel-open]:rotate-90" />
                Worked for 2m 14s
              </CollapsibleTrigger>
              <CollapsiblePanel>
                <ul className="mt-2 flex flex-col gap-1 ps-6 text-muted-foreground text-xs">
                  <li>Read 4 files</li>
                  <li>Ran npm test</li>
                  <li>Edited src/app/router.tsx</li>
                </ul>
              </CollapsiblePanel>
            </Collapsible>
          ))}
        </div>
      ),
    },
    {
      id: "animate",
      title: "Panel animate: on and off",
      note: "Click each trigger. `animate` (the default) travels the panel's height; `animate={false}` snaps open and closed, for panels whose content resizes on its own.",
      render: () => (
        <div className="flex w-80 max-w-full flex-col gap-3">
          {[true, false].map((animate) => (
            <Collapsible key={String(animate)} className="rounded-lg border p-2">
              <CollapsibleTrigger className="group flex w-full items-center gap-1.5 text-sm">
                <ChevronRightIcon className="size-4 transition-transform group-data-[panel-open]:rotate-90" />
                <span className="font-mono text-2xs text-muted-foreground">animate={String(animate)}</span>
              </CollapsibleTrigger>
              <CollapsiblePanel animate={animate}>
                <ul className="mt-2 flex flex-col gap-1 ps-6 text-muted-foreground text-xs">
                  <li>Read 4 files</li>
                  <li>Ran npm test</li>
                  <li>Edited src/app/router.tsx</li>
                </ul>
              </CollapsiblePanel>
            </Collapsible>
          ))}
        </div>
      ),
    },
  ],
};

const RUNS = [
  { id: "run_1", issue: "DET-142", status: "Running", tokens: "48,120", cost: "$0.42" },
  { id: "run_2", issue: "DET-139", status: "Merged", tokens: "212,004", cost: "$1.88" },
  { id: "run_3", issue: "DET-131", status: "Failed", tokens: "9,310", cost: "$0.07" },
];

export const table: GalleryDoc = {
  meta: { name: "Table", kind: "primitive", group: "data-display" },
  specimens: [
    {
      id: "basic",
      title: "Header, body, footer, caption",
      render: () => (
        <div className="max-w-full overflow-x-auto">
          <Table>
            <TableCaption>Runs this week</TableCaption>
            <TableHeader>
              <TableRow>
                <TableHead>Issue</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">Tokens</TableHead>
                <TableHead className="text-right">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {RUNS.map((run) => (
                <TableRow key={run.id} data-state={run.id === "run_1" ? "selected" : undefined}>
                  <TableCell className="font-medium">{run.issue}</TableCell>
                  <TableCell>{run.status}</TableCell>
                  <TableCell className="text-right tabular-nums">{run.tokens}</TableCell>
                  <TableCell className="text-right tabular-nums">{run.cost}</TableCell>
                </TableRow>
              ))}
            </TableBody>
            <TableFooter>
              <TableRow>
                <TableCell colSpan={3}>Total</TableCell>
                <TableCell className="text-right tabular-nums">$2.37</TableCell>
              </TableRow>
            </TableFooter>
          </Table>
        </div>
      ),
    },
    {
      id: "empty",
      title: "Empty body",
      render: () => (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Issue</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow>
              <TableCell colSpan={2} className="text-center text-muted-foreground">
                No runs in this window.
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      ),
    },
  ],
};

export const kbd: GalleryDoc = {
  meta: { name: "Kbd", kind: "primitive", group: "data-display" },
  specimens: [
    {
      id: "keys",
      title: "Single keys and groups",
      render: () => (
        <Row>
          <Cell label="single">
            <Kbd>K</Kbd>
          </Cell>
          <Cell label="group">
            <KbdGroup>
              <Kbd>⌘</Kbd>
              <Kbd>K</Kbd>
            </KbdGroup>
          </Cell>
          <Cell label="word">
            <Kbd>Esc</Kbd>
          </Cell>
          <Cell label="inline in text">
            <span className="text-muted-foreground text-sm">
              Press <Kbd>/</Kbd> to search
            </span>
          </Cell>
        </Row>
      ),
    },
  ],
};
