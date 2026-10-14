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
  document.body.classList.toggle("is-dashboard", view === "dashboard");
}

function showError(element, message) {
  element.textContent = message;
  element.classList.toggle("hidden", !message);
}

function renderSetupStages(snapshot) {
  // The server owns the list of setup stages; build (or rebuild) the list from it.
  const list = $("#progress-stages");
  const labels = snapshot.stageLabels || [];
  if (labels.length && list.children.length !== labels.length) {
    list.replaceChildren(...labels.map((label, index) => {
      const item = document.createElement("li");
      item.dataset.stage = String(index + 1);
      item.innerHTML = '<span class="stage-marker"></span><span class="stage-name"></span><span class="stage-state">Waiting</span>';
      item.querySelector(".stage-name").textContent = label;
      return item;
    }));
  }
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
  // The process manager (snapshot.agent) is the only source of truth for what the assistant is
  // doing; nothing in this page decides it, so every tab agrees.
  const agent = snapshot.agent || { state: "asleep", label: "Assistant is asleep", awake: false, wanted: false };
  ui.agent = agent;
  ui.warm = Boolean(snapshot.runtime && snapshot.runtime.gatewayWarm);
  ui.profile = snapshot.profile || {};
  ui.waking = agent.wanted && !agent.awake;
  ui.awake = agent.awake;
  if (ui.awake && ui.wakeRequested) { ui.wakeRequested = false; greet(); }
  ui.inferenceBusy = Boolean(snapshot.runtime && snapshot.runtime.inferenceBusy);
  ensureOrb();
  applyOrb();
  updateSayBar();
  $("#start-button").disabled = agent.wanted && agent.state !== "failed";
  $("#stop-button").disabled = !agent.wanted;
  $("#overall-status").classList.toggle("online", ui.awake);
  $("#overall-status").title = agent.detail || "";
  $("#overall-status b").textContent = agent.state === "waking" ? "Waking up…" : agent.state === "recovering" ? "Recovering…" : agent.label;
  $("#step-title").textContent = snapshot.step || "Ready";
  $("#activity-state").textContent = snapshot.setupRunning || snapshot.memoryRunning ? "In progress" : snapshot.error ? "Needs attention" : "Updated";
  $("#activity-state").classList.toggle("attention", Boolean(snapshot.error));
  showError($("#error-box"), snapshot.error || "");
  const logs = $("#log-list");
  const follow = logs.scrollHeight - logs.scrollTop - logs.clientHeight < 40;
  logs.replaceChildren();
  if (!snapshot.recentLogs || snapshot.recentLogs.length === 0) {
    logs.innerHTML = '<div class="empty-log">Activity will appear here.</div>';
  } else {
    for (const line of snapshot.recentLogs.slice(-60)) {
      const row = document.createElement("div");
      row.className = "log-row";
      row.textContent = line;
      logs.append(row);
    }
    if (follow) logs.scrollTop = logs.scrollHeight;
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

$("#start-button").addEventListener("click", async () => {
  try {
    // Unlock audio inside this click so the greeting can play when the assistant is ready.
    if ($("#voice-on").checked) await (await ensureSpeech()).prime();
    ui.wakeRequested = true;
    await api("/api/start", { method: "POST", body: "{}" });
    await refresh();
  } catch (e) { ui.wakeRequested = false; showError($("#error-box"), e.message); }
});
$("#stop-button").addEventListener("click", async () => {
  if (ui.sleeping) return;
  ui.sleeping = true;
  try {
    await sayGoodbye();
    await api("/api/stop", { method: "POST", body: "{}" });
    await refresh();
  } catch (e) { showError($("#error-box"), e.message); }
  finally { ui.sleeping = false; }
});
$("#chat-button").addEventListener("click", async () => { try { await api("/api/openclaw", { method: "POST", body: "{}" }); } catch (e) { showError($("#error-box"), e.message); } });
$("#workspace-link").addEventListener("click", async (event) => { event.preventDefault(); try { await api("/api/workspace", { method: "POST", body: "{}" }); } catch (e) { showError($("#error-box"), e.message); } });
// ---- Sphere + conversation -------------------------------------------------
// The sphere shows what the assistant is doing: idle (asleep), active (awake),
// listening, working (the model is running inference) and speaking. It moves to
// the assistant's own voice only; the microphone never drives it.
const ui = { micOn: false, turnSpeaking: false, warm: true, waking: false, wakeRequested: false, sleeping: false, profile: {}, awake: false, chatBusy: false, speaking: false, listening: false, inferenceBusy: false, textPulse: 0 };
let orb = null;
let orbLoading = false;
let speech = null;
let speakingHold = 0;

const ORB_COPY = {
  idle: ["Asleep", "Wake your assistant to start talking"],
  active: ["Ready", "Type below to talk to your assistant"],
  listening: ["Listening", "Go ahead, I'm listening"],
  working: ["Working on it", "Your assistant is thinking"],
  speaking: ["Speaking", ""],
};

function orbState() {
  if (ui.waking) return "working";
  if (!ui.awake) return "idle";
  if (ui.turnSpeaking) return "speaking";
  if (ui.chatBusy || ui.inferenceBusy) return "working";
  if (ui.listening) return "listening";
  return "active";
}

const PREVIEW = new URLSearchParams(location.search);

const MIN_DWELL_MS = 600;
let shownState = null;
let shownAt = 0;
let dwellTimer = 0;

function applyOrb() {
  // Design preview: /?orbstate=speaking&orbdemo=1 forces a state and a synthetic voice.
  const wanted = PREVIEW.get("orbstate") || orbState();
  if (shownState && wanted !== shownState) {
    const held = performance.now() - shownAt;
    const immediate = wanted === "speaking" || wanted === "idle";
    if (!immediate && held < MIN_DWELL_MS) {
      clearTimeout(dwellTimer);
      dwellTimer = setTimeout(applyOrb, MIN_DWELL_MS - held + 20);
      return;
    }
  }
  if (wanted !== shownState) { shownState = wanted; shownAt = performance.now(); }
  const state = shownState;
  const stage = $(".orb-stage");
  if (stage) stage.dataset.state = state;
  let [title, hint] = ui.waking ? ["Waking up", "Getting everything ready, one moment"] : ORB_COPY[state];
  if (ui.awake && !ui.warm && state === "active") hint = "Ready. Your first reply may take a little longer";
  const stateLabel = $("#orb-state");
  if (stateLabel.textContent !== title) {
    // the glitch copies read their text from data-text; a changed word gets a short, harder burst
    stateLabel.textContent = title;
    stateLabel.dataset.text = title;
    stateLabel.classList.remove("burst");
    void stateLabel.offsetWidth;
    stateLabel.classList.add("burst");
    // the burst is three quick jolts; afterwards the state's own rhythm resumes
    clearTimeout(stateLabel.burstTimer);
    stateLabel.burstTimer = setTimeout(() => stateLabel.classList.remove("burst"), 1400);
  }
  $("#orb-hint").textContent = hint;
  orb?.setState(state);
}

async function ensureOrb() {
  if (orb || orbLoading) return;
  const host = $("#orb");
  if (!host || host.offsetWidth === 0) return;
  orbLoading = true;
  try {
    const { createOrb } = await import("/orb.js");
    orb = createOrb(host);
    applyOrb();
    if (PREVIEW.get("orbdemo")) {
      let t = 0;
      setInterval(() => {
        t += 0.09;
        const level = 0.5 + 0.35 * Math.sin(t * 3.1) * Math.sin(t * 0.7);
        orb.setLevel(level, { bass: 0.5 + 0.4 * Math.sin(t * 1.3), mid: 0.45 + 0.4 * Math.sin(t * 2.1 + 1), treble: 0.35 + 0.35 * Math.sin(t * 4.3 + 2) });
      }, 33);
    }
  } catch (error) {
    console.error("Could not start the sphere", error);
  } finally { orbLoading = false; }
}

// Leaves the speaking state only once the reply has finished arriving and the
// voice has nothing queued, after a short pause so a late sentence can still join.
function endSpeakingTurn() {
  clearTimeout(speakingHold);
  speakingHold = setTimeout(() => {
    if (ui.chatBusy || ui.speaking || (speech && speech.busy)) return; // more to say
    ui.turnSpeaking = false;
    applyOrb();
  }, 700);
}

async function ensureSpeech() {
  if (speech) return speech;
  const { AgentSpeech } = await import("/voice.js");
  speech = new AgentSpeech({
    onLevel: (level, bands) => orb?.setLevel(level, bands),
    onSpeaking: (speaking) => {
      ui.speaking = speaking; // audible right now (the sphere's level follows this on its own)
      if (speaking) {
        clearTimeout(speakingHold);
        ui.turnSpeaking = true;
        applyOrb();
      } else {
        endSpeakingTurn();
      }
    },
    onUnavailable: (why) => {
      console.warn("Speech output unavailable:", why);
      addMessage("error", "Voice output isn't available right now, so replies are shown as text. " + why);
    },
  });
  return speech;
}


function escapeHtml(text) {
  return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function renderMarkdown(raw) {
  const inline = (text) => escapeHtml(text)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*])\*([^*\s][^*]*)\*(?!\*)/g, "$1<em>$2</em>");
  const out = [];
  let list = null;
  const close = () => { if (list) { out.push(`</${list}>`); list = null; } };
  for (const line of raw.split("\n")) {
    const bullet = /^\s*[-*•]\s+(.*)$/.exec(line);
    const numbered = /^\s*\d+[.)]\s+(.*)$/.exec(line);
    if (bullet || numbered) {
      const kind = bullet ? "ul" : "ol";
      if (list !== kind) { close(); out.push(`<${kind}>`); list = kind; }
      out.push(`<li>${inline((bullet || numbered)[1])}</li>`);
    } else if (line.trim() === "") {
      close();
    } else {
      close();
      out.push(`<p>${inline(line)}</p>`);
    }
  }
  close();
  return out.join("");
}

