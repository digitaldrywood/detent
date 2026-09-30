import { describe, expect, it } from "vitest";

import { AUTO_PREFERENCE, type PreferenceChoices } from "../../src/contracts/index.ts";
import {
  FAVOURITE_MODELS_KEY,
  effortLabel,
  effortOptions,
  modelProviders,
  partitionModels,
  preferencesForModel,
  readFavouriteModels,
  toggleFavouriteModel,
  writeFavouriteModels,
} from "../../src/app/lib/preferences.ts";
import { providerMark, providerName } from "../../src/app/adapters/providerMark.tsx";
import { selectIssuePullRequest } from "../../src/app/adapters/issuePullRequest.ts";
import type { WorkItemPullRequest } from "../../src/contracts/work.ts";

const CHOICES: PreferenceChoices = {
  models: [
    {
      id: "gpt-6-astra",
      label: "GPT-6-Astra",
      default: true,
      provider: "openai",
      efforts: ["low", "medium", "high", "xhigh", "max"],
      default_effort: "medium",
    },
    {
      id: "claude-opus-5",
      label: "Claude Opus 5",
      default: false,
      provider: "anthropic",
      efforts: ["low", "high"],
    },
    { id: "bare-model", label: "Bare Model", default: false, provider: "openai" },
    {
      id: "gpt-5.6-sol",
      label: "GPT-5.6-Sol",
      default: false,
      provider: "openai",
      legacy: true,
      efforts: ["low"],
    },
  ],
  efforts: [
    { id: "low", label: "Low", default: false },
    { id: "medium", label: "Medium", default: true },
    { id: "high", label: "High", default: false },
  ],
  access: [
    { id: "read_only", label: "Read only", default: false },
    { id: "full", label: "Full", default: true },
  ],
};

const AUTO = { model: AUTO_PREFERENCE, reasoning_effort: AUTO_PREFERENCE, access: AUTO_PREFERENCE };

// §14 plus Michael's review: a provider's reasoning levels are per model, so
// the effort picker has to be too. What the hub publishes on
// `preferences.models[]` is a model's own ladder; the fixed vocabulary is what
// is left when it publishes none.
describe("the effort picker's levels", () => {
  it("offers the fixed vocabulary while the model is Auto", () => {
    expect(effortOptions(CHOICES, AUTO).map((option) => option.id)).toEqual([
      AUTO_PREFERENCE,
      "low",
      "medium",
      "high",
    ]);
  });

  it("offers the selected model's own ladder, Auto still first", () => {
    const options = effortOptions(CHOICES, { ...AUTO, model: "gpt-6-astra" });
    expect(options.map((option) => option.id)).toEqual([
      AUTO_PREFERENCE,
      "low",
      "medium",
      "high",
      "xhigh",
      "max",
    ]);
    // Levels the hub has no word for arrive as bare provider tokens.
    expect(options.find((option) => option.id === "xhigh")?.label).toBe("Extra High");
    expect(options.find((option) => option.id === "max")?.label).toBe("Max");
  });

  it("marks the model's own default rather than the organization's", () => {
    const options = effortOptions(CHOICES, { ...AUTO, model: "gpt-6-astra" });
    expect(options.filter((option) => option.default).map((option) => option.id)).toEqual([
      "medium",
    ]);
    // A model with no default names none; nothing is badged Default.
    const opus = effortOptions(CHOICES, { ...AUTO, model: "claude-opus-5" });
    expect(opus.some((option) => option.default)).toBe(false);
  });

  it("falls back to the fixed vocabulary for a model with no published ladder", () => {
    expect(effortOptions(CHOICES, { ...AUTO, model: "bare-model" }).map((o) => o.id)).toEqual([
      AUTO_PREFERENCE,
      "low",
      "medium",
      "high",
    ]);
  });

  it("keeps a level the model has stopped offering rather than swapping it", () => {
    const options = effortOptions(CHOICES, {
      ...AUTO,
      model: "claude-opus-5",
      reasoning_effort: "ultracode",
    });
    expect(options.map((option) => option.id)).toContain("ultracode");
  });

  it("names every level a provider can send", () => {
    expect(["low", "medium", "high", "xhigh", "max", "ultracode"].map(effortLabel)).toEqual([
      "Low",
      "Medium",
      "High",
      "Extra High",
      "Max",
      "Ultra",
    ]);
    expect(effortLabel("some_new_rung")).toBe("Some new rung");
  });
});

