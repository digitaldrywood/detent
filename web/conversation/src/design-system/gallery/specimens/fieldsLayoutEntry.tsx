// Fields, layout and entry primitives: radio group, number field, input group, draft input, colour controls,
// calendar, collapsible section header, middle truncate, discovery list,
// standalone page, wizard, QR code.
import {
  AtSignIcon,
  CopyIcon,
  FolderGit2Icon,
  GitBranchIcon,
  KeyRoundIcon,
  SearchIcon,
  ServerIcon,
  TerminalIcon,
} from "lucide-react";
import React from "react";

import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import { Calendar } from "~/components/ui/calendar";
import {
  CollapsibleSectionHeader,
  SectionHeaderStatus,
} from "~/components/ui/collapsible-section-header";
import { ColorHueSlider, ColorSaturationValuePlane } from "~/components/ui/color-picker";
import { Dialog, DialogClose, DialogTrigger } from "~/components/ui/dialog";
import { DiscoveryList, DiscoveryListRow } from "~/components/ui/discovery-list";
import { DraftInput } from "~/components/ui/draft-input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "~/components/ui/input-group";
import { Kbd } from "~/components/ui/kbd";
import { Label } from "~/components/ui/label";
import { MiddleTruncate } from "~/components/ui/middle-truncate";
import {
  NumberField,
  NumberFieldDecrement,
  NumberFieldGroup,
  NumberFieldIncrement,
  NumberFieldInput,
} from "~/components/ui/number-field";
import { QRCodeSvg } from "~/components/ui/qr-code";
import { Radio, RadioGroup } from "~/components/ui/radio-group";
import { StandalonePage, StandalonePageHeader } from "~/components/ui/standalone-page";
import { Textarea } from "~/components/ui/textarea";
import {
  WizardFooter,
  WizardHeader,
  WizardPanel,
  WizardPopup,
  WizardSteps,
} from "~/components/ui/wizard";
import { hsvToHex, type HsvColor } from "~/lib/color";

import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

// --- Radio group ------------------------------------------------------------

function RadioOption({ value, label, disabled }: { value: string; label: string; disabled?: boolean }) {
  return (
    <Label>
      <Radio value={value} disabled={disabled ?? false} />
      {label}
    </Label>
  );
}

