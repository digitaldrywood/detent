import {
  BotIcon,
  GaugeIcon,
  LockIcon,
  LockOpenIcon,
  StarIcon,
  type LucideIcon,
} from "lucide-react";
import React from "react";

import {
  AUTO_PREFERENCE,
  type PreferenceChoice,
  type PreferenceChoices,
  type TurnPreferences,
} from "../../contracts/index.ts";
import {
  ComposerControl,
  ComposerControlChevron,
  ComposerControlIcon,
  ComposerControlSeparator,
  ComposerSelectControl,
} from "../../components/chat/ComposerControl.tsx";
import { composerFloatingLayerProps } from "../../components/chat/composerEventScope.ts";
import { useComposerMenuState } from "../../components/chat/useComposerMenuState.ts";
import {
  Command,
  CommandCollection,
  CommandGroup,
  CommandGroupLabel,
  CommandInput,
  CommandItem,
  CommandList,
  CommandPanel,
  CommandShortcut,
} from "../../components/ui/command.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../../components/ui/popover.tsx";
import {
  Select,
  SelectGroup,
  SelectGroupLabel,
  SelectItem,
  SelectPopup,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { cn } from "../../lib/utils.ts";
import { preferenceLabel, preferenceOptions } from "../../contracts/index.ts";
import { providerMark, providerName } from "../adapters/providerMark.tsx";
import {
  effortOptions,
  modelProvider,
  modelProviders,
  partitionModels,
  preferencesForModel,
  readFavouriteModels,
  toggleFavouriteModel,
  writeFavouriteModels,
} from "../lib/preferences.ts";

/** Which field of the triple a picker owns. */
export type PreferenceField = "model" | "reasoning_effort" | "access";

/**
 * A request to open one picker from outside the row — `/model` and its two
 * siblings in the slash menu.
 *
 * It carries a nonce rather than being a bare field because the same field can
 * be asked for twice in a row: a reader who runs `/model`, presses Escape and
 * runs `/model` again is asking for the picker both times, and a value that
 * did not change would not reopen it.
 */
export interface PreferenceOpenRequest {
  readonly field: PreferenceField;
  readonly nonce: number;
}

export interface ComposerPreferenceControlsProps {
  readonly preferences: TurnPreferences;
  readonly choices?: PreferenceChoices | undefined;
  /**
   * Persists the change. Absent where there is nothing to persist to — the
   * draft surface before a conversation exists — in which case the pickers
   * still work and the chosen values travel with the first send.
   */
  readonly onChange: (preferences: TurnPreferences) => void;
  /** True for a reader who may not send: the pickers change nothing for them. */
  readonly disabled?: boolean;

  readonly hidden?: boolean;

  readonly size?: "sm" | "xs";
  /** The picker the slash menu just asked for, or null. */
  readonly openRequest?: PreferenceOpenRequest | null;
}

export function ComposerPreferenceControls(
  props: ComposerPreferenceControlsProps,
): React.ReactElement {
  return (
    <>
      <ModelPicker {...props} />
      <EffortPicker {...props} />
      <AccessPicker {...props} />
    </>
  );
}

function usePickerState(
  field: PreferenceField,
  props: ComposerPreferenceControlsProps,
): readonly [boolean, (open: boolean) => void] {
  const [open, setOpen] = useComposerMenuState(props.hidden);
  const request = props.openRequest ?? null;
  const requestField = request?.field ?? null;
  const requestNonce = request?.nonce ?? null;
  const disabled = props.disabled === true;
  React.useEffect(() => {
    if (requestNonce === null || requestField !== field || disabled) return;
    setOpen(true);
  }, [requestField, requestNonce, field, disabled, setOpen]);
  return [open, setOpen];
}

function Chip(props: {
  readonly description: string;
  readonly size: "sm" | "xs";
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <>
      <ComposerControlSeparator size={props.size} />
      <Tooltip>
        {props.children}
        <TooltipPopup side="top">{props.description}</TooltipPopup>
      </Tooltip>
    </>
  );
}

// --- Model -------------------------------------------------------------------

/** How many rows the ⌘ gutter can reach. Past nine the search is the way. */
const MODEL_SHORTCUT_LIMIT = 9;

function ModelPicker(props: ComposerPreferenceControlsProps): React.ReactElement {
  const size = props.size ?? "sm";
  const [open, setOpen] = usePickerState("model", props);
  const [query, setQuery] = React.useState("");
  const [provider, setProvider] = React.useState<string | null>(null);
  const [legacyOpen, setLegacyOpen] = React.useState(false);
  const [favourites, setFavourites] = React.useState<readonly string[]>(() =>
    readFavouriteModels(globalThis.localStorage),
  );

  const disabled = props.disabled === true;
  const selected = props.preferences.model;
  const providers = modelProviders(props.choices);
  const { current, legacy } = partitionModels(props.choices);

  // Every picker leads with Auto and keeps a value the hub has stopped
  // publishing (§14); the shelf below it is what the search and the rail act
  // on, so Auto is held out of both.
  const auto = preferenceOptions(props.choices?.models, selected).find(
    (option) => option.id === AUTO_PREFERENCE,
  );
  const kept = preferenceOptions(props.choices?.models, selected).filter(
    (option) =>
      option.id !== AUTO_PREFERENCE &&
      !current.some((model) => model.id === option.id) &&
      !legacy.some((model) => model.id === option.id),
  );

  const matches = React.useCallback(
    (model: PreferenceChoice): boolean => {
      const term = query.trim().toLowerCase();
      if (provider !== null && provider !== "favourites" && modelProvider(model) !== provider) {
        return false;
      }
      if (provider === "favourites" && !favourites.includes(model.id)) return false;
      if (term.length === 0) return true;
      return `${model.label} ${model.id} ${model.provider ?? ""}`.toLowerCase().includes(term);
    },
    [query, provider, favourites],
  );

  const listed = [...current, ...kept].filter(matches);
  const shelved = legacy.filter(matches);
  const shortcut = new Map(listed.slice(0, MODEL_SHORTCUT_LIMIT).map((model, index) => [model.id, index + 1]));

  function choose(id: string): void {
    props.onChange(preferencesForModel(props.choices, props.preferences, id));
    setOpen(false);
  }

  function favourite(id: string): void {
    const next = toggleFavouriteModel(favourites, id);
    setFavourites(next);
    writeFavouriteModels(globalThis.localStorage, next);
  }

  // ⌘1…⌘9 pick the row the gutter numbers, which is the only reason to draw
  // the gutter at all. The listener is bound while the popup is open, so it
  // cannot shadow anything the composer does when it is shut.
  React.useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: globalThis.KeyboardEvent): void => {
      if (!event.metaKey && !event.ctrlKey) return;
      const index = Number.parseInt(event.key, 10);
      if (!Number.isFinite(index) || index < 1 || index > MODEL_SHORTCUT_LIMIT) return;
      const model = listed[index - 1];
      if (model === undefined) return;
      event.preventDefault();
      choose(model.id);
    };
    globalThis.addEventListener("keydown", onKeyDown);
    return () => globalThis.removeEventListener("keydown", onKeyDown);
  });

  return (
    <Chip description="The model this conversation's turns run on." size={size}>
      <Popover
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (!next) {
            setQuery("");
            setProvider(null);
          }
        }}
      >
        <TooltipTrigger
          render={
            <PopoverTrigger
              render={
                <ComposerControl
                  size={size}
                  className={size === "xs" ? undefined : "font-medium"}
                  disabled={disabled}
                  aria-label="Model"
                  aria-haspopup="listbox"
                  data-testid="composer-model"
                />
              }
            />
          }
        >
          <ComposerControlIcon icon={BotIcon} size={size} />
          <span className="min-w-0 truncate">
            {preferenceLabel(props.choices?.models, selected)}
          </span>
          <ComposerControlChevron size={size} />
        </TooltipTrigger>
        <PopoverPopup
          {...composerFloatingLayerProps}
          side="top"
          align="start"
          sideOffset={8}
          aria-label="Model"
          data-testid="composer-model-popup"
          className="w-[26rem] max-w-[calc(100vw-2rem)] [--command-shell-inset:--spacing(2)] [--command-content-inset:--spacing(3)]"
          viewportClassName="p-0"
        >
          {/* `mode="none"` is the palette's own setting: the rows are filtered
              here — by label, identifier and provider, and by the rail — so
              the primitive must not filter them a second time by its own
              rule. */}
          <Command
            mode="none"
            value={query}
            onValueChange={(value) => setQuery(String(value ?? ""))}
          >
            <CommandInput placeholder="Search models…" aria-label="Search models" />
            <div className="flex min-h-0 border-border/60 border-t">
              {providers.length > 0 ? (
                <ProviderRail
                  providers={providers}
                  selected={provider}
                  onSelect={setProvider}
                />
              ) : null}
              <CommandPanel className="min-w-0 flex-1">
                <CommandList className="max-h-72">
                  {/* Two different empties: a search that matched nothing,
                      and a hub whose runners have reported no model at all.
                      The second is the feature's own reason (§16), so it is
                      said, not hidden behind the first's wording. */}
                  {listed.length + shelved.length === 0 ? (
                    <p
                      className="px-2 py-6 text-center text-muted-foreground text-sm"
                      data-testid="composer-model-empty"
                    >
                      {query.trim() !== "" || provider !== null
                        ? "No model matches that."
                        : "No runner has reported a model yet. Auto uses what the project configures."}
                    </p>
                  ) : null}
                  {auto === undefined ? null : (
                    <CommandGroup items={[auto]}>
                      <CommandCollection>
                        {(option: PreferenceChoice) => (
                          <ModelRow
                            key={option.id}
                            model={option}
                            selected={selected === option.id}
                            description="Whatever the project's runners are configured to use."
                            onChoose={choose}
                          />
                        )}
                      </CommandCollection>
                    </CommandGroup>
                  )}
                  {listed.length === 0 ? null : (
                    <CommandGroup items={listed}>
                      <CommandGroupLabel className="ps-[9px]">Models</CommandGroupLabel>
                      <CommandCollection>
                        {(model: PreferenceChoice) => (
                          <ModelRow
                            key={model.id}
                            model={model}
                            selected={selected === model.id}
                            shortcut={shortcut.get(model.id) ?? null}
                            favourite={favourites.includes(model.id)}
                            onFavourite={favourite}
                            onChoose={choose}
                          />
                        )}
                      </CommandCollection>
                    </CommandGroup>
                  )}
                  {shelved.length === 0 ? null : (
                    <>
                      <button
                        type="button"
                        data-testid="composer-model-legacy-toggle"
                        aria-expanded={legacyOpen}
                        onClick={() => setLegacyOpen((value) => !value)}
                        className="flex w-full cursor-pointer items-center gap-1 rounded-sm px-2 py-1.5 text-muted-foreground text-xs hover:bg-foreground/[0.06]"
                      >
                        <span>Legacy models</span>
                        <span aria-hidden="true">·</span>
                        <span>
                          {shelved.length} {shelved.length === 1 ? "model" : "models"}
                        </span>
                        <span
                          aria-hidden="true"
                          className={cn(
                            "ms-auto transition-transform",
                            legacyOpen && "rotate-90",
                          )}
                        >
                          ›
                        </span>
                      </button>
                      {legacyOpen ? (
                        <CommandGroup items={shelved}>
                          <CommandCollection>
                            {(model: PreferenceChoice) => (
                              <ModelRow
                                key={model.id}
                                model={model}
                                selected={selected === model.id}
                                favourite={favourites.includes(model.id)}
                                onFavourite={favourite}
                                onChoose={choose}
                              />
                            )}
                          </CommandCollection>
                        </CommandGroup>
                      ) : null}
                    </>
                  )}
                </CommandList>
              </CommandPanel>
            </div>
          </Command>
        </PopoverPopup>
      </Popover>
    </Chip>
  );
}