// Picking a model the current effort does not belong to has to change the
// effort: the hub validates the pair and would refuse the composer's own
// state otherwise.
describe("picking a model", () => {
  it("resets an effort the model does not support to the model's default", () => {
    expect(
      preferencesForModel(CHOICES, { ...AUTO, reasoning_effort: "high" }, "gpt-6-astra"),
    ).toEqual({ ...AUTO, model: "gpt-6-astra", reasoning_effort: "high" });
    expect(
      preferencesForModel(CHOICES, { ...AUTO, reasoning_effort: "max" }, "claude-opus-5"),
    ).toEqual({ ...AUTO, model: "claude-opus-5", reasoning_effort: AUTO_PREFERENCE });
    expect(
      preferencesForModel(CHOICES, { ...AUTO, reasoning_effort: "ultracode" }, "gpt-6-astra"),
    ).toEqual({ ...AUTO, model: "gpt-6-astra", reasoning_effort: "medium" });
  });

  it("leaves an Auto effort alone whatever the model", () => {
    for (const model of ["gpt-6-astra", "bare-model", AUTO_PREFERENCE]) {
      expect(preferencesForModel(CHOICES, AUTO, model).reasoning_effort).toBe(AUTO_PREFERENCE);
    }
  });

  // The rule running the other way. "max" is a level only a model's own ladder
  // publishes, so going back to a model with no ladder — or to Auto, which is
  // not a model — leaves the reader holding a pair the hub would refuse.
  it("resets a model-only level when the new model has no ladder of its own", () => {
    for (const model of ["bare-model", AUTO_PREFERENCE]) {
      expect(
        preferencesForModel(CHOICES, { ...AUTO, reasoning_effort: "max" }, model)
          .reasoning_effort,
      ).toBe(AUTO_PREFERENCE);
    }
    // A level in the fixed vocabulary survives the same move.
    expect(
      preferencesForModel(CHOICES, { ...AUTO, reasoning_effort: "high" }, AUTO_PREFERENCE)
        .reasoning_effort,
    ).toBe("high");
  });

  // A hub that has published no vocabulary has said nothing, not that nothing
  // is allowed; resetting on its silence would throw away a real choice.
  it("keeps the effort when the hub has published no vocabulary at all", () => {
    expect(
      preferencesForModel(undefined, { ...AUTO, reasoning_effort: "max" }, AUTO_PREFERENCE)
        .reasoning_effort,
    ).toBe("max");
  });
});

describe("the model picker's shelves and rail", () => {
  // The hub publishes "auto" at the head of the same list, because it is a
  // value the picker offers. It is not a model, so it is in neither shelf and
  // contributes no provider — a second Auto row would otherwise appear.
  it("keeps Auto out of the shelves and the rail", () => {
    const withAuto: PreferenceChoices = {
      ...CHOICES,
      models: [{ id: AUTO_PREFERENCE, label: "Auto", default: true }, ...CHOICES.models],
    };
    const { current, legacy } = partitionModels(withAuto);
    expect([...current, ...legacy].map((model) => model.id)).not.toContain(AUTO_PREFERENCE);
    expect(modelProviders(withAuto)).toEqual(["openai", "anthropic"]);
  });

  it("shelves the models a provider has named a successor for", () => {
    const { current, legacy } = partitionModels(CHOICES);
    expect(current.map((model) => model.id)).toEqual([
      "gpt-6-astra",
      "claude-opus-5",
      "bare-model",
    ]);
    expect(legacy.map((model) => model.id)).toEqual(["gpt-5.6-sol"]);
  });

  // The hub publishes the runners' own order, and the one move the picker
  // makes is to lead with the model the backend itself defaults to. An
  // alphabet is not a ranking.
  it("leads with the backend's own default, keeping the rest in report order", () => {
    const reported: PreferenceChoices = {
      ...CHOICES,
      models: [
        { id: "gpt-5.6-sol", label: "GPT-5.6-Sol", default: false, provider: "openai" },
        { id: "gpt-6-astra", label: "GPT-6-Astra", default: false, provider: "openai", backend_default: true },
        { id: "gpt-5.3-codex", label: "GPT-5.3-Codex", default: false, provider: "openai" },
      ],
    };
    expect(partitionModels(reported).current.map((model) => model.id)).toEqual([
      "gpt-6-astra",
      "gpt-5.6-sol",
      "gpt-5.3-codex",
    ]);
  });

  it("keeps the report order when no model claims the backend default", () => {
    const reported: PreferenceChoices = {
      ...CHOICES,
      models: [
        { id: "second", label: "Second", default: false },
        { id: "first", label: "First", default: false },
      ],
    };
    expect(partitionModels(reported).current.map((model) => model.id)).toEqual([
      "second",
      "first",
    ]);
  });

  it("lists each provider once, in the order the models arrive", () => {
    expect(modelProviders(CHOICES)).toEqual(["openai", "anthropic"]);
    expect(modelProviders(undefined)).toEqual([]);
  });
});

