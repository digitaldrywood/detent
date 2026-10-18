// What the composer's three pickers offer, worked out from the bootstrap.
//
// Written for this repository. The shapes are the contract's
// (`contracts/conversation.ts`); everything here is the reading of them the
// pickers need and the rules §14 states: "Auto" is always first and always
// preselected, a value the hub has stopped publishing is kept rather than
// silently swapped, and — the part this file adds — reasoning effort is scoped
// to the model that is selected.
//
// A provider's ladder is per model: one offers low through max, the next only
// medium. The hub publishes each model's ladder on `preferences.models[]`
// (decisions.md §14), so a model that has one shows its own levels and a model
// that has none — or "Auto", which is not one model — falls back to the fixed
// vocabulary the hub publishes for the whole organization.
import {
  AUTO_PREFERENCE,
  type PreferenceChoice,
  type PreferenceChoices,
  type TurnPreferences,
  preferenceOptions,
} from "../../contracts/index.ts";

/**
 * The effort levels Detent knows a name for. The hub labels the ones in its
 * own fixed vocabulary; a level that reaches the client only through a model's
 * ladder arrives as a bare provider token, and these are the words the
 * providers use for them.
 */
const EFFORT_LABELS: Readonly<Record<string, string>> = {
  low: "Low",
  medium: "Medium",
  high: "High",
  xhigh: "Extra High",
  max: "Max",
  ultracode: "Ultra",
};

/** A bare provider token as a human level name. */
export function effortLabel(id: string): string {
  const known = EFFORT_LABELS[id.toLowerCase()];
  if (known !== undefined) return known;
  const words = id.replace(/[_-]+/gu, " ").trim();
  return words.length === 0 ? id : words.charAt(0).toUpperCase() + words.slice(1);
}

/** The model choice with this id, or null — including for "auto". */
export function modelChoice(
  choices: PreferenceChoices | undefined,
  id: string,
): PreferenceChoice | null {
  if (id === AUTO_PREFERENCE) return null;
  return (choices?.models ?? []).find((model) => model.id === id) ?? null;
}

/** The provider a model came from, for the picker's rail and its rows. */
export function modelProvider(choice: PreferenceChoice): string | null {
  const provider = (choice.provider ?? "").trim();
  return provider.length === 0 ? null : provider;
}

/**
 * The reasoning levels the picker offers for the model that is selected.
 *
 * "Auto" leads, as it does in every picker. After it come the selected model's
 * own levels when it published any, and otherwise the fixed vocabulary the hub
 * publishes — which is what a model with no catalogue detail, and "Auto"
 * itself, have to go on.
 *
 * A level the conversation is already set to is kept even when the model does
 * not list it: a conversation must never look as though it silently changed
 * the effort it is running under.
 */
export function effortOptions(
  choices: PreferenceChoices | undefined,
  preferences: TurnPreferences,
): readonly PreferenceChoice[] {
  const model = modelChoice(choices, preferences.model);
  const ladder = model?.efforts ?? [];
  if (ladder.length === 0) {
    return preferenceOptions(choices?.efforts, preferences.reasoning_effort);
  }
  const published = ladder.map(
    (id): PreferenceChoice => ({
      id,
      label: publishedEffortLabel(choices, id),
      default: id === (model?.default_effort ?? ""),
    }),
  );
  return preferenceOptions(published, preferences.reasoning_effort);
}

/** The hub's own label for a level when it publishes one, else Detent's. */
function publishedEffortLabel(choices: PreferenceChoices | undefined, id: string): string {
  const published = (choices?.efforts ?? []).find((effort) => effort.id === id);
  return published?.label ?? effortLabel(id);
}

/**
 * The preferences after the reader picks a model.
 *
 * Selecting a model whose levels do not include the current one resets the
 * effort to that model's default, and to "Auto" when it names none. Leaving
 * the effort where it is would put the composer in a state the hub refuses:
 * `ValidatePreferences` accepts a level from the fixed vocabulary, or one the
 * *selected* model publishes, and nothing else. The picker says so on the line
 * under the levels.
 *
 * The same rule running the other way is what makes going back to "Auto" safe.
 * A model-only level — `xhigh` from a Codex ladder, say — is not in the fixed
 * vocabulary, so a reader who picked it and then chose Auto would be left
 * holding a pair the hub rejects; it resets too.
 */
