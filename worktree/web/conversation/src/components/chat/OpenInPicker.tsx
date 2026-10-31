import { ChevronDownIcon, ExternalLinkIcon, FolderClosedIcon } from "lucide-react";
import { memo, useCallback, useEffect, useMemo, type ReactElement } from "react";

import { useHeaderActions } from "../../app/adapters/headerActions.tsx";
import {
  editorOpenUrl,
  openInDisabledReason,
  OPEN_IN_EDITORS,
} from "../../app/adapters/openIn.ts";
import {
  isOpenFavoriteEditorShortcut,
  shortcutLabelForCommand,
} from "../../app/adapters/keybindings.ts";
import type { EditorId, ResolvedKeybindingsConfig } from "../../contracts/ui.ts";
import { detentKeybindings } from "../../app/adapters/keybindings.ts";
import { editorLabelForPlatform } from "../../editorLabels.ts";
import { usePreferredEditor } from "../../editorPreferences.ts";
import { cn } from "../../lib/utils.ts";
import { Button } from "../ui/button.tsx";
import { Group, GroupSeparator } from "../ui/group.tsx";
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuShortcut, MenuTrigger } from "../ui/menu.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../ui/popover.tsx";
import {
  AntigravityIcon,
  CursorIcon,
  type Icon,
  KiroIcon,
  TraeIcon,
  VisualStudioCode,
  VisualStudioCodeInsiders,
  VSCodium,
  Zed,
} from "../Icons.tsx";

type OpenInOption = {
  label: string;
  Icon: Icon;
  value: EditorId;
  kind: "brand" | "generic";
};

const resolveOptions = (platform: string, availableEditors: ReadonlyArray<EditorId>) => {
  const baseOptions: ReadonlyArray<Omit<OpenInOption, "label">> = [
    {
      Icon: CursorIcon,
      value: "cursor",
      kind: "brand",
    },
    {
      Icon: TraeIcon,
      value: "trae",
      kind: "brand",
    },
    {
      Icon: KiroIcon,
      value: "kiro",
      kind: "brand",
    },
    {
      Icon: VisualStudioCode,
      value: "vscode",
      kind: "brand",
    },
    {
      Icon: VisualStudioCodeInsiders,
      value: "vscode-insiders",
      kind: "brand",
    },
    {
      Icon: VSCodium,
      value: "vscodium",
      kind: "brand",
    },
    {
      Icon: Zed,
      value: "zed",
      kind: "brand",
    },
    {
      Icon: AntigravityIcon,
      value: "antigravity",
      kind: "brand",
    },
    {
      Icon: FolderClosedIcon,
      value: "file-manager",
      kind: "generic",
    },
  ];
  const availableEditorSet = new Set(availableEditors);
  return baseOptions
    .filter((option) => availableEditorSet.has(option.value))
    .map((option) => ({ ...option, label: editorLabelForPlatform(option.value, platform) }));
};

function getOpenInIconClass(kind: OpenInOption["kind"]) {
  return cn(kind === "brand" ? "text-foreground opacity-100" : "text-muted-foreground");
}

function rowTestId(editor: EditorId): string {
  return `header-open-${editor}`;
}

