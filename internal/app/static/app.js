// Progressive enhancement only: every control here already works as a plain
// form post if this file never loads.

// Quantity steppers update in place instead of reloading the grid.
document.addEventListener('submit', async (e) => {
  const form = e.target;
  if (!form.classList.contains('step')) return;

  const box = form.closest('.qty, .qtybox');
  const out = box && box.querySelector('.n, .big-qty');
  if (!out) return;

  e.preventDefault();
  try {
    const res = await fetch(form.action, {
      method: 'POST',
      headers: { Accept: 'application/json' },
      body: new FormData(form),
    });
    if (!res.ok) throw new Error(res.statusText);
    const { quantity } = await res.json();
    out.textContent = quantity;
    out.classList.toggle('zero', quantity === 0);
  } catch {
    form.submit(); // fall back to the full page post
  }
});

// One confirm helper for every destructive form.
document.addEventListener('submit', (e) => {
  const msg = e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
}, true);

// Detail gallery: click a thumbnail to swap the hero, click the hero to zoom.
const hero = document.getElementById('hero');
if (hero) {
  const shots = [...document.querySelectorAll('.shot')];
  const mark = () => shots.forEach((s) =>
    s.setAttribute('aria-current', String(s.dataset.full === hero.getAttribute('src'))));
  shots.forEach((s) => s.addEventListener('click', () => {
    hero.src = s.dataset.full;
    mark();
  }));
  mark();

  hero.addEventListener('click', () => {
    const dlg = document.createElement('dialog');
    dlg.className = 'light';
    const img = document.createElement('img');
    img.src = hero.src;
    dlg.append(img);
    dlg.addEventListener('click', () => dlg.close());
    dlg.addEventListener('close', () => dlg.remove());
    document.body.append(dlg);
    dlg.showModal();
  });
}

// Drag photos onto the detail page to attach them.
const zone = document.querySelector('.dropzone');
if (zone) {
  const input = zone.querySelector('input[type=file]');
  const stop = (e) => { e.preventDefault(); e.stopPropagation(); };

  ['dragenter', 'dragover'].forEach((t) =>
    zone.addEventListener(t, (e) => { stop(e); zone.classList.add('over'); }));
  ['dragleave', 'drop'].forEach((t) =>
    zone.addEventListener(t, (e) => { stop(e); zone.classList.remove('over'); }));

  zone.addEventListener('drop', (e) => {
    if (!e.dataTransfer.files.length) return;
    input.files = e.dataTransfer.files;
    zone.submit();
  });

  // Picking files is the whole intent; no reason to make them press Upload.
  input.addEventListener('change', () => { if (input.files.length) zone.submit(); });
}

// Keyboard: "/" focuses search, "n" opens the add form.
document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const tag = document.activeElement && document.activeElement.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

  if (e.key === '/') {
    const s = document.querySelector('input[type=search]');
    if (s) { e.preventDefault(); s.focus(); s.select(); }
  } else if (e.key === 'n') {
    e.preventDefault();
    location.href = '/items/new';
  }
});

// --- import from a link -----------------------------------------------------
// The server does the fetching and parsing; this only fills the form in and
// leaves everything editable, so nothing is saved until Add/Save is pressed.
const importGo = document.getElementById('import-go');
if (importGo) {
  const urlInput = document.getElementById('import-url');
  const status = document.getElementById('import-status');
  const preview = document.getElementById('import-preview');

  const say = (msg, cls) => {
    status.textContent = msg;
    status.className = 'import-status' + (cls ? ' ' + cls : '');
    status.hidden = !msg;
  };

  // Only fill a field the person hasn't already written in, so re-fetching
  // never clobbers a correction.
  const fill = (id, value, { force = false } = {}) => {
    const el = document.getElementById(id);
    if (!el || !value) return;
    if (force || !el.value.trim()) el.value = value;
  };

  const run = async () => {
    const url = urlInput.value.trim();
    if (!url) { say('Paste a link first.', 'bad'); return; }

    importGo.disabled = true;
    say('Fetching…', 'busy');
    preview.hidden = true;

    try {
      const res = await fetch('/import/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ url }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'could not read that page');

      fill('f-name', data.name);
      fill('f-notes', data.notes);
      fill('f-part_number', data.part_number);
      fill('f-link', data.link, { force: true });
      fill('f-image_url', data.image_url, { force: true });
      if (data.price > 0) {
        fill('f-price', String(data.price), { force: true });
        fill('f-price_source', data.site || hostOf(data.link), { force: true });
      }

      if (data.image_url) {
        document.getElementById('import-thumb').src = data.image_url;
      }
      document.getElementById('import-title').textContent = data.name || '(no title found)';
      document.getElementById('import-site').textContent = data.site || new URL(data.link || url).hostname;
      preview.hidden = false;
      say('Filled in what the page provided — edit anything before saving.', '');
    } catch (err) {
      say(err.message, 'bad');
    } finally {
      importGo.disabled = false;
    }
  };

  importGo.addEventListener('click', run);
  urlInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); run(); }
  });
}