const radioGroup: GalleryDoc = {
  meta: { name: "Radio group", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "states",
      title: "Group, disabled option, disabled group, invalid",
      note: "Arrow keys move the selection within a group.",
      render: () => (
        <Row className="gap-x-10">
          <Cell label="default">
            <RadioGroup defaultValue="half" aria-label="Renewal window">
              <RadioOption value="half" label="Half the lease" />
              <RadioOption value="fixed" label="Fixed 20 seconds" />
              <RadioOption value="never" label="Never renew" disabled />
            </RadioGroup>
          </Cell>
          <Cell label="group disabled">
            <RadioGroup defaultValue="a" disabled aria-label="Disabled group">
              <RadioOption value="a" label="Selected" />
              <RadioOption value="b" label="Unselected" />
            </RadioGroup>
          </Cell>
          <Cell label="invalid (aria-invalid)">
            <RadioGroup aria-label="Invalid group">
              <Label>
                <Radio value="x" aria-invalid />
                Pick one
              </Label>
              <Label>
                <Radio value="y" aria-invalid />
                Or this
              </Label>
            </RadioGroup>
          </Cell>
          <Cell label="long label" className="max-w-72">
            <RadioGroup defaultValue="long" aria-label="Long">
              <Label className="items-start">
                <Radio value="long" className="mt-0.5" />
                {LONG_LABEL}
              </Label>
            </RadioGroup>
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Number field -----------------------------------------------------------

function Stepper({
  size,
  ...props
}: React.ComponentProps<typeof NumberField> & { readonly label: string }) {
  const { label, ...rest } = props;
  return (
    <NumberField size={size ?? "default"} className="w-40" {...rest}>
      <NumberFieldGroup>
        <NumberFieldDecrement />
        <NumberFieldInput aria-label={label} />
        <NumberFieldIncrement />
      </NumberFieldGroup>
    </NumberField>
  );
}

const numberField: GalleryDoc = {
  meta: { name: "Number field", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes",
      note: "Arrow keys step by one, Shift+Arrow by ten; the buttons repeat while held.",
      render: () => (
        <Row>
          {(["sm", "default", "lg"] as const).map((size) => (
            <Cell key={size} label={size}>
              <Stepper size={size} defaultValue={4} min={1} max={16} label={`Runners ${size}`} />
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "states",
      title: "States",
      render: () => (
        <Row>
          <Cell label="empty">
            <Stepper label="Empty" />
          </Cell>
          <Cell label="at max">
            <Stepper defaultValue={16} max={16} label="At max" />
          </Cell>
          <Cell label="disabled">
            <Stepper defaultValue={2} disabled label="Disabled" />
          </Cell>
          <Cell label="currency format">
            <Stepper defaultValue={25} step={5} format={{ style: "currency", currency: "USD" }} label="Spend limit" />
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Input group ------------------------------------------------------------

const inputGroup: GalleryDoc = {
  meta: { name: "Input group", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "addons",
      title: "Addons: inline start/end, block start/end",
      render: () => (
        <div className="flex w-80 max-w-full flex-col gap-4">
          <Cell label="inline-start icon" className="w-full">
            <InputGroup>
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput placeholder="Search issues" aria-label="Search issues" />
            </InputGroup>
          </Cell>
          <Cell label="inline-end kbd + badge" className="w-full">
            <InputGroup>
              <InputGroupInput placeholder="Jump to" aria-label="Jump to" />
              <InputGroupAddon align="inline-end">
                <Kbd>⌘K</Kbd>
              </InputGroupAddon>
            </InputGroup>
          </Cell>
          <Cell label="inline-end button" className="w-full">
            <InputGroup>
              <InputGroupAddon>
                <KeyRoundIcon />
              </InputGroupAddon>
              <InputGroupInput defaultValue="dtk_live_8f2c41a0b7d8" aria-label="Token" readOnly />
              <InputGroupAddon align="inline-end">
                <Button variant="ghost" size="icon-xs" aria-label="Copy token">
                  <CopyIcon />
                </Button>
              </InputGroupAddon>
            </InputGroup>
          </Cell>
          <Cell label="block-start + textarea" className="w-full">
            <InputGroup>
              <InputGroupAddon align="block-start">
                <Badge size="sm" variant="secondary">
                  detent#142
                </Badge>
              </InputGroupAddon>
              <Textarea unstyled placeholder="Leave a comment" aria-label="Comment" />
              <InputGroupAddon align="block-end">
                <Button size="xs" className="ms-auto">
                  Comment
                </Button>
              </InputGroupAddon>
            </InputGroup>
          </Cell>
        </div>
      ),
    },
    {
      id: "states",
      title: "Variants and states",
      render: () => (
        <div className="flex w-80 max-w-full flex-col gap-4">
          <Cell label="ghost" className="w-full">
            <InputGroup variant="ghost">
              <InputGroupAddon>
                <AtSignIcon />
              </InputGroupAddon>
              <InputGroupInput placeholder="Mention someone" aria-label="Mention" />
            </InputGroup>
          </Cell>
          <Cell label="invalid" className="w-full">
            <InputGroup>
              <InputGroupAddon>
                <GitBranchIcon />
              </InputGroupAddon>
              <InputGroupInput defaultValue="not a branch" aria-invalid aria-label="Branch (invalid)" />
            </InputGroup>
          </Cell>
          <Cell label="disabled" className="w-full">
            <InputGroup>
              <InputGroupAddon>
                <GitBranchIcon />
              </InputGroupAddon>
              <InputGroupInput defaultValue="main" disabled aria-label="Branch (disabled)" />
            </InputGroup>
          </Cell>
        </div>
      ),
    },
  ],
};

// --- Draft input ------------------------------------------------------------

function DraftInputSpecimen() {
  const [committed, setCommitted] = React.useState("detent");
  return (
    <div className="flex w-72 flex-col gap-2">
      <Label htmlFor="ds-draft-input">Project name</Label>
      <DraftInput id="ds-draft-input" value={committed} onCommit={setCommitted} />
      <p className="text-muted-foreground text-xs">
        Committed value: <code className="font-mono">{committed}</code>
      </p>
    </div>
  );
}

const draftInput: GalleryDoc = {
  meta: { name: "Draft input", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "commit",
      title: "Buffers keystrokes, commits on blur or Enter",
      note: "Type, then press Enter or Tab away: the committed value below updates only then.",
      render: () => <DraftInputSpecimen />,
    },
    {
      id: "states",
      title: "Disabled and invalid",
      render: () => (
        <Row>
          <Cell label="disabled" className="w-56">
            <DraftInput value="Read only" onCommit={() => undefined} disabled aria-label="Disabled" />
          </Cell>
          <Cell label="invalid" className="w-56">
            <DraftInput value="has spaces in it" onCommit={() => undefined} aria-invalid aria-label="Invalid" />
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Colour picker ----------------------------------------------------------

function ColorPickerSpecimen() {
  const [color, setColor] = React.useState<HsvColor>({ h: 220, s: 0.7, v: 0.85 });
  const hex = hsvToHex(color.h, color.s, color.v);
  return (
    <div className="flex w-64 flex-col gap-3">
      <ColorSaturationValuePlane label="Accent" value={color} onChange={setColor} />
      <ColorHueSlider label="Accent hue" value={color.h} onChange={(h) => setColor((current) => ({ ...current, h }))} />
      <div className="flex items-center gap-2 text-sm">
        <span aria-hidden className="size-5 rounded border border-border" style={{ background: hex }} />
        <code className="font-mono text-xs">{hex}</code>
      </div>
    </div>
  );
}

const colorPicker: GalleryDoc = {
  meta: { name: "Color picker", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "controls",
      title: "Saturation/brightness plane and hue slider",
      note: "Drag, or focus and use the arrow keys (Shift for larger steps).",
      render: () => <ColorPickerSpecimen />,
    },
    {
      id: "edge",
      title: "Edge variant (flush to a popover's edge)",
      render: () => (
        <div className="w-64 overflow-hidden rounded-lg border">
          <ColorSaturationValuePlane
            label="Edge"
            variant="edge"
            value={{ h: 150, s: 0.6, v: 0.7 }}
            onChange={() => undefined}
          />
        </div>
      ),
    },
  ],
};

// --- Calendar ---------------------------------------------------------------

const MONTH = new Date(2026, 8, 1);

function RangeCalendar() {
  const [range, setRange] = React.useState<{ from: Date | undefined; to?: Date | undefined } | undefined>({
    from: new Date(2026, 8, 8),
    to: new Date(2026, 8, 14),
  });
  return <Calendar mode="range" defaultMonth={MONTH} selected={range} onSelect={setRange} />;
}

const calendar: GalleryDoc = {
  meta: { name: "Calendar", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "single",
      title: "Single date, disabled weekends",
      note: "Arrow keys move by day, Page Up/Down by month.",
      render: () => (
        <Calendar
          mode="single"
          defaultMonth={MONTH}
          selected={new Date(2026, 8, 9)}
          disabled={{ dayOfWeek: [0, 6] }}
        />
      ),
    },
    { id: "range", title: "Range", render: () => <RangeCalendar /> },
    {
      id: "dropdowns",
      title: "Month and year dropdowns",
      render: () => (
        <Calendar
          mode="single"
          captionLayout="dropdown"
          defaultMonth={MONTH}
          startMonth={new Date(2024, 0)}
          endMonth={new Date(2027, 11)}
        />
      ),
    },
  ],
};

// --- Collapsible section header ---------------------------------------------

function SectionHeaders() {
  const tones = ["muted", "info", "emphasized", "accent"] as const;
  const [open, setOpen] = React.useState<Record<string, boolean>>({ muted: true });
  return (
    <div className="flex w-72 max-w-full flex-col gap-1 bg-sidebar p-2" data-app-sidebar="">
      {tones.map((tone) => (
        <div key={tone}>
          <CollapsibleSectionHeader
            tone={tone}
            expanded={open[tone] === true}
            onClick={() => setOpen((current) => ({ ...current, [tone]: !current[tone] }))}
            accessory={tone === "info" ? <SectionHeaderStatus>2 failing</SectionHeaderStatus> : undefined}
          >
            {tone}
          </CollapsibleSectionHeader>
          {open[tone] === true ? (
            <p className="px-2 pb-2 text-muted-foreground text-xs">Section content for {tone}.</p>
          ) : null}
        </div>
      ))}
    </div>
  );
}

const collapsibleSectionHeader: GalleryDoc = {
  meta: { name: "Collapsible section header", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "tones",
      title: "Tones, expanded and collapsed, with a status accessory",
      note: "The caller owns the open state; click a header to toggle it.",
      render: () => <SectionHeaders />,
    },
  ],
};

// --- Middle truncate --------------------------------------------------------

const middleTruncate: GalleryDoc = {
  meta: { name: "Middle truncate", kind: "primitive", group: "layout" },
  specimens: [
    {
      id: "values",
      title: "Branches, paths and shas at a constrained width",
      note: "Hover for the full value. Short strings are not split.",
      render: () => (
        <div className="flex w-56 max-w-full flex-col gap-2 rounded-lg border p-3 text-sm">
          <MiddleTruncate value="fix/cache-main-20260918-180825" />
          <MiddleTruncate value="web/conversation/src/app/work/components/IssueCard.tsx" />
          <MiddleTruncate value="9dd73354c0a0c1f4a9104c18655cf707ac1c31c4361e2" tail={7} />
          <MiddleTruncate value="main" />
        </div>
      ),
    },
  ],
};

// --- Discovery list ---------------------------------------------------------

const discoveryList: GalleryDoc = {
  meta: { name: "Discovery list", kind: "primitive", group: "data-display" },
  specimens: [
    {
      id: "rows",
      title: "Rows with icon, description, action; disabled; long text",
      render: () => (
        <div className="w-96 max-w-full">
          <DiscoveryList>
            <DiscoveryListRow
              icon={<FolderGit2Icon className="size-4 text-muted-foreground" />}
              title="detent"
              description="~/Development/digitaldrywood/detent"
              action={<Badge variant="success">Found</Badge>}
            />
            <DiscoveryListRow
              icon={<ServerIcon className="size-4 text-muted-foreground" />}
              title="mac-studio runner"
              description="Last seen 2 minutes ago"
            />
            <DiscoveryListRow
              icon={<TerminalIcon className="size-4 text-muted-foreground" />}
              title={LONG_LABEL}
              description={LONG_LABEL}
            />
            <DiscoveryListRow
              icon={<FolderGit2Icon className="size-4 text-muted-foreground" />}
              title="archived-repo"
              description="Not a git repository"
              disabled
            />
          </DiscoveryList>
        </div>
      ),
    },
  ],
};

// --- Standalone page --------------------------------------------------------

function StandalonePageSpecimen({ tone }: { readonly tone: "pairing" | "error" | "brand" }) {
  return (
    <StandalonePage tone={tone}>
      <StandalonePageHeader
        eyebrow={tone === "error" ? "Sign-in failed" : "Detent"}
        title={tone === "pairing" ? "Pair this runner" : tone === "error" ? "That link has expired" : "Welcome to Detent"}
        description="Entry pages outside the app shell share this page and card geometry."
      />
      <div className="mt-6 flex gap-2">
        <Button>Continue</Button>
        <Button variant="ghost">Cancel</Button>
      </div>
    </StandalonePage>
  );
}

const standalonePage: GalleryDoc = {
  meta: { name: "Standalone page", kind: "primitive", group: "entry" },
  specimens: (["brand", "pairing", "error"] as const).map((tone) => ({
    id: tone,
    title: `Tone: ${tone}`,
    note: tone === "brand" ? "Fills its viewport (min-h-screen), so the frame has a fixed height." : undefined,
    height: 520,
    render: () => <StandalonePageSpecimen tone={tone} />,
  })),
};

// --- Wizard -----------------------------------------------------------------

const WIZARD_STEPS = ["Repository", "Runner", "Review"] as const;

function WizardSpecimen({ size }: { readonly size: "default" | "wide" }) {
  const [step, setStep] = React.useState(1);
  return (
    <Dialog>
      <DialogTrigger render={<Button variant="outline" />}>
        Open {size} wizard
      </DialogTrigger>
      <WizardPopup size={size}>
        <WizardHeader title="Add a project" description="Connect a repository and choose where it runs.">
          <WizardSteps
            steps={WIZARD_STEPS}
            currentStep={step}
            summaries={["detent", null, null]}
            showSummaries
            onStepChange={setStep}
            isStepDisabled={(index) => index > step}
          />
        </WizardHeader>
        <WizardPanel>
          <p className="text-muted-foreground text-sm">
            Step {step + 1}: {WIZARD_STEPS[step]}. Panel height animates between steps.
          </p>
        </WizardPanel>
        <WizardFooter leading={<DialogClose render={<Button variant="ghost" />}>Cancel</DialogClose>}>
          <Button variant="outline" disabled={step === 0} onClick={() => setStep((s) => Math.max(0, s - 1))}>
            Back
          </Button>
          <Button onClick={() => setStep((s) => Math.min(WIZARD_STEPS.length - 1, s + 1))}>
            {step === WIZARD_STEPS.length - 1 ? "Finish" : "Next"}
          </Button>
        </WizardFooter>
      </WizardPopup>
    </Dialog>
  );
}

const wizard: GalleryDoc = {
  meta: { name: "Wizard", kind: "primitive", group: "entry" },
  specimens: [
    {
      id: "dialog",
      title: "Steps, panel and footer in a dialog",
      note: "Completed steps are buttons back; later steps are disabled until reached.",
      minHeight: 480,
      render: () => (
        <Row>
          <WizardSpecimen size="default" />
          <WizardSpecimen size="wide" />
        </Row>
      ),
    },
    {
      id: "steps",
      title: "Step indicator alone (static)",
      render: () => (
        <div className="w-[min(32rem,100%)]">
          <WizardSteps steps={WIZARD_STEPS} currentStep={2} summaries={["detent", "mac-studio", null]} showSummaries />
        </div>
      ),
    },
  ],
};

// --- QR code ----------------------------------------------------------------

const qrCode: GalleryDoc = {
  meta: { name: "QR code", kind: "primitive", group: "entry" },
  specimens: [
    {
      id: "levels",
      title: "Sizes, error correction, margin",
      render: () => (
        <Row>
          <Cell label="64px, L">
            <QRCodeSvg value="https://detent.dev/pair/8f2c41a0" size={64} title="Pairing code" />
          </Cell>
          <Cell label="128px, M, margin 2">
            <QRCodeSvg value="https://detent.dev/pair/8f2c41a0" size={128} level="M" marginSize={2} title="Pairing code" />
          </Cell>
          <Cell label="160px, H, long value">
            <QRCodeSvg value={`https://detent.dev/pair?token=${"x".repeat(120)}`} size={160} level="H" marginSize={2} title="Long pairing code" />
          </Cell>
        </Row>
      ),
    },
  ],
};

export const FIELDS_LAYOUT_ENTRY: Readonly<Record<string, GalleryDoc>> = {
  "radio-group": radioGroup,
  "number-field": numberField,
  "input-group": inputGroup,
  "draft-input": draftInput,
  "color-picker": colorPicker,
  calendar,
  "collapsible-section-header": collapsibleSectionHeader,
  "middle-truncate": middleTruncate,
  "discovery-list": discoveryList,
  "standalone-page": standalonePage,
  wizard,
  "qr-code": qrCode,
};
