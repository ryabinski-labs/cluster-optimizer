const state = {
  data: null,
  selectedIndex: 0,
  severity: "all",
  filter: ""
};

const els = {
  clusterId: document.querySelector("#clusterId"),
  limit: document.querySelector("#limit"),
  refresh: document.querySelector("#refresh"),
  tableName: document.querySelector("#tableName"),
  region: document.querySelector("#region"),
  generatedAt: document.querySelector("#generatedAt"),
  reportCount: document.querySelector("#reportCount"),
  metricFindings: document.querySelector("#metricFindings"),
  metricHigh: document.querySelector("#metricHigh"),
  metricMedium: document.querySelector("#metricMedium"),
  metricTwoNode: document.querySelector("#metricTwoNode"),
  metricRequestedMem: document.querySelector("#metricRequestedMem"),
  metricObservedMem: document.querySelector("#metricObservedMem"),
  trendKicker: document.querySelector("#trendKicker"),
  trendDays: document.querySelector("#trendDays"),
  trendStable: document.querySelector("#trendStable"),
  trendReady: document.querySelector("#trendReady"),
  memoryChart: document.querySelector("#memoryChart"),
  rollupList: document.querySelector("#rollupList"),
  timelineList: document.querySelector("#timelineList"),
  findingsList: document.querySelector("#findingsList"),
  errorPanel: document.querySelector("#errorPanel"),
  emptyPanel: document.querySelector("#emptyPanel"),
  viewKicker: document.querySelector("#viewKicker"),
  viewTitle: document.querySelector("#viewTitle"),
  filter: document.querySelector("#filter")
};

document.querySelectorAll("[data-severity]").forEach((button) => {
  button.addEventListener("click", () => {
    state.severity = button.dataset.severity;
    document.querySelectorAll("[data-severity]").forEach((item) => {
      item.classList.toggle("active", item === button);
    });
    render();
  });
});

document.querySelectorAll("[data-severity-shortcut]").forEach((button) => {
  button.addEventListener("click", () => {
    const severity = button.dataset.severityShortcut;
    state.severity = severity;
    document.querySelectorAll("[data-severity]").forEach((item) => {
      item.classList.toggle("active", item.dataset.severity === severity);
    });
    render();
  });
});

els.refresh.addEventListener("click", loadReports);
els.filter.addEventListener("input", () => {
  state.filter = els.filter.value.trim().toLowerCase();
  renderFindings();
});
els.clusterId.addEventListener("keydown", (event) => {
  if (event.key === "Enter") loadReports();
});
els.limit.addEventListener("keydown", (event) => {
  if (event.key === "Enter") loadReports();
});

loadReports();

async function loadReports() {
  clearError();
  els.refresh.disabled = true;
  const clusterId = encodeURIComponent(els.clusterId.value.trim() || "default");
  const limit = encodeURIComponent(els.limit.value || "25");
  try {
    const response = await fetch(`/api/reports?cluster_id=${clusterId}&limit=${limit}`);
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error || `Request failed with ${response.status}`);
    }
    state.data = payload;
    state.selectedIndex = 0;
    render();
  } catch (error) {
    showError(error.message);
  } finally {
    els.refresh.disabled = false;
  }
}

function render() {
  const reports = state.data?.reports || [];
  const report = reports[state.selectedIndex];
  els.tableName.textContent = state.data?.table || "-";
  els.region.textContent = state.data?.region || "-";
  els.reportCount.textContent = String(reports.length);
  els.emptyPanel.classList.toggle("hidden", reports.length !== 0);
  renderTimeline(reports);
  renderSummary(report);
  renderTrends();
  renderFindings();
}

function renderTimeline(reports) {
  els.timelineList.replaceChildren();
  reports.forEach((report, index) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `timeline-item${index === state.selectedIndex ? " active" : ""}`;
    button.innerHTML = `
      <strong>${formatTime(report.generated_at)}</strong>
      <span>${report.findings?.length || 0} findings</span>
    `;
    button.addEventListener("click", () => {
      state.selectedIndex = index;
      render();
    });
    els.timelineList.append(button);
  });
}

