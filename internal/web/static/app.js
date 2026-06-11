"use strict";

const el = (tag, cls, txt) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (txt != null) e.textContent = txt;
  return e;
};
const short = (s) => (s ? s.slice(0, 8) : "");
const shortTime = (ts) => (ts || "").replace("T", " ").replace(/\..*$/, "");
const family = (kind) => (kind === "gitop" ? "gitop" : "session");

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) return null;
  return r.json();
}

let entries = [];

async function boot() {
  entries = (await getJSON("/api/timeline.json")) || [];
  renderTimeline(entries);
  const m = location.pathname.match(/^\/event\/([0-9a-fA-F]+)/);
  const initial = m ? m[1] : entries[0] && entries[0].commit;
  if (initial) select(initial, false);
}

function renderTimeline(items) {
  const list = document.getElementById("timeline");
  list.innerHTML = "";
  if (!items.length) {
    list.appendChild(el("div", "empty", "No recorded events yet. Run `twip init`, then start a session or make git changes."));
    return;
  }
  let lastGroup = null;
  items.forEach((e, i) => {
    // A new group (with a separator header) whenever the session or branch
    // context switches — git ops form their own group.
    const gk = e.session ? `s:${e.session}|${e.branch}` : `g|${e.branch}`;
    if (gk !== lastGroup) {
      lastGroup = gk;
      const label = e.session
        ? `session ${short(e.session)} · ${e.branch || "—"}`
        : `git ops · ${e.branch || "—"}`;
      list.appendChild(groupHeader(label, e.session ? "session" : "gitop"));
    }
    list.appendChild(node(e, i));
  });
}

function groupHeader(text, fam) {
  const h = el("div", "group " + fam);
  h.appendChild(el("span", "group-dot"));
  h.appendChild(el("span", "group-label", text));
  return h;
}

function node(e, i) {
  const n = el("div", "node" + (i % 2 ? " alt" : ""));
  n.dataset.commit = e.commit;
  n.dataset.kind = e.kind;
  n.dataset.family = family(e.kind);
  n.appendChild(el("span", "dot"));

  const body = el("div", "body");
  const row1 = el("div", "row1");
  row1.appendChild(el("span", "kind", e.kind));
  if (e.quality) row1.appendChild(el("span", "flag", "!" + e.quality));
  row1.appendChild(el("span", "time", shortTime(e.ts)));
  body.appendChild(row1);

  if (e.detail) {
    const a = el("div", "annot" + (e.kind === "gitop" ? " code" : ""), e.detail);
    body.appendChild(a);
  }
  n.appendChild(body);
  n.addEventListener("click", () => select(e.commit, true));
  return n;
}

async function select(commit, push) {
  document.querySelectorAll(".node.selected").forEach((n) => n.classList.remove("selected"));
  const n = document.querySelector(`.node[data-commit="${commit}"]`);
  if (n) {
    n.classList.add("selected");
    n.scrollIntoView({ block: "nearest" });
  }
  history.replaceState(null, "", "/event/" + commit);
  const d = await getJSON("/api/event/" + commit);
  renderDetail(d);
}

function section(title) {
  return el("h3", null, title);
}
function pre(text) {
  return el("pre", null, text);
}

function renderDetail(d) {
  const p = document.getElementById("detail");
  p.innerHTML = "";
  if (!d) {
    p.appendChild(el("p", "placeholder", "Event not found."));
    return;
  }
  const title = el("h2", "detail-title", d.kind);
  if (d.quality) {
    const b = el("span", "badge", "!" + d.quality);
    b.style.color = "var(--c-flag)";
    title.appendChild(b);
  }
  p.appendChild(title);

  const meta = el("dl", "meta");
  const add = (k, v) => {
    if (v == null || v === "") return;
    meta.appendChild(el("dt", null, k));
    meta.appendChild(el("dd", null, v));
  };
  add("event", d.commit);
  add("time", shortTime(d.ts));
  if (d.session) add("session", `${d.session} (seq ${d.seq})`);
  add("worktree", d.worktree);
  add("head", short(d.head) + (d.branch ? ` [${d.branch}]` : ""));
  add("model", d.model);
  p.appendChild(meta);

  if (d.gitop) {
    p.appendChild(section("git operation"));
    const m = el("dl", "meta");
    const a2 = (k, v) => {
      m.appendChild(el("dt", null, k));
      m.appendChild(el("dd", null, v));
    };
    a2("argv", "git " + (d.gitop.argv || []).join(" "));
    a2("head", `${short(d.gitop.before_head)} → ${short(d.gitop.after_head)}`);
    a2("exit", String(d.gitop.exit_code));
    a2("worktree dirty", String(d.gitop.dirty));
    if (d.gitop.stashed && d.gitop.stashed.length) a2("stash archived", d.gitop.stashed.map(short).join(", "));
    p.appendChild(m);
  }

  if (d.prompt) {
    p.appendChild(section("prompt"));
    p.appendChild(pre(d.prompt));
  }

  if (d.changed && d.changed.length) {
    p.appendChild(section("changed files vs previous snapshot"));
    const ul = el("ul", "changed");
    d.changed.forEach((c) => {
      const li = el("li");
      li.appendChild(el("span", "st " + c.status, c.status));
      li.appendChild(el("span", null, c.path));
      li.appendChild(el("span", c.inHead ? "inhead" : "nothead", c.inHead ? "✓ in HEAD" : "· not at HEAD"));
      ul.appendChild(li);
    });
    p.appendChild(ul);
  }

  if (d.transcript) {
    p.appendChild(section(`transcript Δ (lines ${d.transcriptFrom}–${d.transcriptTo})`));
    p.appendChild(pre(d.transcript));
  }

  if (d.files && d.files.length) {
    const det = el("details", "files");
    det.appendChild(el("summary", null, `worktree snapshot (${d.files.length} files)`));
    const ul = el("ul", "files");
    d.files.forEach((f) => ul.appendChild(el("li", null, f)));
    det.appendChild(ul);
    p.appendChild(det);
  }
}

boot();
