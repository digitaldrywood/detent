// Navigation primitives: the sidebar kit and the command list.
import { Autocomplete as AutocompletePrimitive } from "@base-ui/react/autocomplete";
import {
  BellIcon,
  FolderIcon,
  GitBranchIcon,
  InboxIcon,
  MessageSquareIcon,
  PlusIcon,
  SearchIcon,
  SettingsIcon,
} from "lucide-react";
import React from "react";

import {
  Command,
  CommandEmpty,
  CommandFooter,
  CommandGroup,
  CommandGroupLabel,
  CommandInput,
  CommandItem,
  CommandList,
  CommandListHeading,
  CommandListVirtualized,
  CommandPanel,
  CommandSeparator,
  CommandShortcut,
} from "~/components/ui/command";
import { Kbd, KbdGroup } from "~/components/ui/kbd";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupAction,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInput,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSkeleton,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  SidebarProvider,
  SidebarSeparator,
} from "~/components/ui/sidebar";

import { BRANCHES, scrollRowIntoView } from "./forms";
import { LONG_LABEL, type GalleryDoc } from "../specimen";

function SidebarKit() {
  return (
    <SidebarProvider className="min-h-0! h-full!">
      <Sidebar collapsible="none" className="border-r border-sidebar-border" data-app-sidebar="">
        <SidebarHeader>
          <SidebarInput placeholder="Search" aria-label="Search" />
        </SidebarHeader>
        <SidebarContent>
          <SidebarGroup>
            <SidebarGroupLabel>Browse</SidebarGroupLabel>
            <SidebarGroupAction aria-label="New">
              <PlusIcon />
            </SidebarGroupAction>
            <SidebarGroupContent>
              <SidebarMenu>
                <SidebarMenuItem>
                  <SidebarMenuButton isActive>
                    <InboxIcon />
                    <span>Work (active)</span>
                  </SidebarMenuButton>
                  <SidebarMenuBadge>12</SidebarMenuBadge>
                </SidebarMenuItem>
                <SidebarMenuItem>
                  <SidebarMenuButton>
                    <MessageSquareIcon />
                    <span>Chat</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
                <SidebarMenuItem>
                  <SidebarMenuButton variant="outline">
                    <SearchIcon />
                    <span>outline variant</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
                <SidebarMenuItem>
                  <SidebarMenuButton disabled>
                    <SettingsIcon />
                    <span>Disabled</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
                <SidebarMenuItem>
                  <SidebarMenuButton size="sm">
                    <FolderIcon />
                    <span>{LONG_LABEL}</span>
                  </SidebarMenuButton>
                  <SidebarMenuSub>
                    <SidebarMenuSubItem>
                      <SidebarMenuSubButton isActive>Sub item (active)</SidebarMenuSubButton>
                    </SidebarMenuSubItem>
                    <SidebarMenuSubItem>
                      <SidebarMenuSubButton>Sub item</SidebarMenuSubButton>
                    </SidebarMenuSubItem>
                  </SidebarMenuSub>
                </SidebarMenuItem>
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
          <SidebarSeparator />
          <SidebarGroup>
            <SidebarGroupLabel>Loading</SidebarGroupLabel>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuSkeleton showIcon />
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuSkeleton />
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroup>
        </SidebarContent>
        <SidebarFooter>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton size="lg">
                <SettingsIcon />
                <span>Settings (lg)</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
          <SidebarMenu className="flex-row gap-1">
            <SidebarMenuItem>
              <SidebarMenuButton size="icon" aria-label="New thread (icon size)">
                <PlusIcon />
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton size="icon" aria-label="Notifications" isActive>
                <BellIcon />
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton size="icon" aria-label="Settings">
                <SettingsIcon />
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarFooter>
      </Sidebar>
    </SidebarProvider>
  );
}

export const sidebar: GalleryDoc = {
  meta: { name: "Sidebar", kind: "primitive", group: "navigation" },
  specimens: [
    {
      id: "kit",
      title: "Header, groups, menu buttons (active, outline, disabled, sizes incl. icon), sub menu, badge, skeleton",
      note: "Rendered with `collapsible=\"none\"` so it sits in the frame; the App sidebar composition shows the offcanvas layout.",
      height: 560,
      render: () => <SidebarKit />,
    },
  ],
};

const ITEMS = [
  { group: "Actions", items: ["New chat", "New issue", "Open settings"] },
  { group: "Projects", items: ["detent", "detent-cloud", "website"] },
];

const ACTIVE_ITEMS = ["New chat", "New issue", "Open settings", "Switch project", "Toggle theme"];

