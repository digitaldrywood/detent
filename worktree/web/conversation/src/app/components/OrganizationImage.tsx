import React from "react";
import { cn } from "../../lib/utils.ts";

export function OrganizationImage({ url, name, fallback, className }: {
  readonly url?: string | undefined;
  readonly name: string;
  readonly fallback: React.ReactNode;
  readonly className?: string;
}) {
  const [failed, setFailed] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState<string | null>(null);
  if (!url || failed === url) return <>{fallback}</>;
  return <span className={cn("relative inline-flex shrink-0", className)}>
    {loaded !== url ? fallback : null}
    <img src={url} alt={`${name} logo`} className={cn("absolute inset-0 size-full rounded-lg object-cover", loaded !== url && "opacity-0")} onLoad={() => setLoaded(url)} onError={() => setFailed(url)} />
  </span>;
}
