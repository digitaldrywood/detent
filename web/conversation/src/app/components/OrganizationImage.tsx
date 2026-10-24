import React from "react";
import { cn } from "../../lib/utils.ts";

export function OrganizationImage({ url, name, fallback, className }: {
  readonly url?: string | undefined;
  readonly name: string;
  readonly fallback: React.ReactNode;
  readonly className?: string;
}) {
  const [failed, setFailed] = React.useState<string | null>(null);
  return url && failed !== url ? <img src={url} alt={`${name} logo`} className={cn("shrink-0 rounded-lg object-cover", className)} onError={() => setFailed(url)} /> : <>{fallback}</>;
}
