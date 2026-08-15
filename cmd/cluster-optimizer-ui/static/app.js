const state = {
  data: null,
  selectedIndex: 0,
  severity: "all",
  filter: "",
  activity: { events: [], loading: false, error: null },
  activityFilter: "all",
  activityCollapsed: false,
  activityCollapsedByUser: false,
  activitySkipsInline: false,
  activitySkipsExpanded: {},
  activityLoadedAt: null,
  reportsLoading: false,
  haltPosting: false,
  haltError: ""
};

const REPORT_REFRESH_INTERVAL_MS = 60 * 1000;
const RELATIVE_TIME_TICK_MS = 30 * 1000;

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
  overviewEvidence: document.querySelector("#overviewEvidence"),
  overviewPools: document.querySelector("#overviewPools"),
  overviewBlockers: document.querySelector("#overviewBlockers"),
  overviewBlockerCount: document.querySelector("#overviewBlockerCount"),
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
  filter: document.querySelector("#filter"),
  enginePillMode: document.querySelector("#enginePillMode"),
  enginePillModeText: document.querySelector("#enginePillModeText"),
  enginePillModeDetail: document.querySelector("#enginePillModeDetail"),
  engineModeVerdict: document.querySelector("#engineModeVerdict"),
  enginePillHalt: document.querySelector("#enginePillHalt"),
  enginePillHaltText: document.querySelector("#enginePillHaltText"),
  enginePillHaltAction: document.querySelector("#enginePillHaltAction"),
  enginePillHaltHelp: document.querySelector("#enginePillHaltHelp"),
  enginePillLastRun: document.querySelector("#enginePillLastRun"),
  enginePillLastRunText: document.querySelector("#enginePillLastRunText"),
  enginePillLastRunDetail: document.querySelector("#enginePillLastRunDetail"),
  engineBanner: document.querySelector("#engineBanner"),
  engineModePopover: document.querySelector("#engineModePopover"),
  engineModeDetails: document.querySelector("#engineModeDetails"),
  activityPanel: document.querySelector("#activityPanel"),
  activityList: document.querySelector("#activityList"),
  activityToggle: document.querySelector("#activityToggle"),
  activitySkipsInline: document.querySelector("#activitySkipsInline"),
  activityLive: document.querySelector("#activityLive"),
  activityLiveText: document.querySelector("#activityLiveText")
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

els.enginePillMode.addEventListener("click", () => {
  const open = !els.engineModePopover.classList.contains("hidden");
  toggleEngineModePopover(!open);
});
els.engineModePopover.querySelector(".engine-popover-close").addEventListener("click", () => toggleEngineModePopover(false));
document.addEventListener("click", (event) => {
  if (els.engineModePopover.classList.contains("hidden")) return;
  if (els.engineModePopover.contains(event.target) || els.enginePillMode.contains(event.target)) return;
  toggleEngineModePopover(false);
});

els.enginePillLastRun.addEventListener("click", () => {
  if (!els.enginePillLastRun.classList.contains("actionable")) return;
  scrollToActivityPanel();
});
els.enginePillLastRun.addEventListener("keydown", (event) => {
  if (!els.enginePillLastRun.classList.contains("actionable")) return;
  if (event.key === "Enter" || event.key === " ") {
    event.preventDefault();
    scrollToActivityPanel();
  }
});

els.enginePillHaltAction.addEventListener("click", () => {
  if (els.enginePillHaltAction.disabled) return;
  const halted = Boolean(state.data?.engine_status?.halt_active);
  if (halted) {
    // Recovery path: single click, no modal. Friction here delays
    // returning to normal operation during an incident.
    postHalt(false);
  } else {
    // Activation: confirm modal protects against accidental clicks.
    showHaltConfirm();
  }
});

document.querySelectorAll("[data-activity-filter]").forEach((button) => {
  button.addEventListener("click", () => {
    state.activityFilter = button.dataset.activityFilter;
    document.querySelectorAll("[data-activity-filter]").forEach((item) => {
      item.classList.toggle("active", item === button);
    });
    renderActivity();
  });
});

els.activityToggle.addEventListener("click", () => {
  state.activityCollapsed = !state.activityCollapsed;
  state.activityCollapsedByUser = true;
  applyActivityCollapse();
});

els.activitySkipsInline.addEventListener("change", () => {
  state.activitySkipsInline = els.activitySkipsInline.checked;
  state.activitySkipsExpanded = {};
  renderActivity();
});

loadReports();
setInterval(() => loadReports({ preserveSelection: true }), REPORT_REFRESH_INTERVAL_MS);
setInterval(refreshRelativeTimes, RELATIVE_TIME_TICK_MS);

async function loadReports(options = {}) {
  if (state.reportsLoading) return;
  state.reportsLoading = true;
  clearError();
  els.refresh.disabled = true;
  const clusterIdRaw = els.clusterId.value.trim() || "default";
  const clusterId = encodeURIComponent(clusterIdRaw);
  const limit = encodeURIComponent(els.limit.value || "25");
  const selectedGeneratedAt = options.preserveSelection
    ? state.data?.reports?.[state.selectedIndex]?.generated_at
    : null;
  try {
    const response = await fetch(`/api/reports?cluster_id=${clusterId}&limit=${limit}`);
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error || `Request failed with ${response.status}`);
    }
    state.data = payload;
    if (selectedGeneratedAt) {
      const nextIndex = (payload.reports || []).findIndex((report) => report.generated_at === selectedGeneratedAt);
      state.selectedIndex = nextIndex >= 0 ? nextIndex : 0;
    } else {
      state.selectedIndex = 0;
    }
    render();
    loadActivity(clusterIdRaw);
  } catch (error) {
    showError(error.message);
  } finally {
    state.reportsLoading = false;
    els.refresh.disabled = false;
  }
}

function refreshRelativeTimes() {
  renderEngineStatus();
  renderActivity();
}

async function loadActivity(clusterId) {
  state.activity = { events: [], loading: true, error: null };
  renderActivity();
  try {
    const response = await fetch(`/api/remediations/history?cluster_id=${encodeURIComponent(clusterId)}&limit=50`);
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error || `Request failed with ${response.status}`);
    }
    state.activity = { events: payload.events || [], loading: false, error: null };
    state.activityLoadedAt = Date.now();
  } catch (error) {
    state.activity = { events: [], loading: false, error: error.message };
  }
  applyActivityAutoCollapse();
  renderActivity();
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
  renderEngineStatus();
  renderTrends();
  renderFindings();
  renderActivity();
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

// Human labels for the binpack predicate names. The engine emits stable
// identifiers; the UI is where they become something an operator reads.
const BLOCKER_LABELS = {
  insufficient_cpu: "CPU requests leave no room",
  insufficient_memory: "Memory requests leave no room",
  insufficient_extended_resource: "A device or vendor resource is unavailable",
  node_pod_capacity: "The node's pod cap is reached",
  untolerated_taint: "A taint no pod tolerates",
  node_selector: "A nodeSelector pins pods to specific nodes",
  node_affinity: "Required node affinity pins pods to specific nodes",
  pod_anti_affinity: "Anti-affinity forces one replica per node",
  topology_spread: "A topology spread constraint forbids the placement",
  no_candidate_nodes: "No node in the pool can take these pods",
};

const UNMODELLED_LABELS = {
  extended_resource: "a device-plugin resource we do not track",
  volume_node_affinity: "a persistent volume that may pin pods to nodes",
  spread_match_label_keys: "newer topology-spread fields we do not evaluate",
  affinity_namespace_selector: "a namespaceSelector on an affinity rule",
  runtime_class_overhead: "RuntimeClass overhead on a pod",
  required_pod_affinity: "required pod affinity, which is order-dependent",
};

