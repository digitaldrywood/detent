import React from "react";
import { ExternalLinkIcon, LinkIcon, PlusIcon } from "lucide-react";

import { MenuGroup, MenuGroupLabel, MenuItem, MenuSeparator } from "../../components/ui/menu.tsx";

export type ProjectActionId = "create-issue" | "open-issue" | "copy-link";

/** One row of the conversation group: not a command, a thing this page can do. */
export interface ConversationAction {
  readonly id: ProjectActionId;
  readonly name: string;
  readonly icon: "plus" | "link" | "external";
}

export function conversationActions(input: {
  /** False for a read-only project or an already linked chat. */
  readonly canCreateIssue: boolean;
  readonly issueIdentifier: string | null;
}): readonly ConversationAction[] {
  const actions: ConversationAction[] = [];
  if (input.canCreateIssue) {
    actions.push({ id: "create-issue", name: "Create linked issue", icon: "plus" });
  }
  if (input.issueIdentifier !== null) {
    actions.push({
      id: "open-issue",
      name: `Open issue ${input.issueIdentifier}`,
      icon: "link",
    });
  }
  actions.push({ id: "copy-link", name: "Copy link", icon: "external" });
  return actions;
}

function ConversationActionIcon({ icon }: { icon: ConversationAction["icon"] }): React.ReactElement {
  if (icon === "link") return <LinkIcon className="size-4" />;
  if (icon === "external") return <ExternalLinkIcon className="size-4" />;
  return <PlusIcon className="size-4" />;
}

export interface ConversationActionsValue {
  readonly actions: readonly ConversationAction[];
  readonly run: (action: ConversationAction) => void;
}

const NO_CONVERSATION_ACTIONS: ConversationActionsValue = {
  actions: [],
  run: () => {},
};

const ConversationActionsContext = React.createContext<ConversationActionsValue>(
  NO_CONVERSATION_ACTIONS,
);

export function ConversationActionsProvider({
  value,
  children,
}: {
  readonly value: ConversationActionsValue;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <ConversationActionsContext.Provider value={value}>
      {children}
    </ConversationActionsContext.Provider>
  );
}

/**
 * The rows on offer here. The copied control asks so it knows whether to draw
 * a menu at all: upstream's third branch is a bare button with no menu, which
 * is right for a project with nothing to list and wrong once Detent has three
 * things to list.
 */
export function useConversationActions(): readonly ConversationAction[] {
  return React.useContext(ConversationActionsContext).actions;
}

export function ConversationActionsMenuGroup({
  itemClassName,
}: {
  readonly itemClassName: string;
}): React.ReactElement | null {
  const { actions, run: onRun } = React.useContext(ConversationActionsContext);
  if (actions.length === 0) return null;
  return (
    <>
      <MenuSeparator />
      <MenuGroup>
        <MenuGroupLabel>This conversation</MenuGroupLabel>
        {actions.map((action) => (
          <MenuItem
            key={action.id}
            className={itemClassName}
            data-testid={`header-action-${action.id}`}
            onClick={() => onRun(action)}
          >
            <ConversationActionIcon icon={action.icon} />
            <span className="truncate">{action.name}</span>
          </MenuItem>
        ))}
      </MenuGroup>
    </>
  );
}