/** A palette that tracks its own active row (arrow keys or pointer), passing `active` to each item. */
function ActiveCommand() {
  const [active, setActive] = React.useState(1);
  return (
    <div
      className="w-[min(32rem,100%)] overflow-hidden rounded-2xl border bg-popover"
      onKeyDown={(event) => {
        if (event.key === "ArrowDown") setActive((index) => Math.min(ACTIVE_ITEMS.length - 1, index + 1));
        if (event.key === "ArrowUp") setActive((index) => Math.max(0, index - 1));
      }}
    >
      <Command items={ACTIVE_ITEMS}>
        <CommandInput placeholder="Arrow keys move the active row" autoFocus={false} />
        <CommandPanel>
          <CommandList>
            {(item: string, index: number) => (
              <CommandItem key={item} value={item} active={index === active} onMouseMove={() => setActive(index)}>
                {item}
              </CommandItem>
            )}
          </CommandList>
        </CommandPanel>
      </Command>
    </div>
  );
}

/**
 * Every filtered row, mounted with its `index`, in a plain scroll container.
 * The inline palette keeps its highlight scrolled into view itself, so an
 * external window that re-renders on scroll would chase it without end; a
 * real list plugs its virtualizer in here.
 */
function BranchCommandRows({ scrollRef }: { readonly scrollRef: React.RefObject<HTMLDivElement | null> }) {
  const filtered = AutocompletePrimitive.useFilteredItems<string>().slice(0, 200);
  return (
    <div ref={scrollRef} role="presentation" className="max-h-72 overflow-y-auto overscroll-contain px-1">
      {filtered.map((branch, index) => (
        <CommandItem key={branch} value={branch} index={index}>
          <GitBranchIcon />
          <span className="truncate">{branch}</span>
        </CommandItem>
      ))}
    </div>
  );
}

function VirtualizedCommand() {
  const scrollRef = React.useRef<HTMLDivElement | null>(null);
  return (
    <div className="w-[min(32rem,100%)] overflow-hidden rounded-2xl border bg-popover">
      <Command
        items={BRANCHES}
        virtualized
        onItemHighlighted={(_item, { reason, index }) => {
          if (reason === "keyboard") scrollRowIntoView(scrollRef.current, index);
        }}
      >
        <CommandInput placeholder="Switch branch" autoFocus={false} />
        <CommandPanel>
          <CommandEmpty>No branch matches.</CommandEmpty>
          <CommandListVirtualized className="py-2">
            <CommandListHeading>Branches</CommandListHeading>
            <BranchCommandRows scrollRef={scrollRef} />
          </CommandListVirtualized>
        </CommandPanel>
      </Command>
    </div>
  );
}

export const command: GalleryDoc = {
  meta: { name: "Command", kind: "primitive", group: "navigation" },
  specimens: [
    {
      id: "inline",
      title: "Inline list with groups, shortcut, empty state and footer",
      note: "The palette composition puts the same parts in a dialog. Type `zzz` for the empty state.",
      render: () => (
        <div className="w-[min(32rem,100%)] overflow-hidden rounded-2xl border bg-popover">
          <Command items={ITEMS}>
            <CommandInput placeholder="Type a command" autoFocus={false} />
            <CommandPanel>
              <CommandEmpty>No results.</CommandEmpty>
              <CommandList>
                {(group: (typeof ITEMS)[number], index: number) => (
                  <CommandGroup key={group.group} items={group.items}>
                    {index > 0 ? <CommandSeparator /> : null}
                    <CommandGroupLabel>{group.group}</CommandGroupLabel>
                    {group.items.map((item) => (
                      <CommandItem key={item} value={item}>
                        {item}
                        {item === "New chat" ? <CommandShortcut>⌘N</CommandShortcut> : null}
                      </CommandItem>
                    ))}
                  </CommandGroup>
                )}
              </CommandList>
            </CommandPanel>
            <CommandFooter>
              <KbdGroup className="items-center gap-1.5">
                <Kbd>Enter</Kbd>
                <span>Run</span>
              </KbdGroup>
            </CommandFooter>
          </Command>
        </div>
      ),
    },
    {
      id: "active",
      title: "Active item",
      note: "When the palette tracks the highlighted row itself it passes `active`; the primitive's hover and keyboard highlight are then ignored and the row shows the active surface.",
      render: () => <ActiveCommand />,
    },
    {
      id: "virtualized",
      title: "Virtualized list with a heading",
      note: "`CommandListVirtualized` leaves scrolling and windowing to an external virtualizer (the app pairs it with LegendList); each row carries its `index`. Here the first 200 of 1,000 branches are mounted. `CommandListHeading` labels rows that cannot sit in a group.",
      render: () => <VirtualizedCommand />,
    },
  ],
};
