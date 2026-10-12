import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { Building2, UserSearch } from "lucide-react";

import {
  Command, CommandDialog, CommandDialogPopup, CommandEmpty, CommandInput,
  CommandItem, CommandList, CommandPanel,
} from "../../components/ui/command.tsx";
import { type AccountError } from "../account/api.ts";
import { type PlatformOrganization } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import { usePlatformResource } from "./usePlatformResource.ts";

type SearchResult = { id: string; label: string; description: string; kind: "tenant" | "user" };

export function PlatformSearch({ open, onOpenChange, tenants, tenantError }: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly tenants: readonly PlatformOrganization[];
  readonly tenantError: AccountError | null;
}): React.ReactElement {
  const api = useEntryApi();
  const navigate = useNavigate();
  const [query, setQuery] = React.useState("");
  const term = query.trim().toLowerCase();
  const validEmail = Array.from(term).length >= 3;
  const users = usePlatformResource(() => open && validEmail ? api.platformUsers(term) : Promise.resolve(null), [api, open, term, validEmail]);
  React.useEffect(() => {
    const shortcut = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k" && !event.defaultPrevented) {
        event.preventDefault();
        onOpenChange(!open);
      }
    };
    window.addEventListener("keydown", shortcut);
    return () => window.removeEventListener("keydown", shortcut);
  }, [open, onOpenChange]);
  React.useEffect(() => { if (!open) setQuery(""); }, [open]);
  const results: SearchResult[] = term === "" ? [] : [
    ...tenants.filter((tenant) => [tenant.name, tenant.id, tenant.creator_email].some((value) => value.toLowerCase().includes(term)))
      .map((tenant): SearchResult => ({ id: tenant.id, label: tenant.name, description: tenant.creator_email || tenant.id, kind: "tenant" })),
    ...(validEmail && !users.loading ? users.value?.users ?? [] : []).map((user): SearchResult => ({ id: user.email, label: user.email, description: "User", kind: "user" })),
  ];
  const select = (result: SearchResult) => {
    onOpenChange(false);
    void navigate((result.kind === "tenant" ? { to: "/platform/tenants/$organization", params: { organization: result.id } } :
      { to: "/platform/users/$email", params: { email: result.id } }) as never);
  };
  return (
    <CommandDialog open={open} onOpenChange={onOpenChange}>
      <CommandDialogPopup aria-label="Find tenant or user">
        <Command items={results} filter={null} value={query} onValueChange={setQuery} itemToStringValue={(item) => (item as SearchResult).label}>
          <CommandInput aria-label="Find tenant or user" placeholder="Search tenant name, ID, owner or user email" />
          <CommandPanel>
            <Problem message={tenantError?.message ?? users.error?.message ?? null} />
            <CommandEmpty>{term === "" ? "Type to find a tenant or user." : users.loading && validEmail ? "Searching…" : "No tenants or users match."}</CommandEmpty>
            <CommandList>
              {(item: SearchResult) => <CommandItem key={`${item.kind}:${item.id}`} value={item} onClick={() => select(item)}>
                {item.kind === "tenant" ? <Building2 /> : <UserSearch />}
                <div className="min-w-0"><div className="truncate">{item.label}</div><div className="truncate text-xs text-muted-foreground">{item.description}</div></div>
              </CommandItem>}
            </CommandList>
          </CommandPanel>
        </Command>
      </CommandDialogPopup>
    </CommandDialog>
  );
}
