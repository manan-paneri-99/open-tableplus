'use strict';
// Open TablePlus UI. No framework, no build. Everything the user sees comes
// from the URL hash (#/conn/db/schema/table or #/conn/db/:sql), so two browser
// tabs are two independent views.

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

const S = {
  conns: [], conn: null, db: null, dbs: [], tree: [],
  view: null,                  // { schema, table } or { sql: true }
  t: null,                     // table view state: { q, filters, sort, desc, page }
  inflight: null,              // AbortController of the running request
};

function setStatus(msg, err) {
  const el = $('#status');
  el.className = err ? 'err' : '';
  el.textContent = msg;
}

function errText(e) {
  return [e.message, e.detail && 'Detail: ' + e.detail, e.hint && 'Hint: ' + e.hint, e.code && `(${e.code})`]
    .filter(Boolean).join('\n');
}

// ---------- routing ----------

function parseHash() {
  const [conn, db, a, b] = location.hash.replace(/^#\/?/, '').split('/').map(decodeURIComponent);
  return { conn, db, view: a === ':sql' ? { sql: true } : a && b ? { schema: a, table: b } : null };
}

function go(conn, db, view) {
  let hash = `#/${enc(conn)}/${enc(db)}`;
  if (view?.sql) hash += '/:sql';
  else if (view) hash += `/${enc(view.schema)}/${enc(view.table)}`;
  if (location.hash === hash) route(); else location.hash = hash;
}

async function route() {
  const want = parseHash();
  const conn = S.conns.find((c) => c.id === want.conn) || S.conns[0];
  if (!conn) { renderConnSelect(); openDialog(null); return; }
  try {
    if (conn.id !== S.conn?.id) {
      S.conn = conn; S.db = null;
      renderConnSelect();
      S.dbs = await api('GET', `/c/${enc(conn.id)}/dbs`);
    }
    const db = S.dbs.includes(want.db) ? want.db : S.dbs.includes(conn.database) ? conn.database : S.dbs[0];
    if (db !== S.db) {
      S.db = db; S.tree = [];
      renderDbSelect();
      S.tree = await api('GET', `/c/${enc(conn.id)}/${enc(db)}/tree`);
    }
  } catch (e) {
    S.dbs = S.dbs || []; renderDbSelect(); renderTree();
    setStatus(`Can't open ${conn.name}: ${errText(e)}`, true);
    return;
  }
  // Normalise the URL to what we actually opened (e.g. the default database).
  const view = want.db === S.db ? want.view : null;
  let hash = `#/${enc(conn.id)}/${enc(S.db)}`;
  if (view?.sql) hash += '/:sql';
  else if (view) hash += `/${enc(view.schema)}/${enc(view.table)}`;
  if (location.hash !== hash) history.replaceState(null, '', hash);
  show(view);
  renderTree();
}

// ---------- sidebar ----------

function renderConnSelect() {
  $('#conn').replaceChildren(...S.conns.map((c) => h('option', { value: c.id, selected: c.id === S.conn?.id }, c.name)));
  $('#editConn').disabled = !S.conn;
}

function renderDbSelect() {
  $('#db').replaceChildren(...(S.dbs || []).map((d) => h('option', { value: d, selected: d === S.db }, d)));
}

const kindLabel = { r: 'table', p: 'table', v: 'view', m: 'matview', f: 'foreign' };

function renderTree() {
  const f = $('#filterTables').value.trim().toLowerCase();
  const bySchema = new Map();
  for (const t of S.tree) {
    if (f && !`${t.schema}.${t.name}`.toLowerCase().includes(f)) continue;
    if (!bySchema.has(t.schema)) bySchema.set(t.schema, []);
    bySchema.get(t.schema).push(t);
  }
  const v = S.view;
  $('#tree').replaceChildren(...[...bySchema].map(([schema, rels]) =>
    h('details', { open: true },
      h('summary', {}, schema),
      rels.map((t) => h('a', {
        href: `#/${enc(S.conn.id)}/${enc(S.db)}/${enc(t.schema)}/${enc(t.name)}`,
        class: v && v.schema === t.schema && v.table === t.name ? 'on' : null,
        title: kindLabel[t.kind],
      }, h('span', { class: 'n' }, t.name), h('small', {}, t.kind === 'v' ? 'view' : t.approxRows > 0 ? '~' + fmt(t.approxRows) : '')))
    )));
  if (!bySchema.size) $('#tree').append(h('p', { class: 'muted' }, S.tree.length ? 'No match.' : ''));
}

// ---------- main pane ----------

function show(view) {
  S.inflight?.abort();
  const same = view && S.view && view.schema === S.view.schema && view.table === S.view.table && !view.sql;
  S.view = view;
  $('#tablePane').hidden = !view || view.sql;
  $('#sqlPane').hidden = !view?.sql;
  $('#cell').hidden = true;
  if (!view) {
    $('#title').textContent = `${S.conn.name} / ${S.db}`;
    $('#grid').replaceChildren();
    setStatus(S.tree.length ? 'Pick a table, or open the SQL query.' : 'No tables in this database.');
    return;
  }
  if (view.sql) {
    $('#title').textContent = `SQL — ${S.conn.name} / ${S.db}`;
    $('#grid').replaceChildren();
    setStatus('Read-only: every statement runs inside a READ ONLY transaction.');
    $('#sql').focus();
    return;
  }
  $('#title').textContent = `${view.schema}.${view.table}`;
  if (!same) {
    S.t = { q: '', filters: [], sort: '', desc: false, page: 0, columns: [] };
    $('#q').value = '';
    renderFilters();
  }
  loadRows();
}

async function loadRows() {
  const v = S.view, t = S.t;
  S.inflight?.abort();
  const ctl = S.inflight = new AbortController();
  $('#search').classList.remove('dirty'); // whatever is in the boxes is what's being loaded
  setStatus('Loading…');
  try {
    const res = await api('POST', `/c/${enc(S.conn.id)}/${enc(S.db)}/rows`, {
      schema: v.schema, table: v.table, q: t.q, sort: t.sort, desc: t.desc, page: t.page,
      filters: t.filters.filter((f) => f.col && (f.op.startsWith('is') || f.val !== '')),
    }, ctl.signal);
    const firstLoad = !t.columns.length;
    t.columns = res.columns;
    if (firstLoad) renderFilters();
    renderGrid(res, true);
    const from = t.page * 100 + 1, to = t.page * 100 + res.rows.length;
    $('#pageInfo').textContent = res.rows.length
      ? `${fmt(from)}–${fmt(to)}${res.approxTotal != null ? ' of ~' + fmt(res.approxTotal) : res.more ? '+' : ''}`
      : 'no rows';
    $('#prev').disabled = t.page === 0;
    $('#next').disabled = !res.more;
    setStatus(`${res.ms} ms`);
  } catch (e) {
    if (e.name !== 'AbortError') setStatus(errText(e), true);
  }
}

const numeric = /^(smallint|integer|bigint|numeric|real|double precision|money|oid)\b/;

function renderGrid(res, sortable) {
  const t = S.t;
  const cut = new Set((res.cut || []).map(([r, c]) => r + ',' + c));
  const num = res.columns.map((c) => numeric.test(c.type));
  const head = h('tr', {}, res.columns.map((c) => h('th', {
    class: sortable ? 'sortable' : null,
    title: sortable ? 'Sort' : null,
    onclick: sortable ? () => {
      if (t.sort === c.name) t.desc = !t.desc; else { t.sort = c.name; t.desc = false; }
      t.page = 0; loadRows();
    } : null,
  }, c.name + (c.pk ? ' 🔑' : '') + (sortable && t.sort === c.name ? (t.desc ? ' ↓' : ' ↑') : ''), h('small', {}, c.type))));
  const body = res.rows.map((row, r) => h('tr', {}, row.map((v, i) => h('td', {
    class: [v === null && 'null', num[i] && 'num', cut.has(r + ',' + i) && 'cut'].filter(Boolean).join(' ') || null,
    onclick: (e) => showCell(e.currentTarget, res.columns[i], v, cut.has(r + ',' + i)),
  }, v === null ? 'NULL' : v.length > 300 ? v.slice(0, 300) : v))));
  $('#grid').replaceChildren(h('table', {}, h('thead', {}, head), h('tbody', {}, body)));
}

function showCell(td, col, v, wasCut) {
  document.querySelectorAll('td.on').forEach((x) => x.classList.remove('on'));
  td.classList.add('on');
  const el = $('#cell');
  el.hidden = false;
  el.textContent = `${col.name} (${col.type})${wasCut ? ' — first 10 KB only' : ''}\n\n` + (v === null ? 'NULL' : v === '' ? "'' (empty string)" : v);
}

// ---------- filters ----------

const ops = ['contains', '=', '!=', '<', '<=', '>', '>=', 'is null', 'is not null'];
// Search and filter edits are pending until Enter or the Search button.
function apply() { S.t.page = 0; loadRows(); }
function edited() { $('#search').classList.add('dirty'); }
const onEnter = (e) => { if (e.key === 'Enter') { e.preventDefault(); apply(); } };

function renderFilters() {
  const t = S.t;
  $('#filters').replaceChildren(...t.filters.map((f, i) => h('div', { class: 'row' },
    h('select', { 'aria-label': 'Column', onchange: (e) => { f.col = e.target.value; edited(); } },
      t.columns.map((c) => h('option', { value: c.name, selected: c.name === f.col }, c.name))),
    h('select', { 'aria-label': 'Operator', onchange: (e) => { f.op = e.target.value; renderFilters(); edited(); } },
      ops.map((o) => h('option', { selected: o === f.op }, o))),
    f.op.startsWith('is') ? null : h('input', { 'aria-label': 'Value', value: f.val, oninput: (e) => { f.val = e.target.value; edited(); }, onkeydown: onEnter }),
    h('button', { 'aria-label': 'Remove filter', onclick: () => { t.filters.splice(i, 1); renderFilters(); edited(); } }, '×'),
  )));
}

$('#addFilter').onclick = () => {
  if (!S.t.columns.length) return;
  S.t.filters.push({ col: S.t.columns[0].name, op: 'contains', val: '' });
  renderFilters();
  $('#filters').lastElementChild.querySelector('input')?.focus();
};
$('#q').oninput = (e) => { S.t.q = e.target.value; edited(); };
$('#q').onkeydown = onEnter;
$('#search').onclick = apply;
$('#prev').onclick = () => { S.t.page--; loadRows(); };
$('#next').onclick = () => { S.t.page++; loadRows(); };

// ---------- SQL ----------

async function runSQL() {
  const ta = $('#sql');
  const selected = ta.selectionStart !== ta.selectionEnd;
  const offset = selected ? ta.selectionStart : 0;
  const sql = selected ? ta.value.slice(ta.selectionStart, ta.selectionEnd) : ta.value;
  if (!sql.trim()) return;
  S.inflight?.abort();
  const ctl = S.inflight = new AbortController();
  const started = Date.now();
  const tick = setInterval(() => setStatus(`Running… ${((Date.now() - started) / 1000).toFixed(1)} s`), 100);
  $('#run').disabled = true; $('#cancel').disabled = false;
  try {
    const res = await api('POST', `/c/${enc(S.conn.id)}/${enc(S.db)}/sql`, { sql }, ctl.signal);
    renderGrid(res, false);
    setStatus(`${res.tag}${res.more ? ` — first ${res.rows.length} rows shown` : ''} · ${res.ms} ms`);
  } catch (e) {
    if (e.name === 'AbortError') setStatus('Cancelled.');
    else {
      setStatus(errText(e) + (e.position ? `\nAt character ${e.position}.` : ''), true);
      if (e.position) { const p = offset + e.position - 1; ta.focus(); ta.setSelectionRange(p, p + 1); }
    }
  } finally {
    clearInterval(tick);
    if (S.inflight === ctl) S.inflight = null;
    $('#run').disabled = false; $('#cancel').disabled = true;
  }
}

$('#run').onclick = runSQL;
$('#cancel').onclick = () => S.inflight?.abort();
$('#sql').onkeydown = (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); runSQL(); } };
$('#sqlBtn').onclick = () => S.conn && go(S.conn.id, S.db, { sql: true });

