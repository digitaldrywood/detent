// Feedback: alert, badge, toast, spinner, skeleton, empty.
import type { VariantProps } from "class-variance-authority";
import {
  CheckCircle2Icon,
  CircleAlertIcon,
  CopyIcon,
  InboxIcon,
  InfoIcon,
  TriangleAlertIcon,
} from "lucide-react";
import React from "react";

import { Alert, AlertAction, AlertDescription, AlertTitle } from "~/components/ui/alert";
import { showAnchoredCopySuccessToast } from "~/components/ui/anchoredCopyToast";
import { Badge, type badgeVariants } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "~/components/ui/empty";
import { Skeleton } from "~/components/ui/skeleton";
import { Spinner } from "~/components/ui/spinner";
import { stackedThreadToast, toastManager } from "~/components/ui/toast";

import { Cell, keysOf, LONG_LABEL, Matrix, Row, type GalleryDoc } from "../specimen";

type AlertVariant = "default" | "info" | "success" | "warning" | "error";
type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;
type BadgeSize = NonNullable<VariantProps<typeof badgeVariants>["size"]>;

const ALERT_ICONS: Record<AlertVariant, React.ReactNode> = {
  default: <InfoIcon />,
  info: <InfoIcon />,
  success: <CheckCircle2Icon />,
  warning: <TriangleAlertIcon />,
  error: <CircleAlertIcon />,
};

const BADGE_VARIANTS = keysOf<BadgeVariant>({
  default: true,
  secondary: true,
  outline: true,
  info: true,
  success: true,
  warning: true,
  error: true,
  destructive: true,
  label: true,
});

const CODE_HOST_LABELS: readonly (readonly [string, string])[] = [
  ["bug", "#d73a4a"],
  ["enhancement", "#a2eeef"],
  ["documentation", "#0075ca"],
  ["good first issue", "#7057ff"],
  ["ui", "#fbca04"],
];

type SpinnerProps = React.ComponentProps<typeof Spinner>;
const SPINNER_SIZES = keysOf<NonNullable<SpinnerProps["size"]>>({ xs: true, sm: true, md: true, lg: true });
const SPINNER_TONES = keysOf<NonNullable<SpinnerProps["tone"]>>({ current: true, muted: true });
const EMPTY_SIZES = keysOf<NonNullable<React.ComponentProps<typeof Empty>["size"]>>({
  compact: true,
  default: true,
  hero: true,
});

const BADGE_SIZES = keysOf<BadgeSize>({ sm: true, default: true, lg: true, control: true });