// --- search a shop's catalogue by name ------------------------------------
// The server queries the sources it can actually reach and says which ones it
// cannot; picking a result fills the form in, exactly like the link importer.
const searchGo = document.getElementById('search-go');
if (searchGo) {
  const input = document.getElementById('search-q');
  const status = document.getElementById('search-status');
  const list = document.getElementById('search-results');
  const external = document.getElementById('search-external');

  const say = (msg, cls) => {
    status.textContent = msg;
    status.className = 'import-status' + (cls ? ' ' + cls : '');
    status.hidden = !msg;
  };

  const pick = (r) => {
    setField('f-name', r.title);
    setField('f-part_number', r.part_number);
    setField('f-link', r.url, true);
    setField('f-image_url', r.image_url, true);
    if (r.price > 0) {
      setField('f-price', r.price.toFixed(2), true);
      setField('f-price_source', r.source, true);
    }
    list.querySelectorAll('.result').forEach((el) => el.classList.remove('on'));
    say(`Filled in from ${r.source}. Edit anything before saving.`, '');
    document.getElementById('f-name').focus();
  };

  const run = async () => {
    const q = input.value.trim();
    if (!q) { say('Type a part name first.', 'bad'); return; }

    searchGo.disabled = true;
    say('Searching…', 'busy');
    list.hidden = true;
    external.hidden = true;

    try {
      const res = await fetch('/import/search', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ q }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'search failed');

      list.textContent = '';
      (data.results || []).forEach((r) => {
        const el = document.createElement('button');
        el.type = 'button';
        el.className = 'result';
        el.innerHTML = `
          <span class="r-img">${r.image_url ? `<img src="${escapeAttr(r.image_url)}" alt="" loading="lazy">` : ''}</span>
          <span class="r-body">
            <span class="r-title"></span>
            <span class="r-meta">
              <span class="r-src"></span>
              ${r.price > 0 ? `<span class="r-price">${escapeHTML(formatPrice(r.price, r.currency))}</span>` : ''}
              ${r.stock ? `<span class="muted"></span>` : ''}
            </span>
          </span>`;
        // Text goes in via textContent so a shop's title cannot inject markup.
        el.querySelector('.r-title').textContent = r.title;
        el.querySelector('.r-src').textContent = r.source;
        if (r.stock) el.querySelector('.r-meta .muted').textContent = r.stock;
        el.addEventListener('click', () => pick(r));
        list.append(el);
      });

      const notes = data.notes || [];
      if (!data.results || data.results.length === 0) {
        say(notes.length ? notes.join(' · ') : 'Nothing found in the catalogues I can search.', 'bad');
      } else {
        list.hidden = false;
        say(notes.length ? notes.join(' · ') : `${data.results.length} result(s) — click one to fill the form.`,
            notes.length ? 'bad' : '');
      }

      // Shops that block server-side lookups still get a link out.
      if (data.external && data.external.length) {
        external.textContent = 'Search there yourself: ';
        data.external.forEach((s) => {
          const a = document.createElement('a');
          a.className = 'chip sm';
          a.href = s.SearchURL;
          a.target = '_blank';
          a.rel = 'noreferrer noopener';
          a.textContent = s.Name;
          a.title = s.Note;
          external.append(a, ' ');
        });
        external.hidden = false;
      }
    } catch (err) {
      say(err.message, 'bad');
    } finally {
      searchGo.disabled = false;
    }
  };

  searchGo.addEventListener('click', run);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); run(); }
  });
}

