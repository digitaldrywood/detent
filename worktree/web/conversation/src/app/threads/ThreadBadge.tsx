import React, { useEffect, useState } from "react";
import { Badge } from "../../components/ui/badge.tsx";
import { ClientContext } from "../client.ts";
import { THREADS_CHANGED, useThreadsApi } from "./api.ts";

export function ThreadUnreadBadge({
  count,
  pending = false,
}: {
  count: number | null;
  pending?: boolean;
}) {
  if (pending && !count)
    return (
      <Badge aria-label="Loading unread threads" aria-busy="true">
        …
      </Badge>
    );
  return count !== null && count > 0 ? (
    <Badge aria-label={`${count} unread threads`} aria-busy={pending}>
      {count}
    </Badge>
  ) : null;
}

export function ThreadsSidebarBadge() {
  const client = React.useContext(ClientContext);
  return client ? <ConnectedThreadsBadge /> : null;
}

function ConnectedThreadsBadge() {
  const api = useThreadsApi();
  const client = React.useContext(ClientContext)!;
  const projects = React.useMemo(
    () =>
      (client.account?.projects ?? client.bootstrap.projects).map(
        (project) => project.id,
      ),
    [client],
  );
  const [summary, setSummary] = useState<{
    count: number | null;
    pending: boolean;
  }>({ count: null, pending: true });
  useEffect(() => {
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setSummary({ count: null, pending: true });
    const load = (refresh: boolean) => {
      clearTimeout(timer);
      controller?.abort();
      controller = new AbortController();
      const { signal } = controller;
      void api
        .unread(projects, refresh, signal)
        .then((value) => {
          if (signal.aborted) return;
          setSummary(value);
          if (value.pending)
            timer = setTimeout(() => load(false), 1000 + Math.random() * 1000);
        })
        .catch(() => {
          if (!signal.aborted) setSummary({ count: null, pending: false });
        });
    };
    const refresh = () => load(true);
    refresh();
    globalThis.addEventListener(THREADS_CHANGED, refresh);
    globalThis.addEventListener("focus", refresh);
    return () => {
      clearTimeout(timer);
      controller?.abort();
      globalThis.removeEventListener(THREADS_CHANGED, refresh);
      globalThis.removeEventListener("focus", refresh);
    };
  }, [api, projects]);
  return <ThreadUnreadBadge {...summary} />;
}
