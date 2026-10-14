import { cva, type VariantProps } from "class-variance-authority";
import type React from "react";

import { cn } from "../../lib/utils.ts";

const avatarVariants = cva(
  "inline-flex shrink-0 items-center justify-center rounded-full bg-muted font-medium text-muted-foreground",
  {
    variants: { size: { sm: "size-4 text-[8px]", md: "size-8 text-xs" } },
    defaultVariants: { size: "sm" },
  },
);

export function Avatar({
  name,
  src,
  size,
  className,
}: VariantProps<typeof avatarVariants> & {
  readonly name: string;
  readonly src?: string | null;
  readonly className?: string;
}): React.ReactElement {
  const classes = cn(avatarVariants({ size }), className);
  return src ? (
    <img
      aria-hidden
      alt=""
      src={src}
      loading="lazy"
      className={cn(classes, "object-cover")}
    />
  ) : (
    <span aria-hidden className={classes}>
      {name
        .trim()
        .split(/\s+/)
        .slice(0, 2)
        .map((part) => part.slice(0, 1))
        .join("")
        .toUpperCase()}
    </span>
  );
}
