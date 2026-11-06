import { InfoIcon, LockKeyholeIcon } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "../../components/ui/button.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";

export function IntegrationTitle({ title, help, secret }: {
  readonly title: string;
  readonly help: string;
  readonly secret: "webhook" | "token";
}) {
  return <span className="inline-flex flex-wrap items-center gap-1.5">
    {title}
    <Tooltip>
      <TooltipTrigger render={<Button type="button" size="icon-micro" variant="ghost-muted" aria-label={`About ${title}`}><InfoIcon aria-hidden="true" className="size-3.5" /></Button>} />
      <TooltipPopup side="top" className="max-w-72">{help}</TooltipPopup>
    </Tooltip>
    <Tooltip>
      <TooltipTrigger render={<Button type="button" size="icon-micro" variant="ghost-muted" aria-label={`${title} security`}><LockKeyholeIcon aria-hidden="true" className="size-3.5" /></Button>} />
      <TooltipPopup side="top" className="max-w-72">
        <span className="block font-medium">Encrypted · write-only</span>
        <span className="block">{secret === "webhook" ? "Detent never shows the webhook again. Replace or disable it here." : "Detent never shows the token again; revoke it in Sprites."}</span>
      </TooltipPopup>
    </Tooltip>
  </span>;
}

export function IntegrationStatus({ tone, children }: {
  readonly tone: "connected" | "failed" | "empty";
  readonly children: ReactNode;
}) {
  return <span className={`flex items-baseline gap-2 ${tone === "failed" ? "text-warning-foreground" : ""}`}>
    <span aria-hidden="true" className={`size-2 shrink-0 self-center rounded-full ${tone === "connected" ? "bg-success" : tone === "failed" ? "bg-warning" : "bg-muted-foreground/50"}`} />
    <span>{children}</span>
  </span>;
}
