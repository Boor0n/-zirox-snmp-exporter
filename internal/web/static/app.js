// Small progressive enhancements; every page also works without JavaScript.
(function () {
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

  // Confirm destructive buttons.
  document.addEventListener('click', (e) => {
    const b = e.target.closest('[data-confirm]');
    if (b && !confirm(b.dataset.confirm)) e.preventDefault();
  });

  // "Select all" checkbox in the device list.
  $$('[data-check-all]').forEach((box) => {
    box.addEventListener('change', () => {
      $$('input[name="' + box.dataset.checkAll + '"]').forEach((c) => { c.checked = box.checked; });
    });
  });

  // Credential: choose saved or enter in place.
  $$('[data-toggle-new]').forEach((sel) => {
    const target = document.getElementById(sel.dataset.toggleNew);
    const sync = () => { target.style.display = sel.value === 'new' ? '' : 'none'; };
    sel.addEventListener('change', sync); sync();
  });

  // SNMP version: show v1/v2c or v3 fields.
  $$('[data-show-v3]').forEach((sel) => {
    const root = sel.closest('fieldset, form, .sub') || document;
    const sync = () => {
      const v3 = sel.value === '3';
      $$('.v3-fields', root).forEach((el) => { el.style.display = v3 ? '' : 'none'; });
      $$('.v12-fields', root).forEach((el) => { el.style.display = v3 ? 'none' : ''; });
    };
    sel.addEventListener('change', sync); sync();
  });

  // Reload while background discovery runs.
  $$('[data-autoreload]').forEach((el) => {
    setTimeout(() => location.reload(), Number(el.dataset.autoreload) * 1000);
  });
  $$('[data-poll-status]').forEach((el) => {
    const tick = () => fetch(el.dataset.pollStatus, { credentials: 'same-origin' })
      .then((r) => r.json())
      .then((s) => { if (!s.discovering) location.replace(location.pathname); else setTimeout(tick, 2000); })
      .catch(() => setTimeout(tick, 5000));
    setTimeout(tick, 2000);
  });

  // "/" jumps to the search box, as in many admin tools.
  const hot = document.querySelector('[data-hotkey="/"]');
  if (hot) document.addEventListener('keydown', (e) => {
    if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.target.closest('input:not([type=checkbox]):not([type=radio]), select, textarea')) return;
    e.preventDefault(); hot.focus(); hot.select();
  });

  // Sortable tables: click a column header. Cells may carry the sort value in data-v.
  $$('table[data-sortable]').forEach((table) => {
    const heads = $$('th[data-sort]', table);
    heads.forEach((th, n) => {
      const col = Array.from(th.parentNode.children).indexOf(th);
      const b = document.createElement('button');
      b.type = 'button';
      while (th.firstChild) b.appendChild(th.firstChild);
      th.appendChild(b);
      b.addEventListener('click', () => {
        const dir = th.getAttribute('aria-sort') === 'ascending' ? 'descending' : 'ascending';
        heads.forEach((h) => h.removeAttribute('aria-sort'));
        th.setAttribute('aria-sort', dir);
        const num = th.dataset.sort === 'num';
        const val = (tr) => { const td = tr.children[col]; return td ? (td.dataset.v !== undefined ? td.dataset.v : td.textContent.trim()) : ''; };
        const body = table.tBodies[0];
        const rows = Array.from(body.rows).filter((tr) => !tr.querySelector('td.empty'));
        rows.sort((x, y) => {
          const a = val(x), c = val(y);
          const r = num ? (Number(a) - Number(c)) : a.localeCompare(c, undefined, { numeric: true, sensitivity: 'base' });
          return dir === 'ascending' ? r : -r;
        });
        rows.forEach((tr) => body.appendChild(tr));
      });
    });
  });

  // Device list: show how many devices are selected; bulk buttons only work with a selection.
  const bulk = document.querySelector('[data-bulk]');
  if (bulk) {
    const count = bulk.querySelector('[data-bulk-count]');
    const btns = $$('button', bulk);
    const sync = () => {
      const n = $$('input[name="ids"]:checked').length;
      count.textContent = n ? n + ' selected:' : 'Select devices to:';
      btns.forEach((b) => { b.disabled = !n; });
      bulk.classList.toggle('active', n > 0);
    };
    document.addEventListener('change', (e) => { if (e.target.matches('input[name="ids"], [data-check-all]')) sync(); });
    sync();
  }

  // Device card: filters and bulk (un)checking of visible rows.
  const form = document.getElementById('sel');
  if (!form) return;
  const text = form.querySelector('[data-filter-text]');
  const up = form.querySelector('[data-filter-up]');
  const phys = form.querySelector('[data-filter-phys]');
  const onlySel = form.querySelector('[data-filter-sel]');
  const rows = $$('tr.obj', form);
  const apply = () => {
    const q = (text.value || '').toLowerCase();
    rows.forEach((tr) => {
      const box = tr.querySelector('input[type=checkbox]');
      let show = !q || tr.textContent.toLowerCase().includes(q);
      // "up" and "physical" filters only apply to ports.
      if (show && up.checked && tr.querySelector('.oper') && !tr.dataset.up) show = false;
      if (show && phys.checked && !tr.dataset.phys) show = false;
      if (show && onlySel.checked && !box.checked) show = false;
      tr.hidden = !show;
    });
  };
  [text, up, phys, onlySel].forEach((el) => el.addEventListener('input', apply));
  [up, phys, onlySel].forEach((el) => el.addEventListener('change', apply));
  $$('[data-select-visible]', form).forEach((b) => b.addEventListener('click', () => {
    const on = b.dataset.selectVisible === '1';
    rows.forEach((tr) => { if (!tr.hidden && !tr.closest('[data-panel][hidden]')) tr.querySelector('input[type=checkbox]').checked = on; });
  }));
  $$('[data-check-group]', form).forEach((box) => box.addEventListener('change', () => {
    $$('tr.obj', box.closest('table')).forEach((tr) => {
      if (!tr.hidden) tr.querySelector('input[type=checkbox]').checked = box.checked;
    });
    markDirty();
  }));

  // Warn about ticks that were changed but not applied yet.
  const dirtyBadge = form.querySelector('[data-dirty]');
  const boxes = $$('input[name=obj]', form);
  const initial = boxes.map((b) => b.checked);
  let submitting = false;
  const isDirty = () => boxes.some((b, i) => b.checked !== initial[i]);
  function markDirty() { if (dirtyBadge) dirtyBadge.hidden = !isDirty(); }
  boxes.forEach((b) => b.addEventListener('change', markDirty));
  $$('[data-select-visible]', form).forEach((b) => b.addEventListener('click', markDirty));
  // Live counters, row highlight, click-a-row and Shift+click ranges.
  const selCount = form.querySelector('[data-sel-count]');
  function counts() {
    boxes.forEach((b) => b.closest('tr').classList.toggle('on', b.checked));
    $$('table.objects', form).forEach((t) => {
      const h = t.closest('.table-wrap').previousElementSibling;
      const c = h && h.querySelector('[data-group-count]');
      if (c) c.textContent = $$('input[name=obj]:checked', t).length;
    });
    if (selCount) {
      const n = boxes.filter((b) => b.checked).length;
      const changed = boxes.filter((b, i) => b.checked !== initial[i]).length;
      selCount.textContent = n + ' selected' + (changed ? ', ' + changed + ' changed' : '');
    }
  }
  form.addEventListener('change', counts);
  $$('[data-select-visible]', form).forEach((b) => b.addEventListener('click', counts));
  counts();
  let last = null;
  form.addEventListener('click', (e) => {
    const tr = e.target.closest('tr.obj');
    if (!tr || e.target.closest('a, button, label')) return;
    const box = tr.querySelector('input[name=obj]');
    if (!box || box.disabled) return;
    if (e.target !== box) box.checked = !box.checked;
    if (e.shiftKey && last && last !== box) {
      const vis = boxes.filter((b) => !b.closest('tr').hidden);
      const i = vis.indexOf(last), j = vis.indexOf(box);
      if (i >= 0 && j >= 0) vis.slice(Math.min(i, j), Math.max(i, j) + 1).forEach((b) => { b.checked = box.checked; });
      document.getSelection().removeAllRanges();
    }
    last = box;
    markDirty(); counts();
  });

  form.addEventListener('submit', () => { submitting = true; });
  window.addEventListener('beforeunload', (e) => {
    if (!submitting && isDirty()) { e.preventDefault(); e.returnValue = ''; }
  });
})();

