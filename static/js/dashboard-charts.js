(function () {
  const chartColors = [
    "rgb(15, 118, 110)",
    "rgb(22, 163, 74)",
    "rgb(202, 138, 4)",
    "rgb(220, 38, 38)",
    "rgb(79, 70, 229)",
    "rgb(147, 51, 234)",
  ];
  const refreshMillis = 5000;

  function color(index) {
    return chartColors[index % chartColors.length];
  }

  function dataset(raw, index, fill) {
    const stroke = color(index);
    return {
      label: raw.label || "Series",
      data: raw.data || [],
      borderColor: stroke,
      backgroundColor: fill ? stroke.replace("rgb", "rgba").replace(")", ", 0.18)") : stroke,
      borderWidth: 2,
      tension: 0.3,
      pointRadius: 0,
      fill: Boolean(fill),
    };
  }

  function makeChart(canvas, type, stacked) {
    return new Chart(canvas, {
      type: type,
      data: { labels: [], datasets: [] },
      options: {
        animation: false,
        maintainAspectRatio: false,
        responsive: true,
        interaction: { intersect: false, mode: "index" },
        plugins: {
          legend: { display: true, labels: { boxWidth: 10, boxHeight: 10 } },
          tooltip: { enabled: true },
        },
        scales: {
          x: { grid: { display: false } },
          y: { beginAtZero: true, stacked: Boolean(stacked), ticks: { precision: 0 } },
        },
      },
    });
  }

  function applyDatasets(chart, labels, datasets) {
    chart.data.labels = labels || [];
    chart.data.datasets = datasets || [];
    chart.update("none");
  }

  async function refresh(root, charts) {
    const endpoint = root.dataset.chartEndpoint;
    if (!endpoint) {
      return;
    }
    const response = await fetch(endpoint, { headers: { Accept: "application/json" } });
    if (!response.ok) {
      return;
    }
    const payload = await response.json();
    const labels = payload.labels || [];

    if (charts.running) {
      applyDatasets(charts.running, labels, (payload.running_agents || []).map(function (raw, index) {
        return dataset(raw, index, false);
      }));
    }
    if (charts.tokens) {
      applyDatasets(charts.tokens, labels, [dataset(payload.tokens_per_second || { label: "Tokens/sec", data: [] }, 0, true)]);
    }
    if (charts.spend) {
      applyDatasets(charts.spend, labels, [dataset(payload.token_spend || { label: "Spend", data: [] }, 1, true)]);
    }
    if (charts.completions) {
      applyDatasets(charts.completions, labels, (payload.completions || []).map(function (raw, index) {
        return dataset(raw, index, true);
      }));
    }
    if (charts.board) {
      applyDatasets(charts.board, labels, (payload.board_flow || []).map(function (raw, index) {
        return dataset(raw, index, false);
      }));
    }
  }

  function initialize(root) {
    if (!window.Chart || root.dataset.chartsReady === "true") {
      return;
    }
    const charts = {};
    const runningCanvas = root.querySelector('[data-chart="running-agents"]');
    const tokensCanvas = root.querySelector('[data-chart="tokens-per-second"]');
    const spendCanvas = root.querySelector('[data-chart="token-spend"]');
    const completionsCanvas = root.querySelector('[data-chart="completions"]');
    const boardCanvas = root.querySelector('[data-chart="board-flow"]');

    if (runningCanvas) {
      charts.running = makeChart(runningCanvas, "line", true);
    }
    if (tokensCanvas) {
      charts.tokens = makeChart(tokensCanvas, "line", false);
    }
    if (spendCanvas) {
      charts.spend = makeChart(spendCanvas, "line", false);
    }
    if (completionsCanvas) {
      charts.completions = makeChart(completionsCanvas, "bar", false);
    }
    if (boardCanvas) {
      charts.board = makeChart(boardCanvas, "line", false);
    }

    root.dataset.chartsReady = "true";
    refresh(root, charts).catch(function () {});
    window.setInterval(function () {
      refresh(root, charts).catch(function () {});
    }, refreshMillis);
  }

  function initializeAll() {
    document.querySelectorAll("[data-detent-charts]").forEach(initialize);
  }

  document.addEventListener("DOMContentLoaded", initializeAll);
  document.addEventListener("htmx:afterSettle", initializeAll);
})();
