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
    els.findingsList.append(card);
  });
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