export const alert: GalleryDoc = {
  meta: { name: "Alert", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "variants",
      title: "Variants",
      render: () => (
        <div className="flex max-w-xl flex-col gap-3">
          {(Object.keys(ALERT_ICONS) as AlertVariant[]).map((variant) => (
            <Alert key={variant} variant={variant}>
              {ALERT_ICONS[variant]}
              <AlertTitle>{variant} alert</AlertTitle>
              <AlertDescription>The runner reconnected and resumed the attempt.</AlertDescription>
            </Alert>
          ))}
        </div>
      ),
    },
    {
      id: "action",
      title: "With action, first-line alignment, long text",
      render: () => (
        <div className="flex max-w-xl flex-col gap-3">
          <Alert variant="warning">
            <TriangleAlertIcon />
            <AlertTitle>Usage limit at 90%</AlertTitle>
            <AlertAction>
              <Button size="xs" variant="warning-outline">
                Review plan
              </Button>
            </AlertAction>
          </Alert>
          <Alert variant="error" controlAlignment="first-line">
            <CircleAlertIcon />
            <AlertTitle>{LONG_LABEL}</AlertTitle>
            <AlertDescription>{LONG_LABEL}. It wraps onto several lines in a narrow frame.</AlertDescription>
            <AlertAction>
              <Button size="xs" variant="outline">
                Retry
              </Button>
            </AlertAction>
          </Alert>
        </div>
      ),
    },
    {
      id: "glass",
      title: "Glass surface over content",
      note: "`surface=\"glass\"` floats the alert over content; the tint follows `variant`.",
      render: () => (
        <div className="relative max-w-xl overflow-hidden rounded-xl border p-4">
          <div aria-hidden className="flex flex-col gap-1.5 text-muted-foreground text-xs">
            {Array.from({ length: 8 }, (_, index) => (
              <span key={index}>
                {index + 1}. The agent edited internal/lock/lease.go and ran the focused tests.
              </span>
            ))}
          </div>
          <div className="absolute inset-x-4 top-1/2 flex -translate-y-1/2 flex-col gap-2">
            {(["default", "warning", "error"] as const).map((variant) => (
              <Alert key={variant} surface="glass" variant={variant}>
                {ALERT_ICONS[variant]}
                <AlertTitle>glass · {variant}</AlertTitle>
              </Alert>
            ))}
          </div>
        </div>
      ),
    },
    {
      id: "sidebar",
      title: "Sidebar variant",
      note: "A compact notice on the sidebar's control surface, at the sidebar's type size.",
      render: () => (
        <div className="w-64 rounded-lg bg-sidebar p-2 text-sidebar-foreground">
          <Alert variant="sidebar">
            <AlertTitle>Runner offline</AlertTitle>
            <AlertDescription>Reconnecting to the hub. Queued work resumes automatically.</AlertDescription>
          </Alert>
        </div>
      ),
    },
  ],
};