// Warn while typing a community that Cisco would split at '@'.
(function () {
  document.querySelectorAll('input[data-community]').forEach((inp) => {
    const warn = inp.closest('label').parentElement.querySelector('[data-community-warn]');
    if (!warn) return;
    const check = () => { warn.hidden = !inp.value.includes('@'); };
    inp.addEventListener('input', check);
    check();
  });
})();

// Theme (auto / light / dark) and compact tables, remembered in this browser.
(function () {
  const root = document.documentElement;
  const store = (k, v) => { try { if (v === null) localStorage.removeItem(k); else localStorage.setItem(k, v); } catch (e) {} };
  const tb = document.querySelector('[data-theme-toggle]');
  if (tb) {
    const name = tb.querySelector('[data-theme-name]');
    const show = () => { name.textContent = root.dataset.theme || 'auto'; };
    tb.addEventListener('click', () => {
      const next = { '': 'light', light: 'dark', dark: '' }[root.dataset.theme || ''];
      if (next) root.dataset.theme = next; else delete root.dataset.theme;
      store('zirox-theme', next || null);
      show();
    });
    show();
  }
  const db = document.querySelector('[data-dense-toggle]');
  if (db) {
    const show = () => db.setAttribute('aria-pressed', root.classList.contains('dense') ? 'true' : 'false');
    db.addEventListener('click', () => {
      const on = root.classList.toggle('dense');
      store('zirox-dense', on ? '1' : null);
      show();
    });
    show();
  }
})();

