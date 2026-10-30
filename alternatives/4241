import { useEffect, useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { Button } from "../../components/ui/button.tsx";
import { useThreadsApi, type TeamThread } from "./api.ts";

export function IssueThreads({ issueId }: { issueId: string }) {
  const api = useThreadsApi();
  const navigate = useNavigate();
  const [threads, setThreads] = useState<readonly TeamThread[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [error, setError] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    void api
      .list({ issue_id: issueId }, controller.signal)
      .then((page) => {
        setThreads(page.threads);
        setCursor(page.next_cursor);
      })
      .catch(() => {
        if (!controller.signal.aborted) setError(true);
      });
    return () => controller.abort();
  }, [api, issueId]);
  return (
    <section className="my-4 flex flex-col gap-2" aria-label="Issue threads">
      <h2 className="text-sm font-medium">Threads</h2>
      {error ? (
        <p role="status" className="text-sm text-muted-foreground">
          Threads could not be loaded.
        </p>
      ) : threads.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No shared threads for this issue.
        </p>
      ) : (
        threads.map((thread) => (
          <Button
            variant="link"
            key={thread.id}
            onClick={() =>
              void navigate({
                to: "/threads/$thread",
                params: { thread: thread.id },
              })
            }
          >
            {thread.title}
          </Button>
        ))
      )}
      {cursor ? (
        <Button
          variant="ghost"
          onClick={() => {
            void api
              .list({ issue_id: issueId, cursor })
              .then((page) => {
                setThreads((old) => [...old, ...page.threads]);
                setCursor(page.next_cursor);
              })
              .catch(() => setError(true));
          }}
        >
          More threads
        </Button>
      ) : null}
    </section>
  );
}
