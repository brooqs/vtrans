'use strict';

const $ = (id) => document.getElementById(id);

// --- formatters ---

function human(b) {
  if (b === null || b === undefined) return '—';
  const neg = b < 0 ? '-' : '';
  b = Math.abs(b);
  if (b >= 1024 ** 4) return neg + (b / 1024 ** 4).toFixed(2) + ' TB';
  if (b >= 1024 ** 3) return neg + (b / 1024 ** 3).toFixed(1) + ' GB';
  if (b >= 1024 ** 2) return neg + Math.round(b / 1024 ** 2) + ' MB';
  if (b >= 1024) return neg + Math.round(b / 1024) + ' KB';
  return neg + b + ' B';
}

function dur(sec) {
  if (!sec || sec < 0 || !isFinite(sec)) return '—';
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function when(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  const diff = (Date.now() - d) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)} min ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} h ago`;
  if (diff < 7 * 86400) return `${Math.floor(diff / 86400)} days ago`;
  return d.toLocaleDateString();
}

function res(w, h) { return w && h ? `${w}×${h}` : '—'; }

// Text is always written with textContent; file names come from the user and
// putting them through innerHTML would mean code injection into the interface.
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// --- server ---

async function api(path, opts) {
  const r = await fetch(path, opts);
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) throw new Error((body && body.error) || `HTTP ${r.status}`);
  return body;
}

const post = (path, data) =>
  api(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(data || {}),
  });

let toastTimer;
function toast(msg, kind) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast' + (kind ? ' ' + kind : '');
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 4200);
}

// --- tabs ---

const loaders = {};       // tab -> loader that runs the first time it is opened
const loaded = new Set();

function showTab(name) {
  const btn = document.querySelector(`.tab[data-tab="${name}"]`);
  if (!btn) return;

  document.querySelectorAll('.tab').forEach((b) => b.classList.remove('is-active'));
  document.querySelectorAll('.panel').forEach((p) => p.classList.remove('is-active'));
  btn.classList.add('is-active');
  $('panel-' + name).classList.add('is-active');

  if (loaders[name] && !loaded.has(name)) {
    loaded.add(name);
    loaders[name]();
  }
}

document.querySelectorAll('.tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    // The tab is part of the address: reloading reopens the same tab and a link
    // can be shared to say "this is what the settings screen shows".
    location.hash = btn.dataset.tab;
    showTab(btn.dataset.tab);
  });
});

window.addEventListener('hashchange', () => showTab(location.hash.slice(1)));

// --- status ---

// An empty status panel used to say nothing about why it was empty, and the
// commonest reason looks exactly like a crash: the queue drained, the worker
// exited cleanly and systemd is sitting on RestartSec before the next cycle.
// systemd reports that as activating/auto-restart, so it can be named.

function idleTitle(st) {
  const sv = st.service || {};
  if (!sv.known) return 'Nothing in progress';
  if (sv.active === 'failed') return 'The service failed';
  if (sv.active === 'activating' && sv.sub === 'auto-restart') return 'Waiting for the next cycle';
  if (sv.active === 'inactive') return 'The service is stopped';
  if (sv.active === 'active') return 'Starting';
  return 'Nothing in progress';
}

function idleReason(st) {
  const sv = st.service || {};
  const q = st.queue.files
    ? `${st.queue.files} files are queued.`
    : 'The queue is empty.';

  if (!sv.known) {
    return `${q} No vtrans.service on this machine - the service state cannot be read.`;
  }
  switch (sv.active) {
    case 'failed':
      return `${q} Check the Logs tab, then: systemctl status vtrans`;
    case 'activating':
      if (sv.sub === 'auto-restart') {
        return `${q} The last run finished and vtrans exited; systemd restarts it on its own`
          + ' (RestartSec). Nothing is wrong - it rescans on the next start.';
      }
      return `${q} The service is starting.`;
    case 'inactive':
      return `${q} The service is not running: systemctl start vtrans`;
    default:
      return `${q} The service is up and will pick up the next file on its own.`;
  }
}

const PHASE_LABEL = {
  probe: 'analysing',
  encode: 'encoding',
  verify: 'verifying',
  commit: 'committing',
  idle: 'idle',
  scan: 'scanning',
};

let lastRunning = null;

async function tick() {
  let st;
  try {
    st = await api('/api/state');
  } catch (e) {
    $('live-dot').className = 'dot bad';
    $('live-text').textContent = 'cannot reach the server';
    return;
  }

  // live indicator
  const sv = st.service || {};
  let dot = st.running ? 'on' : 'off';
  let word = st.running ? 'working' : 'idle';
  if (!st.running && sv.known) {
    if (sv.active === 'failed') { dot = 'bad'; word = 'service failed'; }
    else if (sv.active === 'inactive') { dot = 'bad'; word = 'service stopped'; }
    else if (sv.active === 'activating' && sv.sub === 'auto-restart') { word = 'waiting'; }
  }
  $('live-dot').className = 'dot ' + dot;
  $('live-text').textContent = word;

  // The log tab refreshes on its own clock: each poll spawns a journalctl, so
  // once every few seconds rather than with every tick.
  if ($('panel-logs').classList.contains('is-active') && $('log-follow').checked) {
    logTicks = (logTicks + 1) % 3;
    if (logTicks === 0) loadLogs();
  }

  // notices
  const nb = $('notices');
  nb.textContent = '';
  if (st.notices && st.notices.length) {
    st.notices.forEach((n) => nb.appendChild(el('div', 'notice', n)));
    nb.hidden = false;
  } else {
    nb.hidden = true;
  }

  // current job
  const run = st.run;
  if (st.running && run) {
    $('current-idle').hidden = true;
    $('current-active').hidden = false;

    const ph = run.phase || 'encode';
    const pe = $('cur-phase');
    pe.textContent = PHASE_LABEL[ph] || ph;
    pe.className = 'phase ' + ph;

    $('cur-pos').textContent = run.total > 1 ? `${run.index} / ${run.total}` : '';
    $('cur-name').textContent = (run.current || '').split('/').pop() || '—';

    // Phases other than encoding produce no progress; leaving the bar at its last
    // value looks stuck, and filling it would be a lie.
    const encoding = ph === 'encode';
    $('cur-bar').style.width = (encoding ? (run.pct || 0) : 100) + '%';
    $('cur-pct').textContent = encoding ? (run.pct || 0).toFixed(1) + '%' : '—';
    $('cur-speed').textContent = encoding && run.speed ? run.speed.toFixed(1) + '×' : '—';
    $('cur-fps').textContent = encoding && run.fps ? Math.round(run.fps) : '—';
    $('cur-eta').textContent = encoding ? dur(run.eta_sec) : '—';
    $('cur-size').textContent = human(run.src_size);
    $('cur-elapsed').textContent = dur((Date.now() - new Date(run.started)) / 1000);

    const bits = [`this session: ${run.ok} done`];
    if (run.skip) bits.push(`${run.skip} skipped`);
    if (run.fail) bits.push(`${run.fail} failed`);
    if (run.saved) bits.push(`${human(run.saved)} saved`);
    $('cur-session').textContent = bits.join('  ·  ');
  } else {
    $('current-idle').hidden = false;
    $('current-active').hidden = true;
    $('idle-title').textContent = idleTitle(st);
    $('idle-sub').textContent = idleReason(st);
  }

  // summary
  const s = st.summary;
  $('s-files').textContent = s.files;
  $('s-replaced').textContent = s.replaced
    ? `${s.replaced} replaced the original` : '';
  $('s-saved').textContent = human(s.saved);
  $('s-ratio').textContent = s.src_size
    ? `${human(s.src_size)} → ${human(s.out_size)} (${Math.round((s.saved * 100) / s.src_size)}%)`
    : '';
  $('s-queue').textContent = st.queue.files;
  $('s-queue-size').textContent = st.queue.size ? human(st.queue.size) + ' to process' : '';
  const sk = st.skipped || { files: 0, size: 0 };
  $('s-skipped').textContent = sk.files;
  $('s-skipped-n').textContent = sk.files
    ? `${human(sk.size)} not being attempted` : 'nothing set aside';

  $('s-trash').textContent = st.trash.dir ? human(st.trash.size) : 'off';
  $('s-trash-files').textContent = st.trash.dir
    ? `${st.trash.files} files` : 'originals are deleted outright';

  $('badge-queue').textContent = st.queue.files || '';
  $('badge-failed').textContent = s.failed || '';
  $('badge-ignored').textContent = s.ignored || '';

  $('btn-trash').disabled = !st.trash.dir || !st.trash.files;
  $('badge-trash').textContent = st.trash.files || '';

  // The queue and history go stale the moment a run ends; refresh the open tab.
  if (lastRunning !== null && lastRunning !== st.running) {
    loaded.delete('queue');
    loaded.delete('history');
    const active = document.querySelector('.tab.is-active');
    if (active && loaders[active.dataset.tab]) {
      loaded.add(active.dataset.tab);
      loaders[active.dataset.tab]();
    }
  }
  lastRunning = st.running;
}

// --- queue ---

let qOffset = 0, qTotal = 0, qQuery = '';

async function loadQueue(append) {
  if (!append) qOffset = 0;
  const url = `/api/queue?offset=${qOffset}&limit=100&q=${encodeURIComponent(qQuery)}`;
  let data;
  try {
    data = await api(url);
  } catch (e) {
    toast('could not load the queue: ' + e.message, 'err');
    return;
  }

  qTotal = data.total;
  const tb = $('q-table').querySelector('tbody');
  if (!append) tb.textContent = '';

  data.items.forEach((it) => {
    const tr = document.createElement('tr');

    const name = el('td', 'name');
    const nm = el('span', null, it.name);
    nm.title = it.path;
    name.appendChild(nm);
    const meta = el('div', 'dim',
      `${it.is_tv ? 'tv' : 'movie'} · q${it.quality} · ${dur(it.duration)}`);
    name.appendChild(meta);
    tr.appendChild(name);

    const codec = el('td');
    codec.appendChild(el('span', 'tag', it.codec || '?'));
    tr.appendChild(codec);

    const r = res(it.width, it.height);
    const target = res(it.target_w, it.target_h);
    const rc = el('td', null, r);
    if (target !== r) {
      rc.textContent = '';
      rc.appendChild(el('span', null, r));
      rc.appendChild(el('div', 'dim', '→ ' + target));
    }
    tr.appendChild(rc);

    tr.appendChild(el('td', 'num', human(it.size)));
    tr.appendChild(el('td', 'num dim', '~' + human(it.est_size)));
    tr.appendChild(el('td', 'num gain', '~' + human(it.size - it.est_size)));

    tb.appendChild(tr);
  });

  qOffset += data.items.length;
  $('q-more').hidden = qOffset >= qTotal;

  $('q-summary').textContent = qTotal
    ? `${qTotal} files${qQuery ? ' (filtered)' : ''} — showing ${qOffset}`
    : (qQuery ? 'no matches' : 'the queue is empty');

  if (!qTotal && !tb.children.length) {
    const tr = document.createElement('tr');
    const td = el('td', 'empty', qQuery ? 'no matches' : 'nothing left to process');
    td.colSpan = 6;
    tr.appendChild(td);
    tb.appendChild(tr);
  }
}

let searchTimer;
$('q-search').addEventListener('input', (e) => {
  qQuery = e.target.value.trim();
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => loadQueue(false), 220);
});
$('q-more').addEventListener('click', () => loadQueue(true));
loaders.queue = () => loadQueue(false);

// --- history ---

async function loadDone() {
  let recs;
  try {
    recs = await api('/api/done?limit=200');
  } catch (e) {
    toast('could not load the history: ' + e.message, 'err');
    return;
  }
  const tb = $('d-table').querySelector('tbody');
  tb.textContent = '';

  if (!recs.length) {
    const tr = document.createElement('tr');
    const td = el('td', 'empty', 'no files completed yet');
    td.colSpan = 5;
    tr.appendChild(td);
    tb.appendChild(tr);
    return;
  }

  recs.forEach((r) => {
    const tr = document.createElement('tr');
    const name = el('td', 'name');
    const nm = el('span', null, r.name);
    nm.title = r.path;
    name.appendChild(nm);
    if (!r.replaced) name.appendChild(el('div', 'dim', 'written as a copy'));
    tr.appendChild(name);

    tr.appendChild(el('td', 'num', human(r.src_size)));
    tr.appendChild(el('td', 'num', human(r.out_size)));

    const pct = r.src_size ? Math.round((r.saved * 100) / r.src_size) : 0;
    tr.appendChild(el('td', 'num gain', `${human(r.saved)} (${pct}%)`));

    tr.appendChild(el('td', 'dim', when(r.at)));
    tb.appendChild(tr);
  });
}
loaders.history = loadDone;

// --- failures ---

async function loadFailed() {
  let recs;
  try {
    recs = await api('/api/failed');
  } catch (e) {
    toast('could not load the failure list: ' + e.message, 'err');
    return;
  }
  const box = $('f-list');
  box.textContent = '';
  $('btn-clear-failed').disabled = !recs.length;

  if (!recs.length) {
    box.appendChild(el('p', 'empty', 'no failed files'));
    return;
  }

  recs.forEach((r) => {
    const item = el('div', 'item');
    const main = el('div', 'item-main');
    const nm = el('div', 'item-name', r.name);
    nm.title = r.path;
    main.appendChild(nm);
    main.appendChild(el('div', 'item-reason', r.reason));
    main.appendChild(el('div', 'item-date', when(r.at)));
    item.appendChild(main);

    const acts = el('div', 'item-acts');

    const retry = el('button', 'btn ghost sm', 'Retry');
    retry.addEventListener('click', async () => {
      retry.disabled = true;
      try {
        await post('/api/failed/clear', { path: r.path });
        toast('record cleared, the file will be tried again', 'ok');
        loadFailed();
      } catch (e) {
        toast(e.message, 'err');
        retry.disabled = false;
      }
    });

    // Ignoring also clears the failure record: leaving both would show the file
    // in two lists at once and Retry would not bring it back, since the ignore
    // list wins in the run loop.
    const ign = el('button', 'btn ghost sm', 'Ignore');
    ign.addEventListener('click', async () => {
      ign.disabled = true;
      try {
        await post('/api/ignore', { path: r.path, reason: r.reason });
        await post('/api/failed/clear', { path: r.path });
        toast('moved to the ignore list', 'ok');
        loadFailed();
        if (loaded.has('ignored')) loadIgnored();
      } catch (e) {
        toast(e.message, 'err');
        ign.disabled = false;
      }
    });

    acts.append(retry, ign, diagnoseButton(r.path, main));
    item.appendChild(acts);
    box.appendChild(item);
  });
}
loaders.failures = loadFailed;

// --- ignored ---

async function loadIgnored() {
  let recs;
  try {
    recs = await api('/api/ignored');
  } catch (e) {
    toast('could not load the ignore list: ' + e.message, 'err');
    return;
  }
  const box = $('i-list');
  box.textContent = '';
  $('btn-clear-ignored').disabled = !recs.length;

  if (!recs.length) {
    box.appendChild(el('p', 'empty', 'nothing is ignored'));
    return;
  }

  recs.forEach((r) => {
    const item = el('div', 'item');
    const main = el('div', 'item-main');
    const nm = el('div', 'item-name', r.name);
    nm.title = r.path;
    main.appendChild(nm);

    const reason = el('div', 'item-reason', r.reason);
    if (r.auto) {
      // Worth distinguishing: an automatic entry describes what vtrans found,
      // a manual one records a decision.
      reason.prepend(el('span', 'pill', 'auto'));
    }
    main.appendChild(reason);

    const meta = [when(r.at)];
    if (r.size) meta.push(human(r.size));
    main.appendChild(el('div', 'item-date', meta.join('  ·  ')));
    item.appendChild(main);

    const acts = el('div', 'item-acts');
    const btn = el('button', 'btn ghost sm', 'Un-ignore');
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      try {
        await post('/api/unignore', { path: r.path });
        toast('back in the queue', 'ok');
        loadIgnored();
      } catch (e) {
        toast(e.message, 'err');
        btn.disabled = false;
      }
    });
    acts.append(btn, diagnoseButton(r.path, main));
    item.appendChild(acts);
    box.appendChild(item);
  });
}
loaders.ignored = loadIgnored;

$('btn-clear-ignored').addEventListener('click', async () => {
  if (!confirm('Put every ignored file back in the queue?')) return;
  try {
    await post('/api/unignore', { all: true });
    toast('the ignore list is empty', 'ok');
    loadIgnored();
  } catch (e) {
    toast(e.message, 'err');
  }
});

$('btn-clear-failed').addEventListener('click', async () => {
  if (!confirm('Clear every failure record? The files will be tried again on the next pass.')) return;
  try {
    await post('/api/failed/clear', { all: true });
    toast('failure records cleared', 'ok');
    loadFailed();
  } catch (e) {
    toast(e.message, 'err');
  }
});

// --- trash ---

async function loadTrash() {
  let r;
  try {
    r = await api('/api/trash');
  } catch (e) {
    toast('could not read the trash: ' + e.message, 'err');
    return;
  }
  const box = $('t-list');
  box.textContent = '';
  const n = r.entries.length;
  $('badge-trash').textContent = n || '';
  $('t-total').textContent = n ? `${n} files · ${human(r.total)}` : '';
  $('btn-trash-empty').disabled = !n;

  if (!r.dir) {
    box.appendChild(el('p', 'empty', 'the trash is off: originals are deleted outright'));
    return;
  }
  if (!n) {
    box.appendChild(el('p', 'empty', 'the trash is empty'));
    return;
  }

  r.entries.forEach((t) => {
    const item = el('div', 'item');
    const main = el('div', 'item-main');
    const nm = el('div', 'item-name', t.name);
    nm.title = t.path;
    main.appendChild(nm);
    main.appendChild(el('div', 'item-reason', t.original || 'origin unknown'));
    const meta = [when(t.at), `original ${human(t.size)}`];
    meta.push(t.output ? `AV1 ${human(t.output_size)} in place` : 'no AV1 output found');
    main.appendChild(el('div', 'item-date', meta.join('  ·  ')));
    item.appendChild(main);

    const acts = el('div', 'item-acts');
    const restore = el('button', 'btn ghost sm', 'Restore');
    restore.disabled = !t.original;
    restore.addEventListener('click', async () => {
      const what = t.output ? `The AV1 file (${human(t.output_size)}) will be deleted and the original put back.` : 'The original will be put back.';
      if (!confirm(`Restore ${t.name}?\n${what}`)) return;
      restore.disabled = true;
      try {
        await post('/api/trash/restore', { path: t.path });
        toast('original restored and added to the ignore list', 'ok');
        loadTrash();
        if (loaded.has('ignored')) loadIgnored();
      } catch (e) {
        toast(e.message, 'err');
        restore.disabled = false;
      }
    });
    const del = el('button', 'btn ghost sm', 'Delete');
    del.addEventListener('click', async () => {
      if (!confirm(`Delete ${t.name} from the trash permanently?`)) return;
      del.disabled = true;
      try {
        await post('/api/trash/delete', { path: t.path });
        toast('deleted', 'ok');
        loadTrash();
      } catch (e) {
        toast(e.message, 'err');
        del.disabled = false;
      }
    });
    acts.append(restore, del);
    item.appendChild(acts);
    box.appendChild(item);
  });
}
loaders.trash = loadTrash;

$('btn-trash-empty').addEventListener('click', () => $('btn-trash').click());

$('btn-trash').addEventListener('click', async () => {
  if (!confirm('The trash will be deleted permanently. Replaced originals cannot be recovered. Continue?')) return;
  const btn = $('btn-trash');
  btn.disabled = true;
  try {
    const r = await post('/api/trash/empty');
    toast(`trash emptied: ${r.files} files, ${human(r.freed)} reclaimed`, 'ok');
  } catch (e) {
    toast(e.message, 'err');
  }
  tick();
  if (loaded.has('trash')) loadTrash();
});

// --- settings ---

const list = (s) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
let cfgCurrent = null;

function fillConfig(c) {
  $('c-roots').value = (c.roots || []).join('\n');
  $('c-mode').value = c.mode;
  $('c-dest').value = c.dest_root || '';
  $('c-trash').value = c.trash_dir || '';
  $('c-qmovie').value = c.q_movie;
  $('c-qtv').value = c.q_tv;
  $('c-qmovie-nv').value = c.q_movie_nvenc || 90;
  $('c-qtv-nv').value = c.q_tv_nvenc || 100;
  $('c-mw').value = c.movie_max_width;
  $('c-tw').value = c.tv_max_width;
  $('c-audio').value = c.audio_mode;
  $('c-opus').value = c.opus_stereo_kbps;
  $('c-minbr').value = c.min_bitrate_mbps;
  $('c-minsave').value = c.min_saving_ratio;
  $('c-skip').value = (c.skip_codecs || []).join(', ');
  $('c-tvmark').value = (c.tv_path_markers || []).join(', ');
  $('c-verify').value = c.verify_mode;
  $('c-tol').value = c.duration_tolerance_sec;
  $('c-backend').value = c.backend || '';
  $('c-render').value = c.render_device;
  $('c-cuda').value = c.cuda_device || '0';
  $('c-nvpreset').value = c.nvenc_preset || 'p6';
  $('c-streamloss').checked = !!c.skip_on_stream_loss;
  $('c-stable').value = c.stable_seconds;
  $('c-rescan').value = c.rescan_minutes;
}

function readConfig() {
  return {
    roots: list($('c-roots').value),
    mode: $('c-mode').value,
    dest_root: $('c-dest').value.trim(),
    trash_dir: $('c-trash').value.trim(),
    q_movie: +$('c-qmovie').value,
    q_tv: +$('c-qtv').value,
    q_movie_nvenc: +$('c-qmovie-nv').value,
    q_tv_nvenc: +$('c-qtv-nv').value,
    movie_max_width: +$('c-mw').value,
    tv_max_width: +$('c-tw').value,
    audio_mode: $('c-audio').value,
    opus_stereo_kbps: +$('c-opus').value,
    min_bitrate_mbps: +$('c-minbr').value,
    min_saving_ratio: +$('c-minsave').value,
    skip_codecs: list($('c-skip').value),
    tv_path_markers: list($('c-tvmark').value),
    verify_mode: $('c-verify').value,
    duration_tolerance_sec: +$('c-tol').value,
    backend: $('c-backend').value,
    render_device: $('c-render').value.trim(),
    cuda_device: $('c-cuda').value.trim(),
    nvenc_preset: $('c-nvpreset').value,
    skip_on_stream_loss: $('c-streamloss').checked,
    stable_seconds: +$('c-stable').value,
    rescan_minutes: +$('c-rescan').value,
  };
}

async function loadConfig() {
  try {
    const r = await api('/api/config');
    cfgCurrent = r.config;
    fillConfig(r.config);
    $('cfg-path').textContent = r.path;
  } catch (e) {
    toast('could not read the configuration: ' + e.message, 'err');
  }
}
loaders.settings = loadConfig;

$('cfg-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const msg = $('cfg-msg');
  msg.textContent = '';
  msg.className = 'form-msg';

  const next = readConfig();
  // replace mode cannot be undone; if the trash is off, ask explicitly.
  if (next.mode === 'replace' && !next.trash_dir &&
      !confirm('The trash directory is empty. In replace mode originals will be DELETED OUTRIGHT and cannot be recovered. Are you sure?')) {
    return;
  }

  try {
    const r = await post('/api/config', next);
    cfgCurrent = r.config;
    fillConfig(r.config);
    msg.textContent = 'saved';
    msg.className = 'form-msg ok';
    toast('configuration saved', 'ok');
    setTimeout(() => { msg.textContent = ''; }, 3000);
    loaded.delete('queue'); // the thresholds may have changed
  } catch (e) {
    msg.textContent = e.message;
    msg.className = 'form-msg err';
  }
});

$('cfg-reset').addEventListener('click', () => {
  if (cfgCurrent) fillConfig(cfgCurrent);
  $('cfg-msg').textContent = '';
});

// --- library ---

const LIB_PAGE = 60;
let libOffset = 0;
let libTimer = null;

function libQuery(offset) {
  const p = new URLSearchParams({
    q: $('lib-search').value.trim(),
    state: $('lib-state').value,
    codec: $('lib-codec').value,
    sort: $('lib-sort').value,
    offset: offset,
    limit: LIB_PAGE,
  });
  return '/api/library?' + p;
}

async function loadLibrary(append) {
  if (!append) libOffset = 0;

  let d;
  try {
    d = await api(libQuery(libOffset));
  } catch (e) {
    $('lib-summary').textContent = e.message;
    return;
  }

  const grid = $('lib-grid');
  if (!append) grid.textContent = '';

  // The codec list comes from the library itself rather than a fixed list, and
  // is filled in once so the current choice is not thrown away on every reload.
  const cs = $('lib-codec');
  if (cs.options.length === 1) {
    Object.keys(d.codecs || {})
      .sort((a, b) => d.codecs[b] - d.codecs[a])
      .forEach((c) => {
        const o = document.createElement('option');
        o.value = c;
        o.textContent = `${c} (${d.codecs[c]})`;
        cs.appendChild(o);
      });
  }

  d.items.forEach((it) => grid.appendChild(libCard(it)));
  libOffset += d.items.length;

  // Three different numbers meet here and conflating them is confusing: how
  // many cards are on screen, how many the filter matches, and how big the
  // library is.
  const c = d.counts || {};
  const all = Object.values(c).reduce((a, b) => a + b, 0);
  const breakdown = ['done', 'queued', 'failed', 'ignored', 'skipped']
    .filter((k) => c[k])
    .map((k) => `${c[k]} ${k}`)
    .join(' · ');
  const head =
    d.total === all
      ? `${all} files`
      : `${d.total} of ${all} files match`;
  $('lib-summary').textContent =
    `${head} · showing ${libOffset}${breakdown ? ' — ' + breakdown : ''}`;

  $('lib-more').hidden = libOffset >= d.total;
  if (!d.items.length && !append) {
    grid.appendChild(el('p', 'empty', 'nothing matches'));
  }
}

function libCard(it) {
  const card = el('div', 'lib-card');

  const art = el('div', 'lib-art');
  if (it.has_art) {
    const img = document.createElement('img');
    img.loading = 'lazy';
    img.alt = '';
    img.src = '/api/art?path=' + encodeURIComponent(it.path);
    // An image that fails to load would leave a broken icon; fall back to the
    // same placeholder the files without artwork use.
    img.addEventListener('error', () => {
      img.remove();
      art.classList.add('is-blank');
    });
    art.appendChild(img);
  } else {
    art.classList.add('is-blank');
  }
  art.appendChild(el('span', 'lib-state st-' + it.state, it.state));
  card.appendChild(art);

  const body = el('div', 'lib-body');
  const t = el('div', 'lib-title', it.name);
  t.title = it.path;
  body.appendChild(t);
  body.appendChild(el('div', 'lib-dir', it.dir));

  const meta = el('div', 'lib-meta');
  if (it.codec) meta.appendChild(el('span', 'chip', it.codec));
  if (it.width) meta.appendChild(el('span', 'chip', `${it.width}×${it.height}`));
  meta.appendChild(el('span', 'chip', human(it.size)));
  if (it.duration) meta.appendChild(el('span', 'chip', dur(it.duration)));
  body.appendChild(meta);

  if (it.saved > 0) {
    body.appendChild(el('div', 'lib-saved', human(it.saved) + ' saved'));
  }

  card.appendChild(body);
  return card;
}

function dur(sec) {
  const h = Math.floor(sec / 3600);
  const m = Math.round((sec % 3600) / 60);
  return h ? `${h}h ${m}m` : `${m}m`;
}

// The search box reloads on a delay: typing a title would otherwise send a
// request per keystroke, and each one walks the whole index.
['lib-search'].forEach((id) =>
  $(id).addEventListener('input', () => {
    clearTimeout(libTimer);
    libTimer = setTimeout(() => loadLibrary(false), 250);
  }),
);
['lib-state', 'lib-codec', 'lib-sort'].forEach((id) =>
  $(id).addEventListener('change', () => loadLibrary(false)),
);
$('lib-more').addEventListener('click', () => loadLibrary(true));

loaders.library = () => loadLibrary(false);

// --- doctor ---

const SEV_ORDER = { error: 0, warn: 1, info: 2 };
let docTimer = null;

// Renders one file's report. Used both by the library scan and by the
// per-row Diagnose button, so the two always read the same.
function reportCard(rep, opts) {
  const item = el('div', 'item report');
  const main = el('div', 'item-main');

  const head = el('div', 'item-name');
  head.append(el('span', 'sev sev-' + (worstOf(rep) || 'info'), worstOf(rep) || 'ok'));
  head.append(document.createTextNode(' ' + rep.name));
  head.title = rep.path;
  main.appendChild(head);

  if (rep.err) {
    main.appendChild(el('div', 'item-reason', 'could not be read: ' + rep.err));
  }

  (rep.findings || [])
    .slice()
    .sort((a, b) => SEV_ORDER[a.severity] - SEV_ORDER[b.severity])
    .forEach((f) => {
      const row = el('div', 'finding sev-' + f.severity);
      row.append(el('span', 'pill', f.kind.replace(/_/g, ' ')));
      row.append(document.createTextNode(f.detail));
      if (f.streams && f.streams.length) {
        row.append(el('span', 'muted-inline', ` (stream ${f.streams.join(', ')})`));
      }
      main.appendChild(row);
    });

  if (opts && opts.streams && rep.streams && rep.streams.length) {
    const pre = el('pre', 'streambox');
    pre.textContent = rep.streams
      .map((s) => {
        const bits = [String(s.index).padStart(2), s.type.padEnd(8), s.codec.padEnd(10)];
        if (s.detail) bits.push(s.detail);
        if (s.language) bits.push('[' + s.language + ']');
        if (s.title) bits.push(s.title);
        return bits.join(' ');
      })
      .join('\n');
    main.appendChild(pre);
  }

  item.appendChild(main);
  return item;
}

function worstOf(rep) {
  if (rep.err) return 'error';
  let w = '';
  for (const f of rep.findings || []) {
    if (f.severity === 'error') return 'error';
    if (f.severity === 'warn') w = 'warn';
    else if (!w) w = 'info';
  }
  return w;
}

async function loadDoctor() {
  let st;
  try {
    st = await api('/api/doctor/scan');
  } catch (e) {
    toast('could not read the scan state: ' + e.message, 'err');
    return;
  }
  renderDoctor(st);

  // Poll only while a sweep is running; a finished report does not change.
  if (st.running && !docTimer) {
    docTimer = setInterval(loadDoctor, 1000);
  } else if (!st.running && docTimer) {
    clearInterval(docTimer);
    docTimer = null;
  }
}

function renderDoctor(st) {
  $('doc-scan').hidden = st.running;
  $('doc-stop').hidden = !st.running;
  $('doc-bar-wrap').hidden = !st.running;
  $('doc-progress').hidden = !st.running && !st.ended;

  if (st.running) {
    const pct = st.total ? (st.done * 100) / st.total : 0;
    $('doc-bar').style.width = pct.toFixed(1) + '%';
    $('doc-progress').textContent =
      `${st.done} / ${st.total} — ${st.current || ''}`;
  } else if (st.ended) {
    const bad = (st.reports || []).length;
    $('doc-progress').textContent = st.err
      ? `${st.err} — ${st.done} of ${st.total} checked`
      : `${st.done} files checked · ${st.clean} with no problems · ${bad} needing attention`;
  }

  $('badge-doctor').textContent = (st.reports || []).filter(
    (r) => worstOf(r) === 'error' || worstOf(r) === 'warn',
  ).length || '';

  const box = $('doc-list');
  box.textContent = '';
  if (!st.reports || !st.reports.length) {
    if (st.ended && !st.err) box.appendChild(el('p', 'empty', 'nothing to report'));
    return;
  }
  st.reports.forEach((r) => box.appendChild(reportCard(r, { streams: true })));
}

loaders.doctor = loadDoctor;

$('doc-scan').addEventListener('click', async () => {
  try {
    await post('/api/doctor/scan', {});
    loadDoctor();
  } catch (e) {
    toast(e.message, 'err');
  }
});

$('doc-stop').addEventListener('click', async () => {
  try {
    await post('/api/doctor/scan/stop', {});
    loadDoctor();
  } catch (e) {
    toast(e.message, 'err');
  }
});

// diagnoseButton makes the "why is this file like this" button that appears on
// the queue, failure and ignore rows. The answer opens in place rather than in
// a dialog: it is reference material, and it should be readable next to the
// other rows.
function diagnoseButton(path, container) {
  const btn = el('button', 'btn ghost sm', 'Diagnose');
  btn.addEventListener('click', async () => {
    const open = container.querySelector('.report');
    if (open) {
      open.remove();
      btn.textContent = 'Diagnose';
      return;
    }
    btn.disabled = true;
    btn.textContent = '…';
    try {
      const rep = await api('/api/doctor/file?path=' + encodeURIComponent(path));
      container.appendChild(reportCard(rep, { streams: true }));
      btn.textContent = 'Hide';
    } catch (e) {
      toast(e.message, 'err');
      btn.textContent = 'Diagnose';
    } finally {
      btn.disabled = false;
    }
  });
  return btn;
}

// --- logs ---

let logLines = [];
let logBusy = false;
let logTicks = 0;

async function loadLogs() {
  if (logBusy) return;
  logBusy = true;
  const meta = $('log-meta');
  try {
    const n = $('log-lines').value;
    const d = await api(`/api/logs?n=${n}`);
    logLines = d.lines || [];
    meta.textContent = `${d.unit} — ${logLines.length} lines`;
    meta.className = 'muted';
  } catch (e) {
    // The journal needs group membership the service may not have. Say what
    // went wrong rather than leaving an empty box.
    meta.textContent = e.message;
    meta.className = 'muted err';
  } finally {
    logBusy = false;
  }
  renderLogs();
}

function renderLogs() {
  const box = $('log-box');
  const q = $('log-search').value.trim().toLowerCase();
  const rows = q
    ? logLines.filter((l) => l.text.toLowerCase().includes(q))
    : logLines;

  // Keep the view where the reader put it: only jump to the end when Follow is
  // on, otherwise reading older lines would be yanked away every refresh.
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;

  box.textContent = '';
  for (const l of rows) {
    const div = document.createElement('div');
    div.className = 'logline' + logClass(l.text);
    const t = document.createElement('span');
    t.className = 'logstamp';
    t.textContent = shortStamp(l.at);
    div.append(t, document.createTextNode(l.text));
    box.append(div);
  }
  if ($('log-follow').checked || atEnd) box.scrollTop = box.scrollHeight;
}

function logClass(text) {
  // The run summary is checked first: it reads "39 processed, 10 skipped,
  // 0 failed" and would otherwise be coloured by the words inside it.
  if (/^(Done\.|Total saved)/.test(text)) return ' is-ok';
  if (/FAILED|crashed|segmentation|Failed to|error/i.test(text)) return ' is-err';
  if (/SKIPPED/.test(text)) return ' is-warn';
  if (/replaced$|saved,/.test(text)) return ' is-ok';
  return '';
}

function shortStamp(at) {
  // "2026-08-14T10:14:19+00:00" -> "08-14 10:14:19"
  const m = /^\d{4}-(\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})/.exec(at || '');
  return m ? `${m[1]} ${m[2]} ` : '';
}

$('log-search').addEventListener('input', renderLogs);
$('log-lines').addEventListener('change', loadLogs);
loaders.logs = loadLogs;

// --- startup ---

// The opening tab is chosen here rather than next to the tab listeners: the
// loaders (loaders.*) are not registered until this point, so a hash in the
// address would open its tab empty.
if (location.hash) showTab(location.hash.slice(1));

tick();
setInterval(tick, 1000);