// Device page: tabs, module checkboxes, Apply counter and live port numbers.
(function () {
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  const tabs = document.querySelector('[data-tabs]');
  if (tabs) {
    const links = $$('[data-tab]', tabs);
    const panels = $$('[data-panel]');
    const select = (name, focus) => {
      links.forEach((a) => {
        const on = a.dataset.tab === name;
        a.setAttribute('aria-selected', on ? 'true' : 'false');
        a.tabIndex = on ? 0 : -1;
        if (on && focus) a.focus();
      });
      panels.forEach((p) => { p.hidden = p.dataset.panel !== name; });
      try { sessionStorage.setItem('zirox-tab', name); } catch (e) {}
    };
    links.forEach((a, i) => {
      a.addEventListener('click', (e) => { e.preventDefault(); select(a.dataset.tab); history.replaceState(null, '', '#tab-' + a.dataset.tab); });
      a.addEventListener('keydown', (e) => {
        const d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
        if (d) { e.preventDefault(); select(links[(i + d + links.length) % links.length].dataset.tab, true); }
      });
    });
    if (links.length) {
      let want = (location.hash.match(/^#tab-(\w+)/) || [])[1];
      if (!want) { try { want = sessionStorage.getItem('zirox-tab'); } catch (e) {} }
      if (!links.some((a) => a.dataset.tab === want)) want = links[0].dataset.tab;
      select(want);
    }
  }

  const form = document.getElementById('sel');
  if (!form) return;
  $$('[data-check-module]', form).forEach((box) => box.addEventListener('change', () => {
    $$('tr.obj', box.closest('tbody')).forEach((tr) => {
      const c = tr.querySelector('input[name=obj]');
      if (!tr.hidden && c && !c.disabled) c.checked = box.checked;
    });
    form.dispatchEvent(new Event('change'));
  }));

  // Apply buttons show how many ticks would change.
  const boxes = $$('input[name=obj]', form);
  const initial = boxes.map((b) => b.checked);
  const applies = $$('[data-apply]', form);
  const syncApply = () => {
    let on = 0, off = 0;
    boxes.forEach((b, i) => { if (b.checked !== initial[i]) { if (b.checked) on++; else off++; } });
    applies.forEach((b) => {
      b.textContent = on || off ? 'Apply (' + [on ? '+' + on : '', off ? '−' + off : ''].filter(Boolean).join(' ') + ')' : 'Apply';
      b.classList.toggle('pending', !!(on || off));
    });
  };
  form.addEventListener('change', syncApply);
  form.addEventListener('click', () => setTimeout(syncApply, 0));

  // Live numbers: refresh In/Out/Load/Errors once per poll interval.
  const url = form.dataset.live;
  if (!url || !form.querySelector('[data-live-in]')) return;
  const rows = {};
  $$('tr[data-obj]', form).forEach((tr) => { if (tr.querySelector('[data-live-in]')) rows[tr.dataset.obj] = tr; });
  const dash = '<span class="muted">—</span>';
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  let wait = 30;
  const tick = () => {
    if (document.hidden) { setTimeout(tick, 5000); return; }
    fetch(url, { credentials: 'same-origin', cache: 'no-store' })
      .then((r) => r.json())
      .then((d) => {
        wait = Math.min(Math.max(d.interval || 30, 5), 300);
        Object.keys(rows).forEach((id) => {
          const tr = rows[id], p = d.ports[id];
          tr.querySelector('[data-live-in]').innerHTML = p ? esc(p.in) : dash;
          tr.querySelector('[data-live-out]').innerHTML = p ? esc(p.out) : dash;
          const u = tr.querySelector('[data-live-util]');
          if (p && p.util >= 0) {
            const pc = Math.round(p.util);
            u.innerHTML = '<span class="load' + (pc >= 90 ? ' crit' : pc >= 70 ? ' hi' : '') + '" style="--p:' + pc + '%"><span>' + pc + '%</span></span>';
          } else u.innerHTML = dash;
          tr.querySelector('[data-live-err]').innerHTML = p && p.err ? '<span class="bad">' + esc(p.err) + '</span>' : '';
        });
      })
      .catch(() => { wait = 60; })
      .finally(() => setTimeout(tick, wait * 1000));
  };
  setTimeout(tick, 5000);
})();