export function preferencesForModel(
  choices: PreferenceChoices | undefined,
  preferences: TurnPreferences,
  modelId: string,
): TurnPreferences {
  const next: TurnPreferences = { ...preferences, model: modelId };
  if (next.reasoning_effort === AUTO_PREFERENCE) return next;
  const model = modelChoice(choices, modelId);
  const ladder = model?.efforts ?? [];
  if (ladder.length > 0) {
    return ladder.includes(next.reasoning_effort)
      ? next
      : { ...next, reasoning_effort: model?.default_effort ?? AUTO_PREFERENCE };
  }
  // No ladder — "Auto", or a model whose runner published none. The fixed
  // vocabulary is the whole of what the hub will take. An empty published
  // vocabulary is a hub that has said nothing, not one that allows nothing,
  // so it is left alone rather than reset to Auto.
  const vocabulary = choices?.efforts ?? [];
  if (vocabulary.length === 0) return next;
  return vocabulary.some((effort) => effort.id === next.reasoning_effort)
    ? next
    : { ...next, reasoning_effort: AUTO_PREFERENCE };
}

/**
 * The models the picker lists first, and the ones it shelves under Legacy.
 *
 * "Auto" is in neither. The hub publishes it at the head of the same list
 * because it is a value the picker offers, but it is not a model: it has no
 * provider to group under and no ladder to scope efforts to, and it leads the
 * list rather than sitting in a group.
 *
 * The rest keep the order the runners reported them in — a provider lists its
 * catalogue in the order it wants read, and the hub no longer sorts it — with
 * one move: the model the backend itself defaults to goes first. Alphabetical
 * order put `gpt-5.6-sol` above `gpt-6-astra`, which is the backend's own
 * pick and the row the ⌘1 hint should be on.
 */
export function partitionModels(choices: PreferenceChoices | undefined): {
  readonly current: readonly PreferenceChoice[];
  readonly legacy: readonly PreferenceChoice[];
} {
  const models = (choices?.models ?? []).filter((model) => model.id !== AUTO_PREFERENCE);
  const current = models.filter((model) => model.legacy !== true);
  const lead = current.findIndex((model) => model.backend_default === true);
  return {
    current:
      lead <= 0 ? current : [current[lead] as PreferenceChoice, ...current.filter((_, index) => index !== lead)],
    legacy: models.filter((model) => model.legacy === true),
  };
}

/** Every provider present in the published models, in first-seen order. */
export function modelProviders(choices: PreferenceChoices | undefined): readonly string[] {
  const { current, legacy } = partitionModels(choices);
  const providers: string[] = [];
  for (const model of [...current, ...legacy]) {
    const provider = modelProvider(model);
    if (provider !== null && !providers.includes(provider)) providers.push(provider);
  }
  return providers;
}

/** The key the viewer's favourite models are remembered under. */
export const FAVOURITE_MODELS_KEY = "detent.composer.favourite-models";

/**
 * The viewer's favourite models.
 *
 * Per viewer and per browser, so it is `localStorage` rather than anything the
 * hub stores: a favourite is a convenience, not conversation state, and the
 * hub has nowhere to put one. Every read and write is guarded — a private
 * window, blocked site data or a thumbnail capture can make either throw — and
 * a failure means no favourites, never a broken picker.
 */
export function readFavouriteModels(storage: Storage | undefined): readonly string[] {
  try {
    const raw = storage?.getItem(FAVOURITE_MODELS_KEY);
    if (raw == null) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((entry): entry is string => typeof entry === "string") : [];
  } catch {
    return [];
  }
}

export function writeFavouriteModels(storage: Storage | undefined, models: readonly string[]): void {
  try {
    storage?.setItem(FAVOURITE_MODELS_KEY, JSON.stringify(models));
  } catch {
    // A viewer who cannot persist a favourite still gets to use the picker.
  }
}

export function toggleFavouriteModel(
  favourites: readonly string[],
  id: string,
): readonly string[] {
  return favourites.includes(id)
    ? favourites.filter((entry) => entry !== id)
    : [...favourites, id];
}
