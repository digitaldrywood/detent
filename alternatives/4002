import React from "react";
import { CirclePlusIcon, SearchIcon } from "lucide-react";
import { Combobox, ComboboxInput, ComboboxGroup, ComboboxGroupLabel, ComboboxItem, ComboboxList } from "../../components/ui/combobox.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Label } from "../../components/ui/label.tsx";
import type { GitHubRepositories, RunnerEligibility } from "../../contracts/account.ts";
import { formatRelativeTime } from "../fleet/format.ts";

export function validRepositoryName(value: string): boolean {
  return /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value) && value.split("/").every((part) => part.length <= 100 && part !== "." && part !== "..");
}

export function RepositoryPicker({ runner, value, onChange, refresh, refreshing, disabled = false, github, connectGitHub, connecting = false }: {
  readonly runner: RunnerEligibility | undefined;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly refresh: () => void;
  readonly refreshing: boolean;
  readonly disabled?: boolean;
  readonly github?: GitHubRepositories;
  readonly connectGitHub?: () => void;
  readonly connecting?: boolean;
}): React.ReactElement {
  const [open, setOpen] = React.useState(true);
  const [showHelp, setShowHelp] = React.useState(false);
  const input = React.useRef<HTMLInputElement>(null);
  const list = runner?.repository_list;
  const query = value.trim();
  const repositories = new Map((list?.repositories ?? []).map((repo) => [`${repo.owner}/${repo.name}`.toLowerCase(), { ...repo, source: "runner" }]));
  for (const repo of github?.repositories ?? []) {
    const key = `${repo.owner}/${repo.name}`.toLowerCase();
    const existing = repositories.get(key);
    repositories.set(key, { ...(existing ?? repo), source: existing ? "both" : "github" });
  }
  const matches = [...repositories.values()].filter((repo) => `${repo.owner}/${repo.name}`.toLowerCase().includes(query.toLowerCase()));
  const groups = new Map<string, typeof matches>();
  for (const repo of matches) groups.set(repo.owner, [...(groups.get(repo.owner) ?? []), repo]);
  const typed = matches.length === 0 && query.length > 0;
  const items = typed ? [query] : matches.map((repo) => `${repo.owner}/${repo.name}`);
  const online = (runner?.runner.connection_health ?? runner?.runner.health) === "online";
  return <div className="space-y-2">
    <Label htmlFor="setup-repository">Search or type owner/name</Label>
    <Combobox inline={!disabled} items={items} filter={null} value={validRepositoryName(query) ? query : null} inputValue={value}
      onInputValueChange={onChange} onValueChange={(name) => { if (name) onChange(name); }}
      open={open && !disabled} onOpenChange={setOpen} disabled={disabled}>
      <ComboboxInput id="setup-repository" ref={input} placeholder="Search or type owner/name" showTrigger={false} startAddon={<SearchIcon />} />
      {open && !disabled ? <div className="max-h-64 overflow-y-auto rounded-lg border border-border">
        <ComboboxList>
          {typed ? <ComboboxItem value={query} disabled={!validRepositoryName(query)}>Use {query} as typed</ComboboxItem>
            : [...groups].map(([owner, repos]) => <ComboboxGroup key={owner}>
              <ComboboxGroupLabel>{owner}</ComboboxGroupLabel>
              {repos.map((repo) => <ComboboxItem key={`${repo.owner}/${repo.name}`} value={`${repo.owner}/${repo.name}`}>
                <div className="flex min-w-0 flex-1 flex-col gap-0.5 sm:flex-row sm:items-center sm:gap-2">
                  <span className="min-w-0 flex-1 truncate">{repo.owner}/{repo.name}</span>
                  <span className="flex shrink-0 items-center gap-2">
                    {repo.source !== "runner" ? <Badge variant="outline">{repo.source === "both" ? "Runner + GitHub App" : "GitHub App"}</Badge> : null}
                    {repo.private ? <Badge variant="outline">Private</Badge> : null}
                    {repo.pushed_at ? <time className="text-xs text-muted-foreground" dateTime={repo.pushed_at}>pushed {formatRelativeTime(repo.pushed_at)}</time> : null}
                  </span>
                </div>
              </ComboboxItem>)}
            </ComboboxGroup>)}
        </ComboboxList>
        {items.length === 0 ? <p role="status" className="p-3 text-sm text-muted-foreground">{refreshing || list?.pending ? "Loading repositories…" : list?.reason || "Type owner/name to use a repository."}</p> : null}
        {github?.available && connectGitHub && (matches.length === 0 || showHelp) ? <Button variant="ghost" size="sm-multiline" className="w-full justify-start rounded-none border-t border-border px-3 py-3 text-left" disabled={connecting} onClick={connectGitHub}>
          <CirclePlusIcon aria-hidden="true" />
          <span className="flex min-w-0 flex-col whitespace-normal">
            <span>{connecting ? "Connecting…" : github.reconnect ? "Reconnect GitHub to list more repositories" : "Connect GitHub to list more repositories"}</span>
            <span className="text-xs font-normal text-muted-foreground">Installs the Detent Cloud GitHub App on the accounts you choose. Optional.</span>
          </span>
        </Button> : null}
      </div> : null}
    </Combobox>
    <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
      <span>Listed by {runner?.runner.display_name ?? "your runner"}{github?.connected ? " and the Detent Cloud GitHub App" : ""} · {list?.listed_at ? `updated ${formatRelativeTime(list.listed_at)}` : "not yet updated"}</span>
      <div className="flex items-center gap-2">
        <Button variant="link" size="sm" onClick={() => { setShowHelp(true); setOpen(true); input.current?.focus(); }}>Not listed?</Button>
        <Button variant="link" size="sm" disabled={!runner || refreshing || list?.pending} onClick={refresh}>Refresh</Button>
      </div>
    </div>
    {!online ? <p role="status" className="text-sm text-muted-foreground">The runner is offline. You can still type owner/name.</p> : null}
    {github?.reconnect ? <p role="status" className="text-sm text-muted-foreground">Reconnect GitHub to update its repositories. You can still use the runner’s list or type owner/name.</p> : null}
    {showHelp ? <p role="status" className="text-sm text-muted-foreground">Type owner/name even if it is not listed. The runner verifies access when it clones the repository.</p> : null}
  </div>;
}
