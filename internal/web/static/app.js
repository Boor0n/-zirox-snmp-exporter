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
    rows.forEach((tr) => { if (!tr.hidden) tr.querySelector('input[type=checkbox]').checked = on; });
  }));
  $$('[data-check-group]', form).forEach((box) => box.addEventListener('change', () => {
    $$('tr.obj', box.closest('table')).forEach((tr) => {
      if (!tr.hidden) tr.querySelector('input[type=checkbox]').checked = box.checked;
    });
  }));
})();