function setField(id, value, force) {
  const el = document.getElementById(id);
  if (!el || !value) return;
  if (force || !el.value.trim()) el.value = value;
}

function hostOf(link) {
  try { return new URL(link).hostname.replace(/^www\./, ''); } catch { return ''; }
}

function formatPrice(amount, currency) {
  const symbols = { USD: '$', EUR: '\u20ac', GBP: '\u00a3', JPY: '\u00a5' };
  const s = symbols[currency || 'USD'];
  return s ? s + amount.toFixed(2) : `${currency} ${amount.toFixed(2)}`;
}

const escapeHTML = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const escapeAttr = escapeHTML;

// --- tag picker -------------------------------------------------------------
// Clicking an existing tag toggles it in the comma-separated field, which stays
// the source of truth so typing a brand new tag still works.
const tagSuggest = document.getElementById('tag-suggest');
if (tagSuggest) {
  const field = document.getElementById('f-tags');
  const read = () => field.value.split(',').map((t) => t.trim()).filter(Boolean);
  const sync = () => {
    const active = read().map((t) => t.toLowerCase());
    tagSuggest.querySelectorAll('[data-tag]').forEach((b) =>
      b.classList.toggle('on', active.includes(b.dataset.tag.toLowerCase())));
  };

  tagSuggest.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-tag]');
    if (!btn) return;
    const tag = btn.dataset.tag;
    const tags = read();
    const at = tags.findIndex((t) => t.toLowerCase() === tag.toLowerCase());
    if (at >= 0) tags.splice(at, 1); else tags.push(tag);
    field.value = tags.join(', ');
    sync();
  });

  field.addEventListener('input', sync);
  sync();
}

