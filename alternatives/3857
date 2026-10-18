import { useContext, useSyncExternalStore } from "react";

import type { AccountBootstrap } from "../../contracts/account.ts";
import { ClientContext } from "../client.ts";

const snapshots = new WeakMap<AccountBootstrap, { unread: boolean; listeners: Set<() => void> }>();

function snapshot(account: AccountBootstrap) {
  let value = snapshots.get(account);
  if (value === undefined) {
    value = { unread: account.has_unread ?? false, listeners: new Set() };
    snapshots.set(account, value);
  }
  return value;
}

export function setReleaseNotesUnread(account: AccountBootstrap, unread: boolean): void {
  const value = snapshot(account);
  if (value.unread === unread) return;
  value.unread = unread;
  for (const listener of value.listeners) listener();
}

export function useReleaseNotesUnread(): boolean {
  const account = useContext(ClientContext)?.account ?? null;
  const value = account === null ? null : snapshot(account);
  return useSyncExternalStore(
    (listener) => {
      value?.listeners.add(listener);
      return () => { value?.listeners.delete(listener); };
    },
    () => value?.unread ?? false,
  );
}
