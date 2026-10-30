import { useState } from "react";
import { ThreadAgentBadge, ThreadFiltersBar, ThreadReplyComposer, ThreadUnreadBadge } from "../../../app/threads/ThreadsPage.tsx";
import type { ThreadFilters } from "../../../app/threads/api.ts";
import { Badge } from "../../../components/ui/badge.tsx";
import type { GalleryDoc } from "../specimen.tsx";

function Filters() {
  const [value, setValue] = useState<ThreadFilters>({ status: "open" });
  return <ThreadFiltersBar value={value} onChange={setValue} />;
}

function Reply() {
  const [posted, setPosted] = useState("");
  return <><ThreadReplyComposer name="Alex" onPost={async (kind, body) => setPosted(`${kind}: ${body}`)} />{posted ? <p role="status">Posted {posted}</p> : null}</>;
}

export const teamThreads: GalleryDoc = {
  meta: { name: "Team threads", kind: "surface", group: "Workspace", source: "src/app/threads/ThreadsPage.tsx" },
  specimens: [
    { id: "filters", title: "Thread list filters", note: "Search, status and scope filters retain each other's selections.", render: () => <Filters /> },
    { id: "attribution", title: "Agent attribution and unread threads", render: () => <div className="flex flex-wrap items-center gap-2"><ThreadAgentBadge post={{ author_name: "Alex", acting_for: "principal_example", client_name: "Example client" }} /><Badge variant="info">Finding</Badge><ThreadUnreadBadge count={3} /></div> },
    { id: "reply", title: "Typed reply", note: "A markdown reply with its post type and the shared phone controls.", render: () => <Reply /> },
  ],
};
