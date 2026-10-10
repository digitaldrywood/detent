# Component contracts

This document is generated from [`web/conversation/src/design-system/catalog.json`](../../web/conversation/src/design-system/catalog.json) by `npm run design:catalog` in `web/conversation`. Edit the catalog, not this file; `npm run design:catalog:check` fails when the two disagree.

Each contract states what a component is for, how to import it, the variants its source defines, the interaction states it owns, its keyboard behaviour and what to use instead. The source file is the owner of the component's appearance and behaviour; features compose it and do not restyle it. A proposed entry names a component Detent does not have yet; listing one does not authorize building it.

Catalog totals: 139 entries; by kind: 51 primitive, 81 composition, 7 surface; by status: 139 available, 0 proposed, 0 exception.

## Actions

### Button

Every clickable action: form submission, toolbar and composer actions, icon-only actions. `size="compact"` is the toolbar size family; `icon*` sizes need an accessible name. `ghost-destructive` is the low-emphasis destructive action in menus and toolbars; `media-close` and `media-navigation` belong to media viewers; `sm-multiline` lets a small button wrap. `InlineButton` is a text action inside a sentence, with `tone` default, muted, destructive or picker (a dotted underline that opens a menu). `SplitButton` groups two primary Buttons into a joined 40px control: a main action and a 44px-wide MenuTrigger rendered as an icon Button. Label the group and the menu trigger; Menu owns the popup and keyboard behavior.

