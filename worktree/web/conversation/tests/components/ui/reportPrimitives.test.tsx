// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { Chart, chartTicks } from "../../../src/components/ui/chart";
import { MetricTile } from "../../../src/components/ui/metric-tile";

afterEach(cleanup);

describe("MetricTile", () => {
  it.each([
    { name: "a value and a good delta", value: "276", delta: { text: "56 vs prior 7d", direction: "up", tone: "good" } as const, text: ["276", "▲", "56 vs prior 7d", "up 56 vs prior 7d, better"] },
    { name: "an unrecorded value and prior", value: null, delta: null, text: ["Not recorded", "No prior data"] },
  ])("renders $name", ({ value, delta, text }) => {
    const { container } = render(<MetricTile label="Shipped" value={value} delta={delta} />);
    for (const part of text) expect(container.textContent).toContain(part);
    expect(container.textContent).not.toContain("0");
  });
});

describe("Chart", () => {
  it("covers the largest value with its top tick", () => {
    expect(chartTicks(52)).toEqual([0, 20, 40, 60]);
    expect(chartTicks(0.4)).toEqual([0, 0.2, 0.4]);
  });

  it("reads out every series for the category the arrow keys select", () => {
    render(
      <Chart
        ariaLabel="Shipped per day"
        categories={[
          { key: "a", label: "Oct 6" },
          { key: "b", label: "Oct 7" },
        ]}
        series={[
          { id: "shipped", label: "Shipped", color: "var(--success)", kind: "bar", values: [3, 5] },
          { id: "prior", label: "Prior", color: "var(--muted-foreground)", kind: "line", values: [2, null] },
        ]}
      />,
    );
    const plot = screen.getByRole("img", { name: "Shipped per day" });
    fireEvent.keyDown(plot, { key: "ArrowRight" });
    fireEvent.keyDown(plot, { key: "ArrowRight" });
    const readout = screen.getByRole("status");
    expect(readout.textContent).toContain("Oct 7");
    expect(readout.textContent).toContain("Shipped5");
    expect(readout.textContent).toContain("Prior—");
    fireEvent.keyDown(plot, { key: "Escape" });
    expect(screen.queryByRole("status")).toBeNull();
  });
});