const EVIDENCE_LABELS = {
  historical_p95: "historical p95",
  recommender: "VPA recommendation",
  sampled_rollup: "sampled p95",
  instant: "single live sample",
  none: "no usage data",
};

// Fidelities that span time and can therefore justify removing capacity.
// Mirrors usage.Fidelity.Actionable() in Go; kept as an allowlist here for
// the same reason it is one there — a new weak source must not silently
// inherit permission to act.
const ACTIONABLE_EVIDENCE = new Set(["historical_p95", "recommender", "sampled_rollup"]);

function renderOptimizationOverview(report) {
  if (!report) {
    els.overviewKicker.textContent = "Capacity Fit";
    els.overviewTitle.textContent = "Minimum safe node count";
    els.overviewStatus.textContent = "-";
    els.overviewStatus.className = "overview-status";
    els.overviewEvidence.textContent = "";
    els.overviewEvidence.hidden = true;
    setVerdict("No report selected", "-", "Load a report to see how few nodes this cluster can safely run on.", "neutral");
    els.overviewPools.replaceChildren();
    els.overviewBlockers.replaceChildren();
    els.overviewBlockerCount.textContent = "";
    els.overviewActions.replaceChildren();
    return;
  }

  const summary = report.summary || {};
  const capacity = summary.capacity;
  const currentNodes = Number(summary.node_count || 0);
  els.overviewKicker.textContent = "Capacity Fit";
  els.overviewTitle.textContent = "Minimum safe node count";

  // A report from before the capacity engine, or a run that could not gather
  // evidence. Say so plainly rather than rendering an empty card that looks
  // like a verdict of zero.
  if (!capacity) {
    els.overviewStatus.textContent = "Not evaluated";
    els.overviewStatus.className = "overview-status neutral";
    els.overviewEvidence.textContent = "";
    els.overviewEvidence.hidden = true;
    setVerdict(
      "No capacity verdict",
      `${formatNumber(currentNodes)} node${currentNodes === 1 ? "" : "s"}`,
      "This report predates the minimum-safe-node engine, or the run could not evaluate placement.",
      "neutral"
    );
    els.overviewPools.replaceChildren();
    els.overviewBlockers.replaceChildren();
    els.overviewBlockerCount.textContent = "";
    renderOverviewActions(report);
    return;
  }

  const pools = capacity.pools || [];
  const minimum = Number(capacity.minimum_safe_nodes || 0);
  const removable = Number(capacity.removable_nodes || 0);
  const indeterminate = pools.filter((p) => p.status === "indeterminate");
  // Two independent reasons a verdict may not be enforceable, kept apart on
  // purpose: the usage evidence may be too thin, or a pool may contain
  // constraints the engine cannot model. Collapsing them into one flag makes
  // the card blame the wrong thing and send the operator to fix the wrong
  // problem.
  const evidenceOk = ACTIONABLE_EVIDENCE.has(capacity.usage_fidelity);
  const actionable = capacity.actionable === true;

  els.overviewEvidence.hidden = false;
  els.overviewEvidence.textContent = evidenceLabel(capacity);
  els.overviewEvidence.title = capacity.usage_note
    ? capacity.usage_note
    : `Verdict computed from ${EVIDENCE_LABELS[capacity.usage_fidelity] || capacity.usage_fidelity} usage data.`;
  // The chip reports evidence strength only — never the pool-modelling gap.
  els.overviewEvidence.className = `overview-evidence ${evidenceOk ? "" : "weak"}`.trim();

  // A pool we could not evaluate is a caveat on the answer, not a substitute
  // for it. If other pools can still give nodes back, that is the headline
  // and the unevaluated pool is a qualifier — burying a real saving behind
  // "cannot say" would be its own kind of dishonesty.
  const caveat = indeterminate.length
    ? ` ${indeterminate.map((p) => p.pool).join(", ")} could not be evaluated and ${indeterminate.length === 1 ? "is" : "are"} excluded.`
    : "";

  // Tone rules: red is reserved for something being wrong. A cluster that is
  // already minimal, or that genuinely needs its capacity, is a correct
  // outcome and must not read as a fault.
  if (removable > 0) {
    const plural = removable === 1 ? "" : "s";
    let detail;
    if (!evidenceOk) {
      // Name the fixable thing. This is the one case with a clear next step.
      detail = `${formatNumber(removable)} node${plural} look releasable, but the verdict rests on ${EVIDENCE_LABELS[capacity.usage_fidelity] || "weak"} data, which cannot tell an idle workload from one caught between bursts. Point the engine at a Prometheus endpoint to enable enforcement.`;
    } else if (capacity.survive_node_loss) {
      detail = `${formatNumber(removable)} node${plural} can be released, and the workload still places after losing one more.`;
    } else {
      // The survival gate was not run, so the card must not imply it passed.
      detail = `${formatNumber(removable)} node${plural} can be released. This run did not require the workload to survive losing a node — set CLUSTER_OPTIMIZER_SURVIVE_NODE_LOSS=true for that guarantee.`;
    }
    els.overviewStatus.textContent = actionable ? "Can shrink" : "Advisory";
    els.overviewStatus.className = `overview-status ${actionable ? "good" : "neutral"}`;
    setVerdict(
      "Minimum safe node count",
      `${formatNumber(currentNodes)} → ${formatNumber(minimum)}`,
      detail + caveat,
      actionable ? "good" : "neutral"
    );
  } else if (indeterminate.length === pools.length && pools.length > 0) {
    els.overviewStatus.textContent = "Indeterminate";
    els.overviewStatus.className = "overview-status unknown";
    setVerdict(
      "Cannot say",
      `${formatNumber(currentNodes)} node${currentNodes === 1 ? "" : "s"}`,
      "Every pool contains constraints this engine does not model, so it makes no scale-down claim at all. Enforcement stays blocked.",
      "unknown"
    );
  } else {
    const floorBound = pools.some((p) => p.status === "at_floor");
    els.overviewStatus.textContent = floorBound ? "At floor" : "Correctly sized";
    els.overviewStatus.className = "overview-status neutral";
    setVerdict(
      floorBound ? "Held at the configured floor" : "Already at minimum",
      `${formatNumber(currentNodes)} node${currentNodes === 1 ? "" : "s"}`,
      (floorBound
        ? "The workload would fit on fewer nodes, but the configured floor stops here. Lower CLUSTER_OPTIMIZER_NODE_FLOOR to go further."
        : "Every node is carrying work that has nowhere else to go. There is nothing to reclaim.") + caveat,
      "neutral"
    );
  }

  renderPoolRows(pools);
  renderBlockers(pools);
  renderOverviewActions(report);
}

function evidenceLabel(capacity) {
  const fidelity = capacity.usage_fidelity || "none";
  const label = EVIDENCE_LABELS[fidelity] || fidelity;
  const source = capacity.usage_source ? ` · ${capacity.usage_source}` : "";
  return `evidence: ${label}${source}`;
}

// One row per pool. Each names its own binding constraint, because "the
// cluster can shrink" is not actionable until you know which pool and why.
function renderPoolRows(pools) {
  els.overviewPools.replaceChildren();
  if (pools.length === 0) {
    const empty = document.createElement("p");
    empty.className = "pool-empty";
    empty.textContent = "No node pools were found in this report.";
    els.overviewPools.append(empty);
    return;
  }
  pools.forEach((pool) => {
    const row = document.createElement("div");
    row.className = `pool-row ${pool.status}`;

    const current = Number(pool.current_nodes || 0);
    const min = Number(pool.minimum_safe_nodes || 0);
    const target = pool.status === "indeterminate" ? "—" : formatNumber(min);

    row.innerHTML = `
      <span class="pool-name" title="${escapeHtml(pool.pool)}">${escapeHtml(pool.pool)}</span>
      <span class="pool-count"><b>${formatNumber(current)}</b> → <b>${target}</b></span>
      <span class="pool-status">${escapeHtml(poolStatusLabel(pool))}</span>
      <span class="pool-binding">${escapeHtml(poolBindingText(pool))}</span>
    `;
    els.overviewPools.append(row);
  });
}

