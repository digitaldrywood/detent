export const RUNNER_HELP = {
  routing:
    "A runner needs approved project access before it can take that project's jobs. Tags and host selectors then choose eligible runners from that authorized set; matching a tag never grants project access.",
  tags:
    "Tags describe this runner, such as linux or gpu. Repository policy can require matching tags or host selectors. Tags select eligible runners; they do not grant access to a project.",
  projects:
    "These project IDs define where this runner is authorized to work. A job must also match the repository policy's tags and host selectors. Adding a tag or a home project does not grant project access.",
  limit:
    "The maximum concurrent jobs on this runner, not the number of enrolled runners. A limit of 6 allows up to 6 jobs at once if reported, shared host and provider capacity allow it. If the runner reports 4, the effective runner limit is 4. A limit of 0 pauses new jobs.",
  reported:
    "Reported capacity is the concurrent-job capacity sent by this runner in its heartbeat. The effective runner limit is the smaller of reported capacity and the saved runner capacity limit. For example, a saved limit of 6 with a report of 4 allows at most 4 jobs; a report of 0 pauses new jobs.",
  host:
    "Slots show jobs in use against the shared machine's capacity. Runners on the same host share this pool; their limits do not add more host slots. If the host has 6 slots and other runners use 4, only 2 remain, even when this runner's limit is 6.",
  provider:
    "Provider capacity shows concurrent agent jobs in use against each provider account's limit. A job needs an available account compatible with its provider and model, as well as runner and host room. For example, 2 free provider slots allow only 2 more matching jobs even if the runner limit is 6. The provider summary adds the reported account capacities across runners.",
} as const;