// --- drag a link or image onto the add form ---------------------------------
// Dragging a product page from another tab, or an image file off the desktop,
// is quicker than copy-pasting into the right box. A dropped URL goes through
// the same importer; a dropped file goes into the photo picker.
const itemForm = document.getElementById('item-form');
if (itemForm) {
  let depth = 0; // dragenter/leave fire per element, so nesting needs counting

  document.addEventListener('dragenter', (e) => {
    if (!e.dataTransfer) return;
    depth++;
    document.body.classList.add('drag-target');
  });
  document.addEventListener('dragleave', () => {
    if (--depth <= 0) {
      depth = 0;
      document.body.classList.remove('drag-target');
    }
  });
  document.addEventListener('dragover', (e) => e.preventDefault());

  document.addEventListener('drop', (e) => {
    e.preventDefault();
    depth = 0;
    document.body.classList.remove('drag-target');
    if (!e.dataTransfer) return;

    // An image file dropped from the desktop.
    const file = [...e.dataTransfer.files].find((f) => f.type.startsWith('image/'));
    if (file) {
      const picker = itemForm.querySelector('input[type=file][name=photos]');
      if (picker) {
        const dt = new DataTransfer();
        [...e.dataTransfer.files].forEach((f) => dt.items.add(f));
        picker.files = dt.files;
      }
      return;
    }

    // Otherwise look for a URL: a dragged link, or an image dragged from a page.
    const text = e.dataTransfer.getData('text/uri-list') || e.dataTransfer.getData('text/plain');
    const url = (text || '').trim().split('\n')[0];
    if (!/^https?:\/\//i.test(url)) return;

    const target = document.getElementById('import-url');
    if (target) {
      target.value = url;
      document.getElementById('import-go')?.click();
    }
  });
}

// --- type-ahead over the inventory -------------------------------------------
// Answers "do I already have one of these?" while you type. It searches your
// own shelf, not the shops — the form's own search box does that.
document.querySelectorAll('[data-live-search]').forEach((input) => {
  const form = input.closest('form');
  if (!form) return;

  const panel = document.createElement('div');
  panel.className = 'typeahead';
  panel.hidden = true;
  form.classList.add('has-typeahead');
  form.append(panel);

  let timer, controller, active = -1;

  const close = () => { panel.hidden = true; active = -1; };

  const render = (data, query) => {
    panel.textContent = '';
    if (!data.results.length) {
      const empty = document.createElement('div');
      empty.className = 'ta-empty';
      empty.textContent = `Nothing in the inventory matches “${query}”.`;
      panel.append(empty);
      panel.hidden = false;
      return;
    }

    data.results.forEach((r) => {
      const a = document.createElement('a');
      a.className = 'ta-row';
      a.href = `/items/${r.id}`;

      const img = document.createElement('span');
      img.className = 'ta-img';
      if (r.thumb) {
        const el = document.createElement('img');
        el.src = `/media/thumb/${r.thumb}`;
        el.alt = '';
        el.loading = 'lazy';
        img.append(el);
      } else {
        img.textContent = (r.name || '?').slice(0, 1).toUpperCase();
      }

      const body = document.createElement('span');
      body.className = 'ta-body';
      const name = document.createElement('span');
      name.className = 'ta-name';
      name.textContent = r.name;
      const meta = document.createElement('span');
      meta.className = 'ta-meta';
      meta.textContent = [r.location, r.folder, r.price].filter(Boolean).join(' · ');
      body.append(name, meta);

      const qty = document.createElement('span');
      qty.className = 'ta-qty' + (r.quantity === 0 ? ' zero' : '');
      qty.textContent = r.quantity;

      a.append(img, body, qty);
      panel.append(a);
    });

    if (data.total > data.results.length) {
      const more = document.createElement('button');
      more.type = 'submit';
      more.className = 'ta-more';
      more.textContent = `See all ${data.total} matches`;
      panel.append(more);
    }
    panel.hidden = false;
  };

  const run = async () => {
    const query = input.value.trim();
    if (query.length < 2) { close(); return; }

    // Abandon the previous request: with fast typing the answers can arrive
    // out of order, and a stale one would overwrite the current query.
    if (controller) controller.abort();
    controller = new AbortController();

    try {
      const res = await fetch('/items/search.json?q=' + encodeURIComponent(query),
        { signal: controller.signal, headers: { Accept: 'application/json' } });
      if (!res.ok) throw new Error(res.statusText);
      render(await res.json(), query);
    } catch (err) {
      if (err.name !== 'AbortError') close();
    }
  };

  input.addEventListener('input', () => {
    clearTimeout(timer);
    timer = setTimeout(run, 140);
  });
  input.addEventListener('focus', () => { if (input.value.trim().length >= 2) run(); });

  // Arrow keys walk the list; Enter opens the highlighted row, or submits.
  input.addEventListener('keydown', (e) => {
    const rows = [...panel.querySelectorAll('.ta-row')];
    if (e.key === 'Escape') { close(); return; }
    if (!rows.length || panel.hidden) return;

    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      active += e.key === 'ArrowDown' ? 1 : -1;
      if (active < 0) active = rows.length - 1;
      if (active >= rows.length) active = 0;
      rows.forEach((row, i) => row.classList.toggle('on', i === active));
      rows[active].scrollIntoView({ block: 'nearest' });
    } else if (e.key === 'Enter' && active >= 0) {
      e.preventDefault();
      rows[active].click();
    }
  });

  document.addEventListener('click', (e) => {
    if (!form.contains(e.target)) close();
  });
});

/* --- scanning a label ---------------------------------------------------- */

/* The camera work uses the browser's own BarcodeDetector: no library ships
   with the page, and nothing about the image leaves the device. It is not in
   every browser -- Firefox and Safari have no implementation -- so the page
   says so plainly and the typed field beside it does the same job. */