function poolStatusLabel(pool) {
  switch (pool.status) {
    case "fits":
      return `${formatNumber(Number(pool.removable_nodes || 0))} releasable`;
    case "at_floor":
      return "at floor";
    case "at_minimum":
      return "at minimum";
    case "indeterminate":
      return "indeterminate";
    default:
      return pool.status || "-";
  }
}

function poolBindingText(pool) {
  if (pool.status === "indeterminate") {
    const reasons = (pool.indeterminate_reasons || []).map((r) => UNMODELLED_LABELS[r] || r);
    return reasons.length ? `not modelled: ${reasons.join("; ")}` : "not modelled";
  }
  if (pool.status === "at_floor") {
    return `floor of ${formatNumber(Number(pool.configured_floor || 0))}; workload would fit ${formatNumber(Number(pool.derived_minimum || 0))}`;
  }
  const blocker = (pool.blockers || [])[0];
  if (blocker) {
    return BLOCKER_LABELS[blocker.reason] || blocker.reason;
  }
  return pool.binding_constraint || "—";
}

// The blocker list is ordered by how many pods each cause blocks, so the
// thing worth fixing first is at the top rather than buried in a flat list.
function renderBlockers(pools) {
  els.overviewBlockers.replaceChildren();

  const merged = new Map();
  pools.forEach((pool) => {
    (pool.blockers || []).forEach((blocker) => {
      const key = blocker.reason;
      const existing = merged.get(key) || { reason: key, pods: 0, pools: [], examples: [] };
      existing.pods += Number(blocker.pods || 0);
      existing.pools.push(pool.pool);
      (blocker.examples || []).forEach((ex) => {
        if (existing.examples.length < 3) existing.examples.push(ex);
      });
      merged.set(key, existing);
    });
  });

  const list = [...merged.values()].sort((a, b) => b.pods - a.pods);
  els.overviewBlockerCount.textContent = list.length ? String(list.length) : "";

  if (list.length === 0) {
    const empty = document.createElement("li");
    empty.className = "blocker-empty";
    // A pool held at its floor has no packing blocker, but something is
    // plainly stopping it. Saying "nothing is blocking" there would send an
    // operator looking for a workload problem that does not exist.
    const floorBound = pools.filter((p) => p.status === "at_floor");
    if (floorBound.length > 0) {
      empty.textContent = `Nothing in the workload. ${floorBound.map((p) => p.pool).join(", ")} ${floorBound.length === 1 ? "is" : "are"} held by the configured node floor, not by anything scheduled on ${floorBound.length === 1 ? "it" : "them"}.`;
    } else {
      empty.textContent = "Nothing is blocking further consolidation.";
    }
    els.overviewBlockers.append(empty);
    return;
  }

  list.forEach((blocker) => {
    const item = document.createElement("li");
    item.className = "blocker";
    const label = BLOCKER_LABELS[blocker.reason] || blocker.reason;
    const examples = blocker.examples.length
      ? `<small>${escapeHtml(blocker.examples.join(", "))}</small>`
      : "";
    item.innerHTML = `
      <span class="blocker-label">${escapeHtml(label)}</span>
      <span class="blocker-scope">${escapeHtml(blocker.pools.join(", "))}</span>
      <span class="blocker-count">${formatNumber(blocker.pods)} pod${blocker.pods === 1 ? "" : "s"}</span>
      ${examples}
    `;
    els.overviewBlockers.append(item);
  });
}

function setVerdict(label, value, detail, tone) {
  els.overviewVerdict.className = `overview-verdict ${tone}`;
  els.overviewVerdict.innerHTML = `
    <span>${escapeHtml(label)}</span>
    <strong>${escapeHtml(value)}</strong>
    <p>${escapeHtml(detail)}</p>
  `;
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

function renderEngineStatus() {
  const status = state.data?.engine_status || null;
  const mode = engineMode(status);
  els.enginePillMode.classList.remove("live", "dry-run", "partial", "disabled");
  els.enginePillMode.classList.add(mode.toneClass);
  els.enginePillModeText.textContent = mode.label;
  els.enginePillModeDetail.textContent = mode.detail;
  els.enginePillMode.setAttribute("title", mode.tooltip);
  els.enginePillMode.setAttribute("aria-label", `Mode: ${mode.label}. ${mode.detail}`);

  const halt = Boolean(status?.halt_active);
  els.enginePillHalt.classList.toggle("active", halt);
  els.enginePillHaltText.textContent = halt ? "HALTED" : "Inactive";
  els.enginePillHalt.title = halt
    ? `Cluster ConfigMap cluster-optimizer/cluster-optimizer-halt is set: ${status?.halt_reason || "halt=true"}`
    : "No halt ConfigMap value detected on the last run.";

  renderHaltAction(halt);

  // Whether the Last Run tile becomes a clickable jump-to-activity affordance.
  // Engine has run at least once → it's actionable; otherwise plain text.
  const hasHistory = Boolean(status?.last_run_at);
  els.enginePillLastRun.classList.toggle("actionable", hasHistory);
  if (hasHistory) {
    els.enginePillLastRun.setAttribute("role", "button");
    els.enginePillLastRun.setAttribute("tabindex", "0");
    els.enginePillLastRun.setAttribute("aria-label", "View recent optimization activity");
  } else {
    els.enginePillLastRun.removeAttribute("role");
    els.enginePillLastRun.removeAttribute("tabindex");
    els.enginePillLastRun.removeAttribute("aria-label");
  }

  if (!status || !status.last_run_at) {
    els.enginePillLastRun.classList.remove("errors");
    els.enginePillLastRunText.textContent = "Never";
    els.enginePillLastRunDetail.textContent = "No remediation history yet";
  } else {
    const actions = Number(status.last_run_actions || 0);
    const errors = Number(status.last_run_errors || 0);
    const applied = Number(status.last_run_applied || 0);
    els.enginePillLastRunText.textContent = formatRelative(status.last_run_at);
    const parts = [`${actions} action${actions === 1 ? "" : "s"}`];
    if (mode.live && applied > 0) parts.push(`${applied} applied`);
    if (errors > 0) parts.push(`${errors} error${errors === 1 ? "" : "s"}`);
    parts.push("view activity");
    els.enginePillLastRunDetail.textContent = parts.join(" · ");
    els.enginePillLastRun.classList.toggle("errors", errors > 0);
  }

  els.engineBanner.classList.toggle("hidden", !halt);
  if (halt) {
    els.engineBanner.textContent = `Active remediation is halted: ${status?.halt_reason || "cluster-optimizer/cluster-optimizer-halt ConfigMap is set to halt=true."}`;
  }
  renderEnginePopover(status);
}

function renderHaltAction(halted) {
  const btn = els.enginePillHaltAction;
  const help = els.enginePillHaltHelp;
  const kubeAvailable = state.data?.halt_control_available !== false;

  btn.hidden = false;
  btn.classList.remove("danger", "recover", "busy", "disabled");

  if (state.haltPosting) {
    btn.classList.add("busy");
    btn.disabled = true;
    btn.textContent = halted ? "Deactivating…" : "Activating…";
    btn.setAttribute("aria-busy", "true");
    help.textContent = "";
    help.classList.remove("error");
    return;
  }

  btn.setAttribute("aria-busy", "false");

  if (!kubeAvailable) {
    btn.classList.add("danger", "disabled");
    btn.disabled = true;
    btn.textContent = halted ? "Deactivate" : "Activate halt";
    help.textContent = "Set KUBECONFIG or run the UI on a host with ~/.kube/config to enable halt control.";
    help.classList.remove("error");
    return;
  }

  btn.disabled = false;
  if (halted) {
    btn.classList.add("recover");
    btn.textContent = "Deactivate";
  } else {
    btn.classList.add("danger");
    btn.textContent = "Activate halt";
  }

  if (state.haltError) {
    help.textContent = state.haltError;
    help.classList.add("error");
  } else {
    help.textContent = "";
    help.classList.remove("error");
  }
}

function scrollToActivityPanel() {
  // Force-expand the panel so the click lands on something visible, then scroll.
  state.activityCollapsed = false;
  state.activityCollapsedByUser = false;
  applyActivityCollapse();
  els.activityPanel.scrollIntoView({ behavior: "smooth", block: "start" });
  // Brief attention pulse on the panel header so the eye lands.
  const head = els.activityPanel.querySelector(".panel-head");
  if (head) {
    head.classList.add("flash");
    setTimeout(() => head.classList.remove("flash"), 1500);
  }
}

async function postHalt(active) {
  if (state.haltPosting) return;
  state.haltPosting = true;
  state.haltError = "";
  renderEngineStatus();
  try {
    const response = await fetch("/api/halt", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ active, confirm: true })
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(payload.error || `Request failed with ${response.status}`);
    }
    // Optimistic update so the UI reflects the new state immediately;
    // the next /api/reports poll will refresh with authoritative data.
    if (state.data?.engine_status) {
      state.data.engine_status.halt_active = Boolean(active);
    }
  } catch (error) {
    state.haltError = error.message || String(error);
  } finally {
    state.haltPosting = false;
    renderEngineStatus();
    // Refresh authoritative data so engine_status reflects the cluster
    // truth (the cron loop won't update for ~30m otherwise).
    if (!state.haltError) {
      loadReports({ preserveSelection: true });
    }
    // Auto-clear errors after a few seconds so the pill doesn't shout forever.
    if (state.haltError) {
      setTimeout(() => {
        if (state.haltError) {
          state.haltError = "";
          renderEngineStatus();
        }
      }, 10000);
    }
  }
}

