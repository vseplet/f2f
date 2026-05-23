$(function () {
  // Tab switching. The terminal-styled tabbar at the top is the only UI:
  // we toggle .ax-tab-active on the clicked button and swap visible panels.
  $('.ax-tab').on('click', function () {
    const tab = $(this).data('tab');
    if (!tab) return;
    $('.ax-tab').removeClass('ax-tab-active');
    $(this).addClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-' + tab).removeClass('hidden');
    $(document).trigger('f2f:tab-changed', [tab]);
  });

  const $btnEngine = $('#btn-engine');
  const $engineState = $btnEngine.find('.ax-engine-state');
  const $engineLabel = $btnEngine.find('.ax-engine-label');
  const $engineMeta = $btnEngine.find('.ax-engine-meta');
  let engineRunning = false;
  const $btnAdd = $('#btn-add-intercept');
  const $btnClearLog = $('#btn-clear-log');
  const $list = $('#intercept-list');
  const $log = $('#log');
  const $interceptInput = $('#intercept-input');

  // All per-camp settings (intercepts, my-domains, firewall, name, peer
  // catalog) are owned by the backend now — $HOME/.f2f/<camp_id>.config.json.
  // The frontend just reads /api/status for live state and PUT/POST'es
  // changes to the appropriate endpoint. No more localStorage.
  // One-shot purge of any leftover keys from the old localStorage-backed
  // implementation. Runs at load time; harmless on a clean install.
  ['f2f:intercepts', 'f2f:my-domains', 'f2f:camp-name', 'f2f:camp-id', 'f2f:camp-room']
    .forEach((k) => { try { localStorage.removeItem(k); } catch (_) {} });

  let liveIntercepts = []; // last seen from /api/status
  let livePeers = [];      // last seen camp peers from /api/status
  const expandedIntercepts = new Set(); // keys (spec|peer) currently expanded

  // Camp identity is loaded from the backend on first render — see
  // refreshCamps(). The form fields are no longer the source of truth.
  function restoreForm() { /* no-op: backend is authoritative now */ }

  const fmtBytes = (n) => {
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(1) + ' GB';
  };

  const errorOf = (xhr) => (xhr.responseJSON && xhr.responseJSON.error) || xhr.statusText || 'unknown error';

  // armRemove wires a destructive button to a two-click pattern: first
  // click flips the label to "confirm?" for 3 s; a second click in
  // that window calls onConfirm. No modal dialog. After the window
  // expires the button reverts. Drop-in replacement for confirm().
  function armRemove($btn, onConfirm, opts) {
    opts = opts || {};
    const label = opts.label || 'remove';
    const armed = opts.armedLabel || 'confirm?';
    const windowMs = opts.windowMs || 3000;
    let armedAt = 0;
    let timer = null;
    function disarm() {
      armedAt = 0;
      $btn.text(label).removeClass('is-armed');
      if (timer) { clearTimeout(timer); timer = null; }
    }
    $btn.text(label);
    $btn.on('click', (e) => {
      e.stopPropagation();
      const now = Date.now();
      if (armedAt && (now - armedAt) <= windowMs) {
        disarm();
        onConfirm();
        return;
      }
      armedAt = now;
      $btn.text(armed).addClass('is-armed');
      timer = setTimeout(disarm, windowMs);
    });
  }

  // setEngineState updates the combined status/toggle button in the
  // tabbar. `state` ∈ {running, stopped, loading, error}; `label` is the
  // primary text; `meta` is the small extra ("· utun7", "API error").
  function setEngineState(state, label, meta) {
    const icons = { running: '■', stopped: '▶', loading: '⋯', error: '!' };
    const titles = {
      running: 'click to stop',
      stopped: 'click to start',
      loading: 'loading…',
      error: 'click to start',
    };
    $btnEngine
      .removeClass('state-running state-stopped state-loading state-error')
      .addClass('state-' + state)
      .attr('title', titles[state] || '');
    $engineState.text(icons[state] || '?');
    $engineLabel.text(label);
    $engineMeta.text(meta || '');
    engineRunning = state === 'running';
  }
  function refreshStatus() {
    $.getJSON('/api/status', applyStatus).fail(() => setEngineState('error', 'API error', ''));
  }

  // refreshCamps pulls $HOME/.f2f/state.json — the last selected camp_id
  // and the roster of known camps — and wires up the dropdown. Called
  // once on load, after start, and after Stop.
  let knownCamps = [];
  function refreshCamps() {
    $.getJSON('/api/camps', (st) => {
      knownCamps = (st && Array.isArray(st.known_camps)) ? st.known_camps : [];
      renderCampPicker();
      const last = st && st.last_camp_id;
      // Pre-select the last camp in the picker if engine isn't running
      // and the user hasn't picked anything else yet — gives one-click
      // "start where you left off" affordance.
      if (last && !engineRunning && !$('#camp-picker').val()) {
        $('#camp-picker').val(last);
      }
    });
  }
  // renderCampPicker builds the dropdown: every known camp + a
  // sentinel "+ new camp" at the bottom that reveals the join form.
  function renderCampPicker() {
    const $sel = $('#camp-picker');
    const cur = $sel.val();
    $sel.empty();
    $sel.append($('<option>').val('').text('— pick a camp —'));
    knownCamps.forEach((c) => {
      const label = c.name ? `${c.id} (${c.name})` : c.id;
      $sel.append($('<option>').val(c.id).text(label));
    });
    $sel.append($('<option>').val('__new__').text('+ new camp'));
    if (cur) $sel.val(cur);
  }
  $('#camp-picker').on('change', function () {
    const id = $(this).val();
    if (id === '__new__') {
      $('#camp-name').val('');
      $('#camp-id').val('');
      $('#new-camp-form').removeClass('hidden');
      setTimeout(() => $('#camp-name').focus(), 0);
      return;
    }
    $('#new-camp-form').addClass('hidden');
    if (!id) return;
    // Known camp picked — auto-start. Backend stop+starts if we were
    // running with a different camp_id. triggerStart re-reads picker
    // state itself, so we don't need to mirror values into inputs.
    triggerStart();
  });
  $('#btn-new-camp-start').on('click', () => {
    const id = ($('#camp-id').val() || '').trim();
    const name = ($('#camp-name').val() || '').trim();
    if (!id || !name) {
      alert('camp_id and name are required for a new camp');
      return;
    }
    triggerStart();
  });
  $('#btn-new-camp-cancel').on('click', () => {
    $('#new-camp-form').addClass('hidden');
    $('#camp-picker').val('');
  });
  // Running-state header link: collapse status, expose picker so the
  // user can switch without stopping first.
  $('#identity-switch').on('click', (e) => {
    e.preventDefault();
    $('#identity-status').addClass('hidden');
    $('#identity-picker').removeClass('hidden');
    $('#camp-picker').val('').trigger('focus');
  });
  // Click the pub-key cell to copy its full hex to the clipboard.
  // No alert — flash the cell colour for half a second as feedback.
  $('#identity-pub').on('click', function () {
    const $el = $(this);
    const pub = $el.data('pub');
    if (!pub) return;
    navigator.clipboard.writeText(pub).then(() => {
      const prev = $el.css('color');
      $el.css('color', '#7fc474');
      setTimeout(() => $el.css('color', prev), 500);
    }).catch(() => {});
  });

  // Auto-start fires once after the first /api/status response that says
  // the engine is stopped *and* we have a camp identity stored. After
  // that, the user is in control via the Start/Stop buttons.
  //
  // We also flip `autoStarted` on the first time we *see* the engine
  // running (e.g. it was already up before the page loaded). Otherwise
  // the user's first manual Stop would be immediately followed by an
  // auto-Start, which races with camp's session cleanup and fails with
  // "name_taken".
  // `pendingOp` guards the engine button against periodic /api/status
  // races while a Start/Stop is in flight. Both Start and Stop on the
  // server take a few seconds (utun, routes, STUN, WS close+wait); during
  // that window the 3s refresh would see the stale running flag and
  // overwrite our "starting…/stopping…" loading state, then the user's
  // next click triggers a second operation that races the first and gets
  // "already running" / a name_taken.
  let pendingOp = null; // 'starting' | 'stopping' | null
  // Tracked from /api/status — drives `<name>.<camp_id>.f2f` rendering
  // in the domains panels. With the identity rework #camp-id input is
  // now only filled on the "+ new camp" path, so we can't read it
  // there anymore. Falls back to the picker value if status hasn't
  // arrived yet (page-load → first refreshStatus is a brief window).
  let currentCampID = '';
  function campIDOrPlaceholder() {
    return currentCampID || ($('#camp-picker').val() || '').trim().replace(/^__new__$/, '') || '<camp_id>';
  }

  function applyStatus(s) {
    if (pendingOp) {
      $('#camp-name, #camp-id, #camp-picker').prop('disabled', true);
    } else if (s.running) {
      setEngineState('running', 'running', '· ' + (s.utun_name || '?'));
      currentCampID = s.camp_id || '';
      // Running: collapse the picker and form into a key:value readout.
      // The "switch" link inside #identity-status re-exposes the picker
      // without forcing a manual stop first.
      $('#identity-name').text(s.camp_name || '?');
      $('#identity-camp').text(s.camp_id || '?');
      $('#identity-ip').text(s.local_ip || '—');
      $('#identity-reflex').text(s.camp_reflex || '—');
      const pub = s.identity_pub || '';
      const fp = s.identity_fp || '';
      $('#identity-pub').text(pub || '—').data('pub', pub);
      $('#identity-fp').text(fp ? '· fp ' + fp : '');
      $('#identity-status').removeClass('hidden');
      $('#identity-picker').addClass('hidden');
      $('#new-camp-form').addClass('hidden');
      $('#camp-name, #camp-id, #camp-picker').prop('disabled', false);
    } else {
      setEngineState('stopped', 'start', '');
      currentCampID = '';
      $('#identity-status').addClass('hidden');
      $('#identity-picker').removeClass('hidden');
      $('#camp-name, #camp-id, #camp-picker').prop('disabled', false);
    }
    // Intercept management is always available — list lives in the browser.
    $interceptInput.prop('disabled', false);
    $btnAdd.prop('disabled', false);

    liveIntercepts = s.intercepts || [];
    livePeers = s.peers || [];
    refreshInterceptPeerSelect();
    refreshCallPeerSelect(s.active_peer_tunnel_ip || '');
    renderIntercepts();

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));

    renderCampHealth(s);
    renderDiagnostics(s);
  }

  // Last status sample used to compute tx/rx rates. We poll /api/status
  // every 3s (see setInterval below), so the delta is the per-window
  // throughput; UI converts to per-second.
  let lastDiagSample = null;

  // renderDiagnostics fills the diagnostics tab from Status.diagnostics
  // and a couple of top-level fields. Safe to call even when engine is
  // stopped — we just paint dashes.
  function renderDiagnostics(s) {
    const d = (s && s.diagnostics) || null;
    if (!s || !s.running || !d) {
      $('#diag-uptime,#diag-goroutines,#diag-udp-addr,#diag-utun,#diag-tx-rate,#diag-rx-rate,#diag-dns-resolver,#diag-dns-queries,#diag-dns-last').text('—');
      $('#diag-dns-dot').attr('class', 'ax-dot offline').attr('title', 'engine not running');
      lastDiagSample = null;
      return;
    }
    $('#diag-uptime').text(fmtDuration(d.uptime_seconds || 0));
    $('#diag-goroutines').text(d.goroutines || 0);
    $('#diag-udp-addr').text(d.udp_local_addr || '—');
    $('#diag-utun').text(s.utun_name || '—');

    // Rate: compare to last sample's tx_bytes / rx_bytes.
    const now = Date.now();
    const tx = s.tx_bytes || 0, rx = s.rx_bytes || 0;
    if (lastDiagSample && now > lastDiagSample.t) {
      const dt = (now - lastDiagSample.t) / 1000;
      const txRate = Math.max(0, (tx - lastDiagSample.tx) / dt);
      const rxRate = Math.max(0, (rx - lastDiagSample.rx) / dt);
      $('#diag-tx-rate').text(fmtBytes(Math.round(txRate)) + '/s');
      $('#diag-rx-rate').text(fmtBytes(Math.round(rxRate)) + '/s');
    } else {
      $('#diag-tx-rate').text('—');
      $('#diag-rx-rate').text('—');
    }
    lastDiagSample = { t: now, tx, rx };

    // DNS row. The dot encodes a single question: "is macOS even
    // routing queries to us?" — green when the /etc/resolver file is
    // present AND we've seen a query in the last few minutes.
    const resolverOK = !!d.dns_resolver_ok;
    const lastQ = d.dns_last_query_ms ? (now - d.dns_last_query_ms) : -1;
    let dnsDot = 'offline', dnsTitle = 'no /etc/resolver file';
    if (!resolverOK) {
      dnsDot = 'unreachable';
      dnsTitle = '/etc/resolver missing — macOS not pointed at us';
    } else if (lastQ >= 0 && lastQ < 300000) {
      dnsDot = 'reachable';
      dnsTitle = 'resolver file present, queries arriving';
    } else if (lastQ >= 0) {
      dnsDot = 'degraded';
      dnsTitle = 'resolver file present, last query stale';
    } else {
      dnsDot = 'degraded';
      dnsTitle = 'resolver file present, no queries yet';
    }
    $('#diag-dns-dot').attr('class', 'ax-dot ' + dnsDot).attr('title', dnsTitle);
    $('#diag-dns-resolver').text(resolverOK ? 'present' : 'missing');
    const total = d.dns_total || 0;
    const ok = d.dns_noerror || 0;
    const nx = d.dns_nxdomain || 0;
    const rf = d.dns_refused || 0;
    $('#diag-dns-queries').text(total + ' total · ' + ok + ' ok · ' + nx + ' nxdomain · ' + rf + ' refused');
    $('#diag-dns-last').text(lastQ < 0 ? 'never' : (Math.floor(lastQ / 1000) + 's ago'));
  }

  function fmtDuration(seconds) {
    if (seconds < 60) return seconds + 's';
    const m = Math.floor(seconds / 60);
    if (m < 60) return m + 'm ' + (seconds % 60) + 's';
    const h = Math.floor(m / 60);
    if (h < 24) return h + 'h ' + (m % 60) + 'm';
    const d = Math.floor(h / 24);
    return d + 'd ' + (h % 24) + 'h';
  }

  // renderCampHealth fills the "— camp link" section from Status.camp_health.
  // Two independent rows: UDP announce/reply and HTTP peer-list poll. They
  // travel different transports, so a single side can be wedged while the
  // other is fine — surfacing them separately makes that visible.
  function renderCampHealth(s) {
    const $tbl = $('#camp-health-table');
    const $msg = $('#camp-health-status');
    if (!s || !s.running || !s.camp_health) {
      $tbl.addClass('hidden');
      $msg.text(s && s.running ? 'no camp data yet' : 'engine not running').show();
      return;
    }
    $msg.hide();
    $tbl.removeClass('hidden');
    const h = s.camp_health;
    const now = Date.now();

    // UDP row. Healthy threshold: announce cadence is 20s, so a reply in
    // the last 60s means we're comfortably alive. Beyond 180s call it dead.
    const udpReplyAge = h.udp_last_reply_ms ? (now - h.udp_last_reply_ms) : -1;
    const udpSentAge = h.udp_last_sent_ms ? (now - h.udp_last_sent_ms) : -1;
    let udpDot = 'offline', udpTitle = 'no announce reply ever';
    if (udpReplyAge >= 0 && udpReplyAge < 60000) {
      udpDot = 'reachable'; udpTitle = 'recent reply';
    } else if (udpReplyAge >= 0 && udpReplyAge < 180000) {
      udpDot = 'degraded'; udpTitle = 'reply getting stale';
    } else if (udpReplyAge >= 0) {
      udpDot = 'unreachable'; udpTitle = 'no reply for too long';
    }
    $('#camp-udp-dot').attr('class', 'ax-dot ' + udpDot).attr('title', udpTitle);
    $('#camp-udp-rtt').text(h.udp_rtt_ms ? h.udp_rtt_ms + 'ms' : '—');
    let udpMeta;
    if (udpReplyAge < 0) {
      udpMeta = udpSentAge >= 0 ? 'sent ' + Math.floor(udpSentAge / 1000) + 's ago, no reply' : 'idle';
    } else {
      udpMeta = 'reply ' + Math.floor(udpReplyAge / 1000) + 's ago';
    }
    $('#camp-udp-meta').text(udpMeta);

    // HTTP row. Poll cadence is 30s; healthy if last success < 90s.
    const httpSuccessAge = h.http_last_success_ms ? (now - h.http_last_success_ms) : -1;
    const httpPollAge = h.http_last_poll_ms ? (now - h.http_last_poll_ms) : -1;
    const lastErr = h.http_last_err || '';
    let httpDot = 'offline', httpTitle = 'no poll yet';
    if (lastErr && httpSuccessAge < 0) {
      httpDot = 'unreachable'; httpTitle = 'failing: ' + lastErr;
    } else if (lastErr) {
      httpDot = 'degraded'; httpTitle = 'last attempt failed: ' + lastErr;
    } else if (httpSuccessAge >= 0 && httpSuccessAge < 90000) {
      httpDot = 'reachable'; httpTitle = 'recent success';
    } else if (httpSuccessAge >= 0) {
      httpDot = 'degraded'; httpTitle = 'last success getting stale';
    }
    $('#camp-http-dot').attr('class', 'ax-dot ' + httpDot).attr('title', httpTitle);
    $('#camp-http-rtt').text(h.http_rtt_ms ? h.http_rtt_ms + 'ms' : '—');
    let httpMeta;
    if (lastErr) {
      httpMeta = 'err: ' + lastErr;
    } else if (httpSuccessAge >= 0) {
      const peers = h.http_peers_count || 0;
      httpMeta = peers + ' peer' + (peers === 1 ? '' : 's') + ' · ' + Math.floor(httpSuccessAge / 1000) + 's ago';
    } else if (httpPollAge >= 0) {
      httpMeta = 'polled ' + Math.floor(httpPollAge / 1000) + 's ago, no data';
    } else {
      httpMeta = 'idle';
    }
    $('#camp-http-meta').text(httpMeta);
  }

  function renderIntercepts() {
    $list.empty();
    const items = liveIntercepts.map((l) => ({ spec: l.spec, peer: l.peer, live: l }));

    $('#intercept-meta').text(items.length);

    if (items.length === 0) {
      $list.append('<div class="ax-list-empty">no intercepts. add one below.</div>');
      return;
    }

    items.forEach((it) => {
      const key = it.spec + '\x00' + it.peer;
      const prefixes = it.live ? (it.live.prefixes || []) : [];
      const parsed = prefixes.map(parsePrefixEntry);
      const v4count = parsed.filter((p) => p.kind === 'v4').length;
      const v6count = parsed.filter((p) => p.kind === 'v6').length;
      const expanded = expandedIntercepts.has(key);

      const $row = $('<div class="ax-intercept">').toggleClass('is-expanded', expanded);
      const $head = $('<div class="ax-intercept-head">');
      $head.append($('<span class="ax-intercept-caret">').text(expanded ? '▼' : '▶'));
      $head.append($('<span class="ax-intercept-spec">').text(it.spec));
      $head.append($('<span class="ax-pill ax-pill-peer">').text('via ' + it.peer));
      if (it.live) $head.append($('<span class="ax-pill ax-pill-active">').text('active'));
      else         $head.append($('<span class="ax-pill ax-pill-pending">').text('pending'));

      const $meta = $('<span class="ax-intercept-meta">');
      if (parsed.length) {
        const bits = [];
        if (v4count) bits.push(`<span class="ax-meta-routes">${v4count} route${v4count === 1 ? '' : 's'}</span>`);
        if (v6count) bits.push(`<span class="ax-meta-reject">${v6count} reject</span>`);
        $meta.html(bits.join(' · '));
      }
      $head.append($meta);

      const $rm = $('<button class="ax-list-remove">remove</button>');
      $rm.on('click', (e) => { e.stopPropagation(); removeSpec(it.live); });
      $head.append($rm);

      $head.on('click', () => {
        if (expandedIntercepts.has(key)) expandedIntercepts.delete(key);
        else expandedIntercepts.add(key);
        renderIntercepts();
      });
      $row.append($head);

      if (expanded) {
        const $body = $('<div class="ax-intercept-body">');
        if (parsed.length === 0) {
          $body.append($('<div class="ax-intercept-empty">').text('no resolved routes yet'));
        } else {
          const $tbl = $('<table class="ax-intercept-table">');
          $tbl.append('<thead><tr><th>kind</th><th>resolved</th><th class="ax-policy">policy</th></tr></thead>');
          const $tb = $('<tbody>');
          parsed.forEach((p) => {
            const $tr = $('<tr>').addClass(p.kind);
            $tr.append($('<td class="ax-kind">').text(p.kind));
            $tr.append($('<td class="ax-resolved">').text(p.resolved));
            $tr.append($('<td class="ax-policy">').text(p.policy));
            $tb.append($tr);
          });
          $tbl.append($tb);
          $body.append($tbl);
        }
        $row.append($body);
      }

      $list.append($row);
    });
  }

  // parsePrefixEntry takes a string like "5.255.255.242/32" or
  // "2a02:6b8::2:242/128 (reject)" and pulls the kind/resolved/policy
  // fields the UI shows in the expanded view.
  function parsePrefixEntry(s) {
    const reject = / \(reject\)$/.test(s);
    const cidr = s.replace(/ \(reject\)$/, '');
    const kind = cidr.indexOf(':') >= 0 ? 'v6' : 'v4';
    return {
      kind,
      resolved: cidr,
      policy: reject ? 'reject' : '→ peer',
    };
  }

  function removeSpec(live) {
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

  // peerOptionLabel formats a peer for dropdown display. Offline peers
  // are still selectable (binding survives until they re-announce); we
  // just tag them so the user understands the current state.
  function peerOptionLabel(p) {
    return p.name + (p.online === false ? ' (offline)' : '');
  }

  // refreshInterceptPeerSelect populates the dropdown next to the add-input
  // with currently visible camp peers (excluding self). Preserves the
  // current selection if still valid.
  function refreshInterceptPeerSelect() {
    const $sel = $('#intercept-peer');
    const current = $sel.val();
    $sel.empty();
    $sel.append($('<option>').val('').text('— peer —'));
    livePeers
      .filter((p) => !p.self)
      .forEach((p) => $sel.append($('<option>').val(p.name).text(peerOptionLabel(p))));
    if (current && livePeers.some((p) => p.name === current && !p.self)) {
      $sel.val(current);
    }
  }

  function triggerStart() {
    // Source of truth is the picker. Two paths:
    //   - picker = known camp id → use it directly, name from
    //     knownCamps entry (camp config on disk already has the name).
    //   - picker = "__new__" → user is creating a fresh camp, read
    //     name + id from the join form below.
    //   - picker = "" → nothing selected, surface it.
    const pick = ($('#camp-picker').val() || '').trim();
    let id = '';
    let name = '';
    if (pick && pick !== '__new__') {
      id = pick;
      const entry = knownCamps.find((c) => c.id === pick);
      if (entry && entry.name) name = entry.name;
    } else if (pick === '__new__') {
      id = ($('#camp-id').val() || '').trim();
      name = ($('#camp-name').val() || '').trim();
    }
    if (!id) {
      $('#identity-picker').removeClass('hidden');
      $('#camp-picker').trigger('focus');
      return;
    }
    const cfg = { camp_id: id };
    if (name) cfg.camp_name = name;
    pendingOp = 'starting';
    setEngineState('loading', 'starting…', '');
    $.ajax({
      url: '/api/start',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify(cfg)
    })
      .always(() => { pendingOp = null; })
      .done(() => { refreshStatus(); refreshCamps(); })
      .fail((xhr) => {
        refreshStatus();
        alert('Start failed: ' + errorOf(xhr));
      });
  }

  function triggerStop() {
    pendingOp = 'stopping';
    setEngineState('loading', 'stopping…', '');
    $.ajax({ url: '/api/stop', method: 'POST' })
      .always(() => { pendingOp = null; })
      .done(refreshStatus)
      .fail((xhr) => {
        refreshStatus();
        alert('Stop failed: ' + errorOf(xhr));
      });
  }

  $btnEngine.on('click', () => {
    if ($btnEngine.hasClass('state-loading')) return;
    if (engineRunning) triggerStop();
    else triggerStart();
  });

  function addOne(spec, peer) {
    return $.ajax({
      url: '/api/intercepts',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ spec, peer })
    });
  }

  $btnAdd.on('click', () => {
    const raw = $interceptInput.val();
    const peer = $('#intercept-peer').val();
    const specs = raw.split(',').map((s) => s.trim()).filter(Boolean);
    if (specs.length === 0) return;
    if (!peer) {
      alert('Pick a peer to route this intercept through.');
      return;
    }
    if (!engineRunning) {
      alert('Engine must be running to add intercepts.');
      return;
    }
    $interceptInput.val('');
    const errors = [];
    const requests = specs.map((spec) =>
      addOne(spec, peer).fail((xhr) => errors.push(`${spec}: ${errorOf(xhr)}`))
    );
    $.when(...requests).always(() => {
      refreshStatus();
      if (errors.length) alert('Some intercepts failed to apply:\n' + errors.join('\n'));
    });
  });

  $interceptInput.on('keydown', (e) => {
    if (e.key === 'Enter') $btnAdd.click();
  });

  // Under heavy traffic the engine emits log lines faster than the browser
  // can lay out individual DOM appends. Batch incoming messages and flush
  // once per animation frame to keep the main thread responsive.
  const logEl = $log[0];
  const LOG_MAX = 1000;
  let logQueue = [];
  let logFlushScheduled = false;
  let logLineCount = 0;

  function flushLogs() {
    logFlushScheduled = false;
    if (logQueue.length === 0) return;
    const atBottom = (logEl.scrollTop + logEl.clientHeight) >= (logEl.scrollHeight - 16);

    // If the queue alone exceeds the cap, drop the oldest queued lines so
    // we never build a giant fragment we're about to throw away.
    if (logQueue.length > LOG_MAX) {
      logQueue = logQueue.slice(logQueue.length - LOG_MAX);
    }

    const frag = document.createDocumentFragment();
    for (const msg of logQueue) {
      const div = document.createElement('div');
      div.textContent = msg;
      frag.appendChild(div);
    }
    logLineCount += logQueue.length;
    logQueue = [];
    logEl.appendChild(frag);

    while (logLineCount > LOG_MAX && logEl.firstChild) {
      logEl.removeChild(logEl.firstChild);
      logLineCount--;
    }
    if (atBottom) logEl.scrollTop = logEl.scrollHeight;
  }
  function scheduleLogFlush() {
    if (logFlushScheduled) return;
    logFlushScheduled = true;
    requestAnimationFrame(flushLogs);
  }
  $btnClearLog.on('click', () => {
    logQueue = [];
    logLineCount = 0;
    $log.empty();
  });

  function startLogStream() {
    const es = new EventSource('/api/log/stream');
    es.onmessage = (e) => {
      try {
        const entry = JSON.parse(e.data);
        logQueue.push(entry.message);
        scheduleLogFlush();
      } catch (err) {
        console.error(err);
      }
    };
    es.onerror = () => {
      // Auto-reconnect happens; nothing extra needed.
    };
  }


  // Camp tab — list of peers in our current camp. Polls our local proxy
  // (/api/camp/peers), which in turn fetches /api/id/<camp_id> from the
  // configured camp server. Off-state ("engine not running") is the only
  // non-happy branch; once we're in a camp there's always at least one
  // peer (us).
  const $campStatus = $('#camp-peers-status');
  const $campTable = $('#camp-peers-table');
  const $campBody = $('#camp-peers-tbody');
  const $campIDMeta = $('#camp-id-meta');

  function humanAgo(ts) {
    const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (s < 60) return s + 's';
    const m = Math.floor(s / 60);
    if (m < 60) return m + 'm';
    const h = Math.floor(m / 60);
    if (h < 24) return h + 'h';
    return Math.floor(h / 24) + 'd';
  }

  function renderCampPeers(data) {
    if (!data || data.running === false) {
      $campStatus.text('engine not running').show();
      $campTable.addClass('hidden');
      $campIDMeta.text('');
      return;
    }
    const peers = Array.isArray(data.peers) ? data.peers : [];
    $campIDMeta.text(data.camp_id || '');
    const hasOthers = peers.some((p) => !p.self);
    if (!hasOthers) {
      $campStatus.text('waiting for someone to join').show();
    } else {
      $campStatus.hide();
    }
    $campBody.empty();
    for (const p of peers) {
      const endpoint = p.udp_endpoint || (p.public_ip ? p.public_ip + (p.udp_port ? ':' + p.udp_port : '') : '—');
      // Online   = we received any UDP from peer recently (one-way truth).
      // Verified = we got a pong recently → round-trip works (two-way truth).
      // InCamp   = camp server sees peer's announce.
      // Color matrix:
      //   self                                  → yellow
      //   online + verified                     → green (true bidirectional)
      //   online without verified               → orange (asymmetric: peer
      //                                                   reaches us, our
      //                                                   pings unanswered)
      //   in_camp without online                → red   (camp sees them, we can't reach)
      //   neither                               → gray
      let dotClass, dotTitle;
      if (p.self) {
        dotClass = 'self';
        dotTitle = 'you';
      } else if (p.verified) {
        dotClass = 'reachable';
        dotTitle = 'verified bidirectional' + (p.rtt_ms ? ' — rtt ' + p.rtt_ms + 'ms' : '');
      } else if (p.online) {
        dotClass = 'degraded';
        dotTitle = 'one-way only — peer sends to us but our pings unanswered (peer outdated, or our send-path blocked)';
      } else if (p.in_camp) {
        dotClass = 'unreachable';
        dotTitle = 'in camp roster but no packets received yet (NAT / hole-punch issue?)';
      } else {
        dotClass = 'offline';
        dotTitle = 'not in camp roster';
      }
      const $row = $('<tr>')
        .addClass(p.self ? 'is-self' : '')
        .addClass(!p.online && !p.self ? 'is-offline' : '')
        .attr('data-tunnel-ip', p.tunnel_ip || '');
      // Optional "in camp" badge next to the name — purely informational,
      // shown when camp sees the peer regardless of our local view.
      // Fingerprint pill renders the SHA-256(pub) prefix as the peer's
      // stable identity — name is just a mutable alias.
      const $name = $('<td>');
      $name.append(document.createTextNode(p.name + (p.self ? ' (you)' : '')));
      if (p.fp) {
        $name.append($('<span class="ax-pill ax-pill-fp" style="margin-left:6px">')
          .text('fp ' + p.fp)
          .attr('title', p.pub ? 'pub ' + p.pub : 'fingerprint of ed25519 pub'));
      }
      if (!p.self && p.in_camp) {
        $name.append($('<span class="ax-pill ax-pill-peer" style="margin-left:6px">').text('in camp'));
      }
      // RTT: show the latest measurement when we have one. Muted when
      // the pong is stale (peer marked degraded above) so the number
      // doesn't pretend to be current.
      let rttText = '—';
      let rttTitle = '';
      if (p.rtt_ms && p.last_pong_ms) {
        rttText = p.rtt_ms + 'ms';
        rttTitle = 'last pong ' + humanAgo(p.last_pong_ms) + ' ago';
      }
      const $rtt = $('<td>').text(rttText).attr('title', rttTitle);
      if (!p.verified) $rtt.addClass('muted');
      $row.append(
        $('<td>').append($('<span>').addClass('ax-dot ' + dotClass).attr('title', dotTitle)),
        $name,
        $('<td>').text(p.tunnel_ip || '—'),
        $('<td>').text(endpoint || '—'),
        $rtt,
        $('<td>').addClass('muted').text(p.joined_at ? humanAgo(p.joined_at) : '—'),
      );
      $campBody.append($row);
    }
    $campTable.removeClass('hidden');
  }

  // Meet-tab peer selector: set the engine's active peer (the one
  // signalling/HTTP-forward in /api/signal/outbox goes to). Reflected
  // on every status refresh in refreshCallPeerSelect.
  $('#ax-call-peer').on('change', function () {
    const name = $(this).val();
    const peer = livePeers.find((p) => !p.self && p.name === name);
    const tunnelIP = peer ? peer.tunnel_ip : '';
    $.ajax({
      url: '/api/peers/active',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ tunnel_ip: tunnelIP }),
    })
      .done(refreshStatus)
      .fail((xhr) => alert('Set active failed: ' + errorOf(xhr)));
  });

  // refreshCallPeerSelect mirrors live camp peers into the meet-tab
  // dropdown, preserving the currently-active selection.
  function refreshCallPeerSelect(activeTunnelIP) {
    const $sel = $('#ax-call-peer');
    const others = livePeers.filter((p) => !p.self);
    $sel.empty();
    $sel.append($('<option>').val('').text('— peer —'));
    others.forEach((p) => {
      let label = p.name;
      if (p.online === false) label += ' (offline)';
      else if (!p.reachable) label += ' (unreachable)';
      $sel.append($('<option>').val(p.name).text(label));
    });
    const activePeer = others.find((p) => p.tunnel_ip === activeTunnelIP);
    $sel.val(activePeer ? activePeer.name : '');
  }

  function refreshCampPeers() {
    $.ajax({ url: '/api/camp/peers', dataType: 'json' })
      .done(renderCampPeers)
      .fail(() => {
        $campStatus.text('failed to fetch camp state').show();
        $campTable.addClass('hidden');
      });
  }

  // makeHealthDot renders a small status indicator next to a domain.
  // `entry.health`: "ok" → green, "fail" → red, "" → grey (untested yet).
  function makeHealthDot(entry) {
    const status = entry && entry.health;
    let cls = 'unknown';
    let title = 'health unknown';
    if (status === 'ok') { cls = 'reachable'; title = 'backend is up'; }
    else if (status === 'fail') { cls = 'unreachable'; title = 'backend not responding'; }
    return $('<span class="ax-dot">').addClass(cls).attr('title', title).css({
      'display': 'inline-block', 'width': '8px', 'height': '8px', 'border-radius': '50%',
      'margin-right': '8px',
    });
  }

  // ---- DNS tab: own published domains + known domains across peers ----
  // Backend is the source of truth — engine persists the list in the
  // per-camp config and re-publishes it on start. UI just reads /api/my-domains.
  let myDomains = [];
  function refreshMyDomains() {
    $.getJSON('/api/my-domains', (list) => {
      myDomains = Array.isArray(list) ? list : [];
      renderMyDomains();
    });
  }
  function renderMyDomains() {
    const $list = $('#my-domains-list');
    $list.empty();
    $('#my-domains-meta').text(myDomains.length);
    if (myDomains.length === 0) {
      $list.append('<div class="ax-list-empty">no domains yet. publish one below.</div>');
      return;
    }
    myDomains.forEach((d) => {
      const fqdn = d.name + '.' + campIDOrPlaceholder() + '.f2f';
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      $head.append(makeHealthDot(d));
      const $link = $('<a class="ax-intercept-spec ax-domain-link" target="_blank">')
        .attr('href', 'https://' + fqdn + '/')
        .text(fqdn);
      $head.append($link);
      const target = (d.host || '127.0.0.1') + (d.port ? ':' + d.port : '');
      if (d.port) {
        $head.append($('<span class="ax-pill ax-pill-peer">').text('→ ' + target));
      }
      const $rm = $('<button class="ax-list-remove">remove</button>');
      $rm.on('click', (e) => {
        e.stopPropagation();
        myDomains = myDomains.filter((e) => e.name !== d.name);
        putMyDomains(myDomains);
      });
      $head.append($('<span class="ax-intercept-meta">'));
      $head.append($rm);
      $row.append($head);
      $list.append($row);
    });
  }
  function putMyDomains(list) {
    $.ajax({
      url: '/api/my-domains',
      method: 'PUT',
      contentType: 'application/json',
      data: JSON.stringify(list),
    })
      .done(refreshMyDomains)
      .fail((xhr) => { alert('Save failed: ' + errorOf(xhr)); });
  }
  $('#btn-add-my-domain').on('click', () => {
    const name = ($('#my-domain-name').val() || '').trim().toLowerCase();
    if (!name) return;
    if (!/^[a-z0-9-]+$/.test(name)) {
      alert('Name may contain only lowercase letters, digits, and "-".');
      return;
    }
    const port = parseInt($('#my-domain-port').val(), 10);
    const host = ($('#my-domain-host').val() || '').trim();
    const entry = { name };
    if (port > 0 && port < 65536) entry.port = port;
    if (host) entry.host = host;
    const next = myDomains.filter((e) => e.name !== name).concat(entry);
    putMyDomains(next);
    $('#my-domain-name').val('');
    $('#my-domain-host').val('');
    $('#my-domain-port').val('');
  });

  // makePeerDomainDot renders a tri-state dot for the known-domains panel:
  //   green  — peer online + peer's own health probe reports ok
  //   red    — peer online + peer reports its service is down
  //   gray   — peer offline (we can't even verify the service); also used
  //            when the peer just came online but health hasn't been
  //            checked yet (health is empty)
  function makePeerDomainDot(entry) {
    let cls, title;
    if (!entry.online) {
      cls = 'unknown';
      title = 'peer is offline — can\'t verify service';
    } else if (entry.health === 'ok') {
      cls = 'reachable';
      title = 'service is up';
    } else if (entry.health === 'fail') {
      cls = 'unreachable';
      title = 'peer reachable but service is down';
    } else {
      cls = 'unknown';
      title = 'health not checked yet';
    }
    return $('<span class="ax-dot">').addClass(cls).attr('title', title).css({
      'display': 'inline-block', 'width': '8px', 'height': '8px', 'border-radius': '50%',
      'margin-right': '8px',
    });
  }

  function renderKnownDomains() {
    const $list = $('#known-domains-list');
    $list.empty();
    // Collect from livePeers — includes peers persisted in the
    // catalog with their last-known domains, even when currently
    // offline. Backend doesn't reset the list on poll failure.
    const rows = [];
    livePeers.forEach((p) => {
      if (p.self) return;
      const ds = Array.isArray(p.domains) ? p.domains : [];
      ds.forEach((d) => rows.push({ peer: p.name, peerTunnel: p.tunnel_ip, online: p.online !== false, ...d }));
    });
    $('#known-domains-meta').text(rows.length);
    if (rows.length === 0) {
      $list.append('<div class="ax-list-empty">no domains published by any peer yet.</div>');
      return;
    }
    const campID = campIDOrPlaceholder();
    rows.forEach((r) => {
      const fqdn = r.name + '.' + campID + '.f2f';
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      $head.append(makePeerDomainDot(r));
      const $link = $('<a class="ax-intercept-spec ax-domain-link" target="_blank">')
        .attr('href', 'https://' + fqdn + '/')
        .text(fqdn);
      if (!r.online) $link.css('opacity', '0.5');
      $head.append($link);
      $head.append($('<span class="ax-pill ax-pill-peer">').text('via ' + r.peer));
      if (r.port) $head.append($('<span class="ax-pill ax-pill-peer">').text(':' + r.port));
      if (!r.online) $head.append($('<span class="ax-pill ax-pill-pending">').text('offline'));
      $head.append($('<span class="ax-intercept-meta">').text(r.peerTunnel));
      // Remove from local catalog. Two-click confirm. If the peer is
      // online and still publishes the name, the next poll re-adds it
      // — this is intentional (keeps live state in sync). For stale
      // entries from peers that are offline / no longer publish, this
      // is how you clean up.
      const $rm = $('<button class="ax-list-remove">');
      armRemove($rm, () => {
        $.ajax({
          url: '/api/peer-domains/' + encodeURIComponent(r.peer) + '/' + encodeURIComponent(r.name),
          method: 'DELETE',
        })
          .done(() => refreshStatus())
          .fail((xhr) => alert('Remove failed: ' + errorOf(xhr)));
      });
      $head.append($rm);
      $row.append($head);
      $list.append($row);
    });
  }

  // renderPeerFirewall walks livePeers, lists each peer's user-published
  // open ports under the tunnel tab. Dot semantics same as tri-state on
  // known-domains: green = peer online, red unused (no per-port health
  // signal yet — we just know the rule is in effect on their side),
  // gray = peer offline.
  function renderPeerFirewall() {
    const $list = $('#peer-firewall-list');
    $list.empty();
    const rows = [];
    livePeers.forEach((p) => {
      if (p.self) return;
      const ports = Array.isArray(p.firewall) ? p.firewall : [];
      ports.forEach((fp) => rows.push({
        peer: p.name, peerTunnel: p.tunnel_ip, online: p.online !== false, ...fp,
      }));
    });
    $('#peer-firewall-meta').text(rows.length);
    if (rows.length === 0) {
      $list.append('<div class="ax-list-empty">no peer-published ports yet.</div>');
      return;
    }
    // Stable order: peer name, then port.
    rows.sort((a, b) => (a.peer || '').localeCompare(b.peer || '') || a.port - b.port);
    rows.forEach((r) => {
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      const dotCls = !r.online ? 'unknown' : (r.enabled ? 'reachable' : 'unreachable');
      const dotTitle = !r.online
        ? 'peer is offline'
        : (r.enabled ? 'port is open' : 'rule kept but disabled by peer');
      $head.append($('<span class="ax-dot">').addClass(dotCls).attr('title', dotTitle).css({
        'display': 'inline-block', 'width': '8px', 'height': '8px', 'border-radius': '50%', 'margin-right': '8px',
      }));
      $head.append($('<span class="ax-intercept-spec">').text(r.port + '/' + r.protocol));
      $head.append($('<span class="ax-pill ax-pill-peer">').text('via ' + r.peer));
      if (!r.online) $head.append($('<span class="ax-pill ax-pill-pending">').text('offline'));
      if (r.description) {
        $head.append($('<span class="ax-intercept-meta">').text(r.description));
      } else {
        $head.append($('<span class="ax-intercept-meta">').text(r.peerTunnel));
      }
      $row.append($head);
      $list.append($row);
    });
  }

  // ---- firewall (tunnel tab: open ports) ----
  // Built-in entries are read-only — f2f's own ports must stay open
  // or the engine breaks. User entries support toggle on/off (without
  // losing the row) and delete.
  let firewallUser = [];
  let firewallActive = false;
  function refreshFirewall() {
    $.getJSON('/api/firewall', (data) => {
      const builtin = (data && Array.isArray(data.builtin)) ? data.builtin : [];
      const user = (data && Array.isArray(data.user)) ? data.user : [];
      firewallActive = !!(data && data.active);
      firewallUser = user;
      renderFirewall(builtin, user);
    }).fail(() => {
      $('#firewall-meta').text('?');
    });
  }
  function renderFirewall(builtin, user) {
    const $b = $('#firewall-builtin-list');
    const $u = $('#firewall-user-list');
    $b.empty(); $u.empty();
    const enabledUser = user.filter((p) => p.enabled).length;
    const totalOpen = builtin.length + enabledUser;
    $('#firewall-meta').text(firewallActive ? (totalOpen + ' open') : 'inactive');
    builtin.forEach((p) => $b.append(renderFirewallRow(p, true)));
    if (user.length === 0) {
      $u.append('<div class="ax-list-empty">no user-defined ports · default-deny on everything else.</div>');
    } else {
      user.forEach((p, idx) => $u.append(renderFirewallRow(p, false, idx)));
    }
  }
  // makeFirewallDot renders the same kind of indicator used for
  // domain health. Color reflects what's *actually* enforced in pf:
  // - green: engine running, firewall loaded, rule enabled.
  // - red: engine running, firewall failed to load (pf error).
  // - grey: rule disabled by user, OR engine stopped.
  function makeFirewallDot(enabled) {
    let cls, title;
    if (!enabled) { cls = 'offline'; title = 'rule disabled'; }
    else if (!firewallActive) { cls = 'unreachable'; title = 'firewall not active (engine stopped or pf failed)'; }
    else { cls = 'reachable'; title = 'rule active in pf'; }
    return $('<span class="ax-dot">').addClass(cls).attr('title', title).css({
      'display': 'inline-block', 'width': '8px', 'height': '8px', 'border-radius': '50%',
      'margin-right': '8px',
    });
  }
  function renderFirewallRow(p, builtin, idx) {
    const $row = $('<div class="ax-intercept">');
    const $head = $('<div class="ax-intercept-head" style="cursor:default">');
    $head.append($('<span class="ax-intercept-caret">').text(' '));
    $head.append(makeFirewallDot(!!p.enabled));
    // Checkbox: built-in always checked + disabled; user entries toggleable.
    const $cb = $('<input type="checkbox" style="margin-right:6px">')
      .prop('checked', !!p.enabled)
      .prop('disabled', builtin);
    if (!builtin) {
      $cb.on('change', () => {
        firewallUser[idx].enabled = $cb.is(':checked');
        saveFirewall();
      });
    }
    $head.append($cb);
    $head.append($('<span class="ax-intercept-spec">').text(p.port + '/' + p.protocol));
    if (p.description) {
      $head.append($('<span class="ax-pill ax-pill-peer">').text(p.description));
    }
    if (builtin) {
      $head.append($('<span class="ax-pill ax-pill-active">').text('built-in'));
    }
    $head.append($('<span class="ax-intercept-meta">'));
    if (!builtin) {
      const $rm = $('<button class="ax-list-remove">remove</button>');
      $rm.on('click', (e) => {
        e.stopPropagation();
        firewallUser.splice(idx, 1);
        saveFirewall();
      });
      $head.append($rm);
    }
    $row.append($head);
    return $row;
  }
  function saveFirewall() {
    $.ajax({
      url: '/api/firewall',
      method: 'PUT',
      contentType: 'application/json',
      data: JSON.stringify({ user: firewallUser }),
    })
      .done(() => refreshFirewall())
      .fail((xhr) => alert('Firewall save failed: ' + errorOf(xhr)));
  }
  $('#btn-add-firewall').on('click', () => {
    const port = parseInt($('#firewall-port-input').val(), 10);
    const protocol = $('#firewall-proto-input').val();
    const description = ($('#firewall-desc-input').val() || '').trim();
    if (!(port > 0 && port < 65536)) {
      alert('Port must be 1-65535.');
      return;
    }
    if (protocol !== 'tcp' && protocol !== 'udp') return;
    // Reject duplicates (same port+proto).
    if (firewallUser.some((p) => p.port === port && p.protocol === protocol)) {
      alert(port + '/' + protocol + ' is already in the list.');
      return;
    }
    firewallUser.push({ port, protocol, description, enabled: true });
    saveFirewall();
    $('#firewall-port-input').val('');
    $('#firewall-desc-input').val('');
  });

  // ---- trusted peer CAs (DNS tab, bottom section) ----
  // One row per installed peer CA: peer name + fingerprint + age +
  // two-click remove. Backend lists everything we've ever auto-installed
  // via peerCAPollLoop; remove drops the PEM, keychain entry, and the
  // record in <camp_id>.config.json.
  function refreshTrustedPeers() {
    $.getJSON('/api/trusted-peers', (list) => {
      const rows = Array.isArray(list) ? list : [];
      rows.sort((a, b) => (a.peer_name || '').localeCompare(b.peer_name || ''));
      $('#trusted-peers-meta').text(rows.length);
      const $list = $('#trusted-peers-list');
      $list.empty();
      if (rows.length === 0) {
        $list.append('<div class="ax-list-empty">no peer CAs installed yet · they appear automatically as peers join and you confirm with your macOS password.</div>');
        return;
      }
      rows.forEach((r) => {
        const $row = $('<div class="ax-intercept">');
        const $head = $('<div class="ax-intercept-head" style="cursor:default">');
        $head.append($('<span class="ax-intercept-caret">').text(' '));
        $head.append($('<span class="ax-intercept-spec">').text(r.peer_name || '?'));
        $head.append($('<span class="ax-pill ax-pill-peer">').text(r.fingerprint || ''));
        const when = r.installed_at ? humanAgo(r.installed_at * 1000) : '—';
        $head.append($('<span class="ax-intercept-meta">').text('installed ' + when));
        const $rm = $('<button class="ax-list-remove">');
        armRemove($rm, () => {
          $.ajax({
            url: '/api/trusted-peers/' + encodeURIComponent(r.fingerprint),
            method: 'DELETE',
          })
            .done(refreshTrustedPeers)
            .fail((xhr) => alert('Remove failed: ' + errorOf(xhr)));
        });
        $head.append($rm);
        $row.append($head);
        $list.append($row);
      });
    }).fail(() => {
      $('#trusted-peers-meta').text('?');
    });
  }

  // ---- drop tab: shared files + camp library + active downloads ----
  function refreshMyFiles() {
    $.ajax({ url: '/api/files/mine', dataType: 'json' })
      .done((list) => renderMyFiles(list))
      .fail((xhr) => {
        if (xhr.status === 503) renderMyFiles([], 'torrent client not running');
        else renderMyFiles([]);
      });
  }
  // revealInFinder asks the backend to run `open -R <path>` so macOS
  // pops the file open and selects it in Finder. Use the same helper
  // for every clickable file name across my-files, library (no-op
  // when not downloaded yet), and downloads.
  function revealInFinder(path) {
    if (!path) return;
    $.ajax({
      url: '/api/files/reveal',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ path }),
    }).fail((xhr) => alert('Open in Finder failed: ' + errorOf(xhr)));
  }
  // makeFileLink builds an anchor that triggers revealInFinder on
  // click. If path is empty (e.g. peer-side library entry we haven't
  // downloaded yet) renders plain text — no link.
  function makeFileLink(name, path) {
    if (!path) {
      return $('<span class="ax-intercept-spec">').text(name);
    }
    return $('<a class="ax-intercept-spec ax-domain-link" href="#">').text(name)
      .on('click', (e) => { e.preventDefault(); revealInFinder(path); });
  }

  function renderMyFiles(list, errMsg) {
      const arr = Array.isArray(list) ? list : [];
      $('#drop-my-meta').text(arr.length);
      const $list = $('#drop-my-list');
      $list.empty();
      if (errMsg) {
        $list.append($('<div class="ax-list-empty">').text(errMsg));
        return;
      }
      if (arr.length === 0) {
        $list.append('<div class="ax-list-empty">nothing shared yet.</div>');
        return;
      }
      arr.forEach((f) => {
        const $row = $('<div class="ax-intercept">');
        const $head = $('<div class="ax-intercept-head" style="cursor:default">');
        $head.append($('<span class="ax-intercept-caret">').text(' '));
        $head.append(makeFileLink(f.name, f.path));
        $head.append($('<span class="ax-pill ax-pill-peer">').text(fmtBytes(f.size)));
        $head.append($('<span class="ax-pill ax-pill-peer">').text(f.info_hash.slice(0, 12)));
        const $rm = $('<button class="ax-list-remove">remove</button>');
        $rm.on('click', () => {
          $.ajax({ url: '/api/files/mine/' + encodeURIComponent(f.info_hash), method: 'DELETE' })
            .done(refreshMyFiles)
            .fail((xhr) => alert('Remove failed: ' + errorOf(xhr)));
        });
        $head.append($('<span class="ax-intercept-meta">'));
        $head.append($rm);
        $row.append($head);
        $list.append($row);
      });
  }

  // localDownloads is the latest /api/files/downloads payload, indexed
  // by info_hash. Library uses it to know whether a peer's file is
  // already on our disk (so we render an open-link instead of a
  // download button).
  let localDownloads = {};

  function refreshLibrary() {
    const others = livePeers.filter((p) => !p.self);
    const rows = [];
    others.forEach((p) => {
      const files = Array.isArray(p.files) ? p.files : [];
      files.forEach((f) => rows.push({ peer: p.name, peerTunnel: p.tunnel_ip, ...f }));
    });
    $('#drop-lib-meta').text(rows.length);
    const $list = $('#drop-lib-list');
    $list.empty();
    if (rows.length === 0) {
      $list.append('<div class="ax-list-empty">no files shared by any peer yet.</div>');
      return;
    }
    rows.forEach((r) => {
      const local = localDownloads[r.info_hash];
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      // Already downloaded — name clickable to Finder, no download
      // button. Otherwise — plain name + download button as before.
      if (local && local.complete && local.path) {
        $head.append(makeFileLink(r.name, local.path));
      } else {
        $head.append($('<span class="ax-intercept-spec">').text(r.name));
      }
      $head.append($('<span class="ax-pill ax-pill-peer">').text('from ' + r.peer));
      $head.append($('<span class="ax-pill ax-pill-peer">').text(fmtBytes(r.size)));
      $head.append($('<span class="ax-intercept-meta">'));
      if (local && local.complete) {
        const label = local.seeding ? 'seeding' : 'downloaded';
        $head.append($('<span class="ax-pill ax-pill-active" style="background:#86b86b;color:#000">').text(label));
      } else if (local && !local.complete) {
        // In progress — show percent inline, no extra download button.
        const pct = local.size ? Math.floor(((local.bytes_completed || 0) / local.size) * 100) : 0;
        $head.append($('<span class="ax-pill ax-pill-active">').text(pct + '%'));
      } else {
        const $dl = $('<button class="ax-list-remove" style="color:#86b86b">download</button>');
        $dl.on('click', () => {
          // peer addr: <peer_tunnel_ip>:6881 — BT client listens there.
          const peerAddr = r.peerTunnel + ':6881';
          $.ajax({
            url: '/api/files/download',
            method: 'POST',
            contentType: 'application/json',
            data: JSON.stringify({ magnet: r.magnet, peers: [peerAddr] }),
          })
            .done(refreshDownloads)
            .fail((xhr) => alert('Download failed: ' + errorOf(xhr)));
        });
        $head.append($dl);
      }
      $row.append($head);
      $list.append($row);
    });
  }

  function refreshDownloads() {
    $.ajax({ url: '/api/files/downloads', dataType: 'json' })
      .done((list) => {
        // Update lookup table for library — every entry is what we
        // have locally (in-progress or completed).
        localDownloads = {};
        (Array.isArray(list) ? list : []).forEach((d) => { localDownloads[d.info_hash] = d; });
        renderDownloads(list);
        // Refresh library too — its status pills depend on this.
        refreshLibrary();
      })
      .fail((xhr) => {
        if (xhr.status === 503) renderDownloads([], 'torrent client not running');
        else renderDownloads([]);
      });
  }
  function renderDownloads(list, errMsg) {
    // Active downloads = in-progress only; completed entries appear
    // back in the library section with a "downloaded"/"seeding" pill.
    const all = Array.isArray(list) ? list : [];
    const arr = all.filter((d) => !d.complete);
    $('#drop-dl-meta').text(arr.length);
    const $list = $('#drop-dl-list');
    $list.empty();
    if (errMsg) {
      $list.append($('<div class="ax-list-empty">').text(errMsg));
      return;
    }
    if (arr.length === 0) {
      $list.append('<div class="ax-list-empty">no active downloads.</div>');
      return;
    }
    arr.forEach((d) => {
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      const displayName = d.name || d.info_hash.slice(0, 12);
      $head.append($('<span class="ax-intercept-spec">').text(displayName));
      if (d.fetching_metadata) {
        // Magnet added, anacrolix hasn't fetched the .torrent yet —
        // source peer offline or never connected. Show this so the user
        // knows the row isn't just "0% but downloading", it's stuck
        // waiting on the source.
        $head.append($('<span class="ax-pill ax-pill-pending">').text('fetching metadata…'));
      } else if (d.size) {
        const total = d.size;
        const done = d.bytes_completed || 0;
        const pct = total > 0 ? Math.floor((done / total) * 100) : 0;
        $head.append($('<span class="ax-pill ax-pill-active">').text(pct + '%'));
        $head.append($('<span class="ax-pill ax-pill-peer">').text(fmtBytes(done) + ' / ' + fmtBytes(total)));
      }
      if (Array.isArray(d.peers) && d.peers.length) {
        $head.append($('<span class="ax-pill ax-pill-peer">').text('from ' + d.peers.join(', ')));
      }
      $head.append($('<span class="ax-intercept-meta">'));
      const $rm = $('<button class="ax-list-remove">');
      armRemove($rm, () => {
        $.ajax({
          url: '/api/files/downloads/' + encodeURIComponent(d.info_hash),
          method: 'DELETE',
        })
          .done(refreshDownloads)
          .fail((xhr) => alert('Remove failed: ' + errorOf(xhr)));
      });
      $head.append($rm);
      $row.append($head);
      $list.append($row);
    });
  }

  // Drop-zone wiring.
  (function () {
    const $zone = $('#drop-dropzone');
    const $inp = $('#drop-fileinput');
    function upload(file) {
      const fd = new FormData();
      fd.append('file', file);
      $.ajax({
        url: '/api/files/mine/upload',
        method: 'POST',
        data: fd,
        processData: false,
        contentType: false,
      })
        .done(refreshMyFiles)
        .fail((xhr) => alert('Upload failed: ' + errorOf(xhr)));
    }
    $zone.on('click', () => $inp.click());
    $inp.on('change', (e) => {
      const f = e.target.files && e.target.files[0];
      if (f) upload(f);
      $inp.val('');
    });
    $zone.on('dragover', (e) => { e.preventDefault(); $zone.addClass('is-drag'); });
    $zone.on('dragleave', () => $zone.removeClass('is-drag'));
    $zone.on('drop', (e) => {
      e.preventDefault();
      $zone.removeClass('is-drag');
      const f = e.originalEvent.dataTransfer.files && e.originalEvent.dataTransfer.files[0];
      if (f) upload(f);
    });
  })();

  restoreForm();
  refreshCamps();
  refreshStatus();
  refreshCampPeers();
  refreshMyDomains();
  refreshTrustedPeers();
  refreshMyFiles();
  refreshDownloads();
  refreshFirewall();
  setInterval(refreshStatus, 3000);
  setInterval(refreshCampPeers, 3000);
  setInterval(refreshMyDomains, 5000);
  setInterval(refreshTrustedPeers, 5000);
  setInterval(refreshMyFiles, 5000);
  setInterval(refreshDownloads, 2000);
  setInterval(refreshLibrary, 5000);
  setInterval(refreshFirewall, 5000);
  // Known-domains panel reads from livePeers, which is updated in
  // applyStatus. Trigger a render on each status refresh.
  setInterval(renderKnownDomains, 3000);
  setInterval(renderPeerFirewall, 3000);
  startLogStream();
});
