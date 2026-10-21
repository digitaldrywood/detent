import {
  ChevronDownIcon,
  CloudDownloadIcon,
  CloudUploadIcon,
  GitCommitIcon,
  InfoIcon,
} from "lucide-react";
import React, { memo, useCallback, useMemo, useState, type ReactElement } from "react";

import { NO_GIT_GROUP_REASON, useHeaderActions } from "../app/adapters/headerActions.tsx";
import { gitItemBlockedReason } from "../app/adapters/gitStatus.ts";
import type { GitStackedAction, VcsStatusResult } from "../contracts/ui.ts";
import { getSourceControlPresentation } from "../sourceControlPresentation.ts";
import {
  buildMenuItems,
  resolveQuickAction,
  type GitActionIconName,
  type GitActionMenuItem,
  type GitQuickAction,
} from "./GitActionsControl.logic.ts";
import { Button } from "./ui/button.tsx";
import {
  Dialog,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "./ui/dialog.tsx";
import { Group, GroupSeparator } from "./ui/group.tsx";
import { Label } from "./ui/label.tsx";
import { Menu, MenuItem, MenuPopup, MenuTrigger } from "./ui/menu.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "./ui/popover.tsx";
import { Textarea } from "./ui/textarea.tsx";

function getMenuActionDisabledReason({
  item,
  gitStatus,
  isBusy,
  hasPrimaryRemote,
}: {
  item: GitActionMenuItem;
  gitStatus: VcsStatusResult | null;
  isBusy: boolean;
  hasPrimaryRemote: boolean;
}): string | null {
  if (!item.disabled) return null;
  if (isBusy) return "Git action in progress.";
  if (!gitStatus) return "Git status is unavailable.";

  const hasBranch = gitStatus.refName !== null;
  const hasChanges = gitStatus.hasWorkingTreeChanges;
  const hasOpenPr = gitStatus.pr?.state === "open";
  const isAhead = gitStatus.aheadCount > 0;
  const isBehind = gitStatus.behindCount > 0;
  const terminology = getSourceControlPresentation(gitStatus.sourceControlProvider).terminology;

  if (item.id === "commit") {
    if (!hasChanges) {
      return "Worktree is clean. Make changes before committing.";
    }
    return "Commit is currently unavailable.";
  }

  if (item.id === "push") {
    if (!hasBranch) {
      return "Detached HEAD: checkout a refName before pushing.";
    }
    if (hasChanges) {
      return "Commit or stash local changes before pushing.";
    }
    if (isBehind) {
      return "Branch is behind upstream. Pull/rebase before pushing.";
    }
    if (!gitStatus.hasUpstream && !hasPrimaryRemote) {
      return 'Add an "origin" remote before pushing.';
    }
    if (!isAhead) {
      return "No local commits to push.";
    }
    return "Push is currently unavailable.";
  }

  if (hasOpenPr) {
    return `View ${terminology.singular} is currently unavailable.`;
  }
  if (!hasBranch) {
    return `Detached HEAD: checkout a refName before creating a ${terminology.singular}.`;
  }
  if (hasChanges) {
    return `Commit local changes before creating a ${terminology.singular}.`;
  }
  if (!gitStatus.hasUpstream && !hasPrimaryRemote) {
    return `Add an "origin" remote before creating a ${terminology.singular}.`;
  }
  if (!isAhead) {
    return `No local commits to include in a ${terminology.singular}.`;
  }
  if (isBehind) {
    return `Branch is behind upstream. Pull/rebase before creating a ${terminology.singular}.`;
  }
  return `Create ${terminology.singular} is currently unavailable.`;
}

const COMMIT_DIALOG_TITLE = "Commit changes";
const COMMIT_DIALOG_DESCRIPTION =
  "Review and confirm your commit. Leave the message blank to auto-generate one.";

function GitActionItemIcon({
  icon,
  SourceControlIcon,
}: {
  icon: GitActionIconName;
  SourceControlIcon: ReturnType<typeof getSourceControlPresentation>["Icon"];
}) {
  if (icon === "commit") return <GitCommitIcon />;
  if (icon === "push") return <CloudUploadIcon />;
  return <SourceControlIcon />;
}

function GitQuickActionIcon({
  quickAction,
  SourceControlIcon,
}: {
  quickAction: GitQuickAction;
  SourceControlIcon: ReturnType<typeof getSourceControlPresentation>["Icon"];
}) {
  const iconClassName = "size-3.5";
  if (quickAction.kind === "open_pr") return <SourceControlIcon className={iconClassName} />;
  if (quickAction.kind === "open_publish") return <CloudUploadIcon className={iconClassName} />;
  if (quickAction.kind === "run_pull") return <CloudDownloadIcon className={iconClassName} />;
  if (quickAction.kind === "run_action") {
    if (quickAction.action === "commit") return <GitCommitIcon className={iconClassName} />;
    if (quickAction.action === "push" || quickAction.action === "commit_push") {
      return <CloudUploadIcon className={iconClassName} />;
    }
    return <SourceControlIcon className={iconClassName} />;
  }
  if (quickAction.label === "Commit") return <GitCommitIcon className={iconClassName} />;
  if (quickAction.label === "Push") return <CloudUploadIcon className={iconClassName} />;
  return <InfoIcon className={iconClassName} />;
}

const ROW_TEST_ID: Readonly<Record<GitActionMenuItem["id"], string>> = {
  commit: "header-git-commit",
  push: "header-git-push",
  pr: "header-git-pr",
};

export default memo(function GitActionsControl(_props: {
  gitCwd?: string | null;
  activeThreadRef?: unknown;
  onOpenPullRequest?: ((number: number) => void) | undefined;
  draftId?: string;
}): ReactElement {
  const { git, openPullRequest } = useHeaderActions();
  const status = git?.status ?? null;
  const isBusy = git?.busy ?? false;
  const statusError = git?.statusError ?? null;

  const presentation = useMemo(
    () => getSourceControlPresentation(status?.sourceControlProvider),
    [status?.sourceControlProvider],
  );
  const SourceControlIcon = presentation.Icon;
  const hasPrimaryRemote = status?.hasPrimaryRemote ?? false;
  const isDefaultRef = status?.isDefaultRef ?? false;

  const menuItems = useMemo(
    () => buildMenuItems(status, isBusy, hasPrimaryRemote),
    [hasPrimaryRemote, isBusy, status],
  );

  const policyReason = git === null ? NO_GIT_GROUP_REASON : null;
  const groupReason =
    policyReason ?? (git === null ? null : gitItemBlockedReason("commit", git.policy));

  const quickAction = useMemo(
    () =>

      status === null && groupReason === null && !isBusy
        ? ({
            label: `Commit, push & ${presentation.terminology.shortLabel}`,
            disabled: false,
            kind: "run_action",
            action: "commit_push_pr",
          } satisfies GitQuickAction)
        : resolveQuickAction(status, isBusy, isDefaultRef, hasPrimaryRemote),
    [groupReason, hasPrimaryRemote, isBusy, isDefaultRef, presentation.terminology.shortLabel, status],
  );
  const quickActionDisabledReason =
    groupReason ??
    (quickAction.disabled ? (quickAction.hint ?? "This action is currently unavailable.") : null);

  const [commitDialogOpen, setCommitDialogOpen] = useState(false);
  const [commitMessage, setCommitMessage] = useState("");

  const runAction = useCallback(
    (action: GitStackedAction, message: string) => {
      git?.run(action, message);
    },
    [git],
  );

  const closeCommitDialog = useCallback(() => {
    setCommitDialogOpen(false);
    setCommitMessage("");
  }, []);

  const runQuickAction = useCallback(() => {
    if (quickAction.kind === "open_pr") {
      openPullRequest?.();
      return;
    }
    if (quickAction.kind === "run_action" && quickAction.action !== undefined) {
      // Every action that commits needs a message, and only the reader can
      // write one here (see the file header).
      if (quickAction.action === "commit" || quickAction.action.startsWith("commit")) {
        setCommitDialogOpen(true);
        return;
      }
      runAction(quickAction.action, "");
      return;
    }
    if (quickAction.kind === "run_pull") {

      return;
    }
  }, [openPullRequest, quickAction, runAction]);

  const openRowAction = useCallback(
    (item: GitActionMenuItem) => {
      if (item.kind === "open_pr") {
        openPullRequest?.();
        return;
      }
      if (item.dialogAction === "commit") {
        setCommitDialogOpen(true);
        return;
      }
      if (item.dialogAction === "push") {
        runAction("push", "");
        return;
      }
      if (item.dialogAction === "create_pr") {
        runAction("create_pr", "");
      }
    },
    [openPullRequest, runAction],
  );

  const submitCommit = useCallback(
    (event: React.FormEvent) => {
      event.preventDefault();
      const message = commitMessage.trim();
      if (message.length === 0) return;
      const action: GitStackedAction =
        quickAction.kind === "run_action" && quickAction.action?.startsWith("commit") === true
          ? quickAction.action
          : "commit";
      closeCommitDialog();
      runAction(action, message);
    },
    [closeCommitDialog, commitMessage, quickAction, runAction],
  );

  return (
    <>
      <Group aria-label="Git actions" className="shrink-0">
        {quickActionDisabledReason ? (
          <Popover>
            <PopoverTrigger
              openOnHover
              render={
                <Button
                  aria-disabled="true"
                  data-testid="header-commit-push-pr"
                  data-disabled-reason={quickActionDisabledReason}
                  className="cursor-not-allowed rounded-e-none border-e-0 ps-[8.5px] opacity-64 before:rounded-e-none"
                  size="xs"
                  variant="outline"
                />
              }
            >
              <GitQuickActionIcon
                quickAction={quickAction}
                SourceControlIcon={SourceControlIcon}
              />
              <span className="sr-only @3xl/header-actions:not-sr-only @3xl/header-actions:ml-0.5">
                {quickAction.label}
              </span>
            </PopoverTrigger>
            <PopoverPopup tooltipStyle side="bottom" align="start">
              {quickActionDisabledReason}
            </PopoverPopup>
          </Popover>
        ) : (
          <Button
            variant="outline"
            size="xs"
            className="ps-[8.5px]"
            data-testid="header-commit-push-pr"
            disabled={isBusy || quickAction.disabled}
            onClick={runQuickAction}
          >
            <GitQuickActionIcon quickAction={quickAction} SourceControlIcon={SourceControlIcon} />
            <span className="sr-only @3xl/header-actions:not-sr-only @3xl/header-actions:ml-0.5">
              {quickAction.label}
            </span>
          </Button>
        )}
        <GroupSeparator className="hidden @3xl/header-actions:block" />
        <Menu
          onOpenChange={(open) => {

            if (open) git?.refresh();
          }}
        >
          <MenuTrigger
            render={<Button aria-label="Git action options" size="icon-xs" variant="outline" />}
            data-testid="header-commit-menu"
            disabled={isBusy}
          >
            <ChevronDownIcon aria-hidden="true" className="size-4" />
          </MenuTrigger>
          <MenuPopup align="end" className="w-full">
            {menuItems.length === 0 ? (
              <MenuItem disabled data-disabled-reason={groupReason ?? "Git status is unavailable."}>
                {groupReason ?? "Git status is unavailable."}
              </MenuItem>
            ) : null}
            {menuItems.map((item) => {
              const disabledReason =
                (git === null ? policyReason : gitItemBlockedReason(item.id, git.policy)) ??
                getMenuActionDisabledReason({
                  item,
                  gitStatus: status,
                  isBusy,
                  hasPrimaryRemote,
                });
              const blocked = item.disabled || disabledReason !== null;
              if (blocked && disabledReason) {
                return (
                  <Popover key={`${item.id}-${item.label}`}>
                    <PopoverTrigger
                      openOnHover
                      nativeButton={false}
                      render={
                        <span
                          className="block w-max cursor-not-allowed"
                          data-testid={ROW_TEST_ID[item.id]}
                          data-disabled-reason={disabledReason}
                        />
                      }
                    >
                      <MenuItem className="w-full" disabled>
                        <GitActionItemIcon icon={item.icon} SourceControlIcon={SourceControlIcon} />
                        {item.label}
                      </MenuItem>
                    </PopoverTrigger>
                    <PopoverPopup tooltipStyle side="left" align="center">
                      {disabledReason}
                    </PopoverPopup>
                  </Popover>
                );
              }

              return (
                <MenuItem
                  key={`${item.id}-${item.label}`}
                  data-testid={ROW_TEST_ID[item.id]}
                  disabled={item.disabled}
                  onClick={() => {
                    openRowAction(item);
                  }}
                >
                  <GitActionItemIcon icon={item.icon} SourceControlIcon={SourceControlIcon} />
                  {item.label}
                </MenuItem>
              );
            })}
            {status?.refName === null && (
              <p className="px-2 py-1.5 text-warning text-xs">
                Detached HEAD: create and checkout a refName to enable push and pull request
                actions.
              </p>
            )}
            {status &&
              status.refName !== null &&
              !status.hasWorkingTreeChanges &&
              status.behindCount > 0 &&
              status.aheadCount === 0 && (
                <p className="px-2 py-1.5 text-warning text-xs">
                  Behind upstream. Pull/rebase first.
                </p>
              )}
            {statusError && <p className="px-2 py-1.5 text-destructive text-xs">{statusError}</p>}
          </MenuPopup>
        </Menu>
      </Group>

      <Dialog
        open={commitDialogOpen}
        onOpenChange={(open) => {
          if (!open) closeCommitDialog();
        }}
      >
        <DialogPopup className="max-w-xl">
          <form className="flex min-h-0 flex-1 flex-col" onSubmit={submitCommit}>
            <DialogHeader>
              <DialogTitle>{COMMIT_DIALOG_TITLE}</DialogTitle>
              <DialogDescription>{COMMIT_DIALOG_DESCRIPTION}</DialogDescription>
            </DialogHeader>
            <DialogPanel className="flex min-h-0 flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="dc-commit-message">Commit message</Label>
                <Textarea
                  id="dc-commit-message"
                  data-testid="header-git-commit-message"
                  rows={4}
                  value={commitMessage}
                  onChange={(event) => setCommitMessage(event.target.value)}
                  placeholder="What this commit does"
                />
              </div>
              <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1 text-sm">
                <span className="text-muted-foreground">Branch</span>
                <span className="font-medium">{status?.refName ?? "(detached HEAD)"}</span>
              </div>
            </DialogPanel>
            <DialogFooter>
              <Button variant="outline" size="sm" onClick={closeCommitDialog}>
                Cancel
              </Button>
              <Button
                render={<button type="submit" />}
                size="sm"
                data-testid="header-git-commit-submit"
                disabled={commitMessage.trim().length === 0}
                aria-disabled={commitMessage.trim().length === 0}
              >
                Commit
              </Button>
            </DialogFooter>
          </form>
        </DialogPopup>
      </Dialog>
    </>
  );
});