function renderSummary(report) {
  if (!report) {
    els.generatedAt.textContent = "never";
    els.metricFindings.textContent = "0";
    els.metricHigh.textContent = "0";
    els.metricMedium.textContent = "0";
    els.metricTwoNode.textContent = "-";
    els.metricRequestedMem.textContent = "-";
    els.metricObservedMem.textContent = "-";
    return;
  }
  const findings = report.findings || [];
  const summary = report.summary || {};
  const high = findings.filter((item) => item.severity === "high").length;
  const medium = findings.filter((item) => item.severity === "medium").length;
  const twoNode = summary.two_node_estimate || {};
  els.generatedAt.textContent = formatTime(report.generated_at);
  els.metricFindings.textContent = String(findings.length);
  els.metricHigh.textContent = String(high);
  els.metricMedium.textContent = String(medium);
  els.metricTwoNode.textContent = twoNode.feasible === true ? "Yes" : "No";
  els.metricRequestedMem.textContent = formatMiB(summary.requested_memory_mib);
  els.metricObservedMem.textContent = formatMiB(summary.observed_memory_mib);
}

function renderFindings() {
  els.findingsList.replaceChildren();
  const report = state.data?.reports?.[state.selectedIndex];
  if (!report) return;
  const rollups = rollupMap();
  const findings = filteredFindings(report.findings || []);
  els.viewKicker.textContent = `${findings.length} visible`;
  els.viewTitle.textContent = `${state.severity === "all" ? "All" : titleCase(state.severity)} Recommendations`;
  if (findings.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-list";
    empty.textContent = "No recommendations match the current filter.";
    els.findingsList.append(empty);
    return;
  }
  findings.forEach((finding) => {
    const card = document.createElement("article");
    card.className = `finding ${finding.severity || "low"}`;
    const rollup = rollups.get(findingKey(finding));
    const remediation = rollup?.remediation;
    card.innerHTML = `
      <header>
        <span class="severity">${escapeHtml(finding.severity || "unknown")}</span>
        <h3>${escapeHtml(scope(finding))}</h3>
        <code>${escapeHtml(finding.rule_id || "")}</code>
      </header>
      <p class="recommendation">${escapeHtml(finding.recommendation || "")}</p>
      <dl>
        <div><dt>Evidence</dt><dd>${escapeHtml(finding.evidence || "")}</dd></div>
        <div><dt>Risk</dt><dd>${escapeHtml(finding.risk || "")}</dd></div>
        <div><dt>Confidence</dt><dd>${escapeHtml(finding.confidence || "unknown")}</dd></div>
        <div><dt>Cost effect</dt><dd>${escapeHtml(finding.expected_cost_effect || "")}</dd></div>
      </dl>
    `;
    card.append(remediationRow(finding, rollup, remediation));
    els.findingsList.append(card);
  });
}

function renderTrends() {
  const trend = state.data?.trend || {};
  const window = trend.window || {};
  const rollups = trend.top_recommendations || [];
  const requiredDays = window.required_days || 3;
  const persistent = rollups.filter((item) => item.latest_report_has && item.observed_days >= requiredDays).length;
  const ready = rollups.filter((item) => item.remediation?.available).length;

  els.trendKicker.textContent = `${window.report_count || 0} reports loaded`;
  els.trendDays.textContent = String(window.observed_days || 0);
  els.trendStable.textContent = String(persistent);
  els.trendReady.textContent = String(ready);
  renderMemoryChart(trend.series || []);
  renderRollups(rollups, requiredDays);
}

