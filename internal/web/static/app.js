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

/* ---------- timeline ---------- */

// Two independent lanes:
//   branch  — a continuous spine; breaks only when the branch switches. Git ops
//             (session-independent events) are nodes on this lane.
//   session — present only across a session's span (its newest..oldest event);
//             can gap over git-ops that happen mid-session; breaks at session end.
//             Session turns are nodes on this lane.
// items are newest-first (index 0 = newest); "above" a row = newer = i-1.
function renderTimeline(items) {
  const list = document.getElementById("timeline");
  list.innerHTML = "";
  if (!items.length) {
    list.appendChild(el("div", "empty", "No recorded events yet. Run `twip init`, then start a session or make git changes."));
    return;
  }
  list.appendChild(laneLegend());

  const n = items.length;
  const branchOf = (i) => items[i].branch || "";

  // A session is "active" from its newest event (smallest i) to its oldest
  // (largest i); rows in between (incl. git-ops) keep the session line alive.
  const firstI = {}, lastI = {};
  items.forEach((e, i) => {
    if (!e.session) return;
    if (!(e.session in firstI)) firstI[e.session] = i;
    lastI[e.session] = i;
  });
  const spanAt = new Array(n).fill(null);
  for (const s in firstI) for (let i = firstI[s]; i <= lastI[s]; i++) spanAt[i] = s;

  items.forEach((e, i) => {
    const up = i - 1, down = i + 1; // up = newer (above), down = older (below)
    const sameBranchUp = up >= 0 && branchOf(up) === branchOf(i);
    const sameBranchDown = down < n && branchOf(down) === branchOf(i);
    const sActive = spanAt[i] !== null;
    const sameSessUp = up >= 0 && spanAt[up] === spanAt[i] && sActive;
    const sameSessDown = down < n && spanAt[down] === spanAt[i] && sActive;
    list.appendChild(node(e, i, {
      brTop: sameBranchUp, brBot: sameBranchDown, brDot: !e.session,
      seTop: sameSessUp, seBot: sameSessDown, seDot: !!e.session, sActive,
      branchChanged: !sameBranchUp, // first row of a branch run (reading top-down)
    }));
  });
}

function laneLegend() {
  const lg = el("div", "lane-legend");
  const mk = (cls, text) => {
    const k = el("span", "k " + cls);
    k.appendChild(el("span", "swatch"));
    k.appendChild(el("span", null, text));
    return k;
  };
  lg.appendChild(mk("branch", "branch / git"));
  lg.appendChild(mk("session", "session"));
  return lg;
}

function lane(kind, top, bottom, dot) {
  const l = el("div", "lane " + kind);
  if (top) l.appendChild(el("span", "seg top"));
  if (bottom) l.appendChild(el("span", "seg bottom"));
  if (dot) l.appendChild(el("span", "dot"));
  return l;
}

function node(e, i, r) {
  const n = el("div", "node" + (i % 2 ? " alt" : ""));
  n.dataset.commit = e.commit;
  n.dataset.kind = e.kind;
  n.dataset.family = family(e.kind);

  const rail = el("div", "rail");
  rail.appendChild(lane("branch", r.brTop, r.brBot, r.brDot));
  rail.appendChild(lane("session", r.seTop, r.seBot, r.seDot));
  n.appendChild(rail);

  const body = el("div", "body");
  const row1 = el("div", "row1");
  row1.appendChild(el("span", "kind", e.kind));
  if (r.branchChanged && e.branch) row1.appendChild(el("span", "branch-chip", e.branch));
  if (e.quality) row1.appendChild(el("span", "flag", "!" + e.quality));
  row1.appendChild(el("span", "time", shortTime(e.ts)));
  body.appendChild(row1);
  if (e.detail) body.appendChild(el("div", "annot" + (e.kind === "gitop" ? " code" : ""), e.detail));
  n.appendChild(body);

  n.addEventListener("click", () => select(e.commit, true));
  return n;
}

async function select(commit) {
  document.querySelectorAll(".node.selected").forEach((n) => n.classList.remove("selected"));
  const n = document.querySelector(`.node[data-commit="${commit}"]`);
  if (n) {
    n.classList.add("selected");
    n.scrollIntoView({ block: "nearest" });
  }
  history.replaceState(null, "", "/event/" + commit);
  renderDetail(await getJSON("/api/event/" + commit));
}

/* ---------- detail panel ---------- */

const section = (t) => el("h3", null, t);
const pre = (t) => el("pre", null, t);

function metaRow(dl, k, v) {
  if (v == null || v === "") return;
  dl.appendChild(el("dt", null, k));
  const dd = el("dd");
  if (v instanceof Node) dd.appendChild(v);
  else dd.textContent = v;
  dl.appendChild(dd);
}

