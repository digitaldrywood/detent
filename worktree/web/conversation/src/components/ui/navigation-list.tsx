import type { ComponentProps, ReactNode } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { ArrowUpRightIcon, ChevronRightIcon, PlusIcon } from "lucide-react";

import { cn } from "~/lib/utils";
import { PROJECT_ICON_COLORS } from "~/projectIconColors";

export function NavigationList({ className, ...props }: ComponentProps<"ul">) {
  return (
    <ul
      className={cn("divide-y divide-border overflow-hidden rounded-xl border border-border bg-card", className)}
      {...props}
    />
  );
}

const navigationLinkVariants = cva(
  "flex w-full items-center gap-3.5 text-sm transition-colors hover:bg-accent focus-visible:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
  {
    variants: {
      variant: {
        default: "min-h-16 px-4 py-3 text-foreground",
        create: "min-h-14 px-4 py-2.5 text-muted-foreground",
        quiet: "min-h-11 rounded-lg border border-border px-4 py-2 text-muted-foreground",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

export function NavigationListLink({
  name,
  description,
  leading,
  variant,
  className,
  ...props
}: Omit<ComponentProps<"a">, "children"> & VariantProps<typeof navigationLinkVariants> & {
  readonly name: string;
  readonly description?: ReactNode;
  readonly leading?: ReactNode;
}) {
  return (
    <a aria-label={name} className={cn(navigationLinkVariants({ variant }), className)} {...props}>
      {leading}
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className={cn("truncate", variant == null || variant === "default" ? "font-medium" : undefined)}>
          {name}
        </span>
        {description ? (
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">{description}</span>
        ) : null}
      </span>
      {variant === "create" ? null : variant === "quiet" ? (
        <ArrowUpRightIcon aria-hidden="true" className="size-4 shrink-0" />
      ) : (
        <ChevronRightIcon aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
      )}
    </a>
  );
}

export function NavigationMonogram({
  name,
  identity,
  create = false,
}: {
  readonly name?: string;
  readonly identity?: string;
  readonly create?: boolean;
}) {
  const initials = (name ?? "").trim().split(/\s+/).slice(0, 2)
    .map((word) => Array.from(word)[0] ?? "").join("").toUpperCase();
  const hash = Array.from(identity ?? "").reduce(
    (value, character) => (value * 31 + (character.codePointAt(0) ?? 0)) >>> 0,
    0,
  );
  const color = identity === undefined ? undefined : PROJECT_ICON_COLORS[hash % PROJECT_ICON_COLORS.length];
  return (
    <span
      aria-hidden="true"
      className={cn(
        "relative grid size-9 shrink-0 place-items-center overflow-hidden rounded-lg text-sm font-semibold",
        create ? "border border-dashed border-border text-muted-foreground" : color?.className ?? "bg-muted text-muted-foreground",
      )}
    >
      {color ? <span className={cn("absolute inset-0 opacity-20", color.swatchClassName)} /> : null}
      <span className="relative">{create ? <PlusIcon className="size-4" /> : initials}</span>
    </span>
  );
}