function showHaltConfirm() {
  // Re-uses the existing .modal-overlay / .modal-card primitives from
  // showInstructionsModal; the .danger variant styles the confirm button red.
  const previouslyFocused = document.activeElement;
  const overlay = document.createElement("div");
  overlay.className = "modal-overlay";
  overlay.setAttribute("role", "dialog");
  overlay.setAttribute("aria-modal", "true");
  overlay.setAttribute("aria-labelledby", "haltConfirmTitle");
  overlay.setAttribute("aria-describedby", "haltConfirmBody");

  const card = document.createElement("div");
  card.className = "modal-card";

  const h2 = document.createElement("h2");
  h2.id = "haltConfirmTitle";
  h2.textContent = "Activate halt switch?";

  const p = document.createElement("p");
  p.id = "haltConfirmBody";
  p.textContent = "This pauses auto-apply and live nudge across the cluster immediately. Work already in flight continues; the next scheduled run will refuse to act.";

  const ul = document.createElement("ul");
  ul.className = "modal-bullets";
  ul.innerHTML = `
    <li>Creates ConfigMap <code>cluster-optimizer/cluster-optimizer-halt</code> with <code>halt=true</code>.</li>
    <li>Reversible from this dashboard or via <code>kubectl delete configmap</code>.</li>
  `;

  const actions = document.createElement("div");
  actions.className = "modal-actions";

  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.className = "modal-button cancel";
  cancel.textContent = "Cancel";

  const confirm = document.createElement("button");
  confirm.type = "button";
  confirm.className = "modal-button danger";
  confirm.textContent = "Halt now";

  actions.append(cancel, confirm);
  card.append(h2, p, ul, actions);
  overlay.append(card);
  document.body.append(overlay);

  // Focus management: initial focus on Cancel (safe default; defends
  // against accidental Enter). Esc and backdrop click cancel. Focus trap
  // cycles Tab/Shift+Tab between the two buttons. On close (any path)
  // focus returns to the halt action button.
  const closeAll = () => {
    overlay.removeEventListener("keydown", trapKey);
    overlay.remove();
    if (previouslyFocused && typeof previouslyFocused.focus === "function") {
      previouslyFocused.focus();
    } else {
      els.enginePillHaltAction.focus();
    }
  };
  const trapKey = (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      closeAll();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = [cancel, confirm];
    const active = document.activeElement;
    const idx = focusable.indexOf(active);
    if (event.shiftKey) {
      event.preventDefault();
      const next = idx <= 0 ? focusable[focusable.length - 1] : focusable[idx - 1];
      next.focus();
    } else {
      event.preventDefault();
      const next = idx === focusable.length - 1 ? focusable[0] : focusable[idx + 1];
      next.focus();
    }
  };
  overlay.addEventListener("keydown", trapKey);
  overlay.addEventListener("click", (event) => {
    if (event.target === overlay) closeAll();
  });
  cancel.addEventListener("click", closeAll);
  confirm.addEventListener("click", () => {
    closeAll();
    postHalt(true);
  });

  // Defer focus to the next frame so screen readers announce the dialog.
  requestAnimationFrame(() => cancel.focus());
}

// The three independently gated remediation paths. Each mutates the cluster
// only when its CLI flag and its env gate are both set, so a status where one
// pair is complete and another is not is a genuine mixed posture — not "Live".
const REMEDIATION_PATHS = [
  {
    key: "auto-apply",
    label: "auto-apply",
    effect: "patches workload requests",
    flagName: "--auto-apply",
    envName: "CLUSTER_OPTIMIZER_AUTOAPPLY",
    advisory: "reports right-sizing findings without patching",
    enabled: (s) => Boolean(s?.auto_apply_enabled),
    live: (s) => Boolean(s?.auto_apply_live)
  },
  {
    key: "nudge",
    label: "nudge",
    effect: "cordons and evicts to consolidate",
    flagName: "--nudge",
    envName: "CLUSTER_OPTIMIZER_NUDGE_LIVE",
    advisory: "logs the consolidation plan without cordoning or evicting",
    enabled: (s) => Boolean(s?.nudge_enabled),
    live: (s) => Boolean(s?.nudge_live)
  },
  {
    key: "gc",
    label: "pod GC",
    effect: "deletes completed pods",
    flagName: "--gc-completed-pods",
    envName: "CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE",
    advisory: "logs which completed pods it would delete",
    enabled: (s) => Boolean(s?.gc_enabled),
    live: (s) => Boolean(s?.gc_live)
  }
];

// Splits the deployed paths into the ones that mutate and the ones that only
// report. A path absent from the CronJob args is neither: it is not deployed,
// so counting it as "not enabled" would demand a fix that no deploy flag makes.
function remediationCoverage(status) {
  const deployed = REMEDIATION_PATHS.filter((p) => p.enabled(status));
  const live = deployed.filter((p) => p.live(status));
  const advisory = deployed.filter((p) => !p.live(status));
  return { deployed, live, advisory };
}