- Kind: Primitive; status: available; id: `button`.
- Import: `import { Button, buttonVariants, InlineButton, SplitButton, ButtonSize, ButtonVariant } from "~/components/ui/button";`
- Source: [src/components/ui/button.tsx](../../web/conversation/src/components/ui/button.tsx).
- Variants: `variant`: `default`, `secondary`, `outline`, `ghost`, `ghost-muted`, `ghost-destructive`, `destructive`, `destructive-outline`, `warning-outline`, `glass`, `overlay`, `link`, `media-close`, `media-navigation`; `size`: `micro`, `compact`, `xs`, `sm`, `sm-multiline`, `default`, `lg`, `xl`, `icon-tiny`, `icon-micro`, `icon-xs`, `icon-sm`, `icon`, `icon-lg`, `icon-xl`; `tone`: `default`, `muted`, `destructive`, `picker`.
- States: hover, focus-visible, pressed, disabled.
- Keyboard: Native button: Tab focuses, Enter and Space activate. With `render`, the rendered element must stay focusable and keep button semantics.
- Avoid: Restyling with arbitrary radius, fill or font classes, or calling `buttonVariants` from app code to dress a non-button. Use Badge for static status and a link for navigation that is not an action.
- Related: [Toggle](#toggle), [Group](#group), [Panel tab close button](#panel-tab-close-button).

### Toggle

A two-state control whose state is visible on the control itself, such as a formatting or view toggle. `variant="pill"` is a rounded filter chip; `pill-outline` adds a quiet border and surface for dense runner strips.

- Kind: Primitive; status: available; id: `toggle`.
- Import: `import { Toggle, toggleVariants } from "~/components/ui/toggle";`
- Source: [src/components/ui/toggle.tsx](../../web/conversation/src/components/ui/toggle.tsx).
- Variants: `variant`: `default`, `ghost`, `outline`, `segmented`, `pill`, `pill-outline`; `size`: `compact`, `default`, `lg`, `segmented`, `sm`, `xs`.
- States: hover, focus-visible, pressed, disabled.
- Keyboard: Tab focuses; Enter and Space flip the pressed state, exposed as aria-pressed.
- Avoid: Settings that persist a preference (use Switch) or a choice among peers (use ToggleGroup).
- Related: [Toggle group](#toggle-group), [Switch](#switch), [Button](#button).

### Toggle group

A small set of mutually related options shown inline, such as a view switch (board or list) or a segmented range picker.

- Kind: Primitive; status: available; id: `toggle-group`.
- Import: `import { ToggleGroup, Toggle, ToggleGroupItem, ToggleGroupSeparator } from "~/components/ui/toggle-group";`
- Source: [src/components/ui/toggle-group.tsx](../../web/conversation/src/components/ui/toggle-group.tsx).
- Variants: none.
- States: hover, focus-visible, pressed, disabled.
- Keyboard: Base UI roving focus: Tab enters the group, arrow keys move between items, Enter and Space press the focused item.
- Avoid: Long or dynamic option lists (use Select) and independent actions that merely sit together (use Group).
- Related: [Toggle](#toggle), [Select](#select), [Group](#group).

### Panel tab close button

The close affordance of a right-panel or surface tab.

- Kind: Primitive; status: available; id: `panel-tab-close-button`.
- Import: `import { PanelTabCloseButton } from "~/components/ui/panel-tab-close-button";`
- Source: [src/components/ui/panel-tab-close-button.tsx](../../web/conversation/src/components/ui/panel-tab-close-button.tsx).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: A focusable button inside the tab; Enter and Space close the tab.
- Avoid: Dismissing dialogs, toasts or banners; they own their close controls.
- Related: [Right panel tabs](#right-panel-tabs), [Button](#button).

### Refresh icon

The glyph inside a refresh or retry Button; `refreshing` spins it while the request runs, honouring reduced motion.

- Kind: Primitive; status: available; id: `refresh-icon`.
- Import: `import { RefreshIcon } from "~/components/ui/refresh-icon";`
- Source: [src/components/ui/refresh-icon.tsx](../../web/conversation/src/components/ui/refresh-icon.tsx).
- Variants: `size`: `xs`, `sm`, `md`, `lg`.
- States: refreshing.
- Keyboard: Not focusable; the enclosing Button owns keyboard access and the accessible name.
- Avoid: A standalone loading indicator (use Spinner) or a decorative animation.
- Related: [Button](#button), [Spinner](#spinner).

## Forms

### Input

Single-line text entry with an associated Label; `size="compact"` in toolbars and dense rows.

- Kind: Primitive; status: available; id: `input`.
- Import: `import { Input, InputProps } from "~/components/ui/input";`
- Source: [src/components/ui/input.tsx](../../web/conversation/src/components/ui/input.tsx).
- Variants: `size`: `sm`, `compact`, `default`, `lg`; `font`: `default`, `mono`.
- States: hover, focus-visible, disabled, invalid, read-only.
- Keyboard: Native text input editing and selection.
- Avoid: Multi-line text (use Textarea), choices from a known list (use Select or Combobox) and search inside a popup (the Command and Combobox inputs own that).
- Related: [Label](#label), [Textarea](#textarea), [Input group](#input-group).

### Textarea

Multi-line form text such as descriptions and notes; it sizes to its content.

- Kind: Primitive; status: available; id: `textarea`.
- Import: `import { Textarea, TextareaProps } from "~/components/ui/textarea";`
- Source: [src/components/ui/textarea.tsx](../../web/conversation/src/components/ui/textarea.tsx).
- Variants: `size`: `sm`, `default`, `lg`.
- States: hover, focus-visible, disabled, invalid, read-only.
- Keyboard: Native multi-line editing; Enter inserts a newline.
- Avoid: The conversation or issue prompt: the composer's Lexical editor owns rich prompt input.
- Related: [Input](#input), [Label](#label), [Composer](#composer).

### Label

The visible name of every form control, associated through htmlFor or by wrapping the control.

- Kind: Primitive; status: available; id: `label`.
- Import: `import { Label } from "~/components/ui/label";`
- Source: [src/components/ui/label.tsx](../../web/conversation/src/components/ui/label.tsx).
- Variants: none.
- States: disabled.
- Keyboard: Clicking the label focuses or toggles its associated control.
- Avoid: Section headings or decorative captions; a placeholder or tooltip is not a substitute label.
- Related: [Input](#input), [Checkbox](#checkbox), [Switch](#switch).

### Checkbox

Independent selections and multi-select of rows; indeterminate for a partially selected group.

- Kind: Primitive; status: available; id: `checkbox`.
- Import: `import { Checkbox } from "~/components/ui/checkbox";`
- Source: [src/components/ui/checkbox.tsx](../../web/conversation/src/components/ui/checkbox.tsx).
- Variants: none.
- States: hover, focus-visible, checked, indeterminate, disabled, invalid.
- Keyboard: Tab focuses; Space toggles.
- Avoid: An immediately applied on/off preference (use Switch) or one choice among peers.
- Related: [Switch](#switch), [Label](#label).

### Switch

A boolean setting that takes effect immediately, typically as the trailing control of a SettingsRow.

- Kind: Primitive; status: available; id: `switch`.
- Import: `import { Switch } from "~/components/ui/switch";`
- Source: [src/components/ui/switch.tsx](../../web/conversation/src/components/ui/switch.tsx).
- Variants: `size`: `default`, `sm`.
- States: hover, focus-visible, checked, disabled, mixed.
- Keyboard: Tab focuses; Space or Enter toggles.
- Avoid: Selections submitted with a form (use Checkbox) and actions (use Button).
- Related: [Checkbox](#checkbox), [Settings layout](#settings-layout).

### Radio group

One visible choice among a few peers in a form.

- Kind: Primitive; status: available; id: `radio-group`.
- Import: `import { RadioGroup, Radio, RadioGroupItem } from "~/components/ui/radio-group";`
- Source: [src/components/ui/radio-group.tsx](../../web/conversation/src/components/ui/radio-group.tsx).
- Variants: none.
- States: hover, focus-visible, checked, disabled.
- Keyboard: Base UI radio group: Tab enters the group, arrow keys move and select.
- Avoid: Long lists (use Select), view switches (use ToggleGroup) and independent options (use Checkbox).
- Related: [Select](#select), [Toggle group](#toggle-group).

### Number field

Bounded numeric entry with steppers.

- Kind: Primitive; status: available; id: `number-field`.
- Import: `import { NumberField, NumberFieldGroup, NumberFieldDecrement, NumberFieldIncrement, NumberFieldInput } from "~/components/ui/number-field";`
- Source: [src/components/ui/number-field.tsx](../../web/conversation/src/components/ui/number-field.tsx).
- Variants: `size`: `sm`, `default`, `lg`.
- States: hover, focus-visible, disabled, invalid.
- Keyboard: Base UI number field: arrow keys step the value; increment and decrement buttons are focusable.
- Avoid: Free-form identifiers that happen to be digits; use Input.
- Related: [Input](#input).

### Input group

An input with attached prefix, suffix or action addons.

- Kind: Primitive; status: available; id: `input-group`.
- Import: `import { InputGroup, InputGroupAddon, InputGroupInput } from "~/components/ui/input-group";`
- Source: [src/components/ui/input-group.tsx](../../web/conversation/src/components/ui/input-group.tsx).
- Variants: `variant`: `default`, `ghost`; `align`: `block-end`, `block-start`, `inline-end`, `inline-start`.
- States: focus-visible, disabled, invalid.
- Keyboard: The inner input keeps native editing; addons that are buttons are separately focusable.
- Avoid: Hand-building addon frames around Input in a feature folder.
- Related: [Input](#input), [Group](#group).

### Draft input

A text setting that should commit once editing finishes rather than on every keystroke.

- Kind: Primitive; status: available; id: `draft-input`.
- Import: `import { DraftInput, DraftInputProps } from "~/components/ui/draft-input";`
- Source: [src/components/ui/draft-input.tsx](../../web/conversation/src/components/ui/draft-input.tsx).
- Variants: none.
- States: focus-visible, disabled.
- Keyboard: Native editing; the value commits on blur or Enter.
- Avoid: Form fields submitted together; they keep ordinary controlled Inputs.
- Related: [Input](#input), [Settings layout](#settings-layout).

### Color picker

Choosing an arbitrary colour.

- Kind: Primitive; status: available; id: `color-picker`.
- Import: `import { ColorSaturationValuePlane, ColorHueSlider } from "~/components/ui/color-picker";`
- Source: [src/components/ui/color-picker.tsx](../../web/conversation/src/components/ui/color-picker.tsx).
- Variants: none.
- States: focus-visible, dragging.
- Keyboard: plane and slider respond to arrow keys.
- Avoid: Picking from Detent's semantic palette; offer the named options instead.

### Calendar

Picking a date inside a Popover.

- Kind: Primitive; status: available; id: `calendar`.
- Import: `import { Calendar } from "~/components/ui/calendar";`
- Source: [src/components/ui/calendar.tsx](../../web/conversation/src/components/ui/calendar.tsx).
- Variants: none.
- States: hover, focus-visible, selected, disabled.
- Keyboard: day grid: arrow keys move between days, Enter selects.
- Avoid: Relative time choices (use Select with named ranges).
- Related: [Popover](#popover).

## Choice and search

### Select

One saved value from a short, known list. Use SelectButton to give a Menu or Combobox trigger the same field look.

- Kind: Primitive; status: available; id: `select`.
- Import: `import { Select, SelectTrigger, SelectButton, selectTriggerVariants, SelectValue, SelectPopup, SelectContent, SelectItem, SelectSeparator, SelectGroup, SelectGroupLabel } from "~/components/ui/select";`
- Source: [src/components/ui/select.tsx](../../web/conversation/src/components/ui/select.tsx).
- Variants: `variant`: `default`, `ghost`; `size`: `compact`, `default`, `lg`, `sm`, `xs`.
- States: hover, focus-visible, open, highlighted, selected, disabled, invalid.
- Keyboard: Base UI select: Enter, Space or arrow keys open; arrow keys move; typeahead jumps; Enter selects; Escape closes and returns focus.
- Avoid: Searchable or long lists (use Combobox) and lists of actions (use Menu).
- Related: [Combobox](#combobox), [Menu](#menu), [Toggle group](#toggle-group).

### Combobox

Choosing one or more values from a long or searchable list, such as projects, models or labels.

- Kind: Primitive; status: available; id: `combobox`.
- Import: `import { Combobox, ComboboxChipsInput, ComboboxInput, ComboboxSearchInput, ComboboxTrigger, ComboboxPopup, ComboboxItem, ComboboxSeparator, ComboboxGroup, ComboboxGroupLabel, ComboboxEmpty, ComboboxValue, ComboboxList, ComboboxListVirtualized, ComboboxClear, ComboboxStatus, ComboboxRow, ComboboxCollection, ComboboxChips, ComboboxChip, useComboboxFilter } from "~/components/ui/combobox";`
- Source: [src/components/ui/combobox.tsx](../../web/conversation/src/components/ui/combobox.tsx).
- Variants: `size`: `sm`, `default`, `lg`.
- States: focus-visible, open, highlighted, selected, empty, disabled.
- Keyboard: Type to filter; arrow keys move the highlight; Enter selects; Escape closes. Chips are removable with Backspace from an empty input.
- Avoid: Short fixed lists (use Select) and global navigation (use the command palette).
- Related: [Select](#select), [Autocomplete](#autocomplete), [Command](#command).

### Autocomplete

Free text with suggestions, and the input layer the Command primitive builds on.

- Kind: Primitive; status: available; id: `autocomplete`.
- Import: `import { Autocomplete, AutocompleteInput, AutocompleteTrigger, AutocompletePopup, AutocompleteItem, AutocompleteSeparator, AutocompleteGroup, AutocompleteGroupLabel, AutocompleteEmpty, AutocompleteValue, AutocompleteList, AutocompleteClear, AutocompleteStatus, AutocompleteRow, AutocompleteCollection, useAutocompleteFilter, AutocompleteListHeading, AutocompleteListVirtualized } from "~/components/ui/autocomplete";`
- Source: [src/components/ui/autocomplete.tsx](../../web/conversation/src/components/ui/autocomplete.tsx).
- Variants: `size`: `sm`, `default`, `lg`.
- States: focus-visible, open, highlighted, empty, disabled.
- Keyboard: Type to filter suggestions; arrow keys move the highlight; Enter accepts; Escape closes. The typed text remains the value.
- Avoid: Choosing from a closed set (use Combobox or Select).
- Related: [Combobox](#combobox), [Command](#command).

### Command

The searchable command surface behind the command palette and similar quick-pick dialogs.

- Kind: Primitive; status: available; id: `command`.
- Import: `import { CommandCreateHandle, Command, CommandCollection, CommandDialog, CommandDialogPopup, CommandDialogTrigger, CommandEmpty, CommandFooter, CommandFooterAction, CommandGroup, CommandGroupLabel, CommandInput, CommandItem, CommandList, CommandPanel, CommandSeparator, CommandShortcut, CommandListHeading, CommandListVirtualized } from "~/components/ui/command";`
- Source: [src/components/ui/command.tsx](../../web/conversation/src/components/ui/command.tsx).
- Variants: none.
- States: open, highlighted, empty.
- Keyboard: Focus starts in the search input; arrow keys move between items; Enter runs the highlighted item; Escape closes the dialog and returns focus.
- Avoid: A second palette implementation in a feature folder; extend the command palette's sources instead.
- Related: [Command palette](#command-palette), [Autocomplete](#autocomplete), [Dialog](#dialog).

## Overlays

### Dialog

A focused task that interrupts the page: a short form, a confirmation with input, a detail that needs the whole attention. `variant="media"` is the full-bleed image and video viewer.

- Kind: Primitive; status: available; id: `dialog`.
- Import: `import { DialogCreateHandle, Dialog, DialogTrigger, DialogPortal, DialogClose, DialogBackdrop, DialogOverlay, DialogPopup, DialogContent, DialogHeader, DialogFooter, DialogTitle, DialogDescription, DialogPanel, DialogViewport, DIALOG_BACKDROP_CLASS, DIALOG_POPUP_CLASS, DIALOG_MOBILE_SHEET_CLASS, DIALOG_MEDIA_BACKDROP_CLASS, DIALOG_MEDIA_POPUP_CLASS } from "~/components/ui/dialog";`
- Source: [src/components/ui/dialog.tsx](../../web/conversation/src/components/ui/dialog.tsx), with [src/components/ui/dialog-styles.ts](../../web/conversation/src/components/ui/dialog-styles.ts).
- Variants: `footerVariant`: `default`, `bare`; `variant`: `default`, `media`.
- States: open, nested, focus-visible.
- Keyboard: Focus moves into the dialog and is contained; Escape closes; focus returns to the trigger.
- Avoid: Destructive confirmations (use AlertDialog), docked panels (use Sheet) and contextual pickers (use Popover).
- Related: [Alert dialog](#alert-dialog), [Sheet](#sheet), [Popover](#popover).

### Alert dialog

Confirming a destructive or irreversible action, with a clear destructive Button and a cancel path.

- Kind: Primitive; status: available; id: `alert-dialog`.
- Import: `import { AlertDialogCreateHandle, AlertDialog, AlertDialogPortal, AlertDialogBackdrop, AlertDialogOverlay, AlertDialogTrigger, AlertDialogPopup, AlertDialogContent, AlertDialogHeader, AlertDialogFooter, AlertDialogTitle, AlertDialogDescription, AlertDialogClose, AlertDialogViewport } from "~/components/ui/alert-dialog";`
- Source: [src/components/ui/alert-dialog.tsx](../../web/conversation/src/components/ui/alert-dialog.tsx).
- Variants: `footerVariant`: `default`, `bare`.
- States: open, focus-visible.
- Keyboard: Focus is contained; Escape cancels; the backdrop does not dismiss; focus returns to the trigger.
- Avoid: Informational messages (use Alert or a toast) and forms (use Dialog).
- Related: [Dialog](#dialog), [Button](#button).

### Sheet

A panel docked to a viewport edge, chiefly to present the sidebar and right panel on narrow viewports.

- Kind: Primitive; status: available; id: `sheet`.
- Import: `import { Sheet, SheetTrigger, SheetPortal, SheetClose, SheetBackdrop, SheetOverlay, SheetPopup, SheetContent, SheetHeader, SheetFooter, SheetTitle, SheetDescription, SheetPanel } from "~/components/ui/sheet";`
- Source: [src/components/ui/sheet.tsx](../../web/conversation/src/components/ui/sheet.tsx).
- Variants: `side`: `right`, `left`, `top`, `bottom`; `variant`: `default`, `inset`; `footerVariant`: `default`, `bare`.
- States: open, focus-visible.
- Keyboard: Dialog semantics: focus is contained, Escape closes, focus returns to the trigger.
- Avoid: Centered tasks (use Dialog); wide desktop layouts keep panels inline.
- Related: [Dialog](#dialog), [Right panel sheet](#right-panel-sheet), [Sidebar primitive](#sidebar-primitive).

### Popover

A small contextual interaction anchored to its trigger: a filter, a short form, pinned help. `width` sets sm 256px, md 320px or lg 384px; `padding` default, compact or none; `variant="panel"` docks a details panel at the sheet layer, at the thread-details width; `keepMounted` keeps a heavy popup alive while closed.

- Kind: Primitive; status: available; id: `popover`.
- Import: `import { PopoverCreateHandle, Popover, PopoverTrigger, PopoverPopup, PopoverContent, PopoverTitle, PopoverDescription, PopoverClose } from "~/components/ui/popover";`
- Source: [src/components/ui/popover.tsx](../../web/conversation/src/components/ui/popover.tsx).
- Variants: `variant`: `default`, `panel`; `width`: `auto`, `sm`, `md`, `lg`; `padding`: `default`, `compact`, `none`.
- States: open, focus-visible, kept mounted.
- Keyboard: The trigger opens it; focus moves into interactive content; Escape closes and returns focus.
- Avoid: Lists of actions (use Menu), hover-only supplementary text (use Tooltip) and long tasks (use Dialog).
- Related: [Menu](#menu), [Tooltip](#tooltip), [Preview card](#preview-card).

### Menu

A list of actions or toggles behind a trigger: overflow menus, row actions, lane menus. `density="touch"` gives rows a 40px height for touch; a `ghost` item is a quiet action row; `MenuItemLabel` holds an item's text beside its icon.

- Kind: Primitive; status: available; id: `menu`.
- Import: `import { MenuCreateHandle, Menu, MenuPortal, MenuTrigger, MenuPopup, MenuGroup, MenuItem, MenuCheckboxItem, MenuRadioGroup, MenuRadioItem, MenuRadioItemIndicator, MenuGroupLabel, MenuSeparator, MenuShortcut, MenuSub, MenuSubTrigger, MenuSubPopup, DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, MenuItemLabel } from "~/components/ui/menu";`
- Source: [src/components/ui/menu.tsx](../../web/conversation/src/components/ui/menu.tsx).
- Variants: `itemVariant`: `default`, `destructive`, `ghost`; `checkboxItemVariant`: `default`, `switch`; `density`: `default`, `touch`.
- States: open, highlighted, checked, disabled, kept mounted.
- Keyboard: Enter, Space or arrow keys open from the trigger; arrow keys move; typeahead jumps; Enter activates; ArrowRight opens a submenu; Escape closes and returns focus.
- Avoid: Choosing a saved value (use Select) and searching (use Combobox or Command).
- Related: [Select](#select), [Popover](#popover), [Button](#button).

### Tooltip

Brief supplementary text for a control that already has an accessible name, such as an icon button's label and shortcut. `variant="code"` shows a path or command in monospace; wrap a scrolling region in `TooltipScrollDismissArea` so open tooltips close when it scrolls.

- Kind: Primitive; status: available; id: `tooltip`.
- Import: `import { TooltipCreateHandle, TooltipProvider, Tooltip, TooltipTrigger, TooltipPopup, TooltipScrollDismissArea } from "~/components/ui/tooltip";`
- Source: [src/components/ui/tooltip.tsx](../../web/conversation/src/components/ui/tooltip.tsx).
- Variants: `variant`: `default`, `glass`, `code`.
- States: open.
- Keyboard: Opens when its trigger receives keyboard focus; Escape closes. Content is not focusable.
- Avoid: The only accessible name of a control, essential instructions, or interactive content (use Popover).
- Related: [Popover](#popover), [Preview card](#preview-card), [Keyboard key](#keyboard-key).

### Preview card

A rich preview of a linked entity, such as a pull request link preview.

- Kind: Primitive; status: available; id: `preview-card`.
- Import: `import { PreviewCard, PreviewCardPopup, PreviewCardTrigger } from "~/components/ui/preview-card";`
- Source: [src/components/ui/preview-card.tsx](../../web/conversation/src/components/ui/preview-card.tsx).
- Variants: none.
- States: open.
- Keyboard: Opens on hover or focus of its link trigger; the link itself stays the keyboard path to the content.
- Avoid: Content the user must act on (use Popover) and plain hints (use Tooltip).
- Related: [Tooltip](#tooltip), [Popover](#popover).

## Feedback

### Alert

A persistent inline message in the place the user must act: a warning above a form, an unavailable state with a corrective action. `surface="glass"` sits over imagery; `variant="sidebar"` is for notices inside the sidebar.

- Kind: Primitive; status: available; id: `alert`.
- Import: `import { Alert, AlertTitle, AlertDescription, AlertAction } from "~/components/ui/alert";`
- Source: [src/components/ui/alert.tsx](../../web/conversation/src/components/ui/alert.tsx).
- Variants: `variant`: `default`, `info`, `success`, `warning`, `error`, `sidebar`; `controlAlignment`: `center`, `first-line`; `surface`: `default`, `glass`.
- States: none of its own.
- Keyboard: Not interactive itself; AlertAction holds focusable actions.
- Avoid: Transient confirmations (use a toast) and status labels (use Badge). Optional features not set up use muted text and one Set up action; follow [Feedback and interaction states](patterns.md#feedback-and-interaction-states).
- Related: [Toast](#toast), [Badge](#badge), [Empty state](#empty-state).

### Badge

A short static status or count with semantic colour, paired with text so colour is not the only signal. `variant="label"` renders a code-host label, tinted from the `--label` colour set in `style`.

- Kind: Primitive; status: available; id: `badge`.
- Import: `import { Badge, badgeVariants } from "~/components/ui/badge";`
- Source: [src/components/ui/badge.tsx](../../web/conversation/src/components/ui/badge.tsx).
- Variants: `variant`: `default`, `secondary`, `outline`, `info`, `success`, `warning`, `error`, `destructive`, `label`; `size`: `control`, `default`, `lg`, `sm`.
- States: none of its own.
- Keyboard: Static by default; with `render` it can become a link or button and takes that element's keyboard behaviour.
- Avoid: Actions (use Button) and context reference chips in the composer (use the file tag chip).
- Related: [Alert](#alert), [Thread status indicators](#thread-status-indicators).

### Toast

Transient feedback about an action's outcome, scoped to a thread where relevant; anchored copy toasts confirm a copy next to its button.

- Kind: Primitive; status: available; id: `toast`.
- Import: `import { ToastProvider, toastManager, AnchoredToastProvider, anchoredToastManager, stackedThreadToast, hiddenToastActionProps, showAnchoredCopySuccessToast, showAnchoredCopyErrorToast, ThreadToastData, ToastPosition } from "~/components/ui/toast";`
- Source: [src/components/ui/toast.tsx](../../web/conversation/src/components/ui/toast.tsx), with [src/components/ui/toast.logic.ts](../../web/conversation/src/components/ui/toast.logic.ts), [src/components/ui/toastHelpers.ts](../../web/conversation/src/components/ui/toastHelpers.ts), [src/components/ui/anchoredCopyToast.ts](../../web/conversation/src/components/ui/anchoredCopyToast.ts).
- Variants: `actionLayout`: `inline`, `stacked-end`.
- States: loading, success, error, info, warning, expanded.
- Keyboard: Toasts are announced politely; action and close buttons are focusable; the stacked thread toast expands with its toggle button.
- Avoid: Errors the user must fix in place (use Alert or field errors) and progress that needs a persistent surface.
- Related: [Alert](#alert), [Spinner](#spinner).

### Spinner

Indeterminate progress inside a control or small region, keeping the surrounding layout and label stable. `size` matches the icon sizes (xs 12px to lg 20px); `tone` is `current` (inherits the text colour) or `muted`.

- Kind: Primitive; status: available; id: `spinner`.
- Import: `import { Spinner } from "~/components/ui/spinner";`
- Source: [src/components/ui/spinner.tsx](../../web/conversation/src/components/ui/spinner.tsx).
- Variants: `size`: `xs`, `sm`, `md`, `lg`; `tone`: `current`, `muted`.
- States: loading.
- Keyboard: Not focusable.
- Avoid: Loading a whole region whose shape is known (use Skeleton).
- Related: [Skeleton](#skeleton), [Refresh icon](#refresh-icon).

### Skeleton

A placeholder with the shape of content that is loading, using the shared stepped animation. `shape` is `block` (the default rectangle), `card` or `pill`.

- Kind: Primitive; status: available; id: `skeleton`.
- Import: `import { Skeleton } from "~/components/ui/skeleton";`
- Source: [src/components/ui/skeleton.tsx](../../web/conversation/src/components/ui/skeleton.tsx).
- Variants: `shape`: `block`, `card`, `pill`.
- States: loading.
- Keyboard: Not focusable.
- Avoid: Inventing another shimmer, and showing skeletons for content that failed to load (show the error state).
- Related: [Spinner](#spinner), [Empty state](#empty-state).

### Empty state

Explaining why a region has no content and offering the relevant next action. `size="compact"` fits panels and popovers; `hero` is a first-run page. Optional features not set up stay neutral, with one Set up action and no Needs attention contribution; follow [Feedback and interaction states](patterns.md#feedback-and-interaction-states).

- Kind: Primitive; status: available; id: `empty`.
- Import: `import { Empty, EmptyHeader, EmptyTitle, EmptyDescription, EmptyContent, EmptyMedia } from "~/components/ui/empty";`
- Source: [src/components/ui/empty.tsx](../../web/conversation/src/components/ui/empty.tsx).
- Variants: `variant`: `default`, `icon`; `size`: `compact`, `default`, `hero`.
- States: none of its own.
- Keyboard: Not interactive itself; EmptyContent holds focusable actions.
- Avoid: Errors and unavailable capabilities that are not simply absence (use Alert).
- Related: [Alert](#alert), [Skeleton](#skeleton).

### Runner status dot

Runner health beside a runner name in Fleet and Activity; healthy active runners pulse green, inactive runners remain neutral.

- Kind: Composition; status: available; id: `runner-status-dot`.
- Import: `import { RunnerStatusDot } from "~/components/RunnerStatusDot";`
- Source: [src/components/RunnerStatusDot.tsx](../../web/conversation/src/components/RunnerStatusDot.tsx).
- Variants: none.
- States: healthy, paused, failed, needs attention, asleep, offline.
- Keyboard: Static status image; the surrounding control owns keyboard behavior.
- Avoid: Work stage or job outcome; use StageProgress for stage timing.

### Stage progress

A compact ordered stage bar with a current step, progress, accessible name and timing tooltip. The caller supplies the stage and speed from server timing.

- Kind: Composition; status: available; id: `stage-progress`.
- Import: `import { StageProgress } from "~/components/StageProgress";`
- Source: [src/components/StageProgress.tsx](../../web/conversation/src/components/StageProgress.tsx).
- Variants: none.
- States: working, success, warning, error, muted.
- Keyboard: Tab focuses the timing bar and opens its tooltip.
- Avoid: Runner health, throughput charts or estimates computed from client history.

## Navigation

### Sidebar primitive

The building blocks of Detent's app sidebar: provider, rail, groups and SidebarMenuButton rows with the shared hover, active and selected tokens.

- Kind: Primitive; status: available; id: `sidebar`.
- Import: `import { Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupAction, SidebarGroupContent, SidebarGroupLabel, SidebarHeader, SidebarInput, SidebarInset, SidebarMenu, SidebarMenuBadge, SidebarMenuButton, SidebarMenuItem, SidebarMenuSkeleton, SidebarMenuSub, SidebarMenuSubButton, SidebarMenuSubItem, SidebarProvider, SidebarRail, SidebarSeparator, SidebarTrigger, useSidebar, useSidebarVisibility, resolveSidebarState } from "~/components/ui/sidebar";`
- Source: [src/components/ui/sidebar.tsx](../../web/conversation/src/components/ui/sidebar.tsx), with [src/components/ui/sidebarState.ts](../../web/conversation/src/components/ui/sidebarState.ts).
- Variants: `variant`: `default`, `outline`; `size`: `default`, `icon`, `lg`, `sm`; `side`: `left`, `right`; `sidebarVariant`: `sidebar`, `floating`, `inset`; `collapsible`: `offcanvas`, `icon`, `none`; `subButtonSize`: `sm`, `md`.
- States: hover, focus-visible, active, collapsed, resizing, disabled.
- Keyboard: SidebarTrigger and menu buttons are native buttons or links; the rail is a resize handle. On narrow viewports the sidebar opens as a Sheet with dialog focus handling.
- Avoid: Using it for in-page navigation lists; settings use SettingsSidebarNav, which composes these blocks.
- Related: [Thread sidebar](#thread-sidebar), [App sidebar layout](#app-sidebar-layout), [Settings sidebar nav](#settings-sidebar-nav).

## Structure

### Scroll area

Any bounded scrolling region inside panels, popups and lists, optionally with scroll fades.

- Kind: Primitive; status: available; id: `scroll-area`.
- Import: `import { ScrollArea, ScrollBar, getVirtualizedScrollFadeClassName } from "~/components/ui/scroll-area";`
- Source: [src/components/ui/scroll-area.tsx](../../web/conversation/src/components/ui/scroll-area.tsx).
- Variants: none.
- States: focus-visible, overflowing.
- Keyboard: The viewport is focusable and scrolls with arrow, Page and Home/End keys.
- Avoid: Wrapping the page body; the document scrolls natively.
- Related: [Separator](#separator).

### Separator

A quiet horizontal or vertical divider between rows, groups and toolbar sections.

- Kind: Primitive; status: available; id: `separator`.
- Import: `import { Separator } from "~/components/ui/separator";`
- Source: [src/components/ui/separator.tsx](../../web/conversation/src/components/ui/separator.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable.
- Avoid: Wrapping sections in cards just to separate them.
- Related: [Group](#group), [Scroll area](#scroll-area).

### Group

Joining related buttons, inputs and text segments into one attached control, such as a split action.

- Kind: Primitive; status: available; id: `group`.
- Import: `import { Group, ButtonGroup, GroupText, ButtonGroupText, GroupSeparator, ButtonGroupSeparator, groupVariants } from "~/components/ui/group";`
- Source: [src/components/ui/group.tsx](../../web/conversation/src/components/ui/group.tsx).
- Variants: `orientation`: `horizontal`, `vertical`.
- States: none of its own.
- Keyboard: Each grouped control keeps its own focus; the group adds none.
- Avoid: Exclusive choices (use ToggleGroup) and loose toolbars (use a flex row with gap-2).
- Related: [Button](#button), [Toggle group](#toggle-group).

### Collapsible

Showing and hiding a secondary section in place, such as tool details or a lane.

- Kind: Primitive; status: available; id: `collapsible`.
- Import: `import { Collapsible, CollapsibleTrigger, CollapsiblePanel, CollapsibleContent } from "~/components/ui/collapsible";`
- Source: [src/components/ui/collapsible.tsx](../../web/conversation/src/components/ui/collapsible.tsx).
- Variants: none.
- States: expanded, collapsed, focus-visible, animated.
- Keyboard: The trigger is a button with aria-expanded; Enter and Space toggle.
- Avoid: Hiding primary content or required form fields.
- Related: [Collapsible section header](#collapsible-section-header).

### Collapsible section header

A ruled, toned section header that owns collapse geometry, when a feature needs one.

- Kind: Primitive; status: available; id: `collapsible-section-header`.
- Import: `import { CollapsibleSectionHeader, SectionHeaderStatus } from "~/components/ui/collapsible-section-header";`
- Source: [src/components/ui/collapsible-section-header.tsx](../../web/conversation/src/components/ui/collapsible-section-header.tsx).
- Variants: none.
- States: expanded, collapsed, focus-visible.
- Keyboard: A button header with aria-expanded; Enter and Space toggle.
- Avoid: Disclosure inside content (use Collapsible) and restyling existing sidebar sections with it.
- Related: [Collapsible](#collapsible).

### Middle truncate

Branch names, paths, worktree names and shas whose meaning is at both ends.

- Kind: Primitive; status: available; id: `middle-truncate`.
- Import: `import { MiddleTruncate, splitForMiddleTruncate } from "~/components/ui/middle-truncate";`
- Source: [src/components/ui/middle-truncate.tsx](../../web/conversation/src/components/ui/middle-truncate.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not interactive; the full string remains selectable and readable by assistive technology.
- Avoid: Prose and titles, which truncate at the end.

### Animated height

Easing a container to its content's new height when the content changes, optionally holding the old height while a replacement loads.

- Kind: Primitive; status: available; id: `animated-height`.
- Import: `import { AnimatedHeight } from "~/components/AnimatedHeight";`
- Source: [src/components/AnimatedHeight.tsx](../../web/conversation/src/components/AnimatedHeight.tsx).
- Variants: none.
- States: resizing.
- Keyboard: No behaviour of its own.
- Avoid: Open and close disclosures (use Collapsible) and layout that should not animate.
- Related: [Collapsible](#collapsible), [Wizard](#wizard).

### Render error boundary

Isolating a fragile renderer (highlighted code, diffs, files) so a render error shows a fallback and retries when resetKeys change.

- Kind: Primitive; status: available; id: `render-error-boundary`.
- Import: `import { RenderErrorBoundary } from "~/components/RenderErrorBoundary";`
- Source: [src/components/RenderErrorBoundary.tsx](../../web/conversation/src/components/RenderErrorBoundary.tsx).
- Variants: none.
- States: failed.
- Keyboard: No behaviour of its own.
- Avoid: Handling expected errors such as failed requests; render those states directly.
- Related: [Diff surface](#diff-surface), [Files surface](#files-surface), [Chat markdown](#chat-markdown).

## Data display

### Table

Tabular data such as usage and billing rows: labels left-aligned, comparable numbers right-aligned with tabular numerals.

- Kind: Primitive; status: available; id: `table`.
- Import: `import { Table, TableHeader, TableBody, TableFooter, TableHead, TableRow, TableCell, TableCaption } from "~/components/ui/table";`
- Source: [src/components/ui/table.tsx](../../web/conversation/src/components/ui/table.tsx).
- Variants: none.
- States: hover, selected.
- Keyboard: Native table semantics; interactive cells hold their own focusable controls.
- Avoid: Layout grids and lists of navigable entities (use rows or cards from the owning composition).
- Related: [Usage page](#usage-page).

### Keyboard key

Showing a keyboard shortcut in tooltips, menus and the command palette.

- Kind: Primitive; status: available; id: `kbd`.
- Import: `import { Kbd, KbdGroup } from "~/components/ui/kbd";`
- Source: [src/components/ui/kbd.tsx](../../web/conversation/src/components/ui/kbd.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable.
- Avoid: Inline code or values (use the markdown code style).
- Related: [Tooltip](#tooltip), [Menu](#menu).

### Discovery list

A bordered list of icon, title and description rows for discovering integrations or options.

- Kind: Primitive; status: available; id: `discovery-list`.
- Import: `import { DiscoveryList, DiscoveryListRow } from "~/components/ui/discovery-list";`
- Source: [src/components/ui/discovery-list.tsx](../../web/conversation/src/components/ui/discovery-list.tsx).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: Rows with actions expose them as buttons.
- Avoid: Settings rows (use SettingsRow).
- Related: [Settings layout](#settings-layout).

## Entry

### Standalone page

Page and card geometry for an entry point outside the app shell.

- Kind: Primitive; status: available; id: `standalone-page`.
- Import: `import { StandalonePage, StandalonePageHeader } from "~/components/ui/standalone-page";`
- Source: [src/components/ui/standalone-page.tsx](../../web/conversation/src/components/ui/standalone-page.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: No behaviour of its own.
- Avoid: Pages inside the workspace shell (use WorkspacePageContainer).
- Related: [Wizard](#wizard), [Workspace page container](#workspace-page-container).

### Wizard

A multi-step flow in a dialog whose step logic stays with the caller.

- Kind: Primitive; status: available; id: `wizard`.
- Import: `import { WizardPopup, WizardHeader, WizardFooter, WizardSteps, WizardPanel } from "~/components/ui/wizard";`
- Source: [src/components/ui/wizard.tsx](../../web/conversation/src/components/ui/wizard.tsx).
- Variants: none.
- States: open.
- Keyboard: Dialog semantics from DialogPopup; step navigation buttons are focusable.
- Avoid: Single-step forms (use Dialog).
- Related: [Dialog](#dialog), [Standalone page](#standalone-page).

### QR code

Pairing a device by scanning.

- Kind: Primitive; status: available; id: `qr-code`.
- Import: `import { QRCodeSvg } from "~/components/ui/qr-code";`
- Source: [src/components/ui/qr-code.tsx](../../web/conversation/src/components/ui/qr-code.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable; pair it with the same value as text.
- Avoid: The only way to convey a link or token.

## Workspace

### App sidebar layout

The shell around every workspace route: sidebar provider, resizable thread sidebar and the main inset.

- Kind: Composition; status: available; id: `app-sidebar-layout`.
- Import: `import { AppSidebarLayout } from "~/components/AppSidebarLayout";`
- Source: [src/components/AppSidebarLayout.tsx](../../web/conversation/src/components/AppSidebarLayout.tsx), with [src/components/threadSidebarWidth.ts](../../web/conversation/src/components/threadSidebarWidth.ts).
- Variants: none.
- States: collapsed, resizing.
- Keyboard: Hosts the sidebar control; focus order runs sidebar first, then the main region.
- Avoid: Nesting a second shell or sidebar inside a route.
- Related: [Thread sidebar](#thread-sidebar), [Sidebar primitive](#sidebar-primitive), [Workspace page header](#workspace-page-header).

### Thread sidebar

The app's navigation list: project sections, thread rows (SidebarThreadRow), draft rows, search results and the snooze control, rendered with the Sidebar primitive.

- Kind: Composition; status: available; id: `thread-sidebar`.
- Import: `import Sidebar from "~/components/Sidebar";`
- Source: [src/components/Sidebar.tsx](../../web/conversation/src/components/Sidebar.tsx), with [src/components/Sidebar.logic.ts](../../web/conversation/src/components/Sidebar.logic.ts), [src/components/Sidebar.drag.ts](../../web/conversation/src/components/Sidebar.drag.ts), [src/components/Sidebar.motion.ts](../../web/conversation/src/components/Sidebar.motion.ts), [src/components/Sidebar.pointer.ts](../../web/conversation/src/components/Sidebar.pointer.ts), [src/components/Sidebar.snooze.ts](../../web/conversation/src/components/Sidebar.snooze.ts), [src/components/threadActionMenu.logic.ts](../../web/conversation/src/components/threadActionMenu.logic.ts), [src/components/BranchToolbar.logic.ts](../../web/conversation/src/components/BranchToolbar.logic.ts).
- Variants: none.
- States: hover, active, selected, dragging, collapsed, empty.
- Keyboard: Thread, draft and search-result rows are links or buttons in document order; row menus open with Enter or Space; drag reordering has a menu alternative.
- Avoid: New row shapes or status colours; extend the existing row and ThreadStatusIndicators instead.
- Related: [App sidebar layout](#app-sidebar-layout), [Sidebar chrome](#sidebar-chrome), [Thread status indicators](#thread-status-indicators), [Sidebar primitive](#sidebar-primitive).

### Sidebar chrome

The sidebar's header (brand, primary actions) and footer (utility menu, update pill).

- Kind: Composition; status: available; id: `sidebar-chrome`.
- Import: `import { SidebarChromeHeader, SidebarUtilityMenu, SidebarChromeFooter } from "~/components/sidebar/SidebarChrome";`
- Source: [src/components/sidebar/SidebarChrome.tsx](../../web/conversation/src/components/sidebar/SidebarChrome.tsx), with [src/components/pullRequest/pullRequestListPreferences.ts](../../web/conversation/src/components/pullRequest/pullRequestListPreferences.ts).
- Variants: none.
- States: hover, focus-visible, open.
- Keyboard: Header and footer items are buttons or links; the utility menu is a Menu.
- Avoid: Adding feature shortcuts to the chrome; destinations belong in the sidebar sections or command palette.
- Related: [Thread sidebar](#thread-sidebar), [Detent wordmark](#detent-wordmark).

### Sidebar workspace picker

Switching between the workspaces (organizations) the signed-in account belongs to, from the sidebar footer, with links to add a workspace or manage the current one.

- Kind: Composition; status: available; id: `sidebar-workspace-picker`.
- Import: `import { WorkspacePicker, SidebarWorkspacePicker } from "~/components/sidebar/SidebarWorkspacePicker";`
- Source: [src/components/sidebar/SidebarWorkspacePicker.tsx](../../web/conversation/src/components/sidebar/SidebarWorkspacePicker.tsx).
- Variants: none.
- States: open, pending, error, no results.
- Keyboard: The trigger opens a Menu with focus in the search field; ArrowDown and ArrowUp move from the search field to the first or last workspace; Enter switches; Escape closes and returns focus to the trigger.
- Avoid: Using it to pick a project or another entity; project choice belongs to the sidebar sections and the command palette.
- Related: [Sidebar chrome](#sidebar-chrome), [Menu](#menu), [Input](#input), [Badge](#badge).

### Detent wordmark

The Detent brand mark in the sidebar chrome and entry screens.

- Kind: Composition; status: available; id: `detent-wordmark`.
- Import: `import { DetentWordmark } from "~/components/DetentWordmark";`
- Source: [src/components/DetentWordmark.tsx](../../web/conversation/src/components/DetentWordmark.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable; a surrounding link carries the accessible name.
- Avoid: Redrawing or recolouring the mark per feature.
- Related: [Sidebar chrome](#sidebar-chrome).

### Workspace page header

The top bar of every workspace page at --workspace-topbar-height, with native titlebar and safe-area insets.

- Kind: Composition; status: available; id: `workspace-page-header`.
- Import: `import { WorkspacePageHeader } from "~/components/WorkspacePageHeader";`
- Source: [src/components/WorkspacePageHeader.tsx](../../web/conversation/src/components/WorkspacePageHeader.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: No behaviour of its own; holds the breadcrumb and header actions in reading order.
- Avoid: Building a per-feature header or patching its gutter independently of the page container.
- Related: [Workspace breadcrumb](#workspace-breadcrumb), [Workspace page container](#workspace-page-container), [Chat header](#chat-header).

### Workspace page container

The content column of a non-chat page (settings, usage, work pages) at one of three widths.

- Kind: Composition; status: available; id: `workspace-page-container`.
- Import: `import { WorkspacePageContainer, WorkspacePageWidth } from "~/components/WorkspacePageContainer";`
- Source: [src/components/WorkspacePageContainer.tsx](../../web/conversation/src/components/WorkspacePageContainer.tsx).
- Variants: `width`: `readable`, `wide`, `expanded`.
- States: none of its own.
- Keyboard: No behaviour of its own.
- Avoid: Ad-hoc max-width wrappers.
- Related: [Workspace page header](#workspace-page-header), [Settings layout](#settings-layout).

### Workspace breadcrumb

The location trail inside WorkspacePageHeader.

- Kind: Composition; status: available; id: `workspace-breadcrumb`.
- Import: `import { WorkspaceBreadcrumb, WorkspaceBreadcrumbItem, WorkspaceBreadcrumbSeparator } from "~/components/WorkspaceBreadcrumb";`
- Source: [src/components/WorkspaceBreadcrumb.tsx](../../web/conversation/src/components/WorkspaceBreadcrumb.tsx).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: Items are links in reading order.
- Avoid: Using it as tabs or a menu.
- Related: [Workspace page header](#workspace-page-header).

### Command palette

Global search and command dispatch across conversations, projects and destinations.

- Kind: Composition; status: available; id: `command-palette`.
- Import: `import { CommandPalette } from "~/app/components/CommandPalette";`
- Source: [src/app/components/CommandPalette.tsx](../../web/conversation/src/app/components/CommandPalette.tsx), with [src/components/CommandPalette.logic.ts](../../web/conversation/src/components/CommandPalette.logic.ts).
- Variants: none.
- States: open, highlighted, empty, loading.
- Keyboard: Opened by the command-palette keybinding; arrow keys move; Enter runs; Escape closes and returns focus.
- Avoid: Page-specific pickers; they use Combobox or Menu.
- Related: [Command palette content](#command-palette-content), [Command](#command).

### Command palette content

The palette's input, grouped result rows and footer, shared by the palette and its sub-pickers.

- Kind: Composition; status: available; id: `command-palette-content`.
- Import: `import { CommandPaletteContent, CommandPaletteResults } from "~/components/CommandPaletteContent";`
- Source: [src/components/CommandPaletteContent.tsx](../../web/conversation/src/components/CommandPaletteContent.tsx), with [src/components/CommandPaletteResults.tsx](../../web/conversation/src/components/CommandPaletteResults.tsx).
- Variants: none.
- States: highlighted, empty.
- Keyboard: As Command: arrow keys, Enter, Escape.
- Avoid: Rendering palette rows outside the palette.
- Related: [Command palette](#command-palette), [Command](#command).

### Project favicon

A project's identity mark in sidebar sections, pickers and headers.

- Kind: Composition; status: available; id: `project-favicon`.
- Import: `import { ProjectFavicon, ProjectFaviconProject } from "~/components/ProjectFavicon";`
- Source: [src/components/ProjectFavicon.tsx](../../web/conversation/src/components/ProjectFavicon.tsx).
- Variants: none.
- States: loading, error.
- Keyboard: Not focusable.
- Avoid: Initials or newly drawn logos.
- Related: [Thread sidebar](#thread-sidebar).

### Thread status indicators

The one mapping from thread and pull-request state to icon, colour and label, used by sidebar rows and headers.

- Kind: Composition; status: available; id: `thread-status-indicators`.
- Import: `import { PrStatusIndicator, TerminalStatusIndicator, ChangeRequestStatusIcon, PrStatusTooltipContent, ThreadWorktreeIndicator, ThreadStatusLabel, ThreadRowLeadingStatus } from "~/components/ThreadStatusIndicators";`
- Source: [src/components/ThreadStatusIndicators.tsx](../../web/conversation/src/components/ThreadStatusIndicators.tsx).
- Variants: none.
- States: working, settled, error.
- Keyboard: Indicators are decorative or carry tooltips on focusable rows; they add no focus stops.
- Avoid: A second status mapping in a feature.
- Related: [Thread sidebar](#thread-sidebar), [Badge](#badge).

### Git actions control

The chat header's source-control actions.

- Kind: Composition; status: available; id: `git-actions-control`.
- Import: `import GitActionsControl from "~/components/GitActionsControl";`
- Source: [src/components/GitActionsControl.tsx](../../web/conversation/src/components/GitActionsControl.tsx), with [src/components/GitActionsControl.logic.ts](../../web/conversation/src/components/GitActionsControl.logic.ts).
- Variants: none.
- States: disabled, loading, open.
- Keyboard: A Group of a Button and a Menu trigger; menu keyboard behaviour as Menu.
- Avoid: Separate commit or push buttons elsewhere in the header.
- Related: [Chat header](#chat-header), [Group](#group), [Menu](#menu).

### Thread command subtitle

The second line of a thread result in the command palette: project favicon and name, branch or worktree, and whether it is the current thread.

- Kind: Composition; status: available; id: `thread-command-subtitle`.
- Import: `import { ThreadCommandSubtitle, ThreadCommandSubtitleVariant, CommandPaletteMetaDot, COMMAND_PALETTE_META_ICON_CLASS } from "~/components/ThreadCommandSubtitle";`
- Source: [src/components/ThreadCommandSubtitle.tsx](../../web/conversation/src/components/ThreadCommandSubtitle.tsx).
- Variants: `variant`: `favicon-workspace-harness`, `favicon-workspace`, `favicon-branch-harness`.
- States: none of its own.
- Keyboard: Not interactive; it sits inside a focusable palette row.
- Avoid: Thread metadata in the sidebar or headers, which use the row and ThreadStatusIndicators; do not add new fields here, keep the line to project, checkout and current.
- Related: [Command palette](#command-palette), [Command palette content](#command-palette-content), [Project favicon](#project-favicon).

### Open in picker

The chat header's Open control: open the worktree in the preferred editor, or the first destination (such as the linked issue) when no editor is reachable, with each disabled editor saying why.

- Kind: Composition; status: available; id: `open-in-picker`.
- Import: `import { OpenInPicker } from "~/components/chat/OpenInPicker";`
- Source: [src/components/chat/OpenInPicker.tsx](../../web/conversation/src/components/chat/OpenInPicker.tsx).
- Variants: none.
- States: hover, focus-visible, open, disabled.
- Keyboard: A split button in a Group: the primary is a button, the chevron opens a Menu with arrow-key navigation; the favourite-editor shortcut opens the preferred editor.
- Avoid: A plain link or button that opens an editor or the file manager elsewhere; add a destination through the header actions' open targets instead of a second control.
- Related: [Chat header](#chat-header), [Git actions control](#git-actions-control), [Menu](#menu), [Group](#group).

### Project scripts control

The chat header's project actions: run a project script, add or edit one in the action dialog, and reach the conversation actions from the same menu.

- Kind: Composition; status: available; id: `project-scripts-control`.
- Import: `import ProjectScriptsControl, { ProjectScriptEditorDialog, ScriptIcon, NewProjectScriptInput, ProjectScriptActionResult } from "~/components/ProjectScriptsControl";`
- Source: [src/components/ProjectScriptsControl.tsx](../../web/conversation/src/components/ProjectScriptsControl.tsx), with [src/components/projectScriptEditor.tsx](../../web/conversation/src/components/projectScriptEditor.tsx), [src/components/settings/KeybindingsSettings.logic.ts](../../web/conversation/src/components/settings/KeybindingsSettings.logic.ts).
- Variants: none.
- States: hover, focus-visible, open, empty.
- Keyboard: A split button: the primary runs the preferred script, the chevron opens a Menu of scripts whose edit buttons are reachable by focus; the editor dialog captures a keybinding from a key press.
- Avoid: A separate run button or script form; build on ProjectScriptEditorDialog when another surface needs to edit scripts.
- Related: [Chat header](#chat-header), [Open in picker](#open-in-picker), [Dialog](#dialog), [Menu](#menu).

### Sidebar update pill

The runner update control in the sidebar footer, for readers who manage runners; DesktopUpdateStatusIcon draws its idle, checking, available, downloading and downloaded icons.

- Kind: Composition; status: available; id: `sidebar-update-pill`.
- Import: `import { SidebarUpdatePill, SidebarUpdateArchitectureWarning, SidebarProviderUpdatePill, DesktopUpdateStatusIcon, DesktopUpdateStatusIconState } from "~/components/sidebar/SidebarUpdatePill";`
- Source: [src/components/sidebar/SidebarUpdatePill.tsx](../../web/conversation/src/components/sidebar/SidebarUpdatePill.tsx), with [src/components/sidebar/SidebarProviderUpdatePill.tsx](../../web/conversation/src/components/sidebar/SidebarProviderUpdatePill.tsx), [src/components/sidebar/DesktopUpdateStatusIcon.tsx](../../web/conversation/src/components/sidebar/DesktopUpdateStatusIcon.tsx).
- Variants: `status`: `idle`, `checking`, `available`, `downloading`, `downloaded`.
- States: hover, focus-visible, disabled, checking.
- Keyboard: A focusable button with the status as its accessible name; Enter or Space checks for updates or opens the runner settings.
- Avoid: Announcing other updates with it; SidebarProviderUpdatePill and SidebarUpdateArchitectureWarning render nothing here, so use a toast or a composer notice instead.
- Related: [Sidebar chrome](#sidebar-chrome), [Refresh icon](#refresh-icon).

### Activity page

Read-only organization activity: runners, running attempts, recent finishes and timing against server-provided typical durations.

- Kind: Surface; status: available; id: `activity-page`.
- Import: `import { ActivityRoute, ActivityView } from "~/app/activity/ActivityPage";`
- Source: [src/app/activity/ActivityPage.tsx](../../web/conversation/src/app/activity/ActivityPage.tsx).
- Variants: none.
- States: loading, empty, unavailable, no matching jobs, running, finished, grouped, filtered.
- Keyboard: Tab reaches compact filters, runner chips, issue links, time toggles and stage tooltips; Select and Toggle own their keyboard behavior.
- Avoid: Historical reports, diagnostic evidence or runner controls; use their existing surfaces.
- Related: [Runner status dot](#runner-status-dot), [Stage progress](#stage-progress), [Workspace page header](#workspace-page-header), [Workspace page container](#workspace-page-container).

## Conversation

### Chat header

The conversation's header content inside WorkspacePageHeader: title, git actions, open-in and panel controls. `compactOnMobile` keeps the project glyph and thread title visible on phones while hiding desktop project, editor and Git controls.

- Kind: Composition; status: available; id: `chat-header`.
- Import: `import { ChatHeader } from "~/components/chat/ChatHeader";`
- Source: [src/components/chat/ChatHeader.tsx](../../web/conversation/src/components/chat/ChatHeader.tsx).
- Variants: none.
- States: editing, focus-visible.
- Keyboard: The title is renamable inline (Enter commits, Escape cancels); header actions are buttons and menus.
- Avoid: Adding feature diagnostics to the header.
- Related: [Workspace page header](#workspace-page-header), [Git actions control](#git-actions-control).

### Conversation timeline

Detent's adapter that turns hub messages, questions and proposals into MessagesTimeline entries and hosts the expanded image dialog.

- Kind: Composition; status: available; id: `conversation-timeline`.
- Import: `import { Timeline, TimelineProps } from "~/app/components/Timeline";`
- Source: [src/app/components/Timeline.tsx](../../web/conversation/src/app/components/Timeline.tsx).
- Variants: none.
- States: loading, empty, error.
- Keyboard: As MessagesTimeline.
- Avoid: Rendering messages directly; add entry kinds through the adapter.
- Related: [Messages timeline](#messages-timeline), [Composer](#composer).

### Messages timeline

The transcript: user messages on the message surface, assistant content in the reading lane, tool activity, plans and changed files.

- Kind: Composition; status: available; id: `messages-timeline`.
- Import: `import { MessagesTimeline } from "~/components/chat/MessagesTimeline";`
- Source: [src/components/chat/MessagesTimeline.tsx](../../web/conversation/src/components/chat/MessagesTimeline.tsx), with [src/components/chat/MessagesTimeline.logic.ts](../../web/conversation/src/components/chat/MessagesTimeline.logic.ts), [src/components/chat/timelineScrollAnchoring.ts](../../web/conversation/src/components/chat/timelineScrollAnchoring.ts), [src/components/chat/agentSpawnSummary.ts](../../web/conversation/src/components/chat/agentSpawnSummary.ts), [src/components/chat/userMessageTerminalContexts.ts](../../web/conversation/src/components/chat/userMessageTerminalContexts.ts), [src/components/chat/useAssistantCitationTarget.ts](../../web/conversation/src/components/chat/useAssistantCitationTarget.ts), [src/components/chat/AssistantCitationSource.tsx](../../web/conversation/src/components/chat/AssistantCitationSource.tsx), [src/components/chat/AssistantSelectionToolbar.tsx](../../web/conversation/src/components/chat/AssistantSelectionToolbar.tsx).
- Variants: none.
- States: streaming, working, empty.
- Keyboard: A virtualized list in reading order; message actions (copy, expand) are buttons revealed on hover and focus.
- Avoid: A second transcript renderer, for example for issue discussions.
- Related: [Chat markdown](#chat-markdown), [Conversation timeline](#conversation-timeline), [Proposed plan card](#proposed-plan-card).

### Chat markdown

Every markdown body: assistant messages, plans, issue descriptions and comments (Detent's Markdown wrapper delegates to it).

- Kind: Composition; status: available; id: `chat-markdown`.
- Import: `import ChatMarkdown, { MarkdownCodeBlock, ChatMarkdownAssetImage } from "~/components/ChatMarkdown";`
- Source: [src/components/ChatMarkdown.tsx](../../web/conversation/src/components/ChatMarkdown.tsx), with [src/components/chat/markdownImageGallery.ts](../../web/conversation/src/components/chat/markdownImageGallery.ts), [src/components/preview/fileExplorerLabel.ts](../../web/conversation/src/components/preview/fileExplorerLabel.ts).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: Links, code-block copy buttons and file actions are focusable in reading order.
- Avoid: Another markdown renderer or hand-styled prose.
- Related: [Messages timeline](#messages-timeline).

### Composer

The one prompt surface for conversations and issues: editor, attachments, context strip, pending questions, banners and primary actions, assembled from the composer parts (surface, controls, banners, primary actions).

- Kind: Composition; status: available; id: `composer`.
- Import: `import { Composer, ComposerProps } from "~/app/components/Composer";`
- Source: [src/app/components/Composer.tsx](../../web/conversation/src/app/components/Composer.tsx), with [src/components/chat/composerPromptHistory.ts](../../web/conversation/src/components/chat/composerPromptHistory.ts), [src/components/chat/composerScrollGesture.ts](../../web/conversation/src/components/chat/composerScrollGesture.ts), [src/components/composerFooterLayout.ts](../../web/conversation/src/components/composerFooterLayout.ts), [src/components/chat/useComposerMenuState.ts](../../web/conversation/src/components/chat/useComposerMenuState.ts).
- Variants: none.
- States: focus-visible, disabled, sending, queued, dragging.
- Keyboard: The prompt editor takes focus; Enter sends and Shift+Enter inserts a newline; slash and mention menus open from typed triggers with arrow-key navigation.
- Avoid: Building a second composer or provider picker; the issue page reuses this composition.
- Related: [Composer surface](#composer-surface), [Composer primary actions](#composer-primary-actions), [Composer banner](#composer-banner), [Issue composer](#issue-composer).

### Composer surface

The composer's rounded glass frame and attached-banner geometry.

- Kind: Composition; status: available; id: `composer-surface`.
- Import: `import { ComposerSurface } from "~/components/chat/ComposerSurface";`
- Source: [src/components/chat/ComposerSurface.tsx](../../web/conversation/src/components/chat/ComposerSurface.tsx).
- Variants: none.
- States: focus-within, dragging.
- Keyboard: No behaviour of its own.
- Avoid: Applying the control radius or a card to the composer.
- Related: [Composer](#composer), [Composer banner](#composer-banner).

### Composer control

The composer toolbar's preference controls (model, effort, mode).

- Kind: Composition; status: available; id: `composer-control`.
- Import: `import { ComposerControl, ComposerControlIcon, ComposerControlChevron, ComposerControlSeparator, ComposerSelectControl, ComposerControlSize } from "~/components/chat/ComposerControl";`
- Source: [src/components/chat/ComposerControl.tsx](../../web/conversation/src/components/chat/ComposerControl.tsx).
- Variants: none.
- States: hover, focus-visible, pressed, open.
- Keyboard: Buttons and select triggers; aria-pressed marks toggles such as plan mode.
- Avoid: Using it outside the composer; elsewhere use Button size="compact".
- Related: [Composer](#composer), [Button](#button).

### Composer primary actions

Send, stop and related primary actions at the composer's trailing edge.

- Kind: Composition; status: available; id: `composer-primary-actions`.
- Import: `import { ComposerPrimaryActions } from "~/components/chat/ComposerPrimaryActions";`
- Source: [src/components/chat/ComposerPrimaryActions.tsx](../../web/conversation/src/components/chat/ComposerPrimaryActions.tsx).
- Variants: none.
- States: disabled, sending, running.
- Keyboard: Send and stop are buttons; Enter in the editor triggers send.
- Avoid: A second send button elsewhere.
- Related: [Composer](#composer).

### Composer banner

A notice attached to the top of the composer (provider status, thread error, usage limits).

- Kind: Composition; status: available; id: `composer-banner`.
- Import: `import { ComposerBanner, ComposerBannerVariant } from "~/components/chat/ComposerBanner";`
- Source: [src/components/chat/ComposerBanner.tsx](../../web/conversation/src/components/chat/ComposerBanner.tsx).
- Variants: none.
- States: info, warning, error.
- Keyboard: Banner actions are buttons in reading order.
- Avoid: Page-level alerts or toasts placed over the composer.
- Related: [Composer banner stack](#composer-banner-stack), [Composer](#composer), [Alert](#alert).

### Composer banner stack

Stacking several composer banners so one leads and the rest peek behind it.

- Kind: Composition; status: available; id: `composer-banner-stack`.
- Import: `import { ComposerBannerStack, ComposerBannerStackItem, ComposerBannerStackContent } from "~/components/chat/ComposerBannerStack";`
- Source: [src/components/chat/ComposerBannerStack.tsx](../../web/conversation/src/components/chat/ComposerBannerStack.tsx).
- Variants: none.
- States: expanded, collapsed.
- Keyboard: The stack expands with a button; each banner keeps its actions.
- Avoid: Rendering multiple ComposerBanners side by side.
- Related: [Composer banner](#composer-banner).

### Pending user input panel

A question or approval the agent is waiting on, answered in place above the composer.

- Kind: Composition; status: available; id: `composer-pending-user-input`.
- Import: `import { ComposerPendingUserInputPanel } from "~/components/chat/ComposerPendingUserInputPanel";`
- Source: [src/components/chat/ComposerPendingUserInputPanel.tsx](../../web/conversation/src/components/chat/ComposerPendingUserInputPanel.tsx).
- Variants: none.
- States: pending, responding.
- Keyboard: Answer options are buttons; numeric shortcuts select options where shown.
- Avoid: Dialogs for agent questions.
- Related: [Composer](#composer).

### File tag chip

A file or context reference inside the prompt and in sent messages.

- Kind: Composition; status: available; id: `file-tag-chip`.
- Import: `import { FileTagChipContent, FILE_TAG_CHIP_CLASS_NAME, CHAT_FILE_TAG_CHIP_CLASS_NAME } from "~/components/chat/FileTagChip";`
- Source: [src/components/chat/FileTagChip.tsx](../../web/conversation/src/components/chat/FileTagChip.tsx).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: Inline in the editor; removed with Backspace like text.
- Avoid: Status (use Badge) or actions (use Button).
- Related: [Composer](#composer), [Badge](#badge).

### Proposed plan card

A plan the agent proposes, rendered as a card in the timeline.

- Kind: Composition; status: available; id: `proposed-plan-card`.
- Import: `import { ProposedPlanCard } from "~/components/chat/ProposedPlanCard";`
- Source: [src/components/chat/ProposedPlanCard.tsx](../../web/conversation/src/components/chat/ProposedPlanCard.tsx).
- Variants: none.
- States: collapsed, expanded.
- Keyboard: Expand and copy controls are buttons.
- Avoid: Using the card for ordinary assistant messages.
- Related: [Messages timeline](#messages-timeline), [Chat markdown](#chat-markdown).

### Thread error banner

A thread-level error above the timeline, clamped to three lines, with a dismissal remembered for the session per thread and message.

- Kind: Composition; status: available; id: `thread-error-banner`.
- Import: `import { ThreadErrorBanner, getThreadErrorBannerKey, shouldShowThreadErrorBanner, dismissThreadErrorBannerForSession, isThreadErrorBannerDismissedForSession } from "~/components/chat/ThreadErrorBanner";`
- Source: [src/components/chat/ThreadErrorBanner.tsx](../../web/conversation/src/components/chat/ThreadErrorBanner.tsx).
- Variants: none.
- States: dismissible, truncated.
- Keyboard: The dismiss button is focusable; the clamped error text has a tooltip with the full message.
- Avoid: Errors that belong to one control or turn (show them in place) or provider health (use Provider status banner).
- Related: [Provider status banner](#provider-status-banner), [Alert](#alert), [Composer banner](#composer-banner).

### Provider status banner

The selected provider's health above the composer: limited availability, unavailable, unauthenticated or not installed, with a way into provider setup.

- Kind: Composition; status: available; id: `provider-status-banner`.
- Import: `import { ProviderStatusBanner, getProviderStatusBannerKey, shouldShowProviderStatusBanner, getProviderStatusMessage, hasProviderSetup } from "~/components/chat/ProviderStatusBanner";`
- Source: [src/components/chat/ProviderStatusBanner.tsx](../../web/conversation/src/components/chat/ProviderStatusBanner.tsx).
- Variants: none.
- States: warning, error, unauthenticated, dismissible.
- Keyboard: Open provider setup and dismiss are buttons in reading order; the message has a tooltip with the full text.
- Avoid: Thread errors (use Thread error banner) and usage limits (use Composer notices).
- Related: [Thread error banner](#thread-error-banner), [Composer notices](#composer-notices), [Composer](#composer).

### Composer notices

Ready-made Composer banner stack items: the /usage-limits result with its limit windows, and the progress of a feedback submission.

- Kind: Composition; status: available; id: `composer-notices`.
- Import: `import { usageLimitsBannerItem, feedbackBannerItem } from "~/components/chat/ComposerUsageLimits";`
- Source: [src/components/chat/ComposerUsageLimits.tsx](../../web/conversation/src/components/chat/ComposerUsageLimits.tsx), with [src/components/chat/ComposerFeedback.tsx](../../web/conversation/src/components/chat/ComposerFeedback.tsx).
- Variants: none.
- States: uploading, sent, failed, dismissible.
- Keyboard: As Composer banner stack; Copy ID and dismiss are buttons.
- Avoid: Hand-building the same notice; pass these items to ComposerBannerStack. For a new kind of notice, build a ComposerBannerStackItem.
- Related: [Composer banner stack](#composer-banner-stack), [Usage limits](#usage-limits), [Composer](#composer).

### Diff stat label

Additions and deletions of a file, folder or turn, compacted (12.3k), in success and destructive ink.

- Kind: Composition; status: available; id: `diff-stat-label`.
- Import: `import { DiffStatLabel, hasNonZeroStat } from "~/components/chat/DiffStatLabel";`
- Source: [src/components/chat/DiffStatLabel.tsx](../../web/conversation/src/components/chat/DiffStatLabel.tsx).
- Variants: `layout`: `aligned`, `inline`.
- States: none of its own.
- Keyboard: Not interactive; one accessible label reads both counts.
- Avoid: Other counts or deltas; use a Badge or plain text.
- Related: [Changed files card](#changed-files-card), [Diff surface](#diff-surface).

### Changed files card

A turn's changed files in the transcript or diff surface: a folder tree with per-file stats that opens the diff at a file.

- Kind: Composition; status: available; id: `changed-files-card`.
- Import: `import { ChangedFilesCard, ChangedFilesTree } from "~/components/chat/ChangedFilesTree";`
- Source: [src/components/chat/ChangedFilesTree.tsx](../../web/conversation/src/components/chat/ChangedFilesTree.tsx).
- Variants: none.
- States: hover, focus-visible, expanded, collapsed.
- Keyboard: Folder rows are buttons with aria-expanded; file rows are buttons that open the diff; expand all and Open diff are buttons.
- Avoid: Browsing the whole workspace (use File browser panel) or showing the diff itself (use Diff surface).
- Related: [Diff stat label](#diff-stat-label), [File entry icon](#file-entry-icon), [Messages timeline](#messages-timeline), [Diff surface](#diff-surface).

### Message copy button

Copying a message or snippet with the anchored copy toast and a check confirmation.

- Kind: Composition; status: available; id: `message-copy-button`.
- Import: `import { MessageCopyButton } from "~/components/chat/MessageCopyButton";`
- Source: [src/components/chat/MessageCopyButton.tsx](../../web/conversation/src/components/chat/MessageCopyButton.tsx).
- Variants: `size`: `xs`, `icon-xs`; `variant`: `outline`, `ghost`.
- States: hover, focus-visible, copied.
- Keyboard: A button; Enter or Space copies, then it is disabled while the check shows.
- Avoid: Copy actions that need a label or menu; use a Button with showAnchoredCopySuccessToast.
- Related: [Button](#button), [Toast](#toast), [Messages timeline](#messages-timeline).

### Skill inline text

Text that may name provider skills as $tokens, turning each known skill into a chip; renderSkillInlineMarkdownChildren does the same inside markdown.

- Kind: Composition; status: available; id: `skill-inline-text`.
- Import: `import { SkillInlineText, renderSkillInlineMarkdownChildren } from "~/components/chat/SkillInlineText";`
- Source: [src/components/chat/SkillInlineText.tsx](../../web/conversation/src/components/chat/SkillInlineText.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not interactive; chips read as their skill name.
- Avoid: Mentions of files (use File tag chip) or arbitrary highlighting.
- Related: [File tag chip](#file-tag-chip), [Terminal context chip](#terminal-context-chip), [Chat markdown](#chat-markdown).

### Terminal context chip

Terminal output attached to a sent message, shown inline with its line range.

- Kind: Composition; status: available; id: `terminal-context-chip`.
- Import: `import { TerminalContextInlineChip } from "~/components/chat/TerminalContextInlineChip";`
- Source: [src/components/chat/TerminalContextInlineChip.tsx](../../web/conversation/src/components/chat/TerminalContextInlineChip.tsx).
- Variants: none.
- States: expired.
- Keyboard: Not focusable; the captured lines are in the hover tooltip.
- Avoid: File references (use File tag chip) and quoted assistant text (use Assistant citation chip).
- Related: [File tag chip](#file-tag-chip), [Skill inline text](#skill-inline-text), [Assistant citation chip](#assistant-citation-chip).

### Assistant citation chip

A quote of assistant text in the composer or a sent message, with an optional comment, linking back to where it was said.

- Kind: Composition; status: available; id: `assistant-citation-chip`.
- Import: `import { AssistantCitationChip, AssistantCitationCommentEditor } from "~/components/chat/AssistantCitationChip";`
- Source: [src/components/chat/AssistantCitationChip.tsx](../../web/conversation/src/components/chat/AssistantCitationChip.tsx), with [src/components/chat/AssistantCitationCommentEditor.tsx](../../web/conversation/src/components/chat/AssistantCitationCommentEditor.tsx).
- Variants: none.
- States: hover, focus-visible, editing.
- Keyboard: The quote is a link to its source; the comment and remove controls are buttons; in the comment editor Enter saves, Command or Ctrl+Enter saves and sends, Escape cancels.
- Avoid: Quoting files or terminal output (use their chips) or block quotes in prose (use markdown).
- Related: [File tag chip](#file-tag-chip), [Terminal context chip](#terminal-context-chip), [Composer](#composer), [Messages timeline](#messages-timeline).

### Pull request link preview

A pull request link in markdown with a hover card; resolvePullRequestState and PullRequestActorAvatar give every pull request the same state ink and author mark.

- Kind: Composition; status: available; id: `pull-request-link-preview`.
- Import: `import { PullRequestLinkPreview, PullRequestActorAvatar, resolvePullRequestState } from "~/components/pullRequest/PullRequestLinkPreview";`
- Source: [src/components/pullRequest/PullRequestLinkPreview.tsx](../../web/conversation/src/components/pullRequest/PullRequestLinkPreview.tsx), with [src/components/pullRequest/pullRequestPresentation.tsx](../../web/conversation/src/components/pullRequest/pullRequestPresentation.tsx).
- Variants: none.
- States: hover, open, loading.
- Keyboard: The link keeps its own focus and activation; the card opens on hover after a delay.
- Avoid: Linking other hosts or issues (use a plain link) or drawing pull request state by hand.
- Related: [Chat markdown](#chat-markdown), [Preview card](#preview-card), [Pull request surface](#pull-request-surface), [Thread status indicators](#thread-status-indicators).

## Panels

### Right panel workspace

Detent's owner of the right panel: claims the workspace and routes each tab to its surface.

- Kind: Composition; status: available; id: `right-panel-workspace`.
- Import: `import { RightPanelWorkspace, ConversationPanel, useRightPanelWorkspace } from "~/app/components/RightPanel";`
- Source: [src/app/components/RightPanel.tsx](../../web/conversation/src/app/components/RightPanel.tsx).
- Variants: none.
- States: loading, unavailable, error.
- Keyboard: Delegates to RightPanelTabs and the active surface.
- Avoid: Opening surfaces outside the right panel.
- Related: [Right panel tabs](#right-panel-tabs), [Right panel sheet](#right-panel-sheet).

### Right panel tabs

The tab strip that switches right-panel surfaces (Diff, Files, Terminal, Output, Preview).

- Kind: Composition; status: available; id: `right-panel-tabs`.
- Import: `import { RightPanelTabs } from "~/components/RightPanelTabs";`
- Source: [src/components/RightPanelTabs.tsx](../../web/conversation/src/components/RightPanelTabs.tsx), with [src/components/preview/previewBridge.ts](../../web/conversation/src/components/preview/previewBridge.ts).
- Variants: none.
- States: selected, hover, focus-visible.
- Keyboard: Tabs are focusable; surface shortcuts switch tabs; each tab's close button is separately focusable.
- Avoid: Page-level tabs (use ToggleGroup).
- Related: [Panel tab close button](#panel-tab-close-button), [Right panel workspace](#right-panel-workspace).

### Right panel sheet

Presenting the right panel as a Sheet on narrow viewports.

- Kind: Composition; status: available; id: `right-panel-sheet`.
- Import: `import { RightPanelSheet } from "~/components/RightPanelSheet";`
- Source: [src/components/RightPanelSheet.tsx](../../web/conversation/src/components/RightPanelSheet.tsx).
- Variants: none.
- States: open.
- Keyboard: Sheet semantics: focus is contained, Escape closes.
- Avoid: Using it for content other than the right panel.
- Related: [Sheet](#sheet), [Right panel workspace](#right-panel-workspace).

### Right panel resize handle

Resizing the inline right panel.

- Kind: Composition; status: available; id: `right-panel-resize-handle`.
- Import: `import { RightPanelResizeHandle } from "~/components/preview/RightPanelResizeHandle";`
- Source: [src/components/preview/RightPanelResizeHandle.tsx](../../web/conversation/src/components/preview/RightPanelResizeHandle.tsx).
- Variants: none.
- States: hover, resizing.
- Keyboard: A pointer resize affordance between the main region and the panel.
- Avoid: Custom drag handles per surface.
- Related: [Preview panel shell](#preview-panel-shell).

### Diff panel shell

The header and body frame of the Diff surface, including its loading state.

- Kind: Composition; status: available; id: `diff-panel-shell`.
- Import: `import { DiffPanelShell, DiffPanelLoadingState, DiffPanelMode } from "~/components/DiffPanelShell";`
- Source: [src/components/DiffPanelShell.tsx](../../web/conversation/src/components/DiffPanelShell.tsx).
- Variants: none.
- States: loading.
- Keyboard: No behaviour of its own; header controls are buttons.
- Avoid: Reimplementing the frame for other surfaces; Preview uses PreviewPanelShell.
- Related: [Diff surface](#diff-surface).

### Preview panel shell

The resizable inline frame that hosts right-panel surfaces beside the conversation.

- Kind: Composition; status: available; id: `preview-panel-shell`.
- Import: `import { PreviewPanelShell, PreviewPanelMode, getPreviewPanelMaxWidth } from "~/components/preview/PreviewPanelShell";`
- Source: [src/components/preview/PreviewPanelShell.tsx](../../web/conversation/src/components/preview/PreviewPanelShell.tsx).
- Variants: none.
- States: resizing.
- Keyboard: No behaviour of its own.
- Avoid: Placing surfaces in ad-hoc columns.
- Related: [Right panel resize handle](#right-panel-resize-handle), [Right panel workspace](#right-panel-workspace).

### Diff surface

The right panel's diff of an attempt's changes, rendered by Pierre diffs inside DiffPanelShell.

- Kind: Surface; status: available; id: `diff-surface`.
- Import: `import { DiffSurface, DiffSurfaceProps, AttemptDiffBody } from "~/app/components/surfaces/DiffSurface";`
- Source: [src/app/components/surfaces/DiffSurface.tsx](../../web/conversation/src/app/components/surfaces/DiffSurface.tsx).
- Variants: none.
- States: loading, empty, unavailable, error.
- Keyboard: File navigation and annotations are buttons; the diff renderer is scrollable.
- Avoid: Restyling diff syntax tokens; adapt semantic colours at the renderer boundary.
- Related: [Diff panel shell](#diff-panel-shell), [Right panel workspace](#right-panel-workspace).

### Files surface

The read-only file viewer: breadcrumbs and toggles in a sub-header, the code pane and the explorer aside.

- Kind: Surface; status: available; id: `files-surface`.
- Import: `import { FilesSurface, FileSurface, FilesSurfaceProps } from "~/app/components/surfaces/FilesSurface";`
- Source: [src/app/components/surfaces/FilesSurface.tsx](../../web/conversation/src/app/components/surfaces/FilesSurface.tsx).
- Variants: none.
- States: loading, empty, unavailable, too-large, error.
- Keyboard: Breadcrumb menus and the file tree are keyboard navigable; the code view scrolls.
- Avoid: Adding editing or write actions; this slice is read-only.
- Related: [File breadcrumbs](#file-breadcrumbs), [File browser panel](#file-browser-panel).

### File breadcrumbs

The path of the open file in the Files surface, with sibling navigation.

- Kind: Composition; status: available; id: `file-breadcrumbs`.
- Import: `import { FileBreadcrumbs, FileBreadcrumbsProps } from "~/app/components/surfaces/FileBreadcrumbs";`
- Source: [src/app/components/surfaces/FileBreadcrumbs.tsx](../../web/conversation/src/app/components/surfaces/FileBreadcrumbs.tsx).
- Variants: none.
- States: open, loading.
- Keyboard: Each segment opens a Menu of its directory; arrow keys and Enter navigate.
- Avoid: Page breadcrumbs (use WorkspaceBreadcrumb).
- Related: [Files surface](#files-surface), [Workspace breadcrumb](#workspace-breadcrumb).

### File browser panel

The Files surface's explorer tree.

- Kind: Composition; status: available; id: `file-browser-panel`.
- Import: `import { FileBrowserPanel, FileBrowserPanelProps, TreeFilesClient } from "~/app/components/surfaces/FileBrowserPanel";`
- Source: [src/app/components/surfaces/FileBrowserPanel.tsx](../../web/conversation/src/app/components/surfaces/FileBrowserPanel.tsx).
- Variants: none.
- States: loading, selected, expanded.
- Keyboard: Pierre tree navigation: arrow keys move and expand, Enter opens.
- Avoid: Another tree implementation.
- Related: [Files surface](#files-surface).

### Terminal surface

A runner terminal stream in the right panel.

- Kind: Surface; status: available; id: `terminal-surface`.
- Import: `import { TerminalSurface, TerminalSurfaceProps, terminalStatusLabel } from "~/app/components/surfaces/TerminalSurface";`
- Source: [src/app/components/surfaces/TerminalSurface.tsx](../../web/conversation/src/app/components/surfaces/TerminalSurface.tsx).
- Variants: none.
- States: connecting, running, exited, unavailable, error.
- Keyboard: The xterm view takes keyboard input while focused; Tab is captured by the terminal.
- Avoid: Prose typography or generic cards around terminal output.
- Related: [Output surface](#output-surface), [Workspace status view](#workspace-status-view).

### Output surface

The recorded output of project actions and scripts.

- Kind: Surface; status: available; id: `output-surface`.
- Import: `import { OutputSurface, OutputSurfaceProps, runStatusLabel } from "~/app/components/surfaces/OutputSurface";`
- Source: [src/app/components/surfaces/OutputSurface.tsx](../../web/conversation/src/app/components/surfaces/OutputSurface.tsx).
- Variants: none.
- States: running, succeeded, failed, empty.
- Keyboard: Scrollable output with focusable run controls.
- Avoid: Live interactive sessions (use the Terminal surface).
- Related: [Terminal surface](#terminal-surface).

### Pull request surface

A linked pull request's details in the right panel.

- Kind: Surface; status: available; id: `pull-request-surface`.
- Import: `import { PullRequestSurface } from "~/app/components/surfaces/PullRequestSurface";`
- Source: [src/app/components/surfaces/PullRequestSurface.tsx](../../web/conversation/src/app/components/surfaces/PullRequestSurface.tsx).
- Variants: none.
- States: loading, empty.
- Keyboard: Links and actions in reading order.
- Avoid: Duplicating pull request status outside ThreadStatusIndicators.
- Related: [Thread status indicators](#thread-status-indicators).

### Workspace status view

The shared waiting and unavailable state of a surface while a workspace is claimed, with an elapsed clock.

- Kind: Surface; status: available; id: `workspace-status-view`.
- Import: `import { WorkspaceStatusView, useElapsedLabel } from "~/app/components/surfaces/WorkspaceStatusView";`
- Source: [src/app/components/surfaces/WorkspaceStatusView.tsx](../../web/conversation/src/app/components/surfaces/WorkspaceStatusView.tsx).
- Variants: none.
- States: waiting, unavailable.
- Keyboard: No behaviour of its own.
- Avoid: Per-surface waiting spinners.
- Related: [Terminal surface](#terminal-surface), [Files surface](#files-surface).

### Panel layout controls

The header toggles for the terminal drawer and the right panel, and the maximize toggle inside the panel.

- Kind: Composition; status: available; id: `panel-layout-controls`.
- Import: `import { PanelLayoutControls, RightPanelMaximizeControl } from "~/components/chat/PanelLayoutControls";`
- Source: [src/components/chat/PanelLayoutControls.tsx](../../web/conversation/src/components/chat/PanelLayoutControls.tsx).
- Variants: none.
- States: pressed, disabled, hover, focus-visible.
- Keyboard: Toggles: Tab focuses, Enter and Space flip them; tooltips name the shortcut; the right panel toggle's name includes the working-agent count.
- Avoid: Opening a specific surface (use Right panel tabs) or new layout toggles beside these.
- Related: [Right panel workspace](#right-panel-workspace), [Right panel tabs](#right-panel-tabs), [Toggle](#toggle).

## Settings

### Settings layout

Every settings page: SettingsPageContainer, SettingsSection groups and SettingsRow with title, help, description, status and a trailing control.

- Kind: Composition; status: available; id: `settings-layout`.
- Import: `import { SettingsSection, SettingsRow, SettingsPageContainer, SettingResetButton, SettingsWarning, SettingsUnavailableGroup, SettingsSearchTarget, SettingsSearchTargetProvider, PolicyTooltip, SETTINGS_PICKER_TRIGGER_CLASSNAME } from "~/app/settings/settingsLayout";`
- Source: [src/app/settings/settingsLayout.tsx](../../web/conversation/src/app/settings/settingsLayout.tsx).
- Variants: none.
- States: highlighted, unavailable, overridden.
- Keyboard: Each row's control keeps its own keyboard behaviour; reset buttons and help are focusable.
- Avoid: Cards per setting or bespoke row layouts. SettingsWarning is for enabled features with missing requirements or failed checks, never optional features not set up; follow [Feedback and interaction states](patterns.md#feedback-and-interaction-states).
- Related: [Settings sidebar nav](#settings-sidebar-nav), [Settings pages](#settings-pages), [Switch](#switch).

### Settings sidebar nav

Navigation between settings sections, built on the Sidebar primitive.

- Kind: Composition; status: available; id: `settings-sidebar-nav`.
- Import: `import { SettingsSidebarNav } from "~/components/settings/SettingsSidebarNav";`
- Source: [src/components/settings/SettingsSidebarNav.tsx](../../web/conversation/src/components/settings/SettingsSidebarNav.tsx), with [src/components/settings/settingsSearch.ts](../../web/conversation/src/components/settings/settingsSearch.ts), [src/components/settings/useAvailableSettingsSearchItems.ts](../../web/conversation/src/components/settings/useAvailableSettingsSearchItems.ts), [src/components/settings/settingsLayout.ts](../../web/conversation/src/components/settings/settingsLayout.ts).
- Variants: none.
- States: active, hover, focus-visible.
- Keyboard: Section links in document order.
- Avoid: Tabs for settings sections.
- Related: [Settings layout](#settings-layout), [Sidebar primitive](#sidebar-primitive).

### Settings pages

Detent's settings sections (general, projects, integrations, plan, billing, keybindings, about), each composed from the settings layout.

- Kind: Composition; status: available; id: `settings-route`.
- Import: `import { SettingsRoute, GeneralSettings, ProjectsSettings, IntegrationsSettings, PlanSettings, BillingSettings, KeybindingsSettings, AboutSettings } from "~/app/settings/Settings";`
- Source: [src/app/settings/Settings.tsx](../../web/conversation/src/app/settings/Settings.tsx).
- Variants: none.
- States: loading, error, empty.
- Keyboard: As the settings layout.
- Avoid: New hosted settings screens outside this route.
- Related: [Settings layout](#settings-layout).

### Settings help

In-place help for a settings row.

- Kind: Composition; status: available; id: `settings-help`.
- Import: `import { SettingsHelp } from "~/app/settings/SettingsHelp";`
- Source: [src/app/settings/SettingsHelp.tsx](../../web/conversation/src/app/settings/SettingsHelp.tsx).
- Variants: none.
- States: open.
- Keyboard: A focusable help button; the help stays open for keyboard and touch readers.
- Avoid: Hover-only explanations.
- Related: [Settings layout](#settings-layout), [Context help](#context-help).

### Context help

Explaining a term or metric inline outside settings.

- Kind: Composition; status: available; id: `context-help`.
- Import: `import { ContextHelp } from "~/app/components/ContextHelp";`
- Source: [src/app/components/ContextHelp.tsx](../../web/conversation/src/app/components/ContextHelp.tsx).
- Variants: none.
- States: open, pinned.
- Keyboard: Read by hover or keyboard focus; click or tap pins the same help in a Popover.
- Avoid: Hiding required instructions behind it.
- Related: [Settings help](#settings-help), [Tooltip](#tooltip), [Popover](#popover).

### Expandable text

Long error text clamped to a few lines with a toggle.

- Kind: Composition; status: available; id: `expandable-text`.
- Import: `import { ExpandableText } from "~/app/settings/ExpandableText";`
- Source: [src/app/settings/ExpandableText.tsx](../../web/conversation/src/app/settings/ExpandableText.tsx).
- Variants: none.
- States: collapsed, expanded.
- Keyboard: The toggle is a button.
- Avoid: Prose that should simply wrap.
- Related: [Settings layout](#settings-layout).

### Redacted sensitive text

Showing a secret such as an API key redacted until revealed.

- Kind: Composition; status: available; id: `redacted-sensitive-text`.
- Import: `import { RedactedSensitiveText } from "~/components/settings/RedactedSensitiveText";`
- Source: [src/components/settings/RedactedSensitiveText.tsx](../../web/conversation/src/components/settings/RedactedSensitiveText.tsx).
- Variants: none.
- States: redacted, revealed.
- Keyboard: The reveal control is a button.
- Avoid: Printing secrets in clear text.
- Related: [Settings layout](#settings-layout).

## Usage

### Reports page

Project flow, throughput, stage and cost reports from recorded analytics, with source coverage.

- Kind: Composition; status: available; id: `reports-page`.
- Import: `import { ReportsRoute, ReportsView } from "~/app/reports/ReportsPage";`
- Source: [src/app/reports/ReportsPage.tsx](../../web/conversation/src/app/reports/ReportsPage.tsx).
- Variants: none.
- States: loading, empty, error, partial, unavailable.
- Keyboard: Time and range controls, refresh and issue Diagnostics links in reading order.
- Avoid: Point-in-time snapshots as historical flow metrics or unrecorded values as zeroes.
- Related: [Usage page](#usage-page), [Table](#table), [Toggle group](#toggle-group), [Workspace page header](#workspace-page-header), [Workspace page container](#workspace-page-container).

### Usage page

The usage page: range control, provider chart, aligned totals and runner usage in a WorkspacePageContainer.

- Kind: Composition; status: available; id: `usage-page`.
- Import: `import { UsageRoute, UsageView } from "~/app/usage/UsagePage";`
- Source: [src/app/usage/UsagePage.tsx](../../web/conversation/src/app/usage/UsagePage.tsx).
- Variants: none.
- States: loading, empty, error.
- Keyboard: Range controls and tables in reading order.
- Avoid: Operational diagnostics in product UI.
- Related: [Usage provider chart](#usage-provider-chart), [Usage limits](#usage-limits), [Table](#table).

### Usage provider chart

Per-provider usage over the selected range with one stable colour per series.

- Kind: Composition; status: available; id: `usage-provider-chart`.
- Import: `import { UsageProviderChart, UsageChartMetric } from "~/app/usage/UsageProviderChart";`
- Source: [src/app/usage/UsageProviderChart.tsx](../../web/conversation/src/app/usage/UsageProviderChart.tsx).
- Variants: none.
- States: hover, empty.
- Keyboard: Pointer hover readout; the totals table carries the same data for keyboard readers.
- Avoid: New chart colour mappings.
- Related: [Usage page](#usage-page).

### Usage limits

Provider limit windows and pace indicators, in the usage page and composer usage banner.

- Kind: Composition; status: available; id: `usage-limits`.
- Import: `import { LimitWindows, ResetCredits, PaceIcon, barColor } from "~/components/usage/UsageLimits";`
- Source: [src/components/usage/UsageLimits.tsx](../../web/conversation/src/components/usage/UsageLimits.tsx).
- Variants: none.
- States: warning, exhausted.
- Keyboard: No behaviour of its own.
- Avoid: Another limit meter.
- Related: [Usage page](#usage-page), [Composer banner](#composer-banner).

### Usage limits section

Detent's plan allowances (API mutations and similar) as labelled rows on the usage page.

- Kind: Composition; status: available; id: `usage-limits-section`.
- Import: `import { UsageLimitsSection, limitLabel } from "~/app/usage/UsageLimits";`
- Source: [src/app/usage/UsageLimits.tsx](../../web/conversation/src/app/usage/UsageLimits.tsx).
- Variants: none.
- States: empty.
- Keyboard: No behaviour of its own.
- Avoid: Mixing plan allowances with provider limits.
- Related: [Usage page](#usage-page), [Usage limits](#usage-limits).

## Work

### Work board

The Work board and list: shared filters, search and sort, rendered as lanes (`?view=board`) or a list (`?view=list`).

- Kind: Composition; status: available; id: `work-board`.
- Import: `import { WorkBoard } from "~/app/work/WorkBoard";`
- Source: [src/app/work/WorkBoard.tsx](../../web/conversation/src/app/work/WorkBoard.tsx).
- Variants: none.
- States: loading, empty, error, stale.
- Keyboard: Toolbar controls, lane headers and cards in reading order; cards open the issue page.
- Avoid: Adding card metrics, banners or diagnostics (INV-13).
- Related: [Board lane](#board-lane), [Issue card](#issue-card), [Work list](#work-list), [Work toolbar](#work-toolbar), [Work top bar](#work-top-bar).

### Work top bar

The board's header row: title, freshness and primary actions.

- Kind: Composition; status: available; id: `work-top-bar`.
- Import: `import { WorkTopBar, FreshnessChip } from "~/app/work/components/WorkTopBar";`
- Source: [src/app/work/components/WorkTopBar.tsx](../../web/conversation/src/app/work/components/WorkTopBar.tsx).
- Variants: none.
- States: stale, refreshing.
- Keyboard: Buttons and the view switch in reading order.
- Avoid: A second header inside the board.
- Related: [Work board](#work-board), [Work toolbar](#work-toolbar).

### Work toolbar

The board's facet filters and sort.

- Kind: Composition; status: available; id: `work-toolbar`.
- Import: `import { WorkToolbar, WorkToolbarProps, ToolbarFacets } from "~/app/work/components/WorkToolbar";`
- Source: [src/app/work/components/WorkToolbar.tsx](../../web/conversation/src/app/work/components/WorkToolbar.tsx).
- Variants: none.
- States: active, open.
- Keyboard: Facet menus; menus as Menu.
- Avoid: Filter controls inside lanes.
- Related: [Work board](#work-board).

### Board lane

One workflow lane of the board with its cards.

- Kind: Composition; status: available; id: `board-lane`.
- Import: `import { BoardLane, BoardLaneProps } from "~/app/work/components/BoardLane";`
- Source: [src/app/work/components/BoardLane.tsx](../../web/conversation/src/app/work/components/BoardLane.tsx).
- Variants: none.
- States: populated, empty, collapsed, drop target.
- Keyboard: The lane is a labelled region; its collapse button toggles with Enter or Space; the new-issue button is labelled with the lane name; each card's menu lists the moves.
- Avoid: Writing lane state from the client; the orchestrator owns lanes.
- Related: [Issue card](#issue-card), [Work board](#work-board).

### Issue card

A board card: identity and menu, wrapping title, at most one actionable status line with priority, then the approved compact attempt/update row, per INV-13.

- Kind: Composition; status: available; id: `issue-card`.
- Import: `import { IssueCard, IssueCardProps, Pill, PullRequestBadge, statusPill, priorityTone } from "~/app/work/components/IssueCard";`
- Source: [src/app/work/components/IssueCard.tsx](../../web/conversation/src/app/work/components/IssueCard.tsx).
- Variants: none.
- States: hover, focus-visible, busy, terminal.
- Keyboard: The card is a link to the issue page; its pills are labelled for assistive technology.
- Avoid: Any additional metric, badge, banner or diagnostic on the card (INV-13).
- Related: [Board lane](#board-lane), [Work list](#work-list).

### Work list

The list shape of the Work board, sharing its filters and result set.

- Kind: Composition; status: available; id: `work-list`.
- Import: `import { WorkList } from "~/app/work/components/WorkList";`
- Source: [src/app/work/components/WorkList.tsx](../../web/conversation/src/app/work/components/WorkList.tsx).
- Variants: none.
- States: empty, loading.
- Keyboard: Rows are links in reading order.
- Avoid: Columns beyond what the card shows.
- Related: [Work board](#work-board), [Issue card](#issue-card).

### Issue page

One issue: description, activity feed, composer and the properties column.

- Kind: Composition; status: available; id: `issue-page`.
- Import: `import { IssuePage, useIssue } from "~/app/work/IssuePage";`
- Source: [src/app/work/IssuePage.tsx](../../web/conversation/src/app/work/IssuePage.tsx).
- Variants: none.
- States: loading, error, empty.
- Keyboard: Header, description, activity and properties in reading order; property pickers have digit shortcuts.
- Avoid: A second composer or timeline; reuse the conversation compositions.
- Related: [Issue properties](#issue-properties), [Activity feed](#activity-feed), [Issue composer](#issue-composer).

### Issue properties

The issue page's properties column (status, priority, labels, related issues).

- Kind: Composition; status: available; id: `issue-properties`.
- Import: `import { IssueProperties, IssuePropertiesProps } from "~/app/work/components/IssueProperties";`
- Source: [src/app/work/components/IssueProperties.tsx](../../web/conversation/src/app/work/components/IssueProperties.tsx).
- Variants: none.
- States: open, pending.
- Keyboard: One picker open at a time; property chords open pickers; pickers navigate as Combobox.
- Avoid: Editing lane labels directly; the orchestrator owns lane state.
- Related: [Property picker](#property-picker), [Issue page](#issue-page).

### Property picker

Choosing an issue property value from grouped rows.

- Kind: Composition; status: available; id: `property-picker`.
- Import: `import { PropertyPicker, PropertyPickerProps, PickerRow, PickerGroup } from "~/app/work/components/IssuePickers";`
- Source: [src/app/work/components/IssuePickers.tsx](../../web/conversation/src/app/work/components/IssuePickers.tsx).
- Variants: none.
- States: open, highlighted, selected.
- Keyboard: Type to filter; arrow keys move; digits select numbered rows; Enter commits; Escape closes.
- Avoid: Pickers for non-issue data (use Combobox).
- Related: [Issue properties](#issue-properties), [Combobox](#combobox).

### Activity feed

The issue's comments, events and linked conversation in time order.

- Kind: Composition; status: available; id: `activity-feed`.
- Import: `import { ActivityFeed, ActivityFeedProps, LiveRow } from "~/app/work/components/ActivityFeed";`
- Source: [src/app/work/components/ActivityFeed.tsx](../../web/conversation/src/app/work/components/ActivityFeed.tsx).
- Variants: none.
- States: live, empty.
- Keyboard: Comments and their reply controls in reading order.
- Avoid: Threaded replies; a reply is a comment on the same issue.
- Related: [Issue page](#issue-page), [Chat markdown](#chat-markdown).

### Issue composer

The issue page's composer: the shared Composer with issue-specific slash commands.

- Kind: Composition; status: available; id: `issue-composer`.
- Import: `import { IssueComposer, IssueComposerProps, issueComposerSlashCommands } from "~/app/work/components/IssueComposer";`
- Source: [src/app/work/components/IssueComposer.tsx](../../web/conversation/src/app/work/components/IssueComposer.tsx).
- Variants: none.
- States: focus-visible, sending.
- Keyboard: As Composer, with the issue page's slash commands.
- Avoid: A separate editor for issue comments.
- Related: [Composer](#composer), [Issue page](#issue-page).

## Media

### Expanded image dialog

Full-screen preview of images and videos from messages, markdown and attachments, paged as one set, with media actions and capture details.

- Kind: Composition; status: available; id: `expanded-image-dialog`.
- Import: `import { ExpandedImageDialog, ExpandedImageItem, ExpandedImagePreview, buildExpandedImagePreview } from "~/components/chat/ExpandedImageDialog";`
- Source: [src/components/chat/ExpandedImageDialog.tsx](../../web/conversation/src/components/chat/ExpandedImageDialog.tsx), with [src/components/chat/ExpandedImagePreview.tsx](../../web/conversation/src/components/chat/ExpandedImagePreview.tsx).
- Variants: none.
- States: open, unavailable, video.
- Keyboard: Escape closes and returns focus to the opener; Left and Right arrows page between items; close, previous and next are buttons.
- Avoid: Inline thumbnails (render the image) or document previews (use the Files surface).
- Related: [Media actions](#media-actions), [Media video player](#media-video-player), [Snapshot attachment details](#snapshot-attachment-details), [Dialog](#dialog).

### Snapshot attachment details

The caption over a window capture attachment: app icon, app and window name, and its captured accessibility data.

- Kind: Composition; status: available; id: `snapshot-attachment-details`.
- Import: `import { SnapShotAttachmentDetails, SnapShotContentsButton, SnapShotAccessibilityData, SNAP_SHOT_ATTACHMENT_FRAME_CLASS, snapShotAccessibilityDetails } from "~/components/chat/SnapShotAttachmentDetails";`
- Source: [src/components/chat/SnapShotAttachmentDetails.tsx](../../web/conversation/src/components/chat/SnapShotAttachmentDetails.tsx).
- Variants: none.
- States: hover, open.
- Keyboard: The accessibility data button is focusable and opens a Popover; the data is a focusable, scrollable block.
- Avoid: Captions for ordinary images; show the file name instead.
- Related: [Expanded image dialog](#expanded-image-dialog), [Popover](#popover).

### Media video player

Playing a video from a message, markdown or file preview with loading and failure states in the same 16:9 slot; OpenMediaLink opens or downloads the original.

- Kind: Composition; status: available; id: `media-video-player`.
- Import: `import { MediaVideoPlayer, OpenMediaLink } from "~/components/media/MediaVideoPlayer";`
- Source: [src/components/media/MediaVideoPlayer.tsx](../../web/conversation/src/components/media/MediaVideoPlayer.tsx), with [src/components/media/OpenMediaLink.tsx](../../web/conversation/src/components/media/OpenMediaLink.tsx).
- Variants: none.
- States: loading, failed, retrying.
- Keyboard: Native video controls; Retry video and the open link are a button and a link.
- Avoid: A bare video element, which loses the retry, failure and range-streaming behaviour.
- Related: [Expanded image dialog](#expanded-image-dialog), [Media actions](#media-actions).

### Media actions

Adding copy path, copy URL, open, save and copy image to an image or video without changing its layout.

- Kind: Composition; status: available; id: `media-actions`.
- Import: `import { MediaActions, MediaActionSource } from "~/components/media/MediaActions";`
- Source: [src/components/media/MediaActions.tsx](../../web/conversation/src/components/media/MediaActions.tsx).
- Variants: none.
- States: hover, focus-visible.
- Keyboard: The wrapped element becomes focusable; the context menu key or Shift+F10 opens the media menu; the source path is in the tooltip.
- Avoid: Actions on non-media elements (use Menu) or visible media toolbars.
- Related: [Expanded image dialog](#expanded-image-dialog), [Media video player](#media-video-player), [Chat markdown](#chat-markdown).

## Icons

### Brand icons

Third-party marks: source control hosts, editors and agent providers, sized with the same size classes as Lucide icons.

- Kind: Primitive; status: available; id: `brand-icons`.
- Import: `import { Icon, LinuxIcon, GitHubIcon, GitIcon, JujutsuIcon, GitLabIcon, AzureDevOpsIcon, BitbucketIcon, CursorIcon, GrokIcon, TraeIcon, KiroIcon, VisualStudioCode, VisualStudioCodeInsiders, VSCodium, Zed, OpenAI, ClaudeAI, Gemini, AntigravityIcon, OpenCodeIcon, GithubCopilotIcon, ACPRegistryIcon, PiAgentIcon } from "~/components/Icons";`
- Source: [src/components/Icons.tsx](../../web/conversation/src/components/Icons.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable; pass aria-hidden beside a text label, or an accessible name when the icon stands alone.
- Avoid: Generic symbols (use Lucide) and drawing a new brand mark inline in a feature; add it here.
- Related: [Provider instance icon](#provider-instance-icon), [Open in picker](#open-in-picker), [Environment machine icon](#environment-machine-icon).

### Morph icon

An icon that morphs between two Lucide shapes when its control's state flips, such as copy to check or open to close.

- Kind: Primitive; status: available; id: `morph-icon`.
- Import: `import { MorphIcon } from "~/components/MorphIcon";`
- Source: [src/components/MorphIcon.tsx](../../web/conversation/src/components/MorphIcon.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable; the control around it carries the name and state.
- Avoid: Static icons (use Lucide directly) and loading spinners (use Spinner).
- Related: [Sidebar primitive](#sidebar-primitive), [Toast](#toast), [Spinner](#spinner).

### Environment machine icon

The mark for the kind of machine a runner or environment runs on, with its label.

- Kind: Composition; status: available; id: `environment-machine-icon`.
- Import: `import { EnvironmentMachineIcon, environmentMachineIcon, ENVIRONMENT_MACHINE_KIND_LABELS } from "~/components/EnvironmentMachineIcon";`
- Source: [src/components/EnvironmentMachineIcon.tsx](../../web/conversation/src/components/EnvironmentMachineIcon.tsx).
- Variants: none.
- States: none of its own.
- Keyboard: Not focusable; pair it with ENVIRONMENT_MACHINE_KIND_LABELS or the machine name.
- Avoid: Providers or repositories (use Provider instance icon or Brand icons).
- Related: [Thread status indicators](#thread-status-indicators), [Thread sidebar](#thread-sidebar), [Brand icons](#brand-icons).

### File entry icon

The file-type or folder icon beside a path in chips, trees, tabs and breadcrumbs, coloured for the current theme.

- Kind: Composition; status: available; id: `file-entry-icon`.
- Import: `import { PierreEntryIcon } from "~/components/chat/PierreEntryIcon";`
- Source: [src/components/chat/PierreEntryIcon.tsx](../../web/conversation/src/components/chat/PierreEntryIcon.tsx).
- Variants: `kind`: `file`, `directory`; `theme`: `light`, `dark`.
- States: none of its own.
- Keyboard: Not focusable.
- Avoid: Icons for things that are not paths; use Lucide.
- Related: [File tag chip](#file-tag-chip), [Changed files card](#changed-files-card), [File breadcrumbs](#file-breadcrumbs), [Right panel tabs](#right-panel-tabs).

### Provider instance icon

A provider instance's mark: the driver's brand icon or initials, an optional instance badge and accent, and an optional status dot.

- Kind: Composition; status: available; id: `provider-instance-icon`.
- Import: `import { ProviderInstanceIcon, providerInstanceInitials, PROVIDER_ICON_BY_PROVIDER } from "~/components/chat/ProviderInstanceIcon";`
- Source: [src/components/chat/ProviderInstanceIcon.tsx](../../web/conversation/src/components/chat/ProviderInstanceIcon.tsx), with [src/components/chat/providerIconUtils.ts](../../web/conversation/src/components/chat/providerIconUtils.ts).
- Variants: `badgeContent`: `initials`, `none`.
- States: none of its own.
- Keyboard: Not focusable; the row or control around it names the provider.
- Avoid: Model names or provider labels as text; render the name beside the icon.
- Related: [Brand icons](#brand-icons), [Thread sidebar](#thread-sidebar), [Composer control](#composer-control).

### Favicon image

A small site icon that tries each source in turn and falls back to a given node when none loads.

- Kind: Primitive; status: available; id: `favicon-image`.
- Import: `import { FaviconImage } from "~/components/preview/PreviewFaviconIcon";`
- Source: [src/components/preview/PreviewFaviconIcon.tsx](../../web/conversation/src/components/preview/PreviewFaviconIcon.tsx).
- Variants: none.
- States: error.
- Keyboard: Not focusable; decorative.
- Avoid: Project marks (use Project favicon) and avatars (use PullRequestActorAvatar).
- Related: [Project favicon](#project-favicon), [Right panel tabs](#right-panel-tabs).
