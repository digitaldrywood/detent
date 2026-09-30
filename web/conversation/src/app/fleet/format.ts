/** A local wall-clock time, for a window that ends or a lease that expires. */
export function formatLocalTime(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  return new Intl.DateTimeFormat(undefined, {
    hour: "numeric",
    minute: "2-digit",
    month: "short",
    day: "numeric",
  }).format(at);
}

const UNITS: readonly (readonly [Intl.RelativeTimeFormatUnit, number])[] = [
  ["second", 1000],
  ["minute", 60 * 1000],
  ["hour", 60 * 60 * 1000],
  ["day", 24 * 60 * 60 * 1000],
];

/**
 * "4 minutes ago", "in 3 minutes". `now` is injectable so a test does not
 * depend on the wall clock.
 */
export function formatRelativeTime(iso: string, now: number = Date.now()): string {
  const at = new Date(iso).getTime();
  if (Number.isNaN(at)) return iso;
  const delta = at - now;
  const magnitude = Math.abs(delta);
  let unit: Intl.RelativeTimeFormatUnit = "second";
  let size = 1000;
  for (const [candidate, ms] of UNITS) {
    if (magnitude >= ms) {
      unit = candidate;
      size = ms;
    }
  }
  const format = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  return format.format(Math.round(delta / size), unit);
}
