import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { ToggleGroup, ToggleGroupItem } from "../../components/ui/toggle-group.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { setReleaseNotesUnread } from "../account/releaseNotes.ts";
import { useMutation, useResource } from "../account/useResource.ts";
import { SettingsPageContainer, SettingsRow, SettingsSearchTarget, SettingsSection } from "./settingsLayout.tsx";

const categories = { New: "info", Improved: "secondary", Fixed: "success", Removed: "warning" } as const;
const filters = ["All", "New", "Improved", "Fixed"] as const;
type Filter = typeof filters[number];
const linkClassName = "text-primary underline-offset-4 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

export function WhatsNewSettings(): React.ReactElement {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const feed = useResource(() => api.releaseNotes(), [api]);
  const [filter, setFilter] = React.useState<Filter>("All");
  const filterRef = React.useRef<HTMLButtonElement>(null);
  const seenRetryRef = React.useRef<HTMLButtonElement>(null);
  const [markedSeen, setMarkedSeen] = React.useState(false);
  const opened = React.useRef(false);
  const seen = useMutation(async () => {
    const page = await api.markReleaseNotesSeen();
    if (account !== null) setReleaseNotesUnread(account, page.has_unread);
    if (document.activeElement === seenRetryRef.current) filterRef.current?.focus();
    setMarkedSeen(true);
    return page;
  });
  const markSeen = seen.call;
  React.useEffect(() => {
    if (feed.value === undefined || opened.current) return;
    opened.current = true;
    void markSeen();
  }, [feed.value, markSeen]);
  const older = useMutation(async () => {
    const current = feed.value;
    if (!current?.next_cursor) return null;
    const page = await api.releaseNotes(current.next_cursor);
    feed.set({ ...page, builds: [...current.builds, ...page.builds] });
    return page;
  });
  const version = account?.version;
  const runningVersion = version ? `v${version.replace(/^v/, "")}` : null;

  return (
    <SettingsPageContainer>
      <SettingsSearchTarget id="settings-whats-new" className="space-y-8">
        <div className="flex flex-wrap items-center justify-between gap-3 px-3 sm:px-4">
          <p className="text-[13px] text-muted-foreground">
            {runningVersion ? `Running ${runningVersion}. ` : ""}Each change links to its pull request.
          </p>
          <span role="status" className="sr-only">{seen.pending ? "Marking release notes as seen…" : seen.error ? `Could not mark release notes as seen. ${seen.error.message}` : markedSeen ? "Release notes marked as seen." : ""}</span>
          <ToggleGroup aria-label="Filter release notes" value={[filter]} onValueChange={(values) => {
            const next = values[0];
            if (filters.includes(next as Filter)) setFilter(next as Filter);
          }}>
            {filters.map((value) => <ToggleGroupItem key={value} ref={value === filter ? filterRef : undefined} value={value}>{value}</ToggleGroupItem>)}
          </ToggleGroup>
        </div>
        {feed.value === undefined ? (
          <SettingsSection title="What's new">
            <SettingsRow title={feed.error ? "Release notes are not available" : "Loading release notes…"}
              description={feed.error?.message}
              control={feed.error ? <Button size="sm" variant="outline" onClick={() => void feed.refresh()}>Try again</Button> : null} />
          </SettingsSection>
        ) : feed.value.builds.length === 0 ? (
          <SettingsSection title="What's new"><SettingsRow title="No release notes yet" /></SettingsSection>
        ) : feed.value.builds.map((build) => {
          const entries = build.entries.filter((entry) => filter === "All" || entry.category === filter);
          if (filter !== "All" && entries.length === 0) return null;
          return (
            <SettingsSection key={build.version} title={
              <span className="flex min-w-0 flex-wrap items-center gap-2">
                {build.unread ? <span aria-label="Unread release" role="img" className="size-[7px] shrink-0 rounded-full bg-primary" /> : null}
                <a className={`${linkClassName} break-all`} href={`https://github.com/digitaldrywood/detent/releases/tag/${encodeURIComponent(build.version)}`} target="_blank" rel="noopener noreferrer">{build.version}</a>
                {build.running ? <Badge variant="success">Running now</Badge> : null}
                {build.patch ? <Badge variant="outline">Patch build</Badge> : null}
              </span>
            } headerAction={<time className="text-right text-xs text-muted-foreground" dateTime={build.build_time}>{new Date(build.build_time).toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}</time>}>
              {entries.map((entry) => <SettingsRow key={entry.pr_number} title={
                <span className="flex flex-col items-start gap-1 sm:block"><Badge variant={categories[entry.category]}>{entry.category}</Badge>{" "}<span>{entry.text}</span></span>
              } description={<a href={entry.pr_url} className={linkClassName} target="_blank" rel="noopener noreferrer">#{entry.pr_number}</a>} />)}
              {build.internal_count > 0 ? <SettingsRow title={<span className="text-xs font-normal text-muted-foreground">{build.internal_count} internal changes not shown (tests, CI, release tooling)</span>} /> : null}
            </SettingsSection>
          );
        })}
        {feed.value && filter !== "All" && !feed.value.builds.some((build) => build.entries.some((entry) => entry.category === filter)) ? <p role="status" className="px-3 text-sm text-muted-foreground sm:px-4">No {filter.toLowerCase()} changes in these builds.</p> : null}
        {seen.error || seen.pending ? <SettingsRow title={seen.pending ? "Marking release notes as seen…" : "Could not mark release notes as seen"} description={seen.error?.message} control={<Button ref={seenRetryRef} size="sm" variant="outline" aria-disabled={seen.pending} onClick={() => { if (!seen.pending) void markSeen(); }}>Try again</Button>} /> : null}
        {older.error ? <p role="alert" className="px-3 text-sm text-muted-foreground sm:px-4">{older.error.message}</p> : null}
        {feed.value?.next_cursor ? <Button size="sm" variant="outline" disabled={older.pending} onClick={() => void older.call()}>{older.pending ? "Loading…" : "Load older builds"}</Button> : null}
      </SettingsSearchTarget>
    </SettingsPageContainer>
  );
}
