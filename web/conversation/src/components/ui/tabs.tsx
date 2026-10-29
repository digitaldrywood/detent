"use client";

import { Tabs as TabsPrimitive } from "@base-ui/react/tabs";
import { createContext, useContext } from "react";

import { cn } from "~/lib/utils";

const TabsVariant = createContext<"line" | "segmented">("line");

function Tabs({
  className,
  variant = "line",
  ...props
}: TabsPrimitive.Root.Props & { variant?: "line" | "segmented" }) {
  return (
    <TabsVariant.Provider value={variant}>
      <TabsPrimitive.Root
        className={cn("flex min-w-0 flex-col", className)}
        data-slot="tabs"
        {...props}
      />
    </TabsVariant.Provider>
  );
}

function TabsList({ className, children, ...props }: TabsPrimitive.List.Props) {
  const variant = useContext(TabsVariant);
  return (
    <TabsPrimitive.List
      className={cn(
        "relative flex min-w-0 items-stretch gap-1 overflow-x-auto overscroll-x-contain [scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
        variant === "segmented"
          ? "mb-5 w-fit max-w-full rounded-lg bg-input/40 p-0.5"
          : "border-b border-border",
        className,
      )}
      data-slot="tabs-list"
      {...props}
    >
      {children}
    </TabsPrimitive.List>
  );
}

function TabsTab({ className, ...props }: TabsPrimitive.Tab.Props) {
  const variant = useContext(TabsVariant);
  return (
    <TabsPrimitive.Tab
      className={cn(
        "relative inline-flex shrink-0 cursor-pointer items-center whitespace-nowrap px-3 font-medium text-muted-foreground text-sm outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset disabled:pointer-events-none disabled:opacity-64 data-active:text-foreground",
        variant === "segmented"
          ? "h-8 rounded-md hover:bg-background/55 data-active:bg-background data-active:shadow-xs/10 dark:hover:bg-input/32 dark:data-active:bg-input/72"
          : "h-11 after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:rounded-full after:bg-transparent data-active:after:bg-primary",
        className,
      )}
      data-slot="tabs-tab"
      {...props}
    />
  );
}

function TabsPanel({ className, ...props }: TabsPrimitive.Panel.Props) {
  return (
    <TabsPrimitive.Panel
      className={cn("min-w-0 outline-none", className)}
      data-slot="tabs-panel"
      {...props}
    />
  );
}

export { Tabs, TabsList, TabsTab, TabsPanel };
