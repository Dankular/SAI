// The memory drawer and reminder toasts. Everything shown here is read from /api/memory and the
// state stream; nothing is kept in the page. Text is always set with textContent.
(function () {
  const $ = (selector) => document.querySelector(selector);
  const post = (body) => fetch("/api/memory", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }).then((r) => r.json());

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function button(label, onClick, title) {
    const b = el("button", "mem-btn", label);
    b.type = "button";
    if (title) b.title = title;
    b.addEventListener("click", onClick);
    return b;
  }

  function section(title, count) {
    const wrap = el("section", "mem-section");
    const head = el("h3", "mem-head", title);
    if (count !== undefined) head.appendChild(el("span", "mem-count", String(count)));
    wrap.appendChild(head);
    return wrap;
  }

  function relative(iso) {
    const t = Date.parse(iso);
    if (!t) return "";
    const days = Math.round((Date.now() - t) / 86400000);
    if (days <= 0) return "today";
    if (days === 1) return "yesterday";
    if (days < 60) return days + " days ago";
    return Math.round(days / 30) + " months ago";
  }

  const drawer = el("aside", "mem-drawer hidden");
  drawer.setAttribute("aria-label", "What the assistant remembers");
  const toasts = el("div", "mem-toasts");
  toasts.setAttribute("role", "status");
  toasts.setAttribute("aria-live", "polite");

  async function load() {
    let data;
    try { data = await (await fetch("/api/memory")).json(); } catch { return; }
    for (const key of ["notes", "events", "jobs", "skills"]) data[key] = data[key] || []; // an empty list can arrive as null
    drawer.replaceChildren();

    const top = el("div", "mem-top");
    top.appendChild(el("b", "", "WHAT I REMEMBER"));
    top.appendChild(button("Tidy up", async () => {
      const result = await post({ action: "consolidate" });
      note.textContent = result.message || "Nothing to tidy.";
      load();
    }, "Merge duplicates and drop outdated notes"));
    top.appendChild(button("Reflect now", async () => {
      note.textContent = "Thinking about what is new…";
      const result = await post({ action: "dream" });
      note.textContent = "Reflection: " + (result.message || "done");
      load();
    }, "Let me look over new events and tidy my notes now"));
    top.appendChild(button("Close", () => drawer.classList.add("hidden")));
    drawer.appendChild(top);
    const note = el("p", "mem-note", "Notes are read by me every time we talk. Forgetting removes them for good.");
    drawer.appendChild(note);

    if (data.profile) {
      const s = section("What I've learned about you");
      s.appendChild(el("pre", "mem-profile", data.profile));
      drawer.appendChild(s);
    }

    if (data.skills && data.skills.length) {
      const drafts = section("Skills I drafted for you to review", data.skills.length);
      drafts.appendChild(el("p", "mem-note", "I can't use a draft until you approve it. Read it first: it was written from a conversation."));
      for (const k of data.skills) {
        const card = el("div", "mem-draft");
        card.appendChild(el("b", "", k.name));
        card.appendChild(el("p", "mem-text", k.description));
        card.appendChild(el("small", "", "Why: " + k.reason));
        card.appendChild(el("pre", "mem-profile", k.instructions));
        const row = el("div", "mem-actions");
        row.appendChild(button("Approve", async () => {
          const result = await post({ action: "skill_approve", id: k.name });
          if (!result.ok) note.textContent = result.message || "Could not approve that.";
          load();
        }));
        row.appendChild(button("Discard", async () => { await post({ action: "skill_reject", id: k.name }); load(); }));
        card.appendChild(row);
        drafts.appendChild(card);
      }
      drawer.appendChild(drafts);
    }

    const notes = section("Notes", data.notes.length);
    if (!data.notes.length) notes.appendChild(el("p", "mem-empty", "Nothing yet. Tell me something to remember."));
    for (const n of data.notes) {
      const row = el("div", "mem-row");
      const text = el("div", "mem-text", n.text);
      text.appendChild(el("small", "", n.file.replace(".md", "") + (n.date ? " · " + n.date : "")));
      row.appendChild(text);
      row.appendChild(button("Forget", async () => { await post({ action: "forget_note", file: n.file, text: n.text }); load(); }));
      notes.appendChild(row);
    }
    drawer.appendChild(notes);

    const events = section("What I picked up from our chats", data.events.length);
    if (!data.events.length) events.appendChild(el("p", "mem-empty", "Preferences, decisions and fixes I notice will show up here."));
    for (const e of data.events.slice(0, 40)) {
      const row = el("div", "mem-row" + (e.status === "superseded" || e.status === "stale" ? " mem-dim" : ""));
      const text = el("div", "mem-text", e.summary);
      const quote = e.evidence && e.evidence[0] ? ' · you said: "' + e.evidence[0].quote + '"' : "";
      text.appendChild(el("small", "", e.event.replace(/_/g, " ") + " · " + e.status + " · " + relative(e.created_at) + quote));
      row.appendChild(text);
      row.appendChild(button(e.pinned ? "Unpin" : "Pin", async () => { await post({ action: e.pinned ? "unpin" : "pin", id: e.id }); load(); }, "Pinned memories rank higher and survive tidying"));
      row.appendChild(button("Forget", async () => { await post({ action: "forget_event", id: e.id }); load(); }));
      events.appendChild(row);
    }
    drawer.appendChild(events);

    const jobs = section("Reminders and schedules", data.jobs.length);
    if (!data.jobs.length) jobs.appendChild(el("p", "mem-empty", 'Try "remind me to call mum tomorrow at 9".'));
    for (const j of data.jobs) {
      const row = el("div", "mem-row");
      const text = el("div", "mem-text", j.text);
      const repeat = j.every_sec ? " · every " + Math.round(j.every_sec / 60) + " min" : j.daily_at ? " · " + (j.weekday || "daily") + " at " + j.daily_at : "";
      text.appendChild(el("small", "", j.kind + " · next " + new Date(j.next).toLocaleString() + repeat));
      row.appendChild(text);
      row.appendChild(button("Remove", async () => { await post({ action: "remove_job", id: j.id }); load(); }));
      jobs.appendChild(row);
    }
    drawer.appendChild(jobs);
  }

  const seen = new Set();
  function showNotices(list) {
    for (const n of list || []) {
      if (seen.has(n.id)) continue;
      seen.add(n.id);
      const toast = el("div", "mem-toast");
      toast.appendChild(el("div", "mem-toast-text", n.text));
      toast.appendChild(button("Dismiss", () => { toast.remove(); post({ action: "dismiss_notice", id: n.id }); }));
      toasts.appendChild(toast);
    }
  }

  function start() {
    document.body.appendChild(drawer);
    document.body.appendChild(toasts);
    const link = $("#memory-link");
    if (link) link.addEventListener("click", (event) => {
      event.preventDefault();
      const opening = drawer.classList.contains("hidden");
      drawer.classList.toggle("hidden", !opening);
      if (opening) load();
    });
    const events = new EventSource("/api/events");
    events.addEventListener("state", (event) => {
      try {
        const message = JSON.parse(event.data);
        showNotices((message.snapshot || message).notices);
      } catch { /* a malformed event is skipped */ }
    });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start); else start();
})();
