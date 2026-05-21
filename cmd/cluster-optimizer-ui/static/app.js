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
  metricNodeCount: document.querySelector("#metricNodeCount"),
  metricRequestedMem: document.querySelector("#metricRequestedMem"),
  metricObservedMem: document.querySelector("#metricObservedMem"),
  overviewKicker: document.querySelector("#overviewKicker"),
  overviewTitle: document.querySelector("#overviewTitle"),
  overviewStatus: document.querySelector("#overviewStatus"),
  overviewVerdict: document.querySelector("#overviewVerdict"),
  overviewCpuHeadroom: document.querySelector("#overviewCpuHeadroom"),
  overviewCpuBar: document.querySelector("#overviewCpuBar"),
  overviewCpuTarget: document.querySelector("#overviewCpuTarget"),
  overviewMemHeadroom: document.querySelector("#overviewMemHeadroom"),
  overviewMemBar: document.querySelector("#overviewMemBar"),
  overviewMemTarget: document.querySelector("#overviewMemTarget"),
  overviewObservedRatio: document.querySelector("#overviewObservedRatio"),
  overviewObservedBar: document.querySelector("#overviewObservedBar"),
  overviewObservedDelta: document.querySelector("#overviewObservedDelta"),
  overviewActions: document.querySelector("#overviewActions"),
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
  renderOptimizationOverview(report);
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
    els.metricNodeCount.textContent = "-";
    els.metricRequestedMem.textContent = "-";
    els.metricObservedMem.textContent = "-";
    return;
  }
  const findings = report.findings || [];
  const summary = report.summary || {};
  const high = findings.filter((item) => item.severity === "high").length;
  const medium = findings.filter((item) => item.severity === "medium").length;
  els.generatedAt.textContent = formatTime(report.generated_at);
  els.metricFindings.textContent = String(findings.length);
  els.metricHigh.textContent = String(high);
  els.metricMedium.textContent = String(medium);
  els.metricNodeCount.textContent = formatNumber(summary.node_count);
  els.metricRequestedMem.textContent = formatMiB(summary.requested_memory_mib);
  els.metricObservedMem.textContent = formatMiB(summary.observed_memory_mib);
}

function renderOptimizationOverview(report) {
  if (!report) {
    els.overviewKicker.textContent = "Optimization Overview";
    els.overviewTitle.textContent = "Capacity Fit";
    els.overviewStatus.textContent = "-";
    els.overviewStatus.className = "overview-status";
    setVerdict("No report selected", "-", "Load a report to evaluate node fit and optimization blockers.", "neutral");
    setRail(els.overviewCpuHeadroom, els.overviewCpuBar, els.overviewCpuTarget, "-", 0, "-");
    setRail(els.overviewMemHeadroom, els.overviewMemBar, els.overviewMemTarget, "-", 0, "-");
    setRail(els.overviewObservedRatio, els.overviewObservedBar, els.overviewObservedDelta, "-", 0, "-");
    els.overviewActions.replaceChildren();
    return;
  }

  const summary = report.summary || {};
  const twoNode = summary.two_node_estimate || {};
  const currentNodes = Number(summary.node_count || 0);
  const feasible = twoNode.feasible === true;
  const alreadyAtTarget = currentNodes > 0 && currentNodes <= 2;
  const requestedMem = Number(summary.requested_memory_mib || 0);
  const observedMem = Number(summary.observed_memory_mib || 0);
  const observedRatio = requestedMem > 0 ? observedMem / requestedMem : 0;
  const memDelta = requestedMem - observedMem;

  els.overviewKicker.textContent = `${formatNumber(currentNodes)} node${currentNodes === 1 ? "" : "s"} observed`;
  els.overviewTitle.textContent = alreadyAtTarget ? "Running At Floor" : "Two-Node Fit";
  els.overviewStatus.textContent = feasible || alreadyAtTarget ? "Fit viable" : "Blocked";
  els.overviewStatus.className = `overview-status ${feasible || alreadyAtTarget ? "good" : "blocked"}`;

  if (alreadyAtTarget) {
    setVerdict("Operating at target", "2 nodes", "The cluster is already at the minimum configured node-pool size.", "good");
  } else if (feasible) {
    setVerdict("Scale-down candidate", "2 nodes", "Requested resources clear the CPU and memory headroom guardrails.", "good");
  } else {
    setVerdict("Scale-down blocked", "2 nodes", twoNode.reason || "Requested resources do not clear the two-node headroom guardrails.", "blocked");
  }

  setRail(
    els.overviewCpuHeadroom,
    els.overviewCpuBar,
    els.overviewCpuTarget,
    formatCPU(Number(twoNode.cpu_headroom_m || 0)),
    ratio(Number(twoNode.cpu_headroom_m || 0), Number(twoNode.minimum_cpu_headroom_m || 0)),
    `minimum ${formatCPU(Number(twoNode.minimum_cpu_headroom_m || 0))}`
  );
  setRail(
    els.overviewMemHeadroom,
    els.overviewMemBar,
    els.overviewMemTarget,
    formatMiB(twoNode.memory_headroom_mib),
    ratio(Number(twoNode.memory_headroom_mib || 0), Number(twoNode.minimum_memory_headroom_mib || 0)),
    `minimum ${formatMiB(twoNode.minimum_memory_headroom_mib)}`
  );
  setRail(
    els.overviewObservedRatio,
    els.overviewObservedBar,
    els.overviewObservedDelta,
    requestedMem > 0 ? `${Math.round(observedRatio * 100)}%` : "-",
    Math.max(0, Math.min(1, observedRatio)),
    memDelta > 0 ? `${formatMiB(memDelta)} requested above observed` : "observed usage meets request"
  );
  renderOverviewActions(report);
}

