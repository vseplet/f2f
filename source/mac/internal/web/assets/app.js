$(function () {
  const $status = $('#status-indicator');
  const $btnStart = $('#btn-start');
  const $btnStop = $('#btn-stop');
  const $btnAdd = $('#btn-add-intercept');
  const $btnClearLog = $('#btn-clear-log');
  const $list = $('#intercept-list');
  const $log = $('#log');
  const $interceptInput = $('#intercept-input');

  // The intercept list is owned by the frontend and persisted in
  // localStorage. The engine has its own copy while running (the "live"
  // entries with IDs and resolved prefixes) — we reconcile the two on
  // every status refresh.
  const INTERCEPTS_KEY = 'f2f:intercepts';
  function getStoredSpecs() {
    try {
      const raw = localStorage.getItem(INTERCEPTS_KEY);
      if (!raw) return [];
      const arr = JSON.parse(raw);
      return Array.isArray(arr) ? arr.filter((s) => typeof s === 'string') : [];
    } catch (_) { return []; }
  }
  function setStoredSpecs(arr) {
    localStorage.setItem(INTERCEPTS_KEY, JSON.stringify(arr));
  }
  function addStoredSpec(spec) {
    const list = getStoredSpecs();
    if (!list.includes(spec)) list.push(spec);
    setStoredSpecs(list);
  }
  function removeStoredSpec(spec) {
    setStoredSpecs(getStoredSpecs().filter((s) => s !== spec));
  }

  let liveIntercepts = []; // last seen from /api/status

  // Persist config form values across reloads. Each field has a localStorage
  // key; we restore on load and save on every change. Engine-driven updates
  // (when running) also write to localStorage so the form starts from the
  // last actual state next time.
  const FIELDS = [
    '#local-ip', '#peer-ip', '#listen', '#peer-udp',
    '#egress-iface', '#egress-subnet',
  ];
  const storageKey = (sel) => 'f2f:' + sel.slice(1);
  function restoreForm() {
    FIELDS.forEach((sel) => {
      const v = localStorage.getItem(storageKey(sel));
      if (v !== null && v !== '') $(sel).val(v);
    });
  }
  function persistField(sel) {
    localStorage.setItem(storageKey(sel), $(sel).val() || '');
  }
  FIELDS.forEach((sel) => $(sel).on('change input', () => persistField(sel)));

  const fmtBytes = (n) => {
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(1) + ' GB';
  };

  const errorOf = (xhr) => (xhr.responseJSON && xhr.responseJSON.error) || xhr.statusText || 'unknown error';

  function refreshStatus() {
    $.getJSON('/api/status', applyStatus).fail(() => {
      $status.text('API error').removeClass().addClass('px-3 py-1 rounded-full text-sm font-medium bg-rose-200 text-rose-800');
    });
  }

  function applyStatus(s) {
    if (s.running) {
      $status.text('Running · ' + (s.utun_name || '?')).removeClass().addClass('px-3 py-1 rounded-full text-sm font-medium bg-emerald-200 text-emerald-800');
      $btnStart.addClass('hidden');
      $btnStop.removeClass('hidden');
      $('#local-ip, #peer-ip, #listen, #peer-udp, #egress-iface, #egress-subnet').prop('disabled', true);
      // Reflect the actual running config so the form shows truth, not stale input.
      const live = {
        '#local-ip': s.local_ip,
        '#peer-ip': s.peer_ip,
        '#listen': s.listen_addr,
        '#peer-udp': s.peer_addr,
        '#egress-iface': s.egress_iface,
        '#egress-subnet': s.egress_subnet,
      };
      Object.entries(live).forEach(([sel, val]) => {
        if (val) {
          $(sel).val(val);
          persistField(sel);
        }
      });
    } else {
      $status.text('Stopped').removeClass().addClass('px-3 py-1 rounded-full text-sm font-medium bg-gray-200 text-gray-700');
      $btnStart.removeClass('hidden');
      $btnStop.addClass('hidden');
      $('#local-ip, #peer-ip, #listen, #peer-udp, #egress-iface, #egress-subnet').prop('disabled', false);
    }
    // Intercept management is always available — list lives in the browser.
    $interceptInput.prop('disabled', false);
    $btnAdd.prop('disabled', false);

    liveIntercepts = s.intercepts || [];
    renderIntercepts();

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));
  }

  function renderIntercepts() {
    $list.empty();
    const stored = getStoredSpecs();
    const liveBySpec = {};
    liveIntercepts.forEach((l) => { liveBySpec[l.spec] = l; });

    const seen = new Set();
    const items = stored.map((spec) => {
      seen.add(spec);
      const live = liveBySpec[spec];
      return { spec, live: live || null };
    });
    // Engine-side intercepts that aren't tracked locally (rare — e.g.,
    // started from CLI with --intercept). Show them too so they're visible.
    liveIntercepts.forEach((l) => {
      if (!seen.has(l.spec)) items.push({ spec: l.spec, live: l, orphan: true });
    });

    if (items.length === 0) {
      $list.append('<div class="text-sm text-gray-500">No intercepts yet. Add one below.</div>');
      return;
    }

    items.forEach((it) => {
      const $row = $('<div class="flex items-center justify-between bg-gray-50 rounded p-3">');
      const $info = $('<div>');
      const $title = $('<div class="font-medium text-sm flex items-center gap-2">');
      $title.append($('<span>').text(it.spec));
      if (it.live) {
        $title.append('<span class="text-xs px-2 py-0.5 rounded bg-emerald-100 text-emerald-800">active</span>');
      } else {
        $title.append('<span class="text-xs px-2 py-0.5 rounded bg-amber-100 text-amber-800">pending</span>');
      }
      if (it.orphan) {
        $title.append('<span class="text-xs px-2 py-0.5 rounded bg-gray-200 text-gray-700">not saved</span>');
      }
      $info.append($title);
      const prefixes = it.live ? (it.live.prefixes || []) : [];
      if (prefixes.length) {
        $info.append($('<div class="text-xs text-gray-500 font-mono mt-1">').text(prefixes.join(', ')));
      }
      $row.append($info);

      const $btn = $('<button class="text-rose-600 hover:text-rose-800 text-sm font-medium">Remove</button>');
      $btn.on('click', () => removeSpec(it.spec, it.live));
      $row.append($btn);

      $list.append($row);
    });
  }

  function removeSpec(spec, live) {
    removeStoredSpec(spec);
    const after = () => { refreshStatus(); };
    if (live && live.id) {
      $.ajax({ url: '/api/intercepts/' + encodeURIComponent(live.id), method: 'DELETE' })
        .done(after)
        .fail((xhr) => {
          alert('Remove failed: ' + errorOf(xhr));
          after();
        });
    } else {
      renderIntercepts();
    }
  }

  function loadIfaces() {
    $.getJSON('/api/ifaces', (ifs) => {
      const $sel = $('#egress-iface');
      const current = $sel.val();
      $sel.empty();
      $sel.append($('<option>').val('').text('— disabled —'));
      (ifs || []).forEach((i) => {
        const label = i.name + (i.ip ? '  (' + i.ip + ')' : '');
        $sel.append($('<option>').val(i.name).text(label));
      });
      if (current) $sel.val(current);
    });
  }

  $btnStart.on('click', () => {
    const cfg = {
      local_ip: $('#local-ip').val().trim(),
      peer_ip: $('#peer-ip').val().trim(),
      listen: $('#listen').val().trim(),
      peer: $('#peer-udp').val().trim(),
      // Seed the engine with whatever the user has saved locally — that
      // way pending intercepts become active immediately on Start.
      intercepts: getStoredSpecs(),
      egress_iface: $('#egress-iface').val(),
      egress_subnet: $('#egress-subnet').val().trim()
    };
    $.ajax({
      url: '/api/start',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify(cfg)
    }).done(refreshStatus).fail((xhr) => alert('Start failed: ' + errorOf(xhr)));
  });

  $btnStop.on('click', () => {
    $.ajax({ url: '/api/stop', method: 'POST' })
      .done(refreshStatus)
      .fail((xhr) => alert('Stop failed: ' + errorOf(xhr)));
  });

  function addOne(spec) {
    return $.ajax({
      url: '/api/intercepts',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ spec: spec })
    });
  }

  $btnAdd.on('click', () => {
    const raw = $interceptInput.val();
    const specs = raw.split(',').map((s) => s.trim()).filter(Boolean);
    if (specs.length === 0) return;

    // Save locally first — this is the source of truth for the next Start
    // and survives engine restarts.
    specs.forEach(addStoredSpec);
    $interceptInput.val('');
    renderIntercepts();

    // If the engine is currently running, apply the new entries live so
    // the user doesn't have to Stop/Start.
    const stoppedNow = $btnStart.is(':visible');
    if (stoppedNow) return;

    const errors = [];
    const requests = specs.map((spec) =>
      addOne(spec).fail((xhr) => errors.push(`${spec}: ${errorOf(xhr)}`))
    );
    $.when(...requests).always(() => {
      refreshStatus();
      if (errors.length) alert('Some intercepts failed to apply live:\n' + errors.join('\n'));
    });
  });

  $interceptInput.on('keydown', (e) => {
    if (e.key === 'Enter') $btnAdd.click();
  });

  $btnClearLog.on('click', () => $log.empty());

  function startLogStream() {
    const es = new EventSource('/api/log/stream');
    es.onmessage = (e) => {
      try {
        const entry = JSON.parse(e.data);
        const atBottom = ($log[0].scrollTop + $log[0].clientHeight) >= ($log[0].scrollHeight - 16);
        const $line = $('<div>').text(entry.message);
        $log.append($line);
        // Cap visible log at 1000 lines.
        const $lines = $log.children();
        if ($lines.length > 1000) $lines.first().remove();
        if (atBottom) $log[0].scrollTop = $log[0].scrollHeight;
      } catch (err) {
        console.error(err);
      }
    };
    es.onerror = () => {
      // Auto-reconnect happens; nothing extra needed.
    };
  }

  restoreForm();
  loadIfaces();
  refreshStatus();
  setInterval(refreshStatus, 3000);
  startLogStream();
});