function ProviderRail(props: {
  readonly providers: readonly string[];
  readonly selected: string | null;
  readonly onSelect: (provider: string | null) => void;
}): React.ReactElement {
  const entries: ReadonlyArray<{ id: string; label: string; icon: React.ReactNode }> = [
    { id: "favourites", label: "Favourites", icon: <StarIcon className="size-3.5" /> },
    ...props.providers.map((provider) => {
      const mark = providerMark(provider);
      return {
        id: provider,
        label: providerName(provider),
        icon: <mark.icon aria-hidden="true" className="size-3.5 shrink-0" />,
      };
    }),
  ];
  return (
    <div
      role="group"
      aria-label="Providers"
      data-testid="composer-model-providers"
      className="flex shrink-0 flex-col gap-0.5 border-border/60 border-e p-1.5"
    >
      {entries.map((entry) => (
        <Tooltip key={entry.id}>
          <TooltipTrigger
            render={
              <button
                type="button"
                aria-label={entry.label}
                aria-pressed={props.selected === entry.id}
                data-testid={`composer-model-provider-${entry.id}`}
                onClick={() => props.onSelect(props.selected === entry.id ? null : entry.id)}
                className={cn(
                  "flex size-7 cursor-pointer items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-foreground/[0.06] hover:text-foreground",
                  props.selected === entry.id && "bg-foreground/[0.09] text-foreground",
                )}
              />
            }
          >
            {entry.icon}
          </TooltipTrigger>
          <TooltipPopup side="right">{entry.label}</TooltipPopup>
        </Tooltip>
      ))}
    </div>
  );
}

