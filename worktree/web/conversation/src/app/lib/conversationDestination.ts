// Where a conversation row goes (decisions.md §19.4).
//
// One answer for the sidebar, the command palette and anything else that
// offers a conversation: a linked one belongs to its issue page with the chat
// open in the right panel, and an unlinked one is still a chat of its own.
// `/chat/c/:id` redirects the same way for a pasted link, so this is the
// destination those surfaces link to directly rather than the only place the
// rule lives.
//
// Its own module because both the shell and the command palette need it and
// the palette is mounted *by* the shell: a shared helper in `App.tsx` would
// make the two import one another.
export type ConversationDestination =
  | {
      readonly to: "/work/i/$workItemId";
      readonly params: { readonly workItemId: string };
      readonly search: { readonly panel: string };
    }
  | {
      readonly to: "/chat/c/$conversationId";
      readonly params: { readonly conversationId: string };
    };

export function conversationDestination(conversation: {
  readonly id: string;
  readonly work_item_id: string | null;
}): ConversationDestination {
  const workItem = conversation.work_item_id;
  return workItem === null
    ? { to: "/chat/c/$conversationId", params: { conversationId: conversation.id } }
    : {
        to: "/work/i/$workItemId",
        params: { workItemId: workItem },
        search: { panel: "conversation" },
      };
}
