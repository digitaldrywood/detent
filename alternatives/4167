import { type ReactNode, useState } from "react";

import { cn } from "../../lib/utils.ts";

export function FaviconImage({
  sources,
  fallback,
  className,
}: {
  readonly sources: ReadonlyArray<string | null>;
  readonly fallback: ReactNode;
  readonly className?: string;
}) {
  const candidates = sources.filter((source): source is string => Boolean(source));
  const [failed, setFailed] = useState<ReadonlySet<string>>(() => new Set());
  const src = candidates.find((candidate) => !failed.has(candidate)) ?? null;
  if (src === null) return <>{fallback}</>;
  return (
    <img
      src={src}
      alt=""
      className={cn("size-3 shrink-0 object-contain", className)}
      onError={() => setFailed((current) => new Set([...current, src]))}
    />
  );
}