function ModelRow(props: {
  readonly model: PreferenceChoice;
  readonly selected: boolean;
  readonly description?: string;
  readonly shortcut?: number | null;
  readonly favourite?: boolean;
  readonly onFavourite?: (id: string) => void;
  readonly onChoose: (id: string) => void;
}): React.ReactElement {
  const provider = modelProvider(props.model);
  return (
    <CommandItem
      value={props.model.id}
      data-testid={`composer-model-option-${props.model.id}`}

      {...(props.selected ? { "aria-current": true as const, "data-selected": "" } : {})}
      className="gap-2"
      onClick={() => props.onChoose(props.model.id)}
    >
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="flex min-w-0 items-center gap-1.5 text-sm">
          <span className="truncate">{props.model.label}</span>
          {provider === null ? null : (
            <span className="shrink-0 text-muted-foreground/70 text-xs">
              {providerName(provider)}
            </span>
          )}
        </span>
        {props.description === undefined ? null : (
          <span className="min-w-0 text-muted-foreground/70 text-xs">{props.description}</span>
        )}
      </span>
      {props.shortcut == null ? null : <CommandShortcut>⌘{props.shortcut}</CommandShortcut>}
      {props.onFavourite === undefined ? null : (
        <button
          type="button"
          aria-label={props.favourite === true ? "Remove from favourites" : "Add to favourites"}
          aria-pressed={props.favourite === true}
          data-testid={`composer-model-favourite-${props.model.id}`}
          className="ms-1 shrink-0 cursor-pointer rounded p-0.5 text-muted-foreground/70 hover:text-foreground"
          onClick={(event) => {
            // The star is not a choice of model: a reader who stars a row is
            // marking it for next time, not selecting it now.
            event.stopPropagation();
            event.preventDefault();
            props.onFavourite?.(props.model.id);
          }}
        >
          <StarIcon
            className={cn("size-3.5", props.favourite === true && "fill-current text-warning")}
          />
        </button>
      )}
    </CommandItem>
  );
}