function shaLink(sha) {
  const a = el("a", "sha", short(sha));
  a.title = sha;
  a.addEventListener("click", () => openCommit(sha));
  return a;
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
  metaRow(meta, "event", d.commit);
  metaRow(meta, "time", shortTime(d.ts));
  if (d.session) metaRow(meta, "session", `${d.session} (seq ${d.seq})`);
  metaRow(meta, "worktree", d.worktree);
  if (d.head) {
    const c = el("span");
    c.appendChild(shaLink(d.head));
    if (d.branch) c.appendChild(document.createTextNode(` [${d.branch}]`));
    metaRow(meta, "head", c);
  }
  metaRow(meta, "model", d.model);
  p.appendChild(meta);

  if (d.gitop) {
    p.appendChild(section("git operation"));
    const m = el("dl", "meta");
    metaRow(m, "argv", "git " + (d.gitop.argv || []).join(" "));
    const hd = el("span");
    hd.appendChild(shaLink(d.gitop.before_head));
    hd.appendChild(document.createTextNode(" → "));
    hd.appendChild(shaLink(d.gitop.after_head));
    metaRow(m, "head", hd);
    metaRow(m, "exit", String(d.gitop.exit_code));
    metaRow(m, "worktree dirty", String(d.gitop.dirty));
    if (d.gitop.stashed && d.gitop.stashed.length) {
      const sc = el("span");
      d.gitop.stashed.forEach((s, i) => {
        if (i) sc.appendChild(document.createTextNode(", "));
        sc.appendChild(shaLink(s));
      });
      metaRow(m, "stash archived", sc);
    }
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
      const li = el("li", "clickable");
      li.appendChild(el("span", "st " + c.status, c.status));
      li.appendChild(el("span", "path", c.path));
      li.appendChild(el("span", c.inHead ? "inhead" : "nothead", c.inHead ? "✓ in HEAD" : "· not at HEAD"));
      li.addEventListener("click", () => openFileDiff(d.prevTree, d.worktreeTree, c.path));
      ul.appendChild(li);
    });
    p.appendChild(ul);
  }

  if (d.transcript) {
    p.appendChild(section(`transcript Δ (lines ${d.transcriptFrom}–${d.transcriptTo})`));
    p.appendChild(pre(prettyTranscript(d.transcript)));
  }

  if (d.files && d.files.length) {
    const det = el("details", "files");
    det.appendChild(el("summary", null, `worktree snapshot (${d.files.length} files)`));
    const ul = el("ul", "files");
    d.files.forEach((f) => {
      const li = el("li", "clickable", f);
      li.addEventListener("click", () => openBlob(d.worktreeTree, f));
      ul.appendChild(li);
    });
    det.appendChild(ul);
    p.appendChild(det);
  }
}

// Pretty-print each JSONL transcript line; leave non-JSON lines untouched.
function prettyTranscript(text) {
  return (text || "")
    .split("\n")
    .map((l) => {
      l = l.trim();
      if (!l) return "";
      try {
        return JSON.stringify(JSON.parse(l), null, 2);
      } catch (_) {
        return l;
      }
    })
    .filter((x) => x !== "")
    .join("\n\n");
}

/* ---------- object/diff viewer (slide-over) ---------- */

function openViewer(title) {
  const v = document.getElementById("viewer");
  v.innerHTML = "";
  const head = el("div", "viewer-head");
  head.appendChild(el("span", "viewer-title", title));
  const x = el("button", "viewer-close", "×");
  x.addEventListener("click", closeViewer);
  head.appendChild(x);
  v.appendChild(head);
  const body = el("div", "viewer-body");
  v.appendChild(body);
  v.classList.add("open");
  return body;
}
function closeViewer() {
  document.getElementById("viewer").classList.remove("open");
}
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeViewer();
});

async function openCommit(sha) {
  const body = openViewer("commit " + short(sha));
  const d = await getJSON("/api/commit/" + sha);
  body.appendChild(diffPre(d ? d.text : "commit not found"));
}
async function openFileDiff(base, tree, path) {
  const body = openViewer(path);
  const q = `base=${encodeURIComponent(base)}&tree=${encodeURIComponent(tree)}&path=${encodeURIComponent(path)}`;
  const d = await getJSON("/api/filediff?" + q);
  body.appendChild(diffPre(d && d.diff ? d.diff : "(no diff — content identical or file added empty)"));
}
async function openBlob(rev, path) {
  const body = openViewer(path);
  const d = await getJSON(`/api/blob?rev=${encodeURIComponent(rev)}&path=${encodeURIComponent(path)}`);
  const p = el("pre", "blob");
  p.textContent = d ? d.text : "not found";
  body.appendChild(p);
}

function diffPre(text) {
  const p = el("pre", "diff");
  (text || "").split("\n").forEach((line) => {
    let cls = "";
    if (line.startsWith("@@")) cls = "hunk";
    else if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index ")) cls = "meta";
    else if (line.startsWith("+")) cls = "add";
    else if (line.startsWith("-")) cls = "del";
    p.appendChild(el("span", "dl " + cls, line + "\n"));
  });
  return p;
}

boot();
