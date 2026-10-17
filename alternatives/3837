import type { SlashCommand } from "./slashCommands.ts";

export const CHAT_PROMPTS = {
  plan: { label: "Plan the work", prompt: "Plan the work in this chat. Identify the steps, dependencies, and how we will verify the result." },
  triage: { label: "Review the board", prompt: "Review this project's board and tell me what needs attention first and why." },
  why: { label: "Why is this Blocked?", prompt: "Why is this Blocked?" },
  status: { label: "Summarize current status", prompt: "Summarize what is running and what is blocked in this project." },
  history: { label: "Summarize the history", prompt: "Summarize the history" },
  readiness: { label: "What is left before it can start?", prompt: "What is left before it can start?" },
  split: {
    label: "Split into smaller issues",
    prompt: "Use the split-issue skill to break this issue into smaller issues that can each land on their own. Wire up the dependencies so independent pieces can run in parallel, and show me the whole split as one proposal so I can confirm it once.",
  },
  file: { label: "File a new issue", prompt: "Help me file a new issue in this project. Ask for any missing details, then use propose_file_issue to show an action card for my confirmation. Do not write without confirmation." },
  move: { label: "Move an item", prompt: "Help me move an item to another lane. Ask which item and lane if needed, then use propose_move_item to show an action card for my confirmation. Do not write without confirmation." },
  priority: { label: "Set priority", prompt: "Help me set an item's priority. Ask which item and priority if needed, then use propose_set_priority to show an action card for my confirmation. Do not write without confirmation." },
  "stop-run": { label: "Stop a runner attempt", prompt: "Help me stop a runner attempt. Ask which active attempt if needed, then use propose_stop_run to show an action card for my confirmation. Do not stop it without confirmation." },
  admit: { label: "Admit a Backlog item", prompt: "Help me admit a Backlog item to Todo. Review its admission criteria and ask which item if needed, then use propose_backlog_admission to show an action card for my confirmation. Do not write without confirmation." },
  maintenance: { label: "File a maintenance issue", prompt: "Help me file a maintenance issue. Ask for any missing details, then use propose_maintenance_issue to show an action card for my confirmation. Do not write without confirmation." },
} as const;

export const ISSUE_QUESTIONS = [CHAT_PROMPTS.why, CHAT_PROMPTS.history, CHAT_PROMPTS.readiness, CHAT_PROMPTS.split];

export function chatSlashCommands(options: {
  readonly issue: boolean;
  readonly coordinator: boolean;
  readonly insert: (prompt: string) => void;
  readonly send: (prompt: string) => void;
}): readonly SlashCommand[] {
  const starters = (["plan", "triage", "why", "status"] as const).map((name) => ({
    name,
    description: CHAT_PROMPTS[name].label,
    run: () => options.insert(CHAT_PROMPTS[name].prompt),
  }));
  if (!options.coordinator) return starters;
  const actions = (["split", "file", "move", "priority", "stop-run", "admit", "maintenance"] as const)
    .filter((name) => name !== "split" || options.issue)
    .map((name) => ({ name, description: CHAT_PROMPTS[name].label, run: () => options.send(CHAT_PROMPTS[name].prompt) }));
  return [...starters, ...actions];
}