export const badge: GalleryDoc = {
  meta: { name: "Badge", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "matrix",
      title: "Variants × sizes",
      render: () => (
        <Matrix
          rows={BADGE_VARIANTS}
          columns={BADGE_SIZES}
          render={(variant, size) => (
            <Badge
              variant={variant}
              size={size}
              style={variant === "label" ? ({ "--label": "#a855f7" } as React.CSSProperties) : undefined}
            >
              {variant === "default" ? "3" : variant}
            </Badge>
          )}
        />
      ),
    },
    {
      id: "interactive",
      title: "As a button or link",
      render: () => (
        <Row>
          <Cell label="render={<button />}">
            <Badge variant="outline" render={<button type="button" />}>
              Filter: Todo
            </Badge>
          </Cell>
          <Cell label="render={<a />}">
            <Badge variant="secondary" render={<a href="#specimen" />}>
              DET-142
            </Badge>
          </Cell>
          <Cell label="with icon">
            <Badge variant="success">
              <CheckCircle2Icon />
              Merged
            </Badge>
          </Cell>
        </Row>
      ),
    },
    {
      id: "label",
      title: "Label tinted from --label",
      note: "The consumer sets the code host's label colour as `--label` in `style`; the badge mixes its tint and text from it.",
      render: () => (
        <Row>
          {CODE_HOST_LABELS.map(([name, color]) => (
            <Cell key={name} label={color}>
              <Badge variant="label" style={{ "--label": color } as React.CSSProperties}>
                {name}
              </Badge>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

function ToastButtons() {
  const anchor = React.useRef<HTMLButtonElement>(null);
  return (
    <Row>
      {(["success", "info", "warning", "error", "loading"] as const).map((type) => (
        <Button
          key={type}
          variant="outline"
          size="sm"
          onClick={() =>
            toastManager.add({
              type,
              title: `${type.charAt(0).toUpperCase()}${type.slice(1)} toast`,
              description:
                type === "error"
                  ? "The runner refused the attempt: worktree is dirty."
                  : "Pushed feat/design-system to origin.",
              timeout: type === "loading" ? 4000 : 6000,
            })
          }
        >
          {type}
        </Button>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={() =>
          toastManager.add(
            stackedThreadToast({
              type: "error",
              title: "Could not start the run",
              description: LONG_LABEL,
              actionProps: { children: "Retry", onClick: () => undefined },
            }),
          )
        }
      >
        stacked with action
      </Button>
      <Button ref={anchor} variant="outline" size="sm" onClick={() => showAnchoredCopySuccessToast(anchor)}>
        <CopyIcon />
        anchored copy
      </Button>
    </Row>
  );
}

export const toast: GalleryDoc = {
  meta: { name: "Toast", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "types",
      title: "Types, stacked action, anchored copy",
      note: "Each button adds a real toast through `toastManager`; toasts stack top-right of the frame.",
      minHeight: 380,
      render: () => <ToastButtons />,
    },
  ],
};

export const spinner: GalleryDoc = {
  meta: { name: "Spinner", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes × tones",
      note: "`size` outside a Button; inside one the button sizes the glyph. `current` inherits the text colour.",
      render: () => (
        <Matrix
          rows={SPINNER_TONES}
          columns={SPINNER_SIZES}
          render={(tone, size) => <Spinner size={size} tone={tone} />}
        />
      ),
    },
    {
      id: "button",
      title: "In a button",
      render: () => (
        <Row>
          <Cell label="disabled, sm">
            <Button size="sm" disabled>
              <Spinner />
              Loading
            </Button>
          </Cell>
          <Cell label="outline">
            <Button size="sm" variant="outline" disabled>
              <Spinner tone="muted" />
              Fetching
            </Button>
          </Cell>
        </Row>
      ),
    },
  ],
};

export const skeleton: GalleryDoc = {
  meta: { name: "Skeleton", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "rows",
      title: "Loading rows",
      render: () => (
        <div className="flex w-80 max-w-full flex-col gap-3">
          {[0, 1, 2].map((row) => (
            <div key={row} className="flex items-center gap-3">
              <Skeleton shape="pill" className="size-8" />
              <div className="flex flex-1 flex-col gap-1.5">
                <Skeleton className="h-3 w-3/4" />
                <Skeleton className="h-3 w-1/2" />
              </div>
            </div>
          ))}
        </div>
      ),
    },
    {
      id: "shapes",
      title: "Shapes",
      note: "Size comes from className; the corner radius comes from `shape`.",
      render: () => (
        <Row>
          <Cell label="block (default)">
            <Skeleton shape="block" className="h-3 w-40" />
          </Cell>
          <Cell label="card">
            <Skeleton shape="card" className="h-20 w-40" />
          </Cell>
          <Cell label="pill">
            <Skeleton shape="pill" className="h-6 w-24" />
          </Cell>
        </Row>
      ),
    },
  ],
};

export const empty: GalleryDoc = {
  meta: { name: "Empty", kind: "primitive", group: "feedback" },
  specimens: [
    {
      id: "icon",
      title: "Icon media with action",
      render: () => (
        <Empty className="border border-dashed">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <InboxIcon />
            </EmptyMedia>
            <EmptyTitle>No issues in Todo</EmptyTitle>
            <EmptyDescription>Issues you file, or the agent proposes, land here.</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button size="sm">New issue</Button>
          </EmptyContent>
        </Empty>
      ),
    },
    {
      id: "plain",
      title: "Default media, no action",
      render: () => (
        <Empty>
          <EmptyHeader>
            <EmptyMedia>
              <InboxIcon className="size-6 text-muted-foreground" />
            </EmptyMedia>
            <EmptyTitle>Nothing here yet</EmptyTitle>
            <EmptyDescription>{LONG_LABEL}.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ),
    },
    ...EMPTY_SIZES.map((size) => ({
      id: `size-${size}`,
      title: `Size: ${size}`,
      note:
        size === "compact"
          ? "A card-sized notice."
          : size === "hero"
            ? "Fills a whole route; the title reads larger."
            : "The default room and title size.",
      render: () => (
        <Empty size={size} className="border border-dashed">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <InboxIcon />
            </EmptyMedia>
            <EmptyTitle>No threads yet</EmptyTitle>
            <EmptyDescription>Start a conversation and it shows up here.</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button size="sm">New thread</Button>
          </EmptyContent>
        </Empty>
      ),
    })),
  ],
};
