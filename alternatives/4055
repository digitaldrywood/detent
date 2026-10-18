import * as Schema from "effect/Schema";

import { getLocalStorageItem, setLocalStorageItem } from "../../hooks/useLocalStorage.ts";

const STORAGE_KEY = "detent:usage-page-preferences:v1";
const UsagePagePreferencesSchema = Schema.Struct({
  metric: Schema.Literals(["cost", "tokens", "limits", "runners"]),
  windowDays: Schema.Literals([1, 7, 30, 90]),
});
export type UsagePagePreferences = typeof UsagePagePreferencesSchema.Type;

export function readUsagePagePreferences(): UsagePagePreferences {
  try {
    return (
      getLocalStorageItem(STORAGE_KEY, UsagePagePreferencesSchema) ?? {
        metric: "cost",
        windowDays: 30,
      }
    );
  } catch (error) {
    console.error("Could not read Usage page preferences.", error);
    return { metric: "cost", windowDays: 30 };
  }
}

export function saveUsagePagePreferences(preferences: UsagePagePreferences): void {
  try {
    setLocalStorageItem(STORAGE_KEY, preferences, UsagePagePreferencesSchema);
  } catch (error) {
    console.error("Could not save Usage page preferences.", error);
  }
}