function addMessage(role, text, pending = false) {
  const list = $("#convo");
  list.querySelector(".empty-log")?.remove();
  const bubble = document.createElement("div");
  bubble.className = `msg ${role}${pending ? " pending" : ""}`;
  bubble.textContent = text;
  list.append(bubble);
  list.scrollTop = list.scrollHeight;
  return bubble;
}

function updateSayBar() {
  if (typeof reflectMic === "function") { if (!ui.awake && ui.micOn) stopMic(); reflectMic(); }
  const enabled = ui.awake && !ui.chatBusy;
  $("#say-input").disabled = !enabled;
  $("#say-send").disabled = !enabled;
  $("#say-input").placeholder = ui.awake ? "Type to your assistant…" : "Wake your assistant first";
}

// With no audio to follow, a quick text-driven pulse keeps the sphere responsive.
function pulseFromText(delta) {
  ui.textPulse = Math.min(1, 0.35 + delta.length / 40);
  orb?.setLevel(ui.textPulse, { bass: ui.textPulse * 0.6, mid: ui.textPulse, treble: ui.textPulse * 0.4 });
  setTimeout(() => orb?.setLevel(0, { bass: 0, mid: 0, treble: 0 }), 140);
}

function greeting() {
  const hour = new Date().getHours();
  const part = hour < 5 ? "Hello" : hour < 12 ? "Good morning" : hour < 18 ? "Good afternoon" : "Good evening";
  const name = (ui.profile.userName || "").trim();
  const me = (ui.profile.agentName || "Your assistant").trim();
  return `${part}${name ? ", " + name : ""}. ${me} is online and ready.`;
}