function engineMode(status) {
  if (!status) {
    return { label: "Unknown", detail: "", toneClass: "disabled", live: false, tooltip: "No engine status reported yet." };
  }
  const { deployed, live, advisory } = remediationCoverage(status);
  if (!deployed.length) {
    return {
      label: "Disabled",
      detail: "No remediation path is deployed",
      toneClass: "disabled",
      live: false,
      tooltip: "The CronJob passes none of --auto-apply, --nudge or --gc-completed-pods."
    };
  }
  if (status.halt_active) {
    return {
      label: "Halted",
      detail: `${deployed.length} path${deployed.length === 1 ? "" : "s"} paused by the halt ConfigMap`,
      toneClass: "disabled",
      live: false,
      tooltip: "Halt ConfigMap is set; every remediation path is paused."
    };
  }
  const names = (paths) => paths.map((p) => p.label).join(", ");
  if (!live.length) {
    return {
      label: "Dry-run",
      detail: `${names(advisory)} report only`,
      toneClass: "dry-run",
      live: false,
      tooltip: `Deployed but advisory: ${names(advisory)}. Each is missing its env gate, so nothing is mutated.`
    };
  }
  if (!advisory.length) {
    return {
      label: "Live",
      detail: `All ${live.length} path${live.length === 1 ? "" : "s"} mutate: ${names(live)}`,
      toneClass: "live",
      live: true,
      tooltip: `Every deployed path has both gates set: ${names(live)}. This cluster mutates workloads.`
    };
  }
  // The case the flat "Live" label used to swallow. Say which paths mutate and
  // which only report, in the label itself — an operator reading "Live" should
  // never have to open the popover to learn a corrective action is still off.
  return {
    label: `Partly live · ${live.length} of ${deployed.length}`,
    detail: `${names(live)} mutate · ${names(advisory)} report only`,
    toneClass: "partial",
    live: true,
    tooltip: `Live: ${names(live)}. Not enabled: ${names(advisory)} — deployed but missing the env gate, so ${advisory.length === 1 ? "it reports" : "they report"} without acting.`
  };
}

function gateLine(ok, text) {
  return `<span class="${ok ? "gate-ok" : "gate-off"}">${ok ? "✓" : "✗"} ${escapeHtml(text)}</span>`;
}

function renderEnginePopover(status) {
  els.engineModeDetails.replaceChildren();

  REMEDIATION_PATHS.forEach((path) => {
    const enabled = path.enabled(status);
    const live = path.live(status);
    const halted = Boolean(status?.halt_active);

    let verdict = "Live";
    let verdictClass = "live";
    if (!enabled) {
      verdict = "Not deployed";
      verdictClass = "off";
    } else if (halted) {
      verdict = "Halted";
      verdictClass = "off";
    } else if (!live) {
      verdict = "Reports only";
      verdictClass = "advisory";
    }

    // *Live is the AND of both gates, so when the flag is absent the env
    // value is unknown here. Claiming it is "not true" would be a guess, and
    // the flag alone already decides the outcome.
    const envLine = !enabled
      ? gateLine(false, `${path.envName} not evaluated — the flag decides this path`)
      : gateLine(live, live ? `${path.envName}=true` : `${path.envName} is not true`);

    const li = document.createElement("li");
    li.className = "engine-path";
    li.innerHTML = `
      <span class="engine-path-head">
        <b>${escapeHtml(path.label)}</b>
        <span class="engine-path-verdict ${verdictClass}">${escapeHtml(verdict)}</span>
      </span>
      <span class="engine-path-effect">${escapeHtml(live && !halted ? path.effect : path.advisory)}</span>
      <span class="engine-path-gates">
        ${gateLine(enabled, enabled ? `${path.flagName} is passed` : `${path.flagName} not set on the CronJob`)}
        ${envLine}
      </span>
    `;
    els.engineModeDetails.append(li);
  });

  const haltLi = document.createElement("li");
  haltLi.className = "engine-path";
  haltLi.innerHTML = `
    <span class="engine-path-head">
      <b>halt ConfigMap</b>
      <span class="engine-path-verdict ${status?.halt_active ? "off" : "live"}">${status?.halt_active ? "Halted" : "Clear"}</span>
    </span>
    <span class="engine-path-gates">
      ${gateLine(!status?.halt_active, status?.halt_active
        ? `halted: ${status?.halt_reason || "halt=true"}`
        : "cluster-optimizer/cluster-optimizer-halt is absent or not set to halt=true")}
    </span>
  `;
  els.engineModeDetails.append(haltLi);

  renderEngineModeVerdict(status);
}

// The one sentence an operator needs before acting: how many corrective paths
// actually run, and what the rest are doing instead.
function renderEngineModeVerdict(status) {
  const el = els.engineModeVerdict;
  if (!status) {
    el.textContent = "No engine status reported yet.";
    el.className = "engine-mode-verdict";
    return;
  }
  const { deployed, live, advisory } = remediationCoverage(status);
  const names = (paths) => paths.map((p) => p.label).join(", ");

  if (status.halt_active) {
    el.textContent = `Halted: the halt ConfigMap overrides every gate, so none of the ${deployed.length} deployed path(s) will act.`;
    el.className = "engine-mode-verdict warn";
    return;
  }
  if (!deployed.length) {
    el.textContent = "No remediation path is deployed on this CronJob, so nothing will be corrected.";
    el.className = "engine-mode-verdict warn";
    return;
  }
  if (!live.length) {
    el.textContent = `Nothing is corrected: ${names(advisory)} are deployed but each is missing its env gate.`;
    el.className = "engine-mode-verdict warn";
    return;
  }
  if (!advisory.length) {
    el.textContent = `All ${live.length} deployed path(s) correct: ${names(live)}.`;
    el.className = "engine-mode-verdict ok";
    return;
  }
  el.textContent = `${live.length} of ${deployed.length} paths correct: ${names(live)}. Not enabled: ${names(advisory)} — ${advisory.map((p) => p.advisory).join("; ")}.`;
  el.className = "engine-mode-verdict warn";
}

function toggleEngineModePopover(open) {
  els.engineModePopover.classList.toggle("hidden", !open);
  els.enginePillMode.setAttribute("aria-expanded", String(open));
}

function applyActivityAutoCollapse() {
  // Kept as a no-op for backwards compatibility with existing callers.
  // The panel now stays expanded by default so the operator can see the
  // empty-state messaging (which explains *why* the feed is empty);
  // user-initiated collapse via the toggle button still works.
  if (state.activityCollapsedByUser) {
    applyActivityCollapse();
  }
}

function applyActivityCollapse() {
  if (state.activityCollapsed) {
    els.activityPanel.classList.add("collapsed");
    els.activityToggle.textContent = "Expand";
    els.activityToggle.setAttribute("aria-expanded", "false");
  } else {
    els.activityPanel.classList.remove("collapsed");
    els.activityToggle.textContent = "Collapse";
    els.activityToggle.setAttribute("aria-expanded", "true");
  }
}

function renderActivity() {
  els.activityList.replaceChildren();
  const halt = Boolean(state.data?.engine_status?.halt_active);
  els.activityPanel.classList.toggle("halted", halt);
  applyActivityCollapse();
  renderActivityLive();
  syncActivityStickyTop();

  if (state.activity.loading) {
    appendActivityEmpty("Loading recent activity…");
    return;
  }
  if (state.activity.error) {
    appendActivityEmpty(`Could not load remediation history: ${state.activity.error}`);
    return;
  }

  const allEvents = state.activity.events || [];
  const events = filterActivity(allEvents);

  if (events.length === 0) {
    if (allEvents.length === 0) {
      appendActivityEmpty(emptyStateMessage(state.data?.engine_status));
    } else {
      appendActivityEmpty("No remediation activity matches this filter.");
    }
    return;
  }

  // Group events by run. Events written in the same CronJob tick share a
  // timestamp at second-level precision (RFC3339); the applier, nudger,
  // and skipper all use status.LastRunAt. We cluster events whose
  // timestamps fall within a 30-second window into a single run group so
  // that any small skew between the three writers stays in the same group.
  const groups = groupEventsByRun(events);
  let lastDayKey = null;
  groups.forEach((group, idx) => {
    const dayKey = dayKeyFor(group.anchorTs);
    if (dayKey !== lastDayKey) {
      els.activityList.append(dateDivider(group.anchorTs));
      lastDayKey = dayKey;
    }

    const wrap = document.createElement("div");
    wrap.className = "activity-run";
    wrap.dataset.runIndex = String(idx);
    wrap.append(runDivider(group));

    const { active, skips } = partitionGroupEvents(group.events);
    const dedupedActive = dedupeActive(active);
    dedupedActive.forEach((bundle) => wrap.append(activityRow(bundle)));

    if (skips.length > 0) {
      if (state.activitySkipsInline || dedupedActive.length === 0) {
        skips.forEach((event) => wrap.append(activityRow({ event, kindBundle: false })));
      } else {
        wrap.append(skipsDisclosure(group, skips));
      }
    }
    els.activityList.append(wrap);
  });
}

