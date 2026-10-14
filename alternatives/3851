import React from "react";

import { ClientContext } from "./client.ts";

/** Browser titles put the most useful part first for tabs and history. */
export function formatPageTitle(page: string, context?: string | null): string {
  return [page.trim(), context?.trim(), "Detent"].filter(Boolean).join(" · ");
}

/** Organization routes default to their bootstrap name; entry routes have no client. */
export function usePageTitle(page: string, context?: string | null): void {
  const client = React.useContext(ClientContext);
  const scope = context || client?.bootstrap.organization.name;
  React.useEffect(() => {
    document.title = formatPageTitle(page, scope);
  }, [page, scope]);
}