document.querySelectorAll('[data-scanner]').forEach((root) => {
  const stage = root.querySelector('[data-scan-stage]');
  const video = root.querySelector('[data-scan-video]');
  const status = root.querySelector('[data-scan-status]');
  const startBtn = root.querySelector('[data-scan-start]');
  const stopBtn = root.querySelector('[data-scan-stop]');
  const hit = root.querySelector('[data-scan-hit]');
  const manual = root.querySelector('[data-scan-manual]');

  const supported = 'BarcodeDetector' in window &&
    navigator.mediaDevices && navigator.mediaDevices.getUserMedia;

  if (!supported) {
    status.textContent = 'This browser cannot use the camera for barcodes ' +
      '(Chrome on Android can). Type the code below instead.';
    if (manual) manual.focus();
    return;
  }

  status.textContent = 'Point the camera at a QR code or barcode on a drawer.';
  startBtn.hidden = false;

  let detector = null, stream = null, running = false, lastCode = '', lastAt = 0;

  async function start() {
    try {
      const formats = await window.BarcodeDetector.getSupportedFormats();
      const wanted = ['qr_code', 'code_128', 'ean_13', 'code_39', 'upc_a', 'upc_e']
        .filter((f) => formats.includes(f));
      detector = new window.BarcodeDetector({ formats: wanted.length ? wanted : formats });
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: 'environment' }, audio: false,
      });
    } catch (err) {
      status.textContent = 'Could not open the camera: ' + err.message;
      return;
    }
    video.srcObject = stream;
    await video.play();
    stage.hidden = false;
    startBtn.hidden = true;
    stopBtn.hidden = false;
    status.textContent = 'Looking…';
    running = true;
    tick();
  }

  function stop() {
    running = false;
    if (stream) stream.getTracks().forEach((t) => t.stop());
    stream = null;
    stage.hidden = true;
    stopBtn.hidden = true;
    startBtn.hidden = false;
    status.textContent = 'Camera off.';
  }

  async function tick() {
    if (!running) return;
    try {
      const codes = await detector.detect(video);
      if (codes.length) await found(codes[0].rawValue);
    } catch (_) {
      /* A frame that cannot be decoded is the normal case, not an error. */
    }
    /* Four looks a second is plenty and leaves the phone cool. */
    if (running) setTimeout(() => requestAnimationFrame(tick), 250);
  }

  async function found(code) {
    const now = Date.now();
    /* The same label stays in frame for many frames; only act on it once. */
    if (code === lastCode && now - lastAt < 2500) return;
    lastCode = code;
    lastAt = now;

    if (navigator.vibrate) navigator.vibrate(30);
    status.textContent = 'Found ' + code;

    let res;
    try {
      res = await fetch('/lookup?code=' + encodeURIComponent(code),
        { headers: { Accept: 'application/json' } });
    } catch (err) {
      status.textContent = 'Lookup failed: ' + err.message;
      return;
    }
    const data = await res.json();
    if (!res.ok) {
      hit.hidden = false;
      hit.innerHTML = '';
      hit.append(el('p', 'muted', data.error || 'Nothing matched that code.'));
      const add = el('a', 'btn', 'Add it to the inventory');
      add.href = '/items/new?name=' + encodeURIComponent(code);
      hit.append(add);
      return;
    }
    show(data);
  }

  /* The result is rendered in place rather than navigated to, so the camera
     keeps running and a whole drawer can be counted without leaving the page. */
  function show(item) {
    hit.hidden = false;
    hit.innerHTML = '';

    const row = el('div', 'scan-card');
    const thumb = el('span', 'ta-img');
    if (item.thumb) {
      const img = document.createElement('img');
      img.src = '/media/thumb/' + item.thumb;
      img.alt = '';
      thumb.append(img);
    } else {
      thumb.textContent = (item.name || '?').slice(0, 2).toUpperCase();
    }
    const body = el('div', 'ta-body');
    const link = el('a', 'ta-name', item.name);
    link.href = item.url;
    body.append(link);
    body.append(el('span', 'ta-meta',
      [item.part_number, item.location].filter(Boolean).join(' · ') || 'no location set'));

    const count = el('output', 'big-qty', String(item.quantity));
    const minus = el('button', 'btn', '−');
    const plus = el('button', 'btn', '+');
    minus.type = plus.type = 'button';

    const step = (delta) => async () => {
      const body = new URLSearchParams({ delta: String(delta) });
      const res = await fetch('/items/' + item.id + '/quantity', {
        method: 'POST', headers: { Accept: 'application/json' }, body,
      });
      if (!res.ok) return;
      const data = await res.json();
      count.textContent = data.quantity;
      count.classList.toggle('zero', data.quantity === 0);
    };
    minus.addEventListener('click', step(-1));
    plus.addEventListener('click', step(1));

    const qty = el('div', 'qtybox');
    qty.append(minus, count, plus);

    row.append(thumb, body, qty);
    hit.append(row);
  }

  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  startBtn.addEventListener('click', start);
  stopBtn.addEventListener('click', stop);
  window.addEventListener('pagehide', stop);
});

