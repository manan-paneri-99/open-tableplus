'use strict';
// Open TablePlus UI. No framework, no build.
// Each open table or SQL query is a tab with its own pane (DOM), state and
// in-flight request, so switching tabs never reloads or loses anything.
// Open tabs are remembered in localStorage (layout and inputs only, no data).

const $ = (s) => document.querySelector(s);

// h builds DOM nodes; children are appended as text, so values are never parsed as HTML.
function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else if (v !== false && v != null) el.setAttribute(k, v);
  }
  el.append(...kids.flat().filter((k) => k != null));
  return el;
}

async function api(method, path, body, signal) {
  const res = await fetch('/api' + path, {
    method, signal,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch { data = { error: text || res.statusText }; }
  if (!res.ok) throw Object.assign(new Error(data.error || res.statusText), data);
  return data;
}

const enc = encodeURIComponent;
const fmt = (n) => Number(n).toLocaleString();
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };

function errText(e) {
  return [e.message, e.detail && 'Detail: ' + e.detail, e.hint && 'Hint: ' + e.hint, e.code && `(${e.code})`]
    .filter(Boolean).join('\n');
}

// Sidebar context (which connection/database the tree shows) and open tabs.
const S = { conns: [], conn: null, db: null, dbs: null, tree: [], ctxSeq: 0, tabs: [], active: null, nextId: 1, sqlCount: 0 };

const connName = (id) => S.conns.find((c) => c.id === id)?.name ?? '?';
const activeTab = () => S.tabs.find((t) => t.id === S.active);
const sameTable = (a, b) => a?.kind === 'table' && a.conn === b.conn && a.db === b.db && a.schema === b.schema && a.table === b.table;

// ---------- sidebar ----------

// selectContext points the sidebar at a connection/database and loads its tree.
// Later calls win: a slow earlier load never overwrites a newer one.
async function selectContext(connId, db) {
  const seq = ++S.ctxSeq;
  const conn = S.conns.find((c) => c.id === connId) || S.conns[0];
  if (!conn) { renderConnSelect(); openDialog(null); return; }
  sideMsg('');
  try {
    if (conn.id !== S.conn?.id || !S.dbs) {
      S.conn = conn; S.db = null; S.dbs = null; S.tree = [];
      renderConnSelect(); renderDbSelect(); renderTree();
      const dbs = await api('GET', `/c/${enc(conn.id)}/dbs`);
      if (seq !== S.ctxSeq) return;
      S.dbs = dbs;
    }
    const want = S.dbs.includes(db) ? db : S.dbs.includes(conn.database) ? conn.database : S.dbs[0];
    if (want !== S.db) {
      S.db = want; S.tree = [];
      renderDbSelect(); renderTree();
      const tree = await api('GET', `/c/${enc(conn.id)}/${enc(want)}/tree`);
      if (seq !== S.ctxSeq) return;
      S.tree = tree;
    }
  } catch (e) {
    if (seq === S.ctxSeq) sideMsg(`Can't open ${conn.name}: ${errText(e)}`);
  }
  renderDbSelect(); renderTree();
}

function sideMsg(msg) { $('#sideMsg').textContent = msg; $('#sideMsg').hidden = !msg; }

function renderConnSelect() {
  $('#conn').replaceChildren(...S.conns.map((c) => h('option', { value: c.id, selected: c.id === S.conn?.id }, c.name)));
  $('#editConn').disabled = !S.conn;
  const env = S.conn?.env;
  Object.assign($('#env'), { textContent: env || '', className: env ? 'env-' + env : '', hidden: !env });
}

function renderDbSelect() {
  $('#db').replaceChildren(...(S.dbs || []).map((d) => h('option', { value: d, selected: d === S.db }, d)));
}

function renderTree() {
  const f = $('#filterTables').value.trim().toLowerCase();
  const bySchema = new Map();
  for (const t of S.tree) {
    if (f && !`${t.schema}.${t.name}`.toLowerCase().includes(f)) continue;
    if (!bySchema.has(t.schema)) bySchema.set(t.schema, []);
    bySchema.get(t.schema).push(t);
  }
  const spec = (t) => ({ kind: 'table', conn: S.conn.id, db: S.db, schema: t.schema, table: t.name });
  $('#tree').replaceChildren(...[...bySchema].map(([schema, rels]) =>
    h('details', { open: true },
      h('summary', {}, schema),
      rels.map((t) => h('button', {
        class: [sameTable(activeTab(), spec(t)) && 'on', S.tabs.some((x) => sameTable(x, spec(t))) && 'open'].filter(Boolean).join(' ') || null,
        onclick: () => openTab(spec(t)),
      }, h('span', { class: 'n' }, t.name), h('small', {}, t.kind === 'v' ? 'view' : t.approxRows > 0 ? '~' + fmt(t.approxRows) : '')))
    )));
  if (!bySchema.size && S.tree.length) $('#tree').append(h('p', {}, 'No match.'));
}

// ---------- tabs ----------

// openTab focuses an existing tab for the same table, or creates a new one.
function openTab(spec, { focus = true } = {}) {
  if (spec.kind === 'table') {
    const existing = S.tabs.find((t) => sameTable(t, spec));
    if (existing) { if (focus) activate(existing); return existing; }
  }
  const tab = {
    id: S.nextId++, kind: spec.kind, conn: spec.conn, db: spec.db, schema: spec.schema, table: spec.table,
    t: { q: '', filters: [], sort: '', desc: false, page: 0, ...spec.t, columns: [] },
    sql: spec.sql ?? '', n: spec.kind === 'sql' ? (spec.n || ++S.sqlCount) : 0,
    loaded: false, inflight: null, el: {},
  };
  S.sqlCount = Math.max(S.sqlCount, tab.n);
  (tab.kind === 'table' ? buildTablePane : buildSqlPane)(tab);
  tab.el.pane.hidden = true;
  $('#panes').append(tab.el.pane);
  S.tabs.push(tab);
  if (focus) activate(tab); else renderTabs();
  saveTabs();
  return tab;
}

function activate(tab) {
  S.active = tab.id;
  for (const t of S.tabs) t.el.pane.hidden = t !== tab;
  $('#empty').hidden = true;
  renderTabs();
  if (tab.conn !== S.conn?.id || tab.db !== S.db) selectContext(tab.conn, tab.db); else renderTree();
  if (!tab.loaded) {
    tab.loaded = true;
    if (tab.kind === 'table') loadRows(tab);
  }
  if (tab.kind === 'sql') tab.el.sql.focus();
  saveTabs();
}

function closeTab(tab) {
  tab.inflight?.abort();
  tab.el.pane.remove();
  const i = S.tabs.indexOf(tab);
  S.tabs.splice(i, 1);
  if (S.active === tab.id) {
    const next = S.tabs[i] || S.tabs[i - 1];
    if (next) activate(next);
    else { S.active = null; $('#empty').hidden = false; renderTabs(); renderTree(); }
  } else renderTabs();
  saveTabs();
}

const tabLabel = (t) => t.kind === 'sql' ? `SQL ${t.n}` : t.table;

function renderTabs() {
  // Show the database next to the name only when tabs span more than one.
  const many = new Set(S.tabs.map((t) => t.conn + '/' + t.db)).size > 1;
  $('#tabs').replaceChildren(...S.tabs.map((t) => h('div', {
    class: 'tab' + (t.id === S.active ? ' on' : '') + ((env) => env ? ` env env-${env}` : '')(S.conns.find((c) => c.id === t.conn)?.env), role: 'tab', 'aria-selected': String(t.id === S.active), title: `${connName(t.conn)} / ${t.db} / ` + (t.kind === 'sql' ? 'SQL' : `${t.schema}.${t.table}`),
    onclick: () => activate(t),
    onauxclick: (e) => { if (e.button === 1) { e.preventDefault(); closeTab(t); } },
  },
    h('span', { class: 'n' }, tabLabel(t)),
    many ? h('small', {}, t.db) : null,
    h('button', { class: 'x', 'aria-label': `Close ${tabLabel(t)}`, onclick: (e) => { e.stopPropagation(); closeTab(t); } }, '×'),
  )));
  $('#tabs').querySelector('.on')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
}

// Remembered across reloads: which tabs are open and their inputs. Never results.
const STORE = 'open-tableplus.tabs';
const saveTabs = debounce(() => {
  try {
    localStorage.setItem(STORE, JSON.stringify({
      active: S.tabs.findIndex((t) => t.id === S.active),
      tabs: S.tabs.map((t) => ({
        kind: t.kind, conn: t.conn, db: t.db, schema: t.schema, table: t.table, sql: t.sql, n: t.n,
        t: { q: t.t.q, filters: t.t.filters, sort: t.t.sort, desc: t.t.desc, page: t.t.page },
      })),
    }));
  } catch { /* storage unavailable: tabs just won't survive a reload */ }
}, 300);

function restoreTabs() {
  let saved;
  try { saved = JSON.parse(localStorage.getItem(STORE) || 'null'); } catch { saved = null; }
  if (!saved?.tabs?.length) return null;
  const tabs = saved.tabs.filter((s) => S.conns.some((c) => c.id === s.conn)).map((s) => openTab(s, { focus: false }));
  return tabs[saved.active] || tabs[0] || null;
}

// ---------- table tabs ----------

function buildTablePane(tab) {
  const e = tab.el;
  const pending = () => e.search.classList.add('dirty'); // edits wait for Enter / Search
  const onEnter = (ev) => { if (ev.key === 'Enter') { ev.preventDefault(); apply(tab); } };
  e.pending = pending; e.onEnter = onEnter;
  e.q = h('input', {
    type: 'search', class: 'q', placeholder: 'Search all columns… (Enter)', 'aria-label': 'Search all columns', value: tab.t.q,
    oninput: (ev) => { tab.t.q = ev.target.value; pending(); saveTabs(); }, onkeydown: onEnter,
  });
  e.search = h('button', { title: 'Run search (Enter)', onclick: () => apply(tab) }, 'Search');
  e.prev = h('button', { 'aria-label': 'Previous page', onclick: () => { tab.t.page--; loadRows(tab); } }, '‹');
  e.next = h('button', { 'aria-label': 'Next page', onclick: () => { tab.t.page++; loadRows(tab); } }, '›');
  e.pageInfo = h('span');
  e.filters = h('div', { class: 'filters' });
  e.status = h('div', { class: 'status', role: 'status' });
  e.grid = h('div', { class: 'grid' });
  e.cell = h('pre', { class: 'cell', hidden: true });
  e.dataBtn = h('button', { class: 'on', onclick: () => showStructure(tab, false) }, 'Data');
  e.structBtn = h('button', { onclick: () => showStructure(tab, true) }, 'Structure');
  e.tools = h('span', { class: 'row grow' }, e.q,
    h('button', { onclick: () => addFilter(tab) }, '+ Filter'), e.search,
    h('button', { title: 'Download every matching row (current search, filters and sort)', onclick: () => exportCSV(tab, tableReq(tab)) }, 'Export CSV'),
    h('span', { class: 'grow' }), e.prev, e.pageInfo, e.next);
  e.structure = h('div', { class: 'structure', hidden: true });
  e.pane = h('section', { class: 'pane' },
    h('div', { class: 'row' }, h('span', { class: 'seg' }, e.dataBtn, e.structBtn), e.tools),
    e.filters, e.status, e.grid, e.structure, e.cell);
}

async function showStructure(tab, on) {
  const e = tab.el;
  e.dataBtn.classList.toggle('on', !on); e.structBtn.classList.toggle('on', on);
  e.tools.hidden = e.filters.hidden = e.grid.hidden = on;
  e.structure.hidden = !on; e.cell.hidden = true;
  if (!on || e.structure.childElementCount) return;
  setStatus(tab, 'Loading…');
  try {
    const st = await api('GET', `/c/${enc(tab.conn)}/${enc(tab.db)}/structure?schema=${enc(tab.schema)}&table=${enc(tab.table)}`);
    e.structure.replaceChildren(...[['columns', 'Columns'], ['indexes', 'Indexes'], ['foreignKeys', 'Foreign keys'], ['referencedBy', 'Referenced by']]
      .map(([k, label]) => h('section', {}, h('h3', {}, label), st[k].rows.length ? gridTable(tab, st[k]) : h('p', { class: 'status' }, 'None'))));
    setStatus(tab, '');
  } catch (err) { setStatus(tab, errText(err), true); }
}

// tableReq is the table view as the server sees it: applied search, filters and sort.
function tableReq(tab) {
  const t = tab.t;
  return {
    schema: tab.schema, table: tab.table, q: t.q, sort: t.sort, desc: t.desc,
    filters: t.filters.filter((f) => f.col && (f.op.startsWith('is') || f.val !== '')),
  };
}

function setStatus(tab, msg, err) {
  tab.el.status.className = 'status' + (err ? ' err' : '');
  tab.el.status.textContent = msg;
}

function apply(tab) { tab.t.page = 0; loadRows(tab); }

async function loadRows(tab) {
  const t = tab.t;
  tab.inflight?.abort();
  const ctl = tab.inflight = new AbortController();
  tab.el.search.classList.remove('dirty'); // whatever is in the boxes is what's being loaded
  setStatus(tab, 'Loading…');
  saveTabs();
  try {
    const res = await api('POST', `/c/${enc(tab.conn)}/${enc(tab.db)}/rows`, { ...tableReq(tab), page: t.page }, ctl.signal);
    const firstLoad = !t.columns.length;
    t.columns = res.columns;
    if (firstLoad) renderFilters(tab);
    renderGrid(tab, res, true);
    const from = t.page * 100 + 1, to = t.page * 100 + res.rows.length;
    tab.el.pageInfo.textContent = res.rows.length
      ? `${fmt(from)}–${fmt(to)}${res.approxTotal != null ? ' of ~' + fmt(res.approxTotal) : res.more ? '+' : ''}`
      : 'no rows';
    tab.el.prev.disabled = t.page === 0;
    tab.el.next.disabled = !res.more;
    setStatus(tab, `${res.ms} ms`);
  } catch (e) {
    if (e.name !== 'AbortError') setStatus(tab, errText(e), true);
  } finally {
    if (tab.inflight === ctl) tab.inflight = null;
  }
}

const numeric = /^(smallint|integer|bigint|numeric|real|double precision|money|oid)\b/;

// renderGrid shows a result in the tab's main grid, where cells can be selected and copied.
function renderGrid(tab, res, sortable) {
  tab.res = res; tab.sel = null;
  tab.el.grid.replaceChildren(gridTable(tab, res, sortable, true));
  tab.el.grid.scrollTo(0, 0);
  tab.el.cell.hidden = true;
}

function gridTable(tab, res, sortable, selectable) {
  const t = tab.t;
  const cut = new Set((res.cut || []).map(([r, c]) => r + ',' + c));
  const num = res.columns.map((c) => numeric.test(c.type));
  const head = h('tr', {}, res.columns.map((c) => h('th', {
    class: sortable ? 'sortable' : null,
    title: sortable ? 'Sort' : null,
    onclick: sortable ? () => {
      if (t.sort === c.name) t.desc = !t.desc; else { t.sort = c.name; t.desc = false; }
      t.page = 0; loadRows(tab);
    } : null,
  }, c.name + (c.pk ? ' 🔑' : '') + (sortable && t.sort === c.name ? (t.desc ? ' ↓' : ' ↑') : ''), h('small', {}, c.type))));
  const body = res.rows.map((row, r) => h('tr', {}, row.map((v, i) => h('td', {
    class: [v === null && 'null', num[i] && 'num', cut.has(r + ',' + i) && 'cut'].filter(Boolean).join(' ') || null,
    onmousedown: (e) => { if (e.shiftKey) e.preventDefault(); }, // shift-click extends the cell selection, not a text selection
    onclick: (e) => { if (selectable) select(tab, r, i, e.shiftKey); showCell(tab, res.columns[i], v, cut.has(r + ',' + i)); },
  }, v === null ? 'NULL' : v.length > 300 ? v.slice(0, 300) : v))));
  return h('table', {}, h('thead', {}, head), h('tbody', {}, body));
}

// Selection is a rectangle from the clicked cell to the shift-clicked one.
const bounds = (s) => [Math.min(s.r0, s.r1), Math.max(s.r0, s.r1), Math.min(s.c0, s.c1), Math.max(s.c0, s.c1)];

function select(tab, r, c, extend) {
  tab.sel = extend && tab.sel ? { ...tab.sel, r1: r, c1: c } : { r0: r, c0: c, r1: r, c1: c };
  const [r0, r1, c0, c1] = bounds(tab.sel);
  tab.el.grid.querySelectorAll('tbody tr').forEach((tr, ri) =>
    [...tr.children].forEach((td, ci) => td.classList.toggle('on', ri >= r0 && ri <= r1 && ci >= c0 && ci <= c1)));
}

// ⌘/Ctrl+C on a cell selection copies TSV; copying text in inputs or the cell viewer is left alone.
document.addEventListener('copy', (e) => {
  const tab = activeTab();
  if (!tab?.sel || tab.el.grid.hidden || /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName) || !getSelection().isCollapsed) return;
  const [r0, r1, c0, c1] = bounds(tab.sel);
  const field = (v) => v === null ? '' : /[\t\n\r"]/.test(v) ? `"${v.replaceAll('"', '""')}"` : v;
  e.clipboardData.setData('text/plain', tab.res.rows.slice(r0, r1 + 1).map((row) => row.slice(c0, c1 + 1).map(field).join('\t')).join('\n'));
  e.preventDefault();
  const cut = (tab.res.cut || []).filter(([r, c]) => r >= r0 && r <= r1 && c >= c0 && c <= c1).length;
  setStatus(tab, `Copied ${(r1 - r0 + 1) * (c1 - c0 + 1)} cells.` + (cut ? ` ${cut} ${cut === 1 ? 'was' : 'were'} cut at 10 KB; use Export CSV for full values.` : ''), cut > 0);
});

// Export is a form POST into a hidden frame: the browser streams the CSV to
// disk. Only an error (JSON) ever renders in the frame.
let exportTab = null;
function exportCSV(tab, req) {
  exportTab = tab;
  const f = h('form', { method: 'POST', action: `/api/c/${enc(tab.conn)}/${enc(tab.db)}/export`, target: 'dl', hidden: true },
    h('input', { type: 'hidden', name: 'req', value: JSON.stringify(req) }));
  document.body.append(f); f.submit(); f.remove();
  setStatus(tab, 'Export started: the file downloads as rows stream in.');
}
$('#dl').onload = () => {
  let err;
  try { err = JSON.parse($('#dl').contentDocument.body.textContent); } catch { return; }
  if (exportTab && err?.error) setStatus(exportTab, 'Export failed: ' + errText({ ...err, message: err.error }), true);
};

function showCell(tab, col, v, wasCut) {
  tab.el.cell.hidden = false;
  tab.el.cell.textContent = `${col.name} (${col.type})${wasCut ? ' — first 10 KB only' : ''}\n\n` + (v === null ? 'NULL' : v === '' ? "'' (empty string)" : v);
}

const ops = ['contains', '=', '!=', '<', '<=', '>', '>=', 'is null', 'is not null'];

function renderFilters(tab) {
  const t = tab.t, e = tab.el;
  const changed = () => { e.pending(); saveTabs(); };
  e.filters.replaceChildren(...t.filters.map((f, i) => h('div', { class: 'row' },
    h('select', { 'aria-label': 'Column', onchange: (ev) => { f.col = ev.target.value; changed(); } },
      t.columns.map((c) => h('option', { value: c.name, selected: c.name === f.col }, c.name))),
    h('select', { 'aria-label': 'Operator', onchange: (ev) => { f.op = ev.target.value; renderFilters(tab); changed(); } },
      ops.map((o) => h('option', { selected: o === f.op }, o))),
    f.op.startsWith('is') ? null : h('input', { 'aria-label': 'Value', value: f.val, oninput: (ev) => { f.val = ev.target.value; changed(); }, onkeydown: e.onEnter }),
    h('button', { 'aria-label': 'Remove filter', onclick: () => { t.filters.splice(i, 1); renderFilters(tab); changed(); } }, '×'),
  )));
}

function addFilter(tab) {
  if (!tab.t.columns.length) return;
  tab.t.filters.push({ col: tab.t.columns[0].name, op: 'contains', val: '' });
  renderFilters(tab);
  tab.el.filters.lastElementChild.querySelector('input')?.focus();
}

// ---------- SQL tabs ----------

function buildSqlPane(tab) {
  const e = tab.el;
  e.sql = h('textarea', {
    class: 'sql', spellcheck: 'false', 'aria-label': 'SQL',
    placeholder: 'SELECT …   (⌘/Ctrl+Enter runs; runs the selection if there is one)',
    oninput: (ev) => { tab.sql = ev.target.value; saveTabs(); },
    onkeydown: (ev) => { if (ev.key === 'Enter' && (ev.metaKey || ev.ctrlKey)) { ev.preventDefault(); runSQL(tab); } },
  });
  e.sql.value = tab.sql;
  e.run = h('button', { onclick: () => runSQL(tab) }, 'Run');
  e.cancel = h('button', { disabled: true, onclick: () => tab.inflight?.abort() }, 'Cancel');
  e.status = h('div', { class: 'status', role: 'status' }, 'Read-only: every statement runs inside a READ ONLY transaction.');
  e.grid = h('div', { class: 'grid' });
  e.cell = h('pre', { class: 'cell', hidden: true });
  e.hist = h('div', { class: 'history', hidden: true });
  e.pane = h('section', { class: 'pane' }, e.sql,
    h('div', { class: 'row' }, e.run, e.cancel,
      h('button', { title: 'Download every row of the query', onclick: () => sqlText(tab).sql.trim() && exportCSV(tab, { sql: sqlText(tab).sql }) }, 'Export CSV'),
      h('span', { class: 'grow' }),
      h('button', { onclick: () => { e.hist.hidden = !e.hist.hidden; renderHistory(tab); } }, 'History')),
    e.hist, e.status, e.grid, e.cell);
}

// sqlText is the selection if there is one, else the whole editor.
function sqlText(tab) {
  const ta = tab.el.sql, selected = ta.selectionStart !== ta.selectionEnd;
  return { sql: selected ? ta.value.slice(ta.selectionStart, ta.selectionEnd) : ta.value, offset: selected ? ta.selectionStart : 0 };
}

// History is recorded by the server (history.json in the config dir) for
// every statement run here; this only shows it.
async function renderHistory(tab) {
  if (tab.el.hist.hidden) return;
  let items;
  try { items = await api('GET', `/c/${enc(tab.conn)}/${enc(tab.db)}/history`); }
  catch (e) { setStatus(tab, errText(e), true); return; }
  tab.el.hist.replaceChildren(...items.map((x) => h('button', {
    class: x.ok ? null : 'err', title: x.sql,
    onclick: () => { tab.el.sql.value = tab.sql = x.sql; saveTabs(); tab.el.sql.focus(); },
  }, h('small', {}, `${new Date(x.at).toLocaleString()} · ${x.info}`), h('span', {}, x.sql.trim().split('\n')[0]))));
  if (!items.length) tab.el.hist.append(h('p', { class: 'status' }, 'No queries yet.'));
}

async function runSQL(tab) {
  const ta = tab.el.sql;
  const { sql, offset } = sqlText(tab);
  if (!sql.trim()) return;
  tab.inflight?.abort();
  const ctl = tab.inflight = new AbortController();
  const started = Date.now();
  const tick = setInterval(() => setStatus(tab, `Running… ${((Date.now() - started) / 1000).toFixed(1)} s`), 100);
  tab.el.run.disabled = true; tab.el.cancel.disabled = false;
  try {
    const res = await api('POST', `/c/${enc(tab.conn)}/${enc(tab.db)}/sql`, { sql }, ctl.signal);
    renderGrid(tab, res, false);
    setStatus(tab, `${res.tag}${res.more ? ` — first ${res.rows.length} rows shown` : ''} · ${res.ms} ms`);
  } catch (e) {
    if (e.name === 'AbortError') setStatus(tab, 'Cancelled.');
    else {
      tab.res = tab.sel = null; tab.el.grid.replaceChildren(); // don't leave the previous result under the error
      setStatus(tab, errText(e) + (e.position ? `\nAt character ${e.position}.` : ''), true);
      if (e.position && S.active === tab.id) { const p = offset + e.position - 1; ta.focus(); ta.setSelectionRange(p, p + 1); }
    }
  } finally {
    clearInterval(tick);
    if (tab.inflight === ctl) tab.inflight = null;
    tab.el.run.disabled = false; tab.el.cancel.disabled = true;
    renderHistory(tab);
  }
}

// ---------- connections ----------

$('#sqlBtn').onclick = () => S.conn && S.db && openTab({ kind: 'sql', conn: S.conn.id, db: S.db });
$('#conn').onchange = (e) => selectContext(e.target.value, '');
$('#db').onchange = (e) => selectContext(S.conn.id, e.target.value);
$('#filterTables').oninput = renderTree;
$('#newConn').onclick = () => openDialog(null);
$('#editConn').onclick = () => openDialog(S.conn);

let editing = null;

function openDialog(conn) {
  editing = conn;
  const f = $('#connForm');
  f.reset();
  for (const k of ['name', 'host', 'port', 'user', 'database', 'sslmode', 'env']) if (conn) f.elements[k].value = conn[k] ?? '';
  f.elements.password.placeholder = conn?.hasPassword ? 'saved — leave blank to keep' : 'blank = use ~/.pgpass';
  $('#dlgDelete').hidden = !conn;
  dlgMsg('');
  $('#connDlg').showModal();
}

function dlgMsg(msg, err) { const el = $('#dlgMsg'); el.textContent = msg; el.className = err ? 'err' : ''; }

function formConn() {
  const f = $('#connForm').elements;
  return {
    id: editing?.id || '', name: f.name.value.trim(), host: f.host.value.trim(), port: Number(f.port.value) || 5432,
    user: f.user.value.trim(), password: f.password.value, database: f.database.value.trim() || 'postgres', sslmode: f.sslmode.value, env: f.env.value,
  };
}

$('#dlgTest').onclick = async () => {
  dlgMsg('Testing…');
  try { const r = await api('POST', '/conns/test', formConn()); dlgMsg(`Connected. PostgreSQL ${r.version}`); }
  catch (e) { dlgMsg(errText(e), true); }
};

$('#dlgSave').onclick = async () => {
  if (!$('#connForm').reportValidity()) return;
  try {
    const saved = await api('PUT', '/conns', formConn());
    S.conns = await api('GET', '/conns');
    S.conn = null; // force reload of dbs/tree with the new settings
    $('#connDlg').close();
    renderTabs(); // names and environments may have changed
    const a = activeTab();
    selectContext(saved.id, a?.conn === saved.id ? a.db : '');
  } catch (e) { dlgMsg(errText(e), true); }
};

$('#dlgDelete').onclick = async () => {
  if (!editing || !confirm(`Delete connection "${editing.name}"? Its open tabs will close.`)) return;
  try {
    const id = editing.id;
    await api('DELETE', `/conns/${enc(id)}`);
    S.conns = await api('GET', '/conns');
    S.conn = null;
    $('#connDlg').close();
    for (const t of S.tabs.filter((t) => t.conn === id)) closeTab(t);
    selectContext(activeTab()?.conn ?? S.conns[0]?.id, activeTab()?.db ?? '');
  } catch (e) { dlgMsg(errText(e), true); }
};

// ---------- start ----------

(async () => {
  try { S.conns = await api('GET', '/conns'); }
  catch (e) { sideMsg(errText(e)); return; }
  renderConnSelect();
  if (!S.conns.length) { openDialog(null); return; }
  const restored = restoreTabs();
  if (restored) activate(restored); else selectContext(S.conns[0].id, '');
})();