async function speakLine(text) {
  addMessage("agent", text);
  if (!$("#voice-on").checked) return;
  const voice = await ensureSpeech();
  await voice.prime();
  voice.speak(text);
}

async function greet() { try { await speakLine(greeting()); } catch (error) { console.warn("greeting failed", error); } }

// Say goodbye out loud and let it finish before the assistant actually goes to sleep.
async function sayGoodbye() {
  if (!ui.awake) return;
  try {
    await speakLine("Going to sleep. Goodnight.");
    const started = Date.now();
    while (speech && speech.busy && Date.now() - started < 15000) await new Promise((resolve) => setTimeout(resolve, 120));
    await new Promise((resolve) => setTimeout(resolve, 250));
  } catch (error) { console.warn("goodbye failed", error); }
}

const SPOKEN_CHAR_BUDGET = 420;
let chatAbort = null;

// Stops the assistant mid-reply: silences the voice and cancels the request.
function interruptAgent() {
  speech?.stop();
  chatAbort?.abort();
  clearTimeout(speakingHold);
  ui.turnSpeaking = false;
  ui.speaking = false;
  applyOrb();
}

async function sendToAgent(text) {
  ui.chatBusy = true;
  ui.turnSpeaking = false;
  speech?.stop();
  chatAbort = new AbortController();
  const abortSignal = chatAbort.signal;
  applyOrb();
  updateSayBar();
  const bubble = addMessage("agent", "", true);
  let rawReply = "";
  let spokenChars = 0; // the voice stops after roughly three sentences, however long the reply
  const speakOn = $("#voice-on").checked;
  let splitter = null;
  try {
    if (speakOn) {
      const voice = await ensureSpeech();
      await voice.prime();
      const { SentenceSplitter } = await import("/voice.js");
      splitter = new SentenceSplitter();
    }
    const response = await fetch("/api/voice/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text, spoken: speakOn }),
      signal: abortSignal,
    });
    if (!response.ok) throw new Error((await response.text()).trim() || response.statusText);
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    const speakWithinBudget = (sentences) => {
      for (const sentence of sentences) {
        if (spokenChars >= SPOKEN_CHAR_BUDGET) return;
        spokenChars += sentence.length;
        speech.speak(sentence);
      }
    };
    const handle = (line) => {
      if (!line.trim()) return;
      const event = JSON.parse(line);
      if (event.error) throw new Error(event.error);
      if (!event.delta) return;
      if (!rawReply) orb?.flash(); // the reply has begun: pink flash through the neuron
      rawReply += event.delta;
      bubble.innerHTML = renderMarkdown(rawReply);
      $("#convo").scrollTop = $("#convo").scrollHeight;
      if (splitter && speech && !speech.unavailable) speakWithinBudget(splitter.push(event.delta));
      else pulseFromText(event.delta);
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      pending += decoder.decode(value, { stream: true });
      const lines = pending.split("\n");
      pending = lines.pop();
      lines.forEach(handle);
    }
    if (pending) handle(pending);
    if (splitter && speech && !speech.unavailable) speakWithinBudget(splitter.flush());
    if (!rawReply.trim()) bubble.textContent = "(no reply)";
  } catch (error) {
    if (error.name === "AbortError") {
      bubble.classList.remove("pending");
      bubble.innerHTML = renderMarkdown(rawReply) + (rawReply ? " <em>(interrupted)</em>" : "<em>(interrupted)</em>");
    } else {
      bubble.className = "msg error";
      bubble.textContent = error.message || "Something went wrong talking to your assistant.";
    }
  } finally {
    bubble.classList.remove("pending");
    ui.chatBusy = false;
    endSpeakingTurn();
    applyOrb();
    updateSayBar();
    $("#say-input").focus();
  }
}