// --- Effort ------------------------------------------------------------------

function EffortPicker(props: ComposerPreferenceControlsProps): React.ReactElement {
  const size = props.size ?? "sm";
  const [open, setOpen] = usePickerState("reasoning_effort", props);
  const options = effortOptions(props.choices, props.preferences);
  const selected = props.preferences.reasoning_effort;
  const scoped = props.preferences.model !== AUTO_PREFERENCE && options.length > 1;

  return (
    <Chip description="How much reasoning effort a turn is given." size={size}>
      <Select
        open={open}
        onOpenChange={setOpen}
        value={selected}
        onValueChange={(value) =>
          props.onChange({ ...props.preferences, reasoning_effort: String(value) })
        }
        disabled={props.disabled === true}
      >
        <TooltipTrigger
          render={
            <ComposerSelectControl
              size={size}
              className={size === "xs" ? undefined : "font-medium"}
              aria-label="Reasoning effort"
              data-testid="composer-effort"
            />
          }
        >
          <ComposerControlIcon icon={GaugeIcon} size={size} />
          <SelectValue>{labelFor(options, selected)}</SelectValue>
        </TooltipTrigger>
        <SelectPopup alignItemWithTrigger={false} {...composerFloatingLayerProps}>
          <SelectGroup>
            <SelectGroupLabel>Reasoning</SelectGroupLabel>
            {options.map((option) => (
              <SelectItem key={option.id} value={option.id} hideIndicator className="min-w-64 py-2">
                <span className="flex min-w-0 items-center gap-2">
                  <span className="grid min-w-0 gap-0.5">
                    <span className="font-medium text-foreground">{option.label}</span>
                    {option.id === AUTO_PREFERENCE ? (
                      <span className="text-muted-foreground text-xs leading-4">
                        The effort the project configures for this kind of work.
                      </span>
                    ) : null}
                  </span>
                  {option.default && option.id !== AUTO_PREFERENCE ? (
                    <span
                      data-testid={`composer-effort-default-${option.id}`}
                      className="ms-auto shrink-0 rounded-sm bg-foreground/[0.08] px-1.5 py-0.5 font-medium text-[10px] text-muted-foreground uppercase"
                    >
                      Default
                    </span>
                  ) : null}
                </span>
              </SelectItem>
            ))}
          </SelectGroup>
          {/* Why the list changed, said once. A reader who picks a model and
              finds a different set of levels — or a level they had chosen gone
              — is owed the reason (decisions.md §14). */}
          {scoped ? (
            <p
              data-testid="composer-effort-note"
              className="border-border/60 border-t px-2 pt-2 pb-1 text-muted-foreground/70 text-xs"
            >
              These are the levels this model supports. Changing model resets the effort to that
              model&apos;s default.
            </p>
          ) : null}
        </SelectPopup>
      </Select>
    </Chip>
  );
}