function renderMemoryChart(series) {
  els.memoryChart.replaceChildren();
  if (series.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-list";
    empty.textContent = "No trend data yet.";
    els.memoryChart.append(empty);
    return;
  }
  const width = 720;
  const height = 180;
  const pad = 18;
  const maxValue = Math.max(
    1,
    ...series.map((point) => Number(point.requested_memory_mib || 0)),
    ...series.map((point) => Number(point.observed_memory_mib || 0))
  );
  const x = (index) => {
    if (series.length === 1) return width / 2;
    return pad + (index * (width - pad * 2)) / (series.length - 1);
  };
  const y = (value) => height - pad - (Number(value || 0) / maxValue) * (height - pad * 2);
  const line = (key) => series.map((point, index) => `${x(index)},${y(point[key])}`).join(" ");
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.innerHTML = `
    <path class="gridline" d="M${pad} ${height - pad}H${width - pad}" />
    <polyline class="line requested" points="${line("requested_memory_mib")}" />
    <polyline class="line observed" points="${line("observed_memory_mib")}" />
    ${series.map((point, index) => `
      <circle class="dot requested" cx="${x(index)}" cy="${y(point.requested_memory_mib)}" r="3">
        <title>${formatTime(point.generated_at)} requested ${formatMiB(point.requested_memory_mib)}</title>
      </circle>
      <circle class="dot observed" cx="${x(index)}" cy="${y(point.observed_memory_mib)}" r="3">
        <title>${formatTime(point.generated_at)} observed ${formatMiB(point.observed_memory_mib)}</title>
      </circle>
    `).join("")}
  `;
  const legend = document.createElement("div");
  legend.className = "chart-legend";
  legend.innerHTML = "<span><i class='requested'></i>Requested memory</span><span><i class='observed'></i>Observed memory</span>";
  els.memoryChart.append(svg, legend);
}

function renderRollups(rollups, requiredDays) {
  els.rollupList.replaceChildren();
  if (rollups.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-list";
    empty.textContent = "No recurring recommendations yet.";
    els.rollupList.append(empty);
    return;
  }
  rollups.slice(0, 8).forEach((rollup) => {
    const row = document.createElement("article");
    row.className = `rollup ${rollup.severity || "low"}${rollup.latest_report_has ? "" : " resolved"}`;
    const progress = Math.min(100, Math.round(((rollup.observed_days || 0) / requiredDays) * 100));
    row.innerHTML = `
      <div>
        <strong>${escapeHtml(rollup.scope)}</strong>
        <span>${escapeHtml(rollup.rule_id)} · ${rollup.occurrences} report${rollup.occurrences === 1 ? "" : "s"}</span>
      </div>
      <div class="rollup-progress" title="${rollup.observed_days} observed day(s)">
        <i style="width:${progress}%"></i>
      </div>
      <em>${rollup.latest_report_has ? `${rollup.observed_days}/${requiredDays} days` : "resolved"}</em>
    `;
    els.rollupList.append(row);
  });
}

function remediationRow(finding, rollup, remediation) {
  const row = document.createElement("div");
  row.className = "remediation-row";
  const status = document.createElement("span");
  status.textContent = remediation?.reason || "No remediation data for this recommendation.";
  const button = document.createElement("button");
  button.type = "button";
  button.className = "remediate-button";
  button.textContent = remediation?.button_label || "Remediate";
  button.disabled = !remediation?.available;
  button.title = remediation?.available ? actionTitle(remediation) : status.textContent;
  button.addEventListener("click", () => dispatchRemediation(finding, button, status));
  row.append(status, button);
  return row;
}