// ---- Hands-free conversation ------------------------------------------------
// Microphone -> TEN VAD -> Smart Turn -> Whistle -> the assistant. See listen.js.
let hands = null;
let recognizer = null;

const LISTEN_TEXT = {
  idle: "Listening…",
  hearing: "Hearing you",
  checking: "Hearing you",
  transcribing: "Transcribing…",
  ignored: "Didn't catch that",
};

function setListenStatus(text) {
  const element = $("#listen-status");
  if (element) element.textContent = text;
}

function setMicLevel(rms) {
  const fill = $("#mic-level-fill");
  if (fill) fill.style.width = `${Math.min(100, Math.round(rms * 700))}%`;
}

function onListenStatus(status, detail) {
  ui.listening = status === "hearing" || status === "checking";
  setListenStatus(detail?.thinking ? "Take your time…" : LISTEN_TEXT[status] || "");
  applyOrb();
}

async function handleHeard(text) {
  ui.listening = false;
  if (ui.chatBusy) {
    interruptAgent();
    for (let i = 0; i < 25 && ui.chatBusy; i++) await new Promise((resolve) => setTimeout(resolve, 80));
  }
  const bubble = addMessage("user", text);
  bubble.classList.add("heard");
  await sendToAgent(text);
}

async function ensureHands() {
  if (hands) return hands;
  const [{ HandsFree }, { Recognizer }] = await Promise.all([import("/listen.js"), import("/stt.js")]);
  recognizer = new Recognizer();
  recognizer.onStatus = (text) => setListenStatus(text + "…");
  hands = new HandsFree({
    recognizer,
    assistantBusy: () => ui.chatBusy || ui.turnSpeaking || ui.speaking,
    onStatus: onListenStatus,
    onLevel: setMicLevel,
    onSpeechStart: () => { ui.listening = true; applyOrb(); },
    onBargeIn: interruptAgent,
    onTranscript: handleHeard,
    onError: (error) => { console.warn("listening error", error); addMessage("error", "I couldn't make out that audio. " + (error.message || "")); },
  });
  return hands;
}

