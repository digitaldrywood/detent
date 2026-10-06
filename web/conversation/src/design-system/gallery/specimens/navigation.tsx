// Navigation primitives: the sidebar kit and the command list.
import { FolderIcon, InboxIcon, MessageSquareIcon, PlusIcon, SearchIcon, SettingsIcon } from "lucide-react";

import {
  Command,
  CommandEmpty,
  CommandFooter,
  CommandGroup,
  CommandGroupLabel,
  CommandInput,
  CommandItem,
  CommandList,
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
      title: "Header, groups, menu buttons (active, outline, disabled, sizes), sub menu, badge, skeleton",
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
  ],
};