// ---------- connections ----------

$('#conn').onchange = (e) => go(e.target.value, '', null);
$('#db').onchange = (e) => go(S.conn.id, e.target.value, null);
$('#filterTables').oninput = renderTree;
$('#newConn').onclick = () => openDialog(null);
$('#editConn').onclick = () => openDialog(S.conn);

let editing = null;

function openDialog(conn) {
  editing = conn;
  const f = $('#connForm');
  f.reset();
  for (const k of ['name', 'host', 'port', 'user', 'database', 'sslmode']) if (conn) f.elements[k].value = conn[k] ?? '';
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
    user: f.user.value.trim(), password: f.password.value, database: f.database.value.trim() || 'postgres', sslmode: f.sslmode.value,
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
    go(saved.id, '', null);
  } catch (e) { dlgMsg(errText(e), true); }
};

$('#dlgDelete').onclick = async () => {
  if (!editing || !confirm(`Delete connection "${editing.name}"?`)) return;
  try {
    await api('DELETE', `/conns/${enc(editing.id)}`);
    S.conns = await api('GET', '/conns');
    S.conn = null;
    $('#connDlg').close();
    if (S.conns[0]) go(S.conns[0].id, '', null); else { location.hash = ''; route(); }
  } catch (e) { dlgMsg(errText(e), true); }
};

// ---------- start ----------

window.addEventListener('hashchange', route);
(async () => {
  try { S.conns = await api('GET', '/conns'); }
  catch (e) { setStatus(errText(e), true); return; }
  route();
})();
