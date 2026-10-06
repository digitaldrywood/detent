# Element catalog

This document is generated from [`web/conversation/src/design-system/catalog.json`](../../web/conversation/src/design-system/catalog.json) by `npm run design:catalog` in `web/conversation`. Edit the catalog, not this file; `npm run design:catalog:check` fails when the two disagree.

One row per catalogued element. The [component contracts](components.md) hold the full contract for each. Status *available* means the component ships in Detent; *proposed* means it is planned and has no source yet; *exception* means a deliberate, documented departure from the shared contract.

Totals: 107 entries; by kind: 46 primitive, 55 composition, 6 surface; by status: 107 available, 0 proposed, 0 exception.

| Element | Group | Kind | Status | Source |
| --- | --- | --- | --- | --- |
| [Button](components.md#button) | Actions | primitive | available | `src/components/ui/button.tsx` |
| [Toggle](components.md#toggle) | Actions | primitive | available | `src/components/ui/toggle.tsx` |
| [Toggle group](components.md#toggle-group) | Actions | primitive | available | `src/components/ui/toggle-group.tsx` |
| [Panel tab close button](components.md#panel-tab-close-button) | Actions | primitive | available | `src/components/ui/panel-tab-close-button.tsx` |
| [Refresh icon](components.md#refresh-icon) | Actions | primitive | available | `src/components/ui/refresh-icon.tsx` |
| [Input](components.md#input) | Forms | primitive | available | `src/components/ui/input.tsx` |
| [Textarea](components.md#textarea) | Forms | primitive | available | `src/components/ui/textarea.tsx` |
| [Label](components.md#label) | Forms | primitive | available | `src/components/ui/label.tsx` |
| [Checkbox](components.md#checkbox) | Forms | primitive | available | `src/components/ui/checkbox.tsx` |
| [Switch](components.md#switch) | Forms | primitive | available | `src/components/ui/switch.tsx` |
| [Radio group](components.md#radio-group) | Forms | primitive | available | `src/components/ui/radio-group.tsx` |
| [Number field](components.md#number-field) | Forms | primitive | available | `src/components/ui/number-field.tsx` |
| [Input group](components.md#input-group) | Forms | primitive | available | `src/components/ui/input-group.tsx` |
| [Draft input](components.md#draft-input) | Forms | primitive | available | `src/components/ui/draft-input.tsx` |
| [Color picker](components.md#color-picker) | Forms | primitive | available | `src/components/ui/color-picker.tsx` |
| [Calendar](components.md#calendar) | Forms | primitive | available | `src/components/ui/calendar.tsx` |
| [Select](components.md#select) | Choice and search | primitive | available | `src/components/ui/select.tsx` |
| [Combobox](components.md#combobox) | Choice and search | primitive | available | `src/components/ui/combobox.tsx` |
| [Autocomplete](components.md#autocomplete) | Choice and search | primitive | available | `src/components/ui/autocomplete.tsx` |
| [Command](components.md#command) | Choice and search | primitive | available | `src/components/ui/command.tsx` |
| [Dialog](components.md#dialog) | Overlays | primitive | available | `src/components/ui/dialog.tsx` |
| [Alert dialog](components.md#alert-dialog) | Overlays | primitive | available | `src/components/ui/alert-dialog.tsx` |
| [Sheet](components.md#sheet) | Overlays | primitive | available | `src/components/ui/sheet.tsx` |
| [Popover](components.md#popover) | Overlays | primitive | available | `src/components/ui/popover.tsx` |
| [Menu](components.md#menu) | Overlays | primitive | available | `src/components/ui/menu.tsx` |
| [Tooltip](components.md#tooltip) | Overlays | primitive | available | `src/components/ui/tooltip.tsx` |
| [Preview card](components.md#preview-card) | Overlays | primitive | available | `src/components/ui/preview-card.tsx` |
| [Alert](components.md#alert) | Feedback | primitive | available | `src/components/ui/alert.tsx` |
| [Badge](components.md#badge) | Feedback | primitive | available | `src/components/ui/badge.tsx` |
| [Toast](components.md#toast) | Feedback | primitive | available | `src/components/ui/toast.tsx` |
| [Spinner](components.md#spinner) | Feedback | primitive | available | `src/components/ui/spinner.tsx` |
| [Skeleton](components.md#skeleton) | Feedback | primitive | available | `src/components/ui/skeleton.tsx` |
| [Empty state](components.md#empty-state) | Feedback | primitive | available | `src/components/ui/empty.tsx` |
| [Sidebar primitive](components.md#sidebar-primitive) | Navigation | primitive | available | `src/components/ui/sidebar.tsx` |
| [Scroll area](components.md#scroll-area) | Structure | primitive | available | `src/components/ui/scroll-area.tsx` |
| [Separator](components.md#separator) | Structure | primitive | available | `src/components/ui/separator.tsx` |
| [Group](components.md#group) | Structure | primitive | available | `src/components/ui/group.tsx` |
| [Collapsible](components.md#collapsible) | Structure | primitive | available | `src/components/ui/collapsible.tsx` |
| [Collapsible section header](components.md#collapsible-section-header) | Structure | primitive | available | `src/components/ui/collapsible-section-header.tsx` |
| [Middle truncate](components.md#middle-truncate) | Structure | primitive | available | `src/components/ui/middle-truncate.tsx` |
| [Table](components.md#table) | Data display | primitive | available | `src/components/ui/table.tsx` |
| [Keyboard key](components.md#keyboard-key) | Data display | primitive | available | `src/components/ui/kbd.tsx` |
| [Discovery list](components.md#discovery-list) | Data display | primitive | available | `src/components/ui/discovery-list.tsx` |
| [Standalone page](components.md#standalone-page) | Entry | primitive | available | `src/components/ui/standalone-page.tsx` |
| [Wizard](components.md#wizard) | Entry | primitive | available | `src/components/ui/wizard.tsx` |
| [QR code](components.md#qr-code) | Entry | primitive | available | `src/components/ui/qr-code.tsx` |
| [App sidebar layout](components.md#app-sidebar-layout) | Workspace | composition | available | `src/components/AppSidebarLayout.tsx` |
| [Thread sidebar](components.md#thread-sidebar) | Workspace | composition | available | `src/components/Sidebar.tsx` |
| [Sidebar chrome](components.md#sidebar-chrome) | Workspace | composition | available | `src/components/sidebar/SidebarChrome.tsx` |
| [Detent wordmark](components.md#detent-wordmark) | Workspace | composition | available | `src/components/DetentWordmark.tsx` |
| [Workspace page header](components.md#workspace-page-header) | Workspace | composition | available | `src/components/WorkspacePageHeader.tsx` |
| [Workspace page container](components.md#workspace-page-container) | Workspace | composition | available | `src/components/WorkspacePageContainer.tsx` |
| [Workspace breadcrumb](components.md#workspace-breadcrumb) | Workspace | composition | available | `src/components/WorkspaceBreadcrumb.tsx` |
| [Command palette](components.md#command-palette) | Workspace | composition | available | `src/app/components/CommandPalette.tsx` |
| [Command palette content](components.md#command-palette-content) | Workspace | composition | available | `src/components/CommandPaletteContent.tsx` |
| [Project favicon](components.md#project-favicon) | Workspace | composition | available | `src/components/ProjectFavicon.tsx` |
| [Thread status indicators](components.md#thread-status-indicators) | Workspace | composition | available | `src/components/ThreadStatusIndicators.tsx` |
| [Git actions control](components.md#git-actions-control) | Workspace | composition | available | `src/components/GitActionsControl.tsx` |
| [Chat header](components.md#chat-header) | Conversation | composition | available | `src/components/chat/ChatHeader.tsx` |
| [Conversation timeline](components.md#conversation-timeline) | Conversation | composition | available | `src/app/components/Timeline.tsx` |
| [Messages timeline](components.md#messages-timeline) | Conversation | composition | available | `src/components/chat/MessagesTimeline.tsx` |
| [Chat markdown](components.md#chat-markdown) | Conversation | composition | available | `src/components/ChatMarkdown.tsx` |
| [Composer](components.md#composer) | Conversation | composition | available | `src/app/components/Composer.tsx` |
| [Composer surface](components.md#composer-surface) | Conversation | composition | available | `src/components/chat/ComposerSurface.tsx` |
| [Composer control](components.md#composer-control) | Conversation | composition | available | `src/components/chat/ComposerControl.tsx` |
| [Composer primary actions](components.md#composer-primary-actions) | Conversation | composition | available | `src/components/chat/ComposerPrimaryActions.tsx` |
| [Composer banner](components.md#composer-banner) | Conversation | composition | available | `src/components/chat/ComposerBanner.tsx` |
| [Composer banner stack](components.md#composer-banner-stack) | Conversation | composition | available | `src/components/chat/ComposerBannerStack.tsx` |
| [Pending user input panel](components.md#pending-user-input-panel) | Conversation | composition | available | `src/components/chat/ComposerPendingUserInputPanel.tsx` |
| [File tag chip](components.md#file-tag-chip) | Conversation | composition | available | `src/components/chat/FileTagChip.tsx` |
| [Proposed plan card](components.md#proposed-plan-card) | Conversation | composition | available | `src/components/chat/ProposedPlanCard.tsx` |
| [Right panel workspace](components.md#right-panel-workspace) | Panels | composition | available | `src/app/components/RightPanel.tsx` |
| [Right panel tabs](components.md#right-panel-tabs) | Panels | composition | available | `src/components/RightPanelTabs.tsx` |
| [Right panel sheet](components.md#right-panel-sheet) | Panels | composition | available | `src/components/RightPanelSheet.tsx` |
| [Right panel resize handle](components.md#right-panel-resize-handle) | Panels | composition | available | `src/components/preview/RightPanelResizeHandle.tsx` |
| [Diff panel shell](components.md#diff-panel-shell) | Panels | composition | available | `src/components/DiffPanelShell.tsx` |
| [Preview panel shell](components.md#preview-panel-shell) | Panels | composition | available | `src/components/preview/PreviewPanelShell.tsx` |
| [Diff surface](components.md#diff-surface) | Panels | surface | available | `src/app/components/surfaces/DiffSurface.tsx` |
| [Files surface](components.md#files-surface) | Panels | surface | available | `src/app/components/surfaces/FilesSurface.tsx` |
| [File breadcrumbs](components.md#file-breadcrumbs) | Panels | composition | available | `src/app/components/surfaces/FileBreadcrumbs.tsx` |
| [File browser panel](components.md#file-browser-panel) | Panels | composition | available | `src/app/components/surfaces/FileBrowserPanel.tsx` |
| [Terminal surface](components.md#terminal-surface) | Panels | surface | available | `src/app/components/surfaces/TerminalSurface.tsx` |
| [Output surface](components.md#output-surface) | Panels | surface | available | `src/app/components/surfaces/OutputSurface.tsx` |
| [Pull request surface](components.md#pull-request-surface) | Panels | surface | available | `src/app/components/surfaces/PullRequestSurface.tsx` |
| [Workspace status view](components.md#workspace-status-view) | Panels | surface | available | `src/app/components/surfaces/WorkspaceStatusView.tsx` |
| [Settings layout](components.md#settings-layout) | Settings | composition | available | `src/app/settings/settingsLayout.tsx` |
| [Settings sidebar nav](components.md#settings-sidebar-nav) | Settings | composition | available | `src/components/settings/SettingsSidebarNav.tsx` |
| [Settings pages](components.md#settings-pages) | Settings | composition | available | `src/app/settings/Settings.tsx` |
| [Settings help](components.md#settings-help) | Settings | composition | available | `src/app/settings/SettingsHelp.tsx` |
| [Context help](components.md#context-help) | Settings | composition | available | `src/app/components/ContextHelp.tsx` |
| [Expandable text](components.md#expandable-text) | Settings | composition | available | `src/app/settings/ExpandableText.tsx` |
| [Redacted sensitive text](components.md#redacted-sensitive-text) | Settings | composition | available | `src/components/settings/RedactedSensitiveText.tsx` |
| [Usage page](components.md#usage-page) | Usage | composition | available | `src/app/usage/UsagePage.tsx` |
| [Usage provider chart](components.md#usage-provider-chart) | Usage | composition | available | `src/app/usage/UsageProviderChart.tsx` |
| [Usage limits](components.md#usage-limits) | Usage | composition | available | `src/components/usage/UsageLimits.tsx` |
| [Usage limits section](components.md#usage-limits-section) | Usage | composition | available | `src/app/usage/UsageLimits.tsx` |
| [Work board](components.md#work-board) | Work | composition | available | `src/app/work/WorkBoard.tsx` |
| [Work top bar](components.md#work-top-bar) | Work | composition | available | `src/app/work/components/WorkTopBar.tsx` |
| [Work toolbar](components.md#work-toolbar) | Work | composition | available | `src/app/work/components/WorkToolbar.tsx` |
| [Board lane](components.md#board-lane) | Work | composition | available | `src/app/work/components/BoardLane.tsx` |
| [Issue card](components.md#issue-card) | Work | composition | available | `src/app/work/components/IssueCard.tsx` |
| [Work list](components.md#work-list) | Work | composition | available | `src/app/work/components/WorkList.tsx` |
| [Issue page](components.md#issue-page) | Work | composition | available | `src/app/work/IssuePage.tsx` |
| [Issue properties](components.md#issue-properties) | Work | composition | available | `src/app/work/components/IssueProperties.tsx` |
| [Property picker](components.md#property-picker) | Work | composition | available | `src/app/work/components/IssuePickers.tsx` |
| [Activity feed](components.md#activity-feed) | Work | composition | available | `src/app/work/components/ActivityFeed.tsx` |
| [Issue composer](components.md#issue-composer) | Work | composition | available | `src/app/work/components/IssueComposer.tsx` |