async function dispatchRemediation(finding, button, status) {
  const rollup = rollupMap().get(findingKey(finding));
  if (rollup?.remediation?.action === "rewrite_plan") {
    const clusterId = encodeURIComponent(state.data?.cluster_id || "default");
    const ruleId = encodeURIComponent(finding.rule_id);
    const namespace = encodeURIComponent(finding.namespace || "");
    const workload = encodeURIComponent(finding.workload || "");
    window.location.href = `/api/remediations/download?cluster_id=${clusterId}&rule_id=${ruleId}&namespace=${namespace}&workload=${workload}`;
    return;
  }

  const action = button.textContent || "Remediate";
  showConfirm(
    "Remediation Confirmation",
    `Are you sure you want to run ${action} for ${scope(finding)}?`,
    async () => {
      button.disabled = true;
      const original = button.textContent;
      button.textContent = "Dispatching";
      try {
        const response = await fetch("/api/remediations", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({
            cluster_id: state.data?.cluster_id || "default",
            rule_id: finding.rule_id,
            namespace: finding.namespace || "",
            workload: finding.workload || "",
            confirm: true
          })
        });
        const payload = await response.json();
        if (!response.ok) {
          throw new Error(payload.error || payload.remediation?.reason || `Request failed with ${response.status}`);
        }
        status.innerHTML = `Workflow dispatched. <a href="${escapeHtml(payload.workflow_url)}" target="_blank" rel="noreferrer">Open Actions</a>`;
        button.textContent = "Dispatched";
      } catch (error) {
        status.textContent = error.message;
        button.textContent = original;
        button.disabled = false;
      }
    }
  );
}

function actionTitle(remediation) {
  if (remediation?.action === "rewrite_plan") {
    return "Create coding-agent rewrite instructions through CI/CD";
  }
  return "Create an api.yml pull request through CI/CD";
}

function filteredFindings(findings) {
  return findings.filter((finding) => {
    if (state.severity !== "all" && finding.severity !== state.severity) return false;
    if (!state.filter) return true;
    const haystack = [
      finding.namespace,
      finding.workload,
      finding.rule_id,
      finding.recommendation,
      finding.evidence,
      finding.risk
    ].join(" ").toLowerCase();
    return haystack.includes(state.filter);
  });
}

function scope(finding) {
  if (finding.namespace && finding.workload) return `${finding.namespace}/${finding.workload}`;
  return finding.workload || "cluster";
}

function findingKey(finding) {
  return [finding.rule_id || "", finding.namespace || "", finding.workload || ""].join("\u0000");
}

function rollupMap() {
  const map = new Map();
  (state.data?.trend?.top_recommendations || []).forEach((rollup) => {
    map.set(rollup.key, rollup);
  });
  return map;
}

function showError(message) {
  els.errorPanel.textContent = message;
  els.errorPanel.classList.remove("hidden");
}

function clearError() {
  els.errorPanel.textContent = "";
  els.errorPanel.classList.add("hidden");
}

function formatTime(value) {
  if (!value) return "-";
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit"
  }).format(new Date(value));
}

function formatMiB(value) {
  if (value === undefined || value === null) return "-";
  return `${Math.round(Number(value)).toLocaleString()}Mi`;
}

function titleCase(value) {
  return value.slice(0, 1).toUpperCase() + value.slice(1);
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function showConfirm(title, message, onConfirm) {
  const overlay = document.createElement("div");
  overlay.className = "modal-overlay";

  const card = document.createElement("div");
  card.className = "modal-card";

  const h2 = document.createElement("h2");
  h2.textContent = title;

  const p = document.createElement("p");
  p.textContent = message;

  const actions = document.createElement("div");
  actions.className = "modal-actions";

  const cancelBtn = document.createElement("button");
  cancelBtn.type = "button";
  cancelBtn.className = "modal-button cancel";
  cancelBtn.textContent = "Cancel";

  const confirmBtn = document.createElement("button");
  confirmBtn.type = "button";
  confirmBtn.className = "modal-button confirm";
  confirmBtn.textContent = "OK";

  cancelBtn.addEventListener("click", () => {
    overlay.remove();
  });

  confirmBtn.addEventListener("click", () => {
    overlay.remove();
    onConfirm();
  });

  actions.append(cancelBtn, confirmBtn);
  card.append(h2, p, actions);
  overlay.append(card);
  document.body.append(overlay);
}
