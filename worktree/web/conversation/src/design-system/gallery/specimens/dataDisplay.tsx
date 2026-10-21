// Report primitives: tabs, metric tiles and charts.
import React from "react";

import { Avatar } from "~/components/ui/avatar";
import { BarMeter, Chart, ChartLegend, ChartPanel, ProportionBar } from "~/components/ui/chart";
import { MetricTile, MetricTileGroup } from "~/components/ui/metric-tile";
import { Tabs, TabsList, TabsPanel, TabsTab } from "~/components/ui/tabs";

import type { GalleryDoc } from "../specimen";

const DAYS = ["Oct 7", "Oct 8", "Oct 9", "Oct 10", "Oct 11", "Oct 12", "Oct 13"].map((label, index) => ({
  key: label,
  label,
  shortLabel: String(index + 7),
}));
const INK = (share: number) => `color-mix(in oklab, var(--contrast-foreground) ${share}%, var(--background))`;

function TabsSpecimen({ variant = "line" }: { variant?: "line" | "segmented" }): React.ReactElement {
  const [tab, setTab] = React.useState<string>("overview");
  return (
    <Tabs value={tab} onValueChange={(value) => setTab(String(value))} variant={variant} className="w-full max-w-xl">
      <TabsList aria-label="Report sections">
        {["Overview", "Flow", "Quality", "Cost", "Failures"].map((label) => (
          <TabsTab key={label} value={label.toLowerCase()}>
            {label}
          </TabsTab>
        ))}
        <TabsTab value="archive" disabled>
          Archive
        </TabsTab>
      </TabsList>
      <TabsPanel value={tab} className="pt-3 text-muted-foreground text-sm">
        The {tab} panel.
      </TabsPanel>
    </Tabs>
  );
}

export const tabs: GalleryDoc = {
  meta: { name: "Tabs", kind: "primitive", group: "navigation" },
  specimens: [
    {
      id: "states",
      title: "Selected, hover, focus and disabled; scrolls horizontally at 390px",
      render: () => <TabsSpecimen />,
    },
    { id: "segmented", title: "Segmented tabs", render: () => <TabsSpecimen variant="segmented" /> },
  ],
};

export const metricTile: GalleryDoc = {
  meta: { name: "Metric tile", kind: "primitive", group: "data-display" },
  specimens: [
    {
      id: "group",
      title: "Group: deltas good and bad, not recorded value and prior, detail",
      render: () => (
        <MetricTileGroup aria-label="Headline" className="w-full lg:grid-cols-5">
          <MetricTile label="Shipped" value="301" delta={{ text: "38 vs prior 7d", direction: "up", tone: "good" }} detail="plus 112 cancelled" />
          <MetricTile label="Cycle time p50" value="9h 30m" delta={{ text: "1h 24m faster", direction: "down", tone: "good" }} detail="p90 41h · Todo → Done" />
          <MetricTile label="Landed first try" value="61%" delta={{ text: "4 pts", direction: "down", tone: "bad" }} detail="of 301 shipped" />
          <MetricTile label="Cost per shipped issue" value={null} delta={null} detail="Usage not recorded" />
          <MetricTile label="Stuck over 24h" value="7" delta={null} detail="open, past lane p90" />
        </MetricTileGroup>
      ),
    },
    {
      id: "compact",
      title: "Compact, value over label",
      render: () => (
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-4">
          <MetricTile size="compact" label="Open findings" value="3" />
          <MetricTile size="compact" label="Stalled in lane" value="1" />
          <MetricTile size="compact" label="Merge queue wait p50" value="4m" />
          <MetricTile size="compact" label="Landings refused on conflict" value={null} />
        </div>
      ),
    },
  ],
};