function setVerdict(label, value, detail, tone) {
  els.overviewVerdict.className = `overview-verdict ${tone}`;
  els.overviewVerdict.innerHTML = `
    <span>${escapeHtml(label)}</span>
    <strong>${escapeHtml(value)}</strong>
    <p>${escapeHtml(detail)}</p>
  `;
}

function setRail(valueEl, barEl, targetEl, value, progress, target) {
  valueEl.textContent = value;
  barEl.style.width = `${Math.round(Math.max(0, Math.min(1, progress)) * 100)}%`;
  targetEl.textContent = target;
}

function renderOverviewActions(report) {
  els.overviewActions.replaceChildren();
  const trend = state.data?.trend || {};
  const window = trend.window || {};
  const requiredDays = window.required_days || 3;
  const rollups = trend.top_recommendations || [];
  const currentRollups = rollups.filter((item) => item.latest_report_has);
  const ready = currentRollups.filter((item) => item.remediation?.available);
  const persistent = currentRollups.filter((item) => item.observed_days >= requiredDays);
  const findings = report.findings || [];
  const high = findings.filter((item) => item.severity === "high").length;
  const top = currentRollups[0];

  [
    {
      label: "Ready actions",
      value: formatNumber(ready.length),
      detail: ready.length ? `${ready[0].scope} can be remediated` : "No automated remediation is ready"
    },
    {
      label: "Persistent blockers",
      value: formatNumber(persistent.length),
      detail: `${requiredDays} day persistence threshold`
    },
    {
      label: high > 0 ? "High risk" : "Top blocker",
      value: high > 0 ? formatNumber(high) : topSeverity(top),
      detail: top ? `${top.scope} · ${top.rule_id}` : "No active blocker in the latest report"
    }
  ].forEach((item) => {
    const row = document.createElement("article");
    row.className = "overview-action";
    row.innerHTML = `
      <span>${escapeHtml(item.label)}</span>
      <strong>${escapeHtml(item.value)}</strong>
      <p>${escapeHtml(item.detail)}</p>
    `;
    els.overviewActions.append(row);
  });
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
  const originalText = button.textContent;
  button.disabled = true;
  button.textContent = "Loading";
  try {
    const clusterId = encodeURIComponent(state.data?.cluster_id || "default");
    const ruleId = encodeURIComponent(finding.rule_id);
    const namespace = encodeURIComponent(finding.namespace || "");
    const workload = encodeURIComponent(finding.workload || "");
    const url = `/api/remediations/download?cluster_id=${clusterId}&rule_id=${ruleId}&namespace=${namespace}&workload=${workload}`;

    const response = await fetch(url);
    if (!response.ok) {
      throw new Error(`Failed to load instructions: ${response.statusText}`);
    }
    const markdown = await response.text();
    const filename = `remediation-instructions-${finding.workload.toLowerCase().replaceAll("/", "-")}.md`;
    
    showInstructionsModal(
      originalText === "Plan Rewrite" ? "Rewrite Modernization Instructions" : "Remediation Instructions",
      markdown,
      filename
    );
  } catch (error) {
    status.textContent = error.message;
  } finally {
    button.textContent = originalText;
    button.disabled = false;
  }
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

function formatCPU(value) {
  if (value === undefined || value === null) return "-";
  return `${Math.round(Number(value)).toLocaleString()}m`;
}

function formatNumber(value) {
  if (value === undefined || value === null) return "-";
  return Number(value).toLocaleString();
}

function ratio(value, minimum) {
  if (!minimum) return 0;
  return Math.max(0, value / minimum);
}

function topSeverity(rollup) {
  if (!rollup?.severity) return "-";
  return titleCase(rollup.severity);
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

function showInstructionsModal(title, content, filename) {
  const overlay = document.createElement("div");
  overlay.className = "modal-overlay";

  const card = document.createElement("div");
  card.className = "modal-card instructions-modal";

  const h2 = document.createElement("h2");
  h2.textContent = title;

  const pre = document.createElement("pre");
  pre.className = "instructions-preview";
  pre.textContent = content;

  const actions = document.createElement("div");
  actions.className = "modal-actions";

  const copyBtn = document.createElement("button");
  copyBtn.type = "button";
  copyBtn.className = "modal-button confirm";
  copyBtn.textContent = "Copy to Clipboard";

  const downloadBtn = document.createElement("button");
  downloadBtn.type = "button";
  downloadBtn.className = "modal-button confirm";
  downloadBtn.textContent = "Download Markdown";

  const closeBtn = document.createElement("button");
  closeBtn.type = "button";
  closeBtn.className = "modal-button cancel";
  closeBtn.textContent = "Close";

  copyBtn.addEventListener("click", () => {
    navigator.clipboard.writeText(content).then(() => {
      const original = copyBtn.textContent;
      copyBtn.textContent = "Copied!";
      setTimeout(() => {
        copyBtn.textContent = original;
      }, 2000);
    });
  });

  downloadBtn.addEventListener("click", () => {
    const blob = new Blob([content], { type: "text/markdown;charset=utf-8;" });
    const link = document.createElement("a");
    link.href = URL.createObjectURL(blob);
    link.setAttribute("download", filename);
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
  });

  closeBtn.addEventListener("click", () => {
    overlay.remove();
  });

  actions.append(copyBtn, downloadBtn, closeBtn);
  card.append(h2, pre, actions);
  overlay.append(card);
  document.body.append(overlay);
}
