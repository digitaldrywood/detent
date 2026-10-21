export interface RunnerHours {
  readonly days: string;
  readonly from: string;
  readonly until: string;
  readonly original?: string;
}

export function parseRunnerWindow(value: string): RunnerHours {
  const match = /^(\S+)\s+(\d{2}:\d{2})-(\d{2}:\d{2})$/.exec(value.trim());
  if (match === null) throw new Error(`Invalid runner hours: ${value}`);
  return { days: match[1]!, from: match[2]!, until: match[3]!, original: value };
}

export function serializeRunnerWindow(hours: RunnerHours): string {
  if (hours.original !== undefined) {
    const original = parseRunnerWindow(hours.original);
    if (original.days === hours.days && original.from === hours.from && original.until === hours.until) return hours.original;
  }
  return `${hours.days} ${hours.from}-${hours.until}`;
}
