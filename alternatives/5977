import type { NativeDependency } from "../../../contracts/work.ts";
import type { WorkItemView } from "./model.ts";

export function dependencyLabel(link: NativeDependency): string {
  return link.number ? `#${link.number} ${link.title ?? "Issue"}` : "Related issue";
}

export function waitingLabel(item: WorkItemView): string {
  const blocker = item.blockers?.find((link) => !link.terminal && link.work_item_id === item.blockedBy[0]);
  return item.blockedBy.length === 1 && blocker?.number ? `Waiting on #${blocker.number}` : `Waiting on ${item.blockedBy.length}`;
}