export const chart: GalleryDoc = {
  meta: { name: "Chart", kind: "primitive", group: "data-display" },
  specimens: [
    {
      id: "bars-and-prior",
      title: "Grouped bars with a dashed prior line, in a panel",
      render: () => (
        <ChartPanel
          className="w-full"
          title="Shipped per day"
          actions={
            <ChartLegend
              items={[
                { label: "Shipped", color: "var(--success)" },
                { label: "Failed attempts", color: "var(--warning)" },
                { label: "Prior 7d", color: "var(--muted-foreground)", dashed: true },
              ]}
            />
          }
          footnote="Hover or focus the chart and use the arrow keys for counts."
        >
          <Chart
            ariaLabel="Shipped and failed attempts per day"
            categories={DAYS}
            series={[
              { id: "shipped", label: "Shipped", color: "var(--success)", kind: "bar", values: [36, 44, 32, 48, 52, 24, 40] },
              { id: "failed", label: "Failed attempts", color: "var(--warning)", kind: "bar", values: [12, 16, 10, 18, 8, 6, 10] },
              { id: "prior", label: "Prior 7d", color: "var(--muted-foreground)", kind: "line", dashed: true, values: [29, 35, 27, 39, 40, 17, 33] },
            ]}
          />
        </ChartPanel>
      ),
    },
    {
      id: "stacked-band",
      title: "Stacked ink-mix bars and a p50–p90 band",
      render: () => (
        <div className="grid w-full gap-4 lg:grid-cols-2">
          <ChartPanel title="Spend per day by stage" actions={<ChartLegend items={[{ label: "Code", color: INK(100) }, { label: "Rework", color: INK(72) }, { label: "Not shipped", color: "var(--warning)" }]} />}>
            <Chart
              ariaLabel="Spend per day by stage"
              categories={DAYS}
              formatValue={(value) => `$${Math.round(value)}`}
              series={[
                { id: "code", label: "Code", color: INK(100), kind: "bar", stack: "spend", values: [380, 420, 330, 520, 460, 230, 400] },
                { id: "rework", label: "Rework", color: INK(72), kind: "bar", stack: "spend", values: [150, 170, 130, 210, 180, 90, 160] },
                { id: "not", label: "Not shipped", color: "var(--warning)", kind: "bar", stack: "spend", values: [180, 170, 130, 240, 200, 90, 160] },
              ]}
            />
          </ChartPanel>
          <ChartPanel title="Cycle time per day" actions={<ChartLegend items={[{ label: "p50", color: "var(--primary)" }]} />}>
            <Chart
              ariaLabel="Cycle time p50 with a p50 to p90 band"
              categories={DAYS}
              formatValue={(value) => `${Math.round(value)}h`}
              series={[{ id: "cycle", label: "p50", color: "var(--primary)", kind: "band", values: [9, 10, 8, 11, null, 8, 9], upper: [38, 42, 34, 46, null, 31, 40] }]}
            />
          </ChartPanel>
        </div>
      ),
    },
    {
      id: "shares-and-empty",
      title: "Proportion bar, bar meters and the empty chart",
      render: () => (
        <div className="grid w-full gap-4 lg:grid-cols-2">
          <ChartPanel title="Where cycle time goes">
            <ProportionBar
              ariaLabel="Todo 58%, In Progress 12%, Rework 19%, held 11%"
              segments={[
                { label: "Todo", value: 58, color: "var(--info)" },
                { label: "In Progress", value: 12, color: "var(--success)" },
                { label: "Rework", value: 19, color: "var(--warning)" },
                { label: "Held", value: 11, color: "var(--muted-foreground)" },
              ]}
            />
            <div className="flex flex-col gap-2 text-sm">
              {[
                ["Gate failed before landing", 88],
                ["Review findings to address", 54],
                ["Conflict on landing", 12],
              ].map(([label, value]) => (
                <div key={label} className="flex items-center gap-3">
                  <span className="w-48 truncate">{label}</span>
                  <BarMeter value={Number(value)} max={88} color="var(--info)" className="flex-1" />
                  <span className="w-8 text-right tabular-nums">{value}</span>
                </div>
              ))}
            </div>
          </ChartPanel>
          <ChartPanel title="Failed attempts per day">
            <Chart ariaLabel="No failed attempts" categories={DAYS} series={[{ id: "failed", label: "Failed", color: "var(--warning)", kind: "bar", values: [0, 0, 0, 0, 0, 0, 0] }]} />
          </ChartPanel>
        </div>
      ),
    },
  ],
};

export const avatar: GalleryDoc = {
  meta: { name: "Avatar", kind: "primitive", group: "Feedback" },
  specimens: [{
    id: "identity",
    title: "Compact and member sizes, image and initials",
    render: () => <div className="flex flex-wrap items-center gap-6">
      <div className="flex items-center gap-2"><Avatar name="Example Member" />Example Member</div>
      <div className="flex items-center gap-2"><Avatar name="Example Member" size="md" />Example Member</div>
      <div className="flex items-center gap-2"><Avatar name="Another Member" size="md" src="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' fill='%236b7280'/%3E%3Ccircle cx='16' cy='12' r='6' fill='%23e5e7eb'/%3E%3Cpath d='M4 32a12 12 0 0 1 24 0' fill='%23e5e7eb'/%3E%3C/svg%3E" />Another Member</div>
    </div>,
  }],
};
