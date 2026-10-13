const $ = (selector) => document.querySelector(selector);

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const raw = await response.text();
  let body = {};
  try { body = JSON.parse(raw); } catch { body = { error: raw }; }
  if (!response.ok) throw new Error(body.error || body.message || response.statusText);
  return body;
}

function reveal(view) {
  $("#onboarding").classList.toggle("hidden", view !== "onboarding");
  $("#dashboard").classList.toggle("hidden", view !== "dashboard");
  $("#top-section").textContent = view === "dashboard" ? "Overview" : "Getting started";
}

function showError(element, message) {
  element.textContent = message;
  element.classList.toggle("hidden", !message);
}

function renderSetupStages(snapshot) {
  const current = Number(snapshot.progressStep) || 0;
  const stagePercent = Math.max(0, Math.min(100, Number(snapshot.progressStagePercent) || 0));
  for (const item of document.querySelectorAll("#progress-stages [data-stage]")) {
    const stage = Number(item.dataset.stage);
    const done = Boolean(snapshot.setupComplete) || stage < current;
    const failed = Boolean(snapshot.error) && stage === current;
    const active = !snapshot.error && !snapshot.setupComplete && snapshot.setupRunning && stage === current;
    item.classList.toggle("is-complete", done);
    item.classList.toggle("is-active", active);
    item.classList.toggle("is-failed", failed);
    if (active) item.setAttribute("aria-current", "step");
    else item.removeAttribute("aria-current");

    const state = item.querySelector(".stage-state");
    state.textContent = done ? "Done" : failed ? "Failed" : active ? (stagePercent > 0 ? `${stagePercent}%` : "Running") : "Waiting";
  }
}

function render(snapshot) {
  if (!snapshot.setupComplete) {
    reveal("onboarding");
    $("#install-card").classList.toggle("hidden", !snapshot.profile.userName);
    $("#profile-form").classList.toggle("hidden", Boolean(snapshot.profile.userName));
    const retry = $("#install-button");
    retry.classList.toggle("hidden", !snapshot.profile.userName || snapshot.setupRunning || !snapshot.error);
    retry.disabled = snapshot.setupRunning;
    retry.innerHTML = "Retry setup <span>→</span>";
    showError($("#profile-error"), "");
    const progress = $("#setup-progress");
    const percent = Math.max(0, Math.min(100, Number(snapshot.progressPercent) || 0));
    progress.classList.toggle("hidden", !snapshot.profile.userName || (!snapshot.setupRunning && !snapshot.error && percent === 0));
    progress.classList.toggle("failed", Boolean(snapshot.error));
    progress.setAttribute("aria-busy", String(Boolean(snapshot.setupRunning)));
    $("#progress-step").textContent = snapshot.error ? "Setup stopped" : snapshot.step || "Preparing setup";
    $("#progress-count").textContent = snapshot.progressTotal ? `Step ${snapshot.progressStep || 0} of ${snapshot.progressTotal}` : "";
    $("#progress-fill").style.width = `${percent}%`;
    renderSetupStages(snapshot);
    const bar = progress.querySelector('[role="progressbar"]');
    bar.setAttribute("aria-valuenow", String(percent));
    bar.setAttribute("aria-valuetext", `${percent}% complete`);
    $("#progress-detail").textContent = snapshot.error || snapshot.progressDetail || (snapshot.setupRunning ? "Working in the background" : "");
    return;
  }

  reveal("dashboard");
  $("#welcome-name").textContent = snapshot.profile.userName || "there";
  $("#agent-title").textContent = snapshot.profile.agentName || "Sovereign AI";
  $("#persona-summary").textContent = `${snapshot.profile.agentName || "Sovereign AI"} knows how you like to work`;
  $("#purpose-summary").textContent = snapshot.profile.purpose || "Your assistant is ready for its first conversation.";
  $("#gateway-status").textContent = snapshot.gatewayRunning ? "Running" : "Stopped";
  $("#gateway-detail").textContent = snapshot.gatewayRunning ? "Running on this computer" : "Ready when you are";
  $("#gateway-dot").classList.toggle("running", snapshot.gatewayRunning);
  $("#start-button").disabled = snapshot.gatewayRunning;
  $("#stop-button").disabled = !snapshot.gatewayRunning;
  $("#overall-status").classList.toggle("online", snapshot.gatewayRunning);
  $("#overall-status b").textContent = snapshot.gatewayRunning ? "Assistant is awake" : "Assistant is asleep";
  $("#step-title").textContent = snapshot.step || "Ready";
  $("#activity-state").textContent = snapshot.setupRunning || snapshot.memoryRunning ? "In progress" : snapshot.error ? "Needs attention" : "Updated";
  $("#activity-state").classList.toggle("attention", Boolean(snapshot.error));
  showError($("#error-box"), snapshot.error || "");
  const logs = $("#log-list");
  logs.replaceChildren();
  if (!snapshot.recentLogs || snapshot.recentLogs.length === 0) {
    logs.innerHTML = '<div class="empty-log">Setup activity will appear here.</div>';
  } else {
    for (const line of snapshot.recentLogs.slice(-8).reverse()) {
      const row = document.createElement("div");
      row.className = "log-row";
      row.textContent = line;
      logs.append(row);
    }
  }
}

async function refresh() {
  try { render(await api("/api/state")); } catch (error) { console.error(error); }
}

$("#profile-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const payload = Object.fromEntries(form.entries());
  const submit = event.currentTarget.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    await api("/api/setup", { method: "POST", body: JSON.stringify(payload) });
    event.currentTarget.classList.add("hidden");
    $("#install-card").classList.remove("hidden");
    await refresh();
  } catch (error) {
    showError($("#profile-error"), error.message);
  } finally { submit.disabled = false; }
});

$("#install-button").addEventListener("click", async () => {
  const button = $("#install-button");
  button.disabled = true;
  try { await api("/api/install", { method: "POST", body: "{}" }); await refresh(); }
  catch (error) { showError($("#profile-error"), error.message); button.disabled = false; }
});

$("#start-button").addEventListener("click", async () => { try { await api("/api/start", { method: "POST", body: "{}" }); await refresh(); } catch (e) { showError($("#error-box"), e.message); } });
$("#stop-button").addEventListener("click", async () => { try { await api("/api/stop", { method: "POST", body: "{}" }); await refresh(); } catch (e) { showError($("#error-box"), e.message); } });
$("#chat-button").addEventListener("click", async () => { try { await api("/api/openclaw", { method: "POST", body: "{}" }); } catch (e) { showError($("#error-box"), e.message); } });
$("#workspace-link").addEventListener("click", async (event) => { event.preventDefault(); try { await api("/api/workspace", { method: "POST", body: "{}" }); } catch (e) { showError($("#error-box"), e.message); } });
refresh();
setInterval(refresh, 1500);
