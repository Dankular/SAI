// The desktop orb mirrors assistant state and microphone level from the local app.
import { createOrb } from "/orb.js";

const $ = (selector) => document.querySelector(selector);
const orb = createOrb($("#orb"));
let shown = "";
let waking = false;
let appSnapshot = { agent: {}, runtime: {} };
let latestRevision = 0;
let pollingOrb = false;
let lastFlashID = 0;
let lastStimulusID = 0;

function apply(state, isWaking) {
  if (state !== shown) {
    shown = state;
    orb.setState?.(state);
  }
  if (isWaking !== waking) {
    waking = isWaking;
    orb.setWaking?.(isWaking);
  }
}

function stateFrom(snapshot) {
  if (snapshot.orb?.state) return snapshot.orb.state;
  const agent = snapshot.agent || {};
  if (agent.wanted && !agent.awake) return "working";
  if (!agent.awake) return "idle";
  return snapshot.runtime && snapshot.runtime.inferenceBusy ? "working" : "active";
}

function receiveState(snapshot) {
  const revision = Number(snapshot?.revision) || 0;
  if (revision && revision < latestRevision) return;
  latestRevision = Math.max(latestRevision, revision);
  appSnapshot = snapshot;
  apply(stateFrom(snapshot), Boolean(snapshot.orb?.waking));
}

async function pollAppState() {
  try {
    const response = await fetch("/api/state", { cache: "no-store" });
    if (!response.ok) throw new Error("state request failed");
    receiveState(await response.json());
  } catch {
    apply("idle", false);
  }
}

async function pollOrb() {
  if (pollingOrb) return;
  pollingOrb = true;
  try {
    const response = await fetch("/api/orb", { cache: "no-store" });
    if (!response.ok) return;
    const mirrored = await response.json();
    apply(stateFrom({ ...appSnapshot, orb: mirrored }), Boolean(mirrored.waking));
    const freshLevel = mirrored.levelAgeMs >= 0 && mirrored.levelAgeMs < 300;
    orb.setLevel?.(freshLevel ? mirrored.level : 0, {
      bass: freshLevel ? mirrored.bass : 0,
      mid: freshLevel ? mirrored.mid : 0,
      treble: freshLevel ? mirrored.treble : 0,
    });
    if (mirrored.waking) orb.setWaking?.(true);
    if (mirrored.stimulusId && mirrored.stimulusId !== lastStimulusID) {
      lastStimulusID = mirrored.stimulusId;
      if (mirrored.stimulusAgeMs >= 0 && mirrored.stimulusAgeMs < 850) orb.stimulus?.();
    }
    if (mirrored.flashId && mirrored.flashId !== lastFlashID) {
      lastFlashID = mirrored.flashId;
      if (mirrored.flashAgeMs >= 0 && mirrored.flashAgeMs < 870) orb.flash?.();
    }
  } catch {
    orb.setLevel?.(0);
  } finally {
    pollingOrb = false;
  }
}

// A drag is accepted only on rendered orb pixels: the native color-key window passes empty space
// through to the desktop. Position updates are coalesced, and only pointer release writes to disk.
let drag = null;
let queuedPosition = null;
let sendingPosition = false;
let lastSentAt = 0;
const sendPosition = (dragging) => {
  if (!drag) return;
  const screenX = drag.startLeft + drag.lastPointerX - drag.startPointerX;
  const screenY = drag.startTop + drag.lastPointerY - drag.startPointerY;
  const area = window.screen;
  const maxX = Math.max(1, area.availWidth - window.outerWidth);
  const maxY = Math.max(1, area.availHeight - window.outerHeight);
  queuedPosition = {
    x: Math.max(0, Math.min(1, (screenX - (area.availLeft || 0)) / maxX)),
    y: Math.max(0, Math.min(1, (screenY - (area.availTop || 0)) / maxY)),
    dragging,
  };
  if (!sendingPosition) void flushPosition();
};

async function flushPosition() {
  if (sendingPosition || !queuedPosition) return;
  sendingPosition = true;
  const wait = Math.max(0, 34 - (performance.now() - lastSentAt));
  if (wait) await new Promise((resolve) => setTimeout(resolve, wait));
  const position = queuedPosition;
  queuedPosition = null;
  lastSentAt = performance.now();
  try {
    await fetch("/api/widget", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(position),
    });
  } catch { /* the app may be shutting down */ }
  sendingPosition = false;
  if (queuedPosition) void flushPosition();
}

$("#orb").addEventListener("pointerdown", (event) => {
  if (event.button !== 0) return;
  drag = {
    startLeft: window.screenX,
    startTop: window.screenY,
    startPointerX: event.screenX,
    startPointerY: event.screenY,
    lastPointerX: event.screenX,
    lastPointerY: event.screenY,
    moved: false,
  };
  event.currentTarget.setPointerCapture(event.pointerId);
  event.preventDefault();
});

$("#orb").addEventListener("pointermove", (event) => {
  if (!drag) return;
  drag.lastPointerX = event.screenX;
  drag.lastPointerY = event.screenY;
  drag.moved ||= Math.hypot(event.screenX - drag.startPointerX, event.screenY - drag.startPointerY) > 2;
  if (drag.moved) sendPosition(true);
});

function finishDrag() {
  if (!drag) return;
  if (drag.moved) sendPosition(false);
  drag = null;
}

$("#orb").addEventListener("pointerup", finishDrag);
$("#orb").addEventListener("pointercancel", finishDrag);
document.addEventListener("contextmenu", (event) => event.preventDefault());

apply("idle", false);
void pollAppState();
void pollOrb();
const stateEvents = new EventSource("/api/events");
stateEvents.addEventListener("state", (event) => {
  try {
    const message = JSON.parse(event.data);
    receiveState(message.snapshot || message);
  } catch { /* reconnect and wait for the next state snapshot */ }
});
setInterval(pollOrb, 34);