describe("the provider mark", () => {
  it("uses the mark for the providers they have one for", () => {
    for (const provider of ["openai", "OpenAI", "codex", "anthropic", "claude", "grok", "cursor"]) {
      expect(providerMark(provider).branded, provider).toBe(true);
    }
  });

  it("falls back to a neutral glyph rather than initials", () => {
    for (const provider of ["someone-else", "", "  "]) {
      const mark = providerMark(provider);
      expect(mark.branded).toBe(false);
      expect(typeof mark.icon).toBe("object");
    }
  });

  it("spells a provider the way its vendor does", () => {
    expect(providerName("openai")).toBe("OpenAI");
    expect(providerName("anthropic")).toBe("Anthropic");
    expect(providerName("xai")).toBe("xAI");
    expect(providerName("someone-else")).toBe("Someone-else");
  });
});

describe("favourite models", () => {
  function memoryStorage(): Storage {
    const entries = new Map<string, string>();
    return {
      get length() {
        return entries.size;
      },
      clear: () => entries.clear(),
      getItem: (key: string) => entries.get(key) ?? null,
      key: (index: number) => [...entries.keys()][index] ?? null,
      removeItem: (key: string) => entries.delete(key),
      setItem: (key: string, value: string) => void entries.set(key, value),
    } as Storage;
  }

  it("remembers a viewer's favourites and takes them back off", () => {
    const storage = memoryStorage();
    writeFavouriteModels(storage, toggleFavouriteModel([], "gpt-6-astra"));
    expect(readFavouriteModels(storage)).toEqual(["gpt-6-astra"]);
    expect(storage.getItem(FAVOURITE_MODELS_KEY)).toBe('["gpt-6-astra"]');
    writeFavouriteModels(storage, toggleFavouriteModel(["gpt-6-astra"], "gpt-6-astra"));
    expect(readFavouriteModels(storage)).toEqual([]);
  });

  // A private window, blocked site data or a thumbnail capture makes either
  // accessor throw. The picker still has to open.
  it("survives storage that is absent or throws", () => {
    const hostile = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    } as unknown as Storage;
    expect(readFavouriteModels(undefined)).toEqual([]);
    expect(readFavouriteModels(hostile)).toEqual([]);
    expect(() => writeFavouriteModels(hostile, ["x"])).not.toThrow();
  });

  it("ignores anything stored that is not a list of ids", () => {
    const storage = memoryStorage();
    storage.setItem(FAVOURITE_MODELS_KEY, '{"not":"a list"}');
    expect(readFavouriteModels(storage)).toEqual([]);
    storage.setItem(FAVOURITE_MODELS_KEY, '["ok", 7, null]');
    expect(readFavouriteModels(storage)).toEqual(["ok"]);
  });
});

// The context strip's chips (decisions.md §18.6). One row is picked, because
// the strip has room for one pair and the reader is asking where the work is.
describe("the linked issue's pull request", () => {
  function row(overrides: Partial<WorkItemPullRequest>): WorkItemPullRequest {
    return {
      id: "chg_1",
      change_id: "chg_1",
      number: 0,
      title: "",
      state: "open",
      draft: false,
      url: "",
      head: { ref: "" },
      base: { ref: "main" },
      updated_at: "2026-09-01T00:00:00Z",
      ...overrides,
    };
  }

  it("prefers an open pull request, then the most recent", () => {
    const selected = selectIssuePullRequest([
      row({ id: "a", number: 1, state: "merged", url: "https://x/1", head: { ref: "one" }, updated_at: "2026-09-09T00:00:00Z" }),
      row({ id: "b", number: 2, state: "open", url: "https://x/2", head: { ref: "two" }, updated_at: "2026-09-02T00:00:00Z" }),
      row({ id: "c", number: 3, state: "open", url: "https://x/3", head: { ref: "three" }, updated_at: "2026-09-08T00:00:00Z" }),
    ]);
    expect(selected).toEqual({
      number: 3,
      url: "https://x/3",
      branch: "three",
      state: "open",
      draft: false,
    });
  });

  it("keeps a branch that has no pull request yet", () => {
    expect(selectIssuePullRequest([row({ head: { ref: "feat/lease" } })])).toEqual({
      number: null,
      url: "",
      branch: "feat/lease",
      state: "open",
      draft: false,
    });
  });

  it("has nothing to show for a change with neither", () => {
    expect(selectIssuePullRequest([row({})])).toBeNull();
    expect(selectIssuePullRequest([])).toBeNull();
  });
});