function renderActivityLive() {
  const loadedAt = state.activityLoadedAt;
  if (!loadedAt) {
    els.activityLive.hidden = true;
    return;
  }
  els.activityLive.hidden = false;
  const deltaSec = Math.round((Date.now() - loadedAt) / 1000);
  els.activityLiveText.textContent = deltaSec < 5
    ? "just now"
    : formatRelative(new Date(loadedAt).toISOString());
}

function syncActivityStickyTop() {
  // Sticky run dividers need to clear the page-level topbar (which is
  // itself position:sticky and varies in height with viewport width).
  // Hardcoding `top` was wrong on mobile where the topbar wraps and is
  // ~3x taller. Measure it once per render and once per resize.
  const topbar = document.querySelector(".topbar");
  if (!topbar || !els.activityPanel) return;
  const rect = topbar.getBoundingClientRect();
  els.activityPanel.style.setProperty("--activity-sticky-top", `${Math.ceil(rect.height)}px`);
}
window.addEventListener("resize", syncActivityStickyTop);

function dayKeyFor(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

function dateDivider(ts) {
  const wrap = document.createElement("div");
  wrap.className = "activity-date-divider";
  const d = ts ? new Date(ts) : new Date();
  const now = new Date();
  const today = dayKeyFor(now.toISOString());
  const yesterday = (() => {
    const y = new Date(now);
    y.setDate(y.getDate() - 1);
    return dayKeyFor(y.toISOString());
  })();
  const key = dayKeyFor(ts || now.toISOString());
  let label;
  let showDate = false;
  if (key === today) { label = "Today"; showDate = true; }
  else if (key === yesterday) { label = "Yesterday"; showDate = true; }
  else label = d.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric", year: "numeric" });
  const dateAttr = ts || now.toISOString();
  wrap.innerHTML = `<span>${escapeHtml(label)}</span>${
    showDate
      ? `<time datetime="${escapeHtml(dateAttr)}">${escapeHtml(d.toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" }))}</time>`
      : ""
  }`;
  return wrap;
}

// Events whose subject is a node rather than a workload. They share a scope
// layout and a summary chip, but say opposite things: cordon_evict takes a
// node out of service, uncordon_stale puts one back.
function isNodeEvent(event) {
  return event.kind === "cordon_evict" || event.kind === "uncordon_stale";
}

function partitionGroupEvents(events) {
  const active = [];
  const skips = [];
  events.forEach((event) => {
    if (event.kind === "skip") {
      skips.push(event);
    } else {
      active.push(event);
    }
  });
  // Sort active so highest-signal items surface first within a group.
  const weight = (event) => {
    if (event.halt_active) return 0;
    if (event.error || event.eviction_errors > 0) return 1;
    if (event.applied) return 2;
    if (isNodeEvent(event)) return 3;
    if (event.mode === "dry-run") return 4;
    return 5;
  };
  active.sort((a, b) => weight(a) - weight(b));
  return { active, skips };
}

function dedupeActive(events) {
  // Two events for the same workload (cpu + memory) collapse into a single
  // row carrying both rule chips and a combined change summary.
  const bundles = [];
  const byKey = new Map();
  events.forEach((event) => {
    if (event.kind !== "patch_request" && event.kind !== "nudge") {
      bundles.push({ event, extras: [] });
      return;
    }
    const key = [
      event.kind,
      event.namespace || "",
      event.workload || "",
      event.container || "",
      event.mode || "",
      event.applied ? "1" : "0",
      event.error ? "e" : "",
      event.halt_active ? "h" : ""
    ].join("|");
    const existing = byKey.get(key);
    if (existing) {
      existing.extras.push(event);
    } else {
      const bundle = { event, extras: [] };
      byKey.set(key, bundle);
      bundles.push(bundle);
    }
  });
  return bundles;
}

function skipsDisclosure(group, skips) {
  const wrap = document.createElement("div");
  wrap.className = "activity-skip-disclosure";
  const expanded = Boolean(state.activitySkipsExpanded[group.anchorTs]);
  if (expanded) wrap.classList.add("expanded");

  const clusters = clusterSkipReasons(skips);
  const summaryBits = clusters.map((c) => `<b>${c.count}</b> ${escapeHtml(c.label.toLowerCase())}`).join(" · ");

  const head = document.createElement("button");
  head.type = "button";
  head.className = "activity-skip-head";
  head.setAttribute("aria-expanded", expanded ? "true" : "false");
  head.innerHTML = `
    <span class="caret" aria-hidden="true">▸</span>
    <span class="count"><b>${skips.length}</b> skipped</span>
    <span class="reasons">${summaryBits}</span>
  `;
  head.addEventListener("click", () => {
    state.activitySkipsExpanded[group.anchorTs] = !state.activitySkipsExpanded[group.anchorTs];
    renderActivity();
  });
  wrap.append(head);

  if (expanded) {
    const body = document.createElement("div");
    body.className = "activity-skip-body";
    skips.forEach((event) => body.append(activityRow({ event, extras: [] })));
    wrap.append(body);
  }
  return wrap;
}

function clusterSkipReasons(skips) {
  const buckets = new Map();
  skips.forEach((event) => {
    const r = humanizeReason(event.reason || "Skipped");
    const key = r.key;
    if (!buckets.has(key)) {
      buckets.set(key, { label: r.label, count: 0 });
    }
    buckets.get(key).count++;
  });
  return [...buckets.values()].sort((a, b) => b.count - a.count);
}

function humanizeReason(raw) {
  // help is set only for skip reasons the user can actually act on — and the
  // link text names the action ("Configure target", "Enable persistence")
  // rather than the generic "why?". Reasons that are intentional engine
  // guardrails (low confidence, provider-managed, no safe trim) or just need
  // time (needs more runs, not in snapshot) get no link; the label + tooltip
  // already say everything the user can do about them.
  const r = String(raw || "").toLowerCase();
  if (r.includes("confidence") && r.includes("below minimum")) {
    return { key: "low-confidence", label: "Low confidence", detail: "Finding hasn't reached the configured minimum confidence threshold.", help: null, helpText: null };
  }
  if (r.includes("no remediation target")) {
    return { key: "no-target", label: "No remediation target", detail: "This rule isn't enabled for the workload in config/remediation-targets.json.", help: helpLink("remediation-workflow"), helpText: "Configure target →" };
  }
  if (r.includes("provider-managed")) {
    return { key: "provider-managed", label: "Provider-managed", detail: "Workload is managed by the cloud provider and intentionally excluded.", help: null, helpText: null };
  }
  if (r.includes("missing container")) {
    return { key: "missing-container", label: "Missing container name", detail: "The remediation target entry needs an explicit container name.", help: helpLink("remediation-workflow"), helpText: "Configure target →" };
  }
  if (r.includes("seen") && r.includes("need")) {
    return { key: "min-occurrences", label: "Needs more runs", detail: raw, help: null, helpText: null };
  }
  if (r.includes("persistence")) {
    return { key: "persistence", label: "Persistence not configured", detail: raw, help: helpLink("dynamodb-persistence"), helpText: "Enable persistence →" };
  }
  if (r.includes("not found in snapshot")) {
    return { key: "no-snapshot", label: "Not in snapshot", detail: raw, help: null, helpText: null };
  }
  if (r.includes("no safe trim")) {
    return { key: "no-trim", label: "No safe trim available", detail: "Trim would breach the 50% max-trim cap or 10m / 32Mi floor.", help: null, helpText: null };
  }
  return { key: r || "skipped", label: raw || "Skipped", detail: "", help: null, helpText: null };
}

function helpLink(anchor) {
  return `https://github.com/GipsyChef/cluster-optimizer/blob/main/README.md#${anchor}`;
}

function appendActivityEmpty(message) {
  const div = document.createElement("div");
  div.className = "activity-empty";
  if (message instanceof Node) {
    div.append(message);
  } else {
    div.textContent = message;
  }
  els.activityList.append(div);
}

function emptyStateMessage(status) {
  if (!status) {
    return "No remediation activity yet. Run the CronJob at least once to populate this feed.";
  }
  if (status.halt_active) {
    return "Halt switch is active. No new actions will be recorded until it's deactivated.";
  }
  if (!status.auto_apply_enabled && !status.nudge_enabled) {
    return "No remediation activity. The engine is in advisory-only mode — set --auto-apply or --nudge on the CronJob to start recording actions here.";
  }
  const live = status.auto_apply_live || status.nudge_live;
  if (live) {
    const since = status.last_run_at ? formatRelative(status.last_run_at) : "recently";
    return `No actions in the last 7 days. Engine has been live (last run ${since}); the applier waits for the same finding to appear in 3 consecutive runs before patching.`;
  }
  return "No remediation activity. The engine is enabled but the matching env-var gate is missing, so it's stuck in dry-run.";
}

function groupEventsByRun(events) {
  // Events are sorted newest-first by the API. Cluster adjacent events
  // whose timestamps are within RUN_CLUSTER_MS of the group's anchor
  // timestamp into the same run.
  const RUN_CLUSTER_MS = 30 * 1000;
  const groups = [];
  let current = null;
  events.forEach((event) => {
    const ts = event.timestamp ? new Date(event.timestamp).getTime() : 0;
    if (!current || Math.abs((current.anchorMs - ts)) > RUN_CLUSTER_MS) {
      current = { anchorMs: ts, anchorTs: event.timestamp, events: [] };
      groups.push(current);
    }
    current.events.push(event);
  });
  return groups;
}

function runDivider(group) {
  const header = document.createElement("header");
  header.className = "activity-run-divider";

  const time = document.createElement("time");
  time.dateTime = group.anchorTs || "";
  time.textContent = group.anchorTs ? new Date(group.anchorTs).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : "—";
  time.title = group.anchorTs ? new Date(group.anchorTs).toLocaleString() : "";

  const rel = document.createElement("span");
  rel.className = "rel";
  rel.textContent = formatRelative(group.anchorTs);

  const chips = document.createElement("span");
  chips.className = "chips";
  const c = summarizeRun(group.events);
  const segments = [];
  if (c.applied > 0) segments.push(chipHTML("applied", c.applied, c.applied === 1 ? "applied" : "applied"));
  if (c.dry > 0) segments.push(chipHTML("dry", c.dry, "dry-run"));
  if (c.cordons > 0) segments.push(chipHTML("node", c.cordons, c.cordons === 1 ? "node action" : "node actions"));
  if (c.errors > 0) segments.push(chipHTML("error", c.errors, c.errors === 1 ? "error" : "errors"));
  if (segments.length === 0 && c.skips > 0) {
    segments.push(chipHTML("idle", c.skips, "no changes"));
  } else if (c.skips > 0) {
    segments.push(chipHTML("skip", c.skips, c.skips === 1 ? "skipped" : "skipped"));
  }
  chips.innerHTML = segments.join("");

  header.append(time, rel, chips);
  return header;
}

function chipHTML(kind, value, label) {
  return `<span class="chip chip-${kind}"><b>${value}</b>${escapeHtml(label)}</span>`;
}

function summarizeRun(events) {
  let applied = 0, dry = 0, cordons = 0, skips = 0, errors = 0;
  events.forEach((event) => {
    if (event.error || event.eviction_errors > 0) { errors++; return; }
    if (event.kind === "skip") { skips++; return; }
    if (isNodeEvent(event)) { cordons++; return; }
    if (event.applied) applied++;
    else if (event.mode === "dry-run") dry++;
  });
  return { applied, dry, cordons, skips, errors };
}

function filterActivity(events) {
  switch (state.activityFilter) {
    case "dry-run":
      return events.filter((event) => event.mode === "dry-run");
    case "errors":
      return events.filter((event) => event.error || event.eviction_errors > 0);
    default:
      return events;
  }
}

function activityRow(input) {
  const bundle = input && input.event ? input : { event: input, extras: [] };
  const event = bundle.event;
  const extras = bundle.extras || [];
  const article = document.createElement("article");
  let toneClass;
  if (event.error || event.eviction_errors > 0) {
    toneClass = "error";
  } else if (event.kind === "skip") {
    toneClass = "skip";
  } else if (event.applied) {
    toneClass = "applied";
  } else {
    toneClass = event.mode || "dry-run";
  }
  article.className = `activity-event ${toneClass}`;

  const time = document.createElement("time");
  time.dateTime = event.timestamp || "";
  time.textContent = formatRelative(event.timestamp);
  time.title = event.timestamp ? new Date(event.timestamp).toLocaleString() : "";

  const scope = document.createElement("div");
  scope.className = "scope";
  if (isNodeEvent(event)) {
    const fallback = event.kind === "uncordon_stale" ? "stale-cordon sweep" : "consolidation pass";
    const target = event.target_node ? `node ${event.target_node}` : fallback;
    const chip = event.kind === "uncordon_stale" ? "returned to service" : "node action";
    scope.innerHTML = `<strong>${escapeHtml(target)}</strong><code>${chip}</code>`;
  } else {
    const scopeText = [event.namespace, event.workload].filter(Boolean).join("/") || "cluster";
    const container = event.container ? ` · container ${escapeHtml(event.container)}` : "";
    const ruleEvents = [event, ...extras].filter((e) => e.rule_id);
    const ruleChips = ruleEvents
      .map((e) => `<code class="rule-link" data-rule="${escapeHtml(e.rule_id)}" data-namespace="${escapeHtml(e.namespace || "")}" data-workload="${escapeHtml(e.workload || "")}" title="Jump to this finding">${escapeHtml(e.rule_id)}</code>`)
      .join("");
    scope.innerHTML = `<strong>${escapeHtml(scopeText)}${container}</strong>${ruleChips}`;
  }
  scope.querySelectorAll(".rule-link").forEach((link) => {
    link.addEventListener("click", () => jumpToFinding(link.dataset.rule, link.dataset.namespace, link.dataset.workload));
  });

  const change = document.createElement("div");
  change.className = "change";
  if (event.kind === "skip") {
    change.innerHTML = skipChangeSummary(event);
    const help = change.querySelector(".reason-help");
    if (help) help.addEventListener("click", (e) => e.stopPropagation());
  } else {
    const summaries = [event, ...extras].map(changeSummary).filter(Boolean);
    change.innerHTML = summaries.join(" · ");
  }

  const badge = document.createElement("span");
  badge.className = "status-badge";
  if (event.halt_active) {
    badge.classList.add("halted");
    badge.textContent = "Halted";
  } else if (event.error || event.eviction_errors > 0) {
    badge.classList.add("errored");
    badge.textContent = "Error";
  } else if (event.applied) {
    badge.classList.add("applied");
    badge.textContent = "Applied";
  } else if (event.mode === "dry-run") {
    badge.classList.add("dry");
    badge.textContent = "Dry-run";
  } else if (event.kind === "skip") {
    badge.classList.add("skipped");
    badge.textContent = "Skipped";
  } else {
    badge.textContent = event.reason ? "Skipped" : "Reported";
  }
  if (event.kind === "patch_request" && event.applied && !findingStillActive(event)) {
    const resolved = document.createElement("span");
    resolved.className = "status-badge resolved";
    resolved.textContent = "Resolved";
    resolved.title = "This finding is no longer in the latest report.";
    article.append(time, scope, change, badge, resolved);
  } else {
    article.append(time, scope, change, badge);
  }

  if (event.error) {
    const details = document.createElement("details");
    const summary = document.createElement("summary");
    summary.textContent = "Show error";
    const pre = document.createElement("pre");
    pre.textContent = event.error;
    details.append(summary, pre);
    article.append(details);
  }
  return article;
}

function skipChangeSummary(event) {
  const r = humanizeReason(event.reason || "Skipped");
  const help = r.help
    ? ` <a class="reason-help" href="${escapeHtml(r.help)}" target="_blank" rel="noopener" title="${escapeHtml(r.detail || "")}">${escapeHtml(r.helpText || "Details →")}</a>`
    : "";
  return `<span class="reason-label" title="${escapeHtml(r.detail || event.reason || "")}">${escapeHtml(r.label)}</span>${help}`;
}

function changeSummary(event) {
  if (event.kind === "uncordon_stale") {
    // The reaper's rows are the inverse of a consolidation: capacity coming
    // back, not going away. A run of these means drained nodes are never
    // being removed, so say that rather than showing a bare dash.
    if (event.error) return escapeHtml(event.error);
    return `<b>uncordoned</b> · ${escapeHtml(event.reason || "cordon had outlived its TTL")}`;
  }
  if (event.kind === "cordon_evict") {
    const parts = [];
    if (event.evicted > 0 || event.eviction_errors > 0) {
      parts.push(`evicted <b>${event.evicted}</b>${event.eviction_errors > 0 ? ` · <span class="delta-down">${event.eviction_errors} failed</span>` : ""}`);
    }
    if (parts.length === 0 && event.reason) {
      parts.push(escapeHtml(event.reason));
    }
    return parts.join(" · ") || "—";
  }
  const segments = [];
  if (event.before_cpu_m || event.after_cpu_m) {
    segments.push(`cpu <b>${event.before_cpu_m || 0}m → ${event.after_cpu_m || 0}m</b>${deltaPctSpan(event.before_cpu_m, event.after_cpu_m)}`);
  }
  if (event.before_memory_mib || event.after_memory_mib) {
    segments.push(`mem <b>${event.before_memory_mib || 0}Mi → ${event.after_memory_mib || 0}Mi</b>${deltaPctSpan(event.before_memory_mib, event.after_memory_mib)}`);
  }
  if (segments.length === 0) {
    return escapeHtml(event.reason || "no field changed");
  }
  return segments.join(" · ");
}

function deltaPctSpan(before, after) {
  const b = Number(before) || 0;
  const a = Number(after) || 0;
  if (b <= 0) return "";
  const pct = Math.round(((a - b) / b) * 100);
  if (pct === 0) return "";
  const cls = pct < 0 ? "delta-down" : "delta-up";
  const sign = pct > 0 ? "+" : "";
  return ` <span class="${cls}">${sign}${pct}%</span>`;
}

function findingStillActive(event) {
  const reports = state.data?.reports || [];
  const latest = reports[0];
  if (!latest) return true;
  const key = [event.rule_id || "", event.namespace || "", event.workload || ""].join("\u0000");
  return (latest.findings || []).some((finding) =>
    findingKey(finding) === key
  );
}

function jumpToFinding(rule, namespace, workload) {
  if (state.severity !== "all") {
    state.severity = "all";
    document.querySelectorAll("[data-severity]").forEach((item) => {
      item.classList.toggle("active", item.dataset.severity === "all");
    });
  }
  state.filter = "";
  els.filter.value = "";
  renderFindings();
  const cards = els.findingsList.querySelectorAll(".finding");
  for (const card of cards) {
    const heading = card.querySelector("h3")?.textContent || "";
    const code = card.querySelector("header code")?.textContent || "";
    const expectedScope = namespace && workload ? `${namespace}/${workload}` : (workload || "cluster");
    if (heading === expectedScope && code === rule) {
      card.scrollIntoView({ behavior: "smooth", block: "center" });
      card.classList.add("highlight");
      setTimeout(() => card.classList.remove("highlight"), 1800);
      return;
    }
  }
  // Fallback: scroll to findings panel.
  els.findingsList.scrollIntoView({ behavior: "smooth", block: "start" });
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
    const progress = rollupProgress(rollup, requiredDays);
    const statusLabel = rollupStatusLabel(rollup, requiredDays);
    const progressTitle = rollupProgressTitle(rollup, requiredDays);
    row.innerHTML = `
      <div>
        <strong>${escapeHtml(rollup.scope)}</strong>
        <span>${escapeHtml(rollup.rule_id)} · ${rollup.occurrences} report${rollup.occurrences === 1 ? "" : "s"}</span>
      </div>
      <div class="rollup-progress" title="${escapeHtml(progressTitle)}">
        <i style="width:${progress}%"></i>
      </div>
      <em>${escapeHtml(statusLabel)}</em>
    `;
    els.rollupList.append(row);
  });
}