/* --- appearance ------------------------------------------------------------
 *
 * Three states, not two: light, dark, and following the system. "System" is
 * the default and has to stay reachable, because somebody whose laptop flips
 * at sunset wants that back after trying the other two.
 *
 * The <head> applies the saved choice before first paint; this only handles
 * changing it. With this script blocked the page still renders in the system
 * scheme, so nothing here is load-bearing. */
(function () {
  const root = document.documentElement;

  function read() {
    try {
      const v = localStorage.getItem('theme');
      return v === 'light' || v === 'dark' ? v : 'system';
    } catch (e) {
      return 'system'; // private mode, or storage switched off
    }
  }

  function apply(choice) {
    if (choice === 'system') {
      root.removeAttribute('data-theme');
    } else {
      root.setAttribute('data-theme', choice);
    }
    try {
      if (choice === 'system') localStorage.removeItem('theme');
      else localStorage.setItem('theme', choice);
    } catch (e) { /* the page still looks right, it just will not persist */ }
    mark(choice);
  }

  function mark(choice) {
    document.querySelectorAll('[data-theme-set]').forEach(function (b) {
      const on = b.dataset.themeSet === choice;
      b.classList.toggle('on', on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    mark(read());
    document.querySelectorAll('[data-theme-set]').forEach(function (b) {
      b.addEventListener('click', function () { apply(b.dataset.themeSet); });
    });
  });
})();

/* --- menus -----------------------------------------------------------------
 *
 * A <details> menu is keyboard-accessible on its own, but it will not close
 * when you click away from it or press Escape, which every real menu does. */
(function () {
  document.addEventListener('click', function (e) {
    document.querySelectorAll('details.menu[open]').forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute('open');
    });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    document.querySelectorAll('details.menu[open]').forEach(function (d) {
      d.removeAttribute('open');
      const s = d.querySelector('summary');
      if (s) s.focus();
    });
  });
})();

// --- printer status ------------------------------------------------------------
// The dashboard draws the printers' names straight away and asks how they are
// afterwards, so a printer that is switched off never makes the page wait.
// Refreshes every 20 seconds while the tab is visible; stops when it is not.
(() => {
  const strip = document.querySelector('[data-printer-strip]');
  if (!strip) return;

  async function refresh() {
    if (document.hidden) return;
    let list;
    try {
      const res = await fetch('/printing/status', { headers: { Accept: 'application/json' } });
      if (!res.ok) return;
      list = await res.json();
    } catch { return; }
    for (const p of list) {
      const li = strip.querySelector(`[data-printer="${p.id}"]`);
      if (!li) continue;
      li.className = 's-' + p.state;
      li.querySelector('[data-headline]').textContent =
        p.headline + (p.file && (p.state === 'printing' || p.state === 'paused') ? ' · ' + p.file : '');
      li.title = p.error || '';
      const bar = li.querySelector('.print-bar');
      const printing = p.state === 'printing' || p.state === 'paused';
      bar.hidden = !printing;
      if (printing) bar.firstElementChild.style.width = p.percent + '%';
    }
  }

  refresh();
  setInterval(refresh, 20000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
})();