// --- Access ------------------------------------------------------------------

const ACCESS_COPY: Readonly<Record<string, { label: string; description: string; icon: LucideIcon }>> =
  {
    [AUTO_PREFERENCE]: {
      label: "Auto",
      description: "The access the project's policy allows.",
      icon: LockIcon,
    },
    read_only: {
      label: "Read only",
      description: "The turn may read the checkout and answer. It changes nothing.",
      icon: LockIcon,
    },
    full: {
      label: "Full access",
      description: "The turn may edit files and run the project's commands.",
      icon: LockOpenIcon,
    },
  };

function accessCopy(option: PreferenceChoice): {
  label: string;
  description: string;
  icon: LucideIcon;
} {
  return ACCESS_COPY[option.id] ?? { label: option.label, description: "", icon: LockIcon };
}

function AccessPicker(props: ComposerPreferenceControlsProps): React.ReactElement {
  const size = props.size ?? "sm";
  const [open, setOpen] = usePickerState("access", props);
  const selected = props.preferences.access;
  const options = preferenceOptions(props.choices?.access, selected);

  return (
    <Chip description="What a turn may do on the runner." size={size}>
      <Select
        open={open}
        onOpenChange={setOpen}
        value={selected}
        onValueChange={(value) => props.onChange({ ...props.preferences, access: String(value) })}
        disabled={props.disabled === true}
      >
        <TooltipTrigger
          render={
            <ComposerSelectControl
              size={size}
              className={size === "xs" ? undefined : "font-medium"}
              aria-label="Runtime access"
              data-testid="composer-access"
            />
          }
        >
          <ComposerControlIcon
            icon={accessCopy({ id: selected, label: "", default: false }).icon}
            size={size}
          />
          <SelectValue>
            {ACCESS_COPY[selected]?.label ?? preferenceLabel(props.choices?.access, selected)}
          </SelectValue>
        </TooltipTrigger>
        <SelectPopup alignItemWithTrigger={false} {...composerFloatingLayerProps}>
          {options.map((option) => {
            const copy = accessCopy(option);
            return (
              <SelectItem key={option.id} value={option.id} hideIndicator className="min-w-72 py-2">
                <span className="flex min-w-0 items-start gap-2">
                  <copy.icon className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
                  <span className="grid min-w-0 gap-0.5">
                    <span className="font-medium text-foreground">{copy.label}</span>
                    {copy.description === "" ? null : (
                      <span className="text-muted-foreground text-xs leading-4">
                        {copy.description}
                      </span>
                    )}
                  </span>
                </span>
              </SelectItem>
            );
          })}
        </SelectPopup>
      </Select>
    </Chip>
  );
}

/** The label a trigger shows for a value, from the options it opens on. */
function labelFor(options: readonly PreferenceChoice[], selected: string): string {
  return options.find((option) => option.id === selected)?.label ?? "Auto";
}