function rollupProgress(rollup, requiredDays) {
  const required = Math.max(1, Number(requiredDays) || 1);
  const observed = Math.max(0, Number(rollup.observed_days) || 0);
  return Math.min(100, Math.round((observed / required) * 100));
}

function rollupStatusLabel(rollup, requiredDays) {
  if (!rollup.latest_report_has) {
    return "Resolved";
  }
  const required = Math.max(1, Number(requiredDays) || 1);
  const observed = Math.max(0, Number(rollup.observed_days) || 0);
  if (observed >= required) {
    return "Ready for review";
  }
  const remaining = required - observed;
  return `${remaining} day${remaining === 1 ? "" : "s"} left`;
}

function rollupProgressTitle(rollup, requiredDays) {
  if (!rollup.latest_report_has) {
    return "Resolved; this recommendation no longer appears in the latest report.";
  }
  const required = Math.max(1, Number(requiredDays) || 1);
  const observed = Math.max(0, Number(rollup.observed_days) || 0);
  if (observed >= required) {
    return `Observed for ${observed} day${observed === 1 ? "" : "s"}; ${required}-day review threshold met.`;
  }
  return `Observed for ${observed} of ${required} required day${required === 1 ? "" : "s"}.`;
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

function formatRelative(value) {
  if (!value) return "Never";
  const then = new Date(value).getTime();
  if (!Number.isFinite(then)) return "Never";
  const diffSec = Math.round((Date.now() - then) / 1000);
  if (diffSec < 0) return formatTime(value);
  if (diffSec < 60) return `${diffSec}s ago`;
  if (diffSec < 3600) return `${Math.round(diffSec / 60)}m ago`;
  if (diffSec < 86400) return `${Math.round(diffSec / 3600)}h ago`;
  if (diffSec < 86400 * 7) return `${Math.round(diffSec / 86400)}d ago`;
  return formatTime(value);
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