export const OpenInPicker = memo(function OpenInPicker({
  keybindings,
  availableEditors,
  compact = false,
  enableShortcut = true,
}: {
  environmentId?: string;
  keybindings?: ResolvedKeybindingsConfig;
  availableEditors?: ReadonlyArray<EditorId>;
  openInCwd?: string | null;
  compact?: boolean;
  enableShortcut?: boolean;
}): ReactElement {
  const { openLocation, openTargets } = useHeaderActions();

  const effectiveEditors = availableEditors ?? OPEN_IN_EDITORS;
  const resolvedKeybindings = keybindings ?? detentKeybindings;
  const [preferredEditor, setPreferredEditor] = usePreferredEditor(effectiveEditors);
  const platform = globalThis.navigator?.platform ?? "";
  const options = useMemo(
    () => resolveOptions(platform, effectiveEditors),
    [effectiveEditors, platform],
  );
  const primaryOption = options.find(({ value }) => value === preferredEditor) ?? null;

  const openInEditor = useCallback(
    (editorId: EditorId | null) => {
      const editor = editorId ?? preferredEditor;
      if (!editor) return;
      if (openLocation.worktreePath === null) return;
      if (openInDisabledReason(editor, openLocation) !== null) return;
      const url = editorOpenUrl(editor, openLocation.worktreePath);
      if (url === null) return;
      // The one thing a page can do: hand the URL to the operating system and
      // let whatever is registered for the scheme answer. There is no result
      // to report — a browser cannot observe whether a handler existed — which
      // is why the reasons above are checked before the press rather than after.
      globalThis.location?.assign(url);
      setPreferredEditor(editor);
    },
    [openLocation, preferredEditor, setPreferredEditor],
  );

  const openFavoriteEditorShortcutLabel = useMemo(
    () => shortcutLabelForCommand(resolvedKeybindings, "editor.openFavorite"),
    [resolvedKeybindings],
  );

  useEffect(() => {
    if (!enableShortcut) return;
    const handler = (e: globalThis.KeyboardEvent) => {
      if (!isOpenFavoriteEditorShortcut(e, resolvedKeybindings)) return;
      if (openLocation.worktreePath === null) return;
      if (!preferredEditor) return;

      e.preventDefault();
      void openInEditor(preferredEditor);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [enableShortcut, resolvedKeybindings, openInEditor, openLocation.worktreePath, preferredEditor]);

  const editorReason =
    primaryOption === null ? null : openInDisabledReason(primaryOption.value, openLocation);
  const editorReachable = preferredEditor !== null && editorReason === null;
  /**
   * What the primary does when no editor is reachable.
   *
   * Upstream the primary is always "open in the preferred editor", because
   * upstream the checkout is on the machine the reader is looking at. On
   * Detent Cloud it usually is not, so a primary that only ever launches an
   * editor is a control that is dead in the ordinary case — and this header
   * *already had* a working `Open` before the editors arrived: it opened the
   * linked issue, and one of the browser specs has asserted that since the
   * port began. Making the editors the only meaning would have quietly taken
   * that away, which is a removal nobody asked for dressed up as a feature.
   *
   * So the split button opens the best thing it can reach: the preferred
   * editor where that is a real place, and otherwise the first destination
   * below the separator. The rows themselves are unchanged — each still says
   * for itself why it cannot run — so nothing here hides a refusal; it only
   * stops the *primary* from being the one control in the header that never
   * does anything.
   */
  const fallbackTarget = editorReachable ? null : (openTargets[0] ?? null);
  const primaryReason = editorReachable || fallbackTarget !== null ? null : editorReason;
  const primaryDisabled = !editorReachable && fallbackTarget === null;

  const everythingBlocked =
    options.length > 0 &&
    options.every((option) => openInDisabledReason(option.value, openLocation) !== null);

  return (
    <Group aria-label="Open in editor">
      <Button
        aria-label={compact ? "Open file in preferred editor" : undefined}
        className="ps-[8.5px]"
        size="xs"
        variant="outline"
        data-testid="header-open"
        disabled={primaryDisabled}
        {...(primaryReason === null ? {} : { "data-disabled-reason": primaryReason })}
        onClick={() => {
          if (editorReachable) {
            openInEditor(preferredEditor);
            return;
          }
          fallbackTarget?.run();
        }}
      >
        {editorReachable && primaryOption?.Icon ? (
          <primaryOption.Icon
            aria-hidden="true"
            className={cn("size-3.5", getOpenInIconClass(primaryOption.kind))}
          />
        ) : null}

        {!editorReachable && fallbackTarget !== null ? (
          fallbackTarget.external ? (
            <ExternalLinkIcon aria-hidden="true" className="size-3.5 text-foreground" />
          ) : (
            <FolderClosedIcon aria-hidden="true" className="size-3.5 text-muted-foreground" />
          )
        ) : null}
        <span
          className={
            compact
              ? "sr-only"
              : "sr-only @3xl/header-actions:not-sr-only @3xl/header-actions:ml-0.5"
          }
        >
          Open
        </span>
      </Button>
      <GroupSeparator {...(!compact ? { className: "hidden @3xl/header-actions:block" } : {})} />
      <Menu>
        <MenuTrigger
          render={<Button aria-label="Choose editor" size="icon-xs" variant="outline" />}
          data-testid="header-open-menu"
        >
          <ChevronDownIcon aria-hidden="true" className="size-4" />
        </MenuTrigger>
        <MenuPopup align="end">
          {options.length === 0 && <MenuItem disabled>No installed editors found</MenuItem>}
          {options.map(({ label, Icon, value, kind }) => {
            const reason = openInDisabledReason(value, openLocation);
            if (reason !== null) {

              return (
                <Popover key={value}>
                  <PopoverTrigger
                    openOnHover
                    nativeButton={false}
                    render={
                      <span
                        className="block w-max cursor-not-allowed"
                        data-testid={rowTestId(value)}
                        data-disabled-reason={reason}
                      />
                    }
                  >
                    <MenuItem className="w-full" disabled>
                      <Icon aria-hidden="true" className={getOpenInIconClass(kind)} />
                      {label}
                      {value === preferredEditor && openFavoriteEditorShortcutLabel && (
                        <MenuShortcut>{openFavoriteEditorShortcutLabel}</MenuShortcut>
                      )}
                    </MenuItem>
                  </PopoverTrigger>
                  <PopoverPopup tooltipStyle side="left" align="center">
                    {reason}
                  </PopoverPopup>
                </Popover>
              );
            }
            return (
              <MenuItem
                key={value}
                data-testid={rowTestId(value)}
                onClick={() => openInEditor(value)}
              >
                <Icon aria-hidden="true" className={getOpenInIconClass(kind)} />
                {label}
                {value === preferredEditor && openFavoriteEditorShortcutLabel && (
                  <MenuShortcut>{openFavoriteEditorShortcutLabel}</MenuShortcut>
                )}
              </MenuItem>
            );
          })}
          {everythingBlocked && (
            <MenuItem disabled>{openInDisabledReason("cursor", openLocation)}</MenuItem>
          )}
          {openTargets.length > 0 && <MenuSeparator />}
          {openTargets.map((target) => (
            <MenuItem
              key={target.id}
              data-testid={`header-open-${target.id}`}
              onClick={() => target.run()}
            >
              {target.external ? <ExternalLinkIcon aria-hidden="true" /> : null}
              {target.label}
            </MenuItem>
          ))}
        </MenuPopup>
      </Menu>
    </Group>
  );
});
