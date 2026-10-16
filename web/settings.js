// The Settings page. For now it has one section, the desktop widget: size, spiral and a colour for each
// state. A preview drawn by the same renderer the widget uses (/api/widget/preview) follows every change,
// before it is saved. Text is always set with textContent.
(function () {
  const $ = (selector) => document.querySelector(selector);
  const STATES = [
    ["idle", "Asleep", "Nothing is running."],
    ["active", "Awake", "Ready and waiting."],
    ["listening", "Listening", "Taking in what you say."],
    ["working", "Thinking", "The model is working on a reply."],
    ["speaking", "Speaking", "Its voice is playing."],
  ];
  const CORNERS = [["top-right", "Top right"], ["top-left", "Top left"], ["bottom-right", "Bottom right"], ["bottom-left", "Bottom left"]];
  const LIMITS = { min: 240, max: 1000, spiralMin: 0.3, spiralMax: 1.6 };

  const el = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };

  let current = null; // what the server last said: { enabled, corner, appearance }
  let modelState = null;
  let displayedModelCurrent = "";
  let previewState = "active";
  let modelPollTimer = 0;

  async function request(method, body) {
    const response = await fetch("/api/widget", { method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
    const text = await response.text();
    if (!response.ok) throw new Error(text.trim() || response.statusText);
    return JSON.parse(text);
  }

  async function requestModel(method, body) {
    const response = await fetch("/api/model", { method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
    const text = await response.text();
    if (!response.ok) throw new Error(text.trim() || response.statusText);
    return JSON.parse(text);
  }

  const view = el("section", "settings-view hidden");
  view.id = "settings-view";
  view.setAttribute("aria-label", "Settings");
  const status = el("div", "set-status");
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");
  const modelStatus = el("div", "set-status");
  modelStatus.setAttribute("role", "status");
  modelStatus.setAttribute("aria-live", "polite");

  function say(message, bad) {
    status.textContent = message;
    status.classList.toggle("set-bad", Boolean(bad));
  }

  function sayModel(message, bad) {
    modelStatus.textContent = message;
    modelStatus.classList.toggle("set-bad", Boolean(bad));
  }

  // --- small controls -----------------------------------------------------------------------------

  function row(label, hint, control) {
    const wrap = el("div", "set-row");
    const text = el("div", "set-label");
    text.appendChild(el("b", "", label));
    if (hint) text.appendChild(el("small", "", hint));
    wrap.append(text, control);
    return wrap;
  }

  function slider({ min, max, step, value, format, onInput, onCommit, label }) {
    const box = el("div", "set-slider");
    const range = el("input");
    range.type = "range";
    Object.assign(range, { min, max, step, value });
    range.setAttribute("aria-label", label);
    const out = el("output", "", format(value));
    const refresh = () => { out.textContent = format(Number(range.value)); };
    range.addEventListener("input", () => { refresh(); onInput(Number(range.value)); });
    range.addEventListener("change", () => onCommit(Number(range.value)));
    box.append(range, out);
    box.set = (v) => { range.value = v; refresh(); };
    box.disable = (off) => { range.disabled = off; box.classList.toggle("set-off", off); };
    return box;
  }

  function toggle(label, checked, onChange) {
    const wrap = el("label", "set-toggle");
    const box = el("input");
    box.type = "checkbox";
    box.checked = checked;
    box.setAttribute("aria-label", label);
    box.addEventListener("change", () => onChange(box.checked));
    wrap.append(box, el("span", "", ""));
    wrap.set = (v) => { box.checked = v; };
    return wrap;
  }

  // --- the page --------------------------------------------------------------------------------------

  const controls = {};
  let previewTimer = 0;

  function pendingColors() {
    const stock = current.appearance.StockColors;
    const chosen = [];
    for (const [state] of STATES) {
      const value = controls.colors[state].picker.value.toLowerCase();
      if (value !== stock[state]) chosen.push(`${state}:${value.slice(1)}`);
    }
    return chosen.join(",");
  }

  function refreshPreview() {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(() => {
      if (!current) return;
      const q = new URLSearchParams({
        state: previewState, w: String(controls.width.value()), h: String(controls.height.value()),
        spiral: controls.spiral.checked() ? "1" : "0", size: String(controls.spiralSize.value()),
      });
      const colors = pendingColors();
      if (colors) q.set("colors", colors);
      controls.previewImage.src = "/api/widget/preview?" + q.toString();
    }, 180);
  }

  // Saves run one after another, so a change made while another is still being saved is not lost.
  let queue = Promise.resolve();
  function save(change, message) {
    queue = queue.then(() => persist(change, message));
    return queue;
  }

  async function persist(change, message) {
    try {
      const result = await request("POST", change);
      current = { ...current, enabled: result.enabled, appearance: result.appearance };
      fill();
      say(message || "Saved.");
    } catch (error) {
      say(error.message || "Could not save that.", true);
      await load();
    }
  }

  function build() {
    view.replaceChildren();
    const head = el("header", "set-head");
    head.append(el("h1", "", "Settings"));
    const close = el("button", "mem-btn", "Back to overview");
    close.type = "button";
    close.addEventListener("click", closeSettings);
    head.append(close);
    view.append(head);

    const modelSection = el("section", "set-section set-model-section");
    modelSection.append(el("h2", "", "Language model"));
    modelSection.append(el("p", "set-note", "Choose the local model used for chat. Models download only when selected; the current model stays active until the download and restart succeed."));
    const modelSelect = el("select", "set-select");
    modelSelect.setAttribute("aria-label", "Language model");
    modelSelect.disabled = true;
    modelSelect.addEventListener("change", () => {
      if (modelState && !modelState.switching) controls.modelButton.disabled = modelSelect.value === modelState.current;
    });
    controls.modelSelect = modelSelect;
    modelSection.append(row("Model", "The selected model is shared by every dashboard tab.", modelSelect));
    controls.modelCurrent = el("p", "set-model-current", "");
    modelSection.append(controls.modelCurrent);
    controls.modelButton = el("button", "mem-btn", "Use selected model");
    controls.modelButton.type = "button";
    controls.modelButton.disabled = true;
    controls.modelButton.addEventListener("click", applySelectedModel);
    const modelActions = el("div", "set-model-actions");
    modelActions.append(controls.modelButton);
    modelSection.append(modelActions, modelStatus);
    view.append(modelSection);

    const section = el("section", "set-section");
    section.append(el("h2", "", "Desktop widget"));
    section.append(el("p", "set-note", "The orb that sits on your desktop. Changes show here first, and the widget follows when you let go of a slider or pick a colour."));

    const layout = el("div", "set-layout");
    const left = el("div", "set-controls");

    // show / corner
    controls.enabled = toggle("Show the widget on the desktop", false, (on) => save({ enabled: on }, on ? "The widget is on." : "The widget is off."));
    left.append(row("Show on the desktop", "Switch the widget on or off.", controls.enabled));
    const corner = el("select", "set-select");
    corner.setAttribute("aria-label", "Where the widget sits");
    for (const [value, name] of CORNERS) corner.append(new Option(name, value));
    corner.addEventListener("change", () => save({ corner: corner.value }, "Moved."));
    controls.corner = corner;
    left.append(row("Position", "Dragging the widget on the desktop also works.", corner));

    // size
    const size = (axis, label) => {
      const ctl = slider({
        min: LIMITS.min, max: LIMITS.max, step: 10, value: 500, label, format: (v) => `${v} px`,
        onInput: refreshPreview, onCommit: (v) => save({ [axis]: v }, "Resized. The widget restarts to take the new size."),
      });
      ctl.value = () => Number(ctl.querySelector("input").value);
      return ctl;
    };
    controls.width = size("width", "Widget width");
    controls.height = size("height", "Widget height");
    left.append(row("Width", "In pixels at 100% display scale.", controls.width));
    left.append(row("Height", "The orb scales with the height.", controls.height));

    // spiral
    controls.spiral = toggle("Show the spiral", true, (on) => { refreshPreview(); controls.spiralSize.disable(!on); save({ spiral: on }, on ? "Spiral on." : "Spiral off."); });
    controls.spiral.checked = () => controls.spiral.querySelector("input").checked;
    left.append(row("Spiral", "The arms of the galaxy around the sphere.", controls.spiral));
    controls.spiralSize = slider({
      min: LIMITS.spiralMin, max: LIMITS.spiralMax, step: 0.05, value: 1, label: "Spiral size", format: (v) => `${Math.round(v * 100)}%`,
      onInput: refreshPreview, onCommit: (v) => save({ spiralSize: v }, "Spiral resized."),
    });
    controls.spiralSize.value = () => Number(controls.spiralSize.querySelector("input").value);
    left.append(row("Spiral size", "100% is the standard look.", controls.spiralSize));

    // colours
    const colorBox = el("div", "set-colors");
    controls.colors = {};
    for (const [state, name, hint] of STATES) {
      const line = el("div", "set-color");
      const picker = el("input");
      picker.type = "color";
      picker.setAttribute("aria-label", `${name} colour`);
      const text = el("div", "set-color-name");
      text.append(el("b", "", name), el("small", "", hint));
      const reset = el("button", "mem-btn", "Reset");
      reset.type = "button";
      reset.addEventListener("click", () => save({ colors: { [state]: "" } }, `${name} colour reset.`));
      const preview = el("button", "mem-btn set-see", "See it");
      preview.type = "button";
      preview.addEventListener("click", () => { previewState = state; markSeen(); refreshPreview(); });
      picker.addEventListener("input", () => { previewState = state; markSeen(); refreshPreview(); });
      picker.addEventListener("change", () => save({ colors: { [state]: picker.value } }, `${name} colour saved.`));
      line.append(picker, text, preview, reset);
      colorBox.append(line);
      controls.colors[state] = { picker, reset, preview, line };
    }
    left.append(row("State colours", "Pick one colour for each state; the shades are worked out from it.", colorBox));

    const all = el("button", "mem-btn set-reset", "Reset the widget to its standard look");
    all.type = "button";
    all.addEventListener("click", () => save({ reset: true }, "Back to the standard look."));
    left.append(all);

    // preview
    const right = el("aside", "set-preview");
    right.append(el("h3", "", "Preview"));
    const image = el("img", "set-image");
    image.alt = "The desktop orb as it will look with these settings";
    controls.previewImage = image;
    right.append(image);
    const tabs = el("div", "set-tabs");
    tabs.setAttribute("role", "group");
    tabs.setAttribute("aria-label", "State to preview");
    controls.tabs = {};
    for (const [state, name] of STATES) {
      const tab = el("button", "mem-btn", name);
      tab.type = "button";
      tab.addEventListener("click", () => { previewState = state; markSeen(); refreshPreview(); });
      tabs.append(tab);
      controls.tabs[state] = tab;
    }
    right.append(tabs);
    right.append(el("p", "set-note", "Drawn by the same code as the desktop widget, so it matches what you get."));

    layout.append(left, right);
    section.append(layout, status);
    view.append(section);
  }

  function markSeen() {
    for (const [state] of STATES) {
      controls.tabs[state].setAttribute("aria-pressed", String(state === previewState));
      controls.colors[state].line.classList.toggle("set-seen", state === previewState);
    }
  }

  function fill() {
    const a = current.appearance;
    controls.enabled.set(Boolean(current.enabled));
    controls.corner.value = current.corner;
    controls.width.set(a.Width);
    controls.height.set(a.Height);
    controls.spiral.querySelector("input").checked = a.SpiralOn;
    controls.spiralSize.set(a.SpiralSize);
    controls.spiralSize.disable(!a.SpiralOn);
    for (const [state] of STATES) {
      const c = controls.colors[state];
      c.picker.value = a.Colors[state];
      c.reset.disabled = a.Colors[state] === a.StockColors[state];
    }
    markSeen();
    refreshPreview();
  }

  async function load() {
    try {
      current = await request("GET");
      fill();
    } catch (error) {
      say("Could not read the widget settings: " + error.message, true);
    }
  }

  function fillModelSettings() {
    if (!modelState || !controls.modelSelect) return;
    const { models, current: active, switching, target, status: detail, error } = modelState;
    const previousSelection = controls.modelSelect.value;
    const keepLocalSelection = !switching && document.activeElement === controls.modelSelect && displayedModelCurrent === active;
    controls.modelSelect.replaceChildren();
    for (const model of models || []) {
      const suffix = model.installed ? " - downloaded" : " - download on selection";
      controls.modelSelect.append(new Option(`${model.name} (${model.quantization})${suffix}`, model.id));
    }
    controls.modelSelect.value = keepLocalSelection ? previousSelection : switching ? target : active;
    displayedModelCurrent = active;
    const activeModel = (models || []).find((model) => model.id === active);
    controls.modelCurrent.textContent = `Current model: ${activeModel ? activeModel.name : active}`;
    controls.modelSelect.disabled = Boolean(switching);
    controls.modelButton.disabled = Boolean(switching) || controls.modelSelect.value === active;
    controls.modelButton.textContent = switching ? "Changing model..." : "Use selected model";
    sayModel(error || detail || "", Boolean(error));
  }

  async function loadModelSettings() {
    try {
      modelState = await requestModel("GET");
      fillModelSettings();
      clearTimeout(modelPollTimer);
      if (!view.classList.contains("hidden")) {
        modelPollTimer = setTimeout(loadModelSettings, modelState.switching ? 1000 : 3000);
      }
    } catch (error) {
      sayModel("Could not read the model settings: " + error.message, true);
    }
  }

  async function applySelectedModel() {
    if (!controls.modelSelect || controls.modelButton.disabled) return;
    controls.modelButton.disabled = true;
    sayModel("Starting model change...");
    try {
      modelState = await requestModel("POST", { id: controls.modelSelect.value });
      fillModelSettings();
      if (modelState.switching) {
        clearTimeout(modelPollTimer);
        modelPollTimer = setTimeout(loadModelSettings, 1000);
      }
    } catch (error) {
      sayModel(error.message || "Could not change the model.", true);
      await loadModelSettings();
    }
  }

  function openSettings() {
    if (!controls.width) build();
    view.classList.remove("hidden");
    document.body.classList.add("settings-open");
    const crumb = $("#top-section");
    if (crumb) crumb.textContent = "Settings";
    for (const link of document.querySelectorAll(".nav-item")) link.classList.toggle("selected", link.id === "settings-link");
    say("");
    load();
    loadModelSettings();
  }

  function closeSettings() {
    clearTimeout(modelPollTimer);
    view.classList.add("hidden");
    document.body.classList.remove("settings-open");
    const crumb = $("#top-section");
    if (crumb) crumb.textContent = "Overview";
    for (const link of document.querySelectorAll(".nav-item")) link.classList.toggle("selected", link.getAttribute("href") === "#overview");
  }

  function start() {
    document.body.appendChild(view);
    const link = $("#settings-link");
    if (link) link.addEventListener("click", (event) => { event.preventDefault(); openSettings(); });
    for (const other of document.querySelectorAll('.nav-item[href="#overview"]')) {
      other.addEventListener("click", (event) => { event.preventDefault(); closeSettings(); });
    }
    document.addEventListener("keydown", (event) => { if (event.key === "Escape" && !view.classList.contains("hidden")) closeSettings(); });
    if (location.hash === "#settings") openSettings();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start); else start();
})();