function reflectMic() {
  const button = $("#mic-button");
  button.setAttribute("aria-pressed", String(ui.micOn));
  $("#mic-label").textContent = ui.micOn ? "Mic on" : "Mic off";
  $("#listen-row").classList.toggle("hidden", !ui.micOn);
  button.disabled = !ui.awake;
}

async function startMic() {
  if (ui.micOn || !ui.awake) return;
  ui.micOn = true;
  reflectMic();
  setListenStatus("Loading speech recognition…");
  try {
    // Unlock audio playback inside this click so replies can be spoken.
    if ($("#voice-on").checked) await (await ensureSpeech()).prime();
    const controller = await ensureHands();
    await Promise.all([recognizer.start(), controller.start()]);
  } catch (error) {
    console.warn("microphone could not start", error);
    ui.micOn = false;
    reflectMic();
    const denied = error && (error.name === "NotAllowedError" || error.name === "SecurityError");
    addMessage("error", denied
      ? "I need permission to use your microphone. Allow it in the browser's address bar, then try again."
      : "Speech recognition isn't ready yet (" + (error.message || "unknown problem") + "). It may still be downloading; try again in a moment.");
  }
}

function stopMic() {
  if (!ui.micOn) return;
  ui.micOn = false;
  ui.listening = false;
  hands?.stop();
  reflectMic();
  applyOrb();
}

$("#mic-button").addEventListener("click", () => (ui.micOn ? stopMic() : startMic()));

$("#say-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const input = $("#say-input");
  const text = input.value.trim();
  if (!text || ui.chatBusy || !ui.awake) return;
  input.value = "";
  addMessage("user", text);
  await sendToAgent(text);
});

$("#voice-on").addEventListener("change", (event) => { if (!event.target.checked) speech?.stop(); });

// ---- Terminal drawer in the sidebar ---------------------------------------
function setTerminal(open) {
  $("#terminal-panel").classList.toggle("hidden", !open);
  const toggle = $("#terminal-toggle");
  toggle.setAttribute("aria-expanded", String(open));
  toggle.querySelector(".terminal-label").textContent = open ? "Hide terminal" : "View terminal";
  document.body.classList.toggle("terminal-open", open);
  try { localStorage.setItem("sai-terminal", open ? "1" : "0"); } catch { /* storage may be unavailable */ }
  if (open) { const logs = $("#log-list"); logs.scrollTop = logs.scrollHeight; }
}
$("#terminal-toggle").addEventListener("click", () => setTerminal($("#terminal-panel").classList.contains("hidden")));
try { if (localStorage.getItem("sai-terminal") === "1") setTerminal(true); } catch { /* ignore */ }

refresh();
setInterval(refresh, 1500);
