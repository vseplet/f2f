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

  // The intercept list is owned by the frontend and persisted in
  // localStorage. The engine has its own copy while running (the "live"
  // entries with IDs and resolved prefixes) — we reconcile the two on
  // every status refresh.
  // Stored as a JSON array of {spec, peer} objects. Legacy entries
  // (strings, or {spec} without peer) are dropped on load — peer is
  // mandatory now.
  const STORE_KEY = 'f2f:intercepts';
  function getStoredSpecs() {
    try {
      const raw = localStorage.getItem(STORE_KEY);
      if (!raw) return [];
      const arr = JSON.parse(raw);
      if (!Array.isArray(arr)) return [];
      return arr.filter((e) => e && typeof e === 'object'
        && typeof e.spec === 'string' && e.spec
        && typeof e.peer === 'string' && e.peer);
    } catch (_) { return []; }
  }
  function setStoredSpecs(arr) {
    localStorage.setItem(STORE_KEY, JSON.stringify(arr));
  }
  function addStoredSpec(spec, peer) {
    const list = getStoredSpecs();
    if (!list.some((e) => e.spec === spec && e.peer === peer)) {
      list.push({ spec, peer });
      setStoredSpecs(list);
    }
  }
  function removeStoredSpec(spec, peer) {
    setStoredSpecs(getStoredSpecs().filter((e) => !(e.spec === spec && e.peer === peer)));
  }

  let liveIntercepts = []; // last seen from /api/status
  let livePeers = [];      // last seen camp peers from /api/status
  const expandedIntercepts = new Set(); // keys (spec|peer) currently expanded

  // Persist config form values across reloads. Each field has a localStorage
  // key; we restore on load and save on every change. Engine-driven updates
  // (when running) also write to localStorage so the form starts from the
  // last actual state next time.
  const FIELDS = ['#camp-name', '#camp-id'];
  const storageKey = (sel) => 'f2f:' + sel.slice(1);
  function restoreForm() {
    // One-shot migration: the old key was f2f:camp-room before the
    // rename. If the user has a value there and nothing yet under the
    // new key, carry it over once. Safe to leave indefinitely.
    const legacyRoom = localStorage.getItem('f2f:camp-room');
    if (legacyRoom && !localStorage.getItem('f2f:camp-id')) {
      localStorage.setItem('f2f:camp-id', legacyRoom);
    }
    if (legacyRoom !== null) localStorage.removeItem('f2f:camp-room');

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
  let autoStarted = false;
  function maybeAutoStart(s) {
    if (autoStarted) return;
    if (s.running) {
      autoStarted = true;
      return;
    }
    const name = $('#camp-name').val().trim();
    const id = $('#camp-id').val().trim();
    if (!name || !id) return;
    autoStarted = true;
    triggerStart();
  }

  function applyStatus(s) {
    maybeAutoStart(s);
    if (pendingOp) {
      // Hold the loading state while an op is in flight; inputs stay
      // locked too so the user doesn't edit them mid-transition.
      $('#camp-name, #camp-id').prop('disabled', true);
    } else if (s.running) {
      setEngineState('running', 'running', '· ' + (s.utun_name || '?'));
      $('#camp-name, #camp-id').prop('disabled', true);
    } else {
      setEngineState('stopped', 'start', '');
      $('#camp-name, #camp-id').prop('disabled', false);
    }
    // Intercept management is always available — list lives in the browser.
    $interceptInput.prop('disabled', false);
    $btnAdd.prop('disabled', false);

    liveIntercepts = s.intercepts || [];
    livePeers = s.peers || [];
    refreshInterceptPeerSelect();
    refreshCallPeerSelect(s.active_peer_tunnel_ip || '');
    reconcileStoredIntercepts();
    renderIntercepts();

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));
  }

  function renderIntercepts() {
    $list.empty();
    const stored = getStoredSpecs();
    const liveKey = (spec, peer) => spec + '\x00' + peer;
    const liveByKey = {};
    liveIntercepts.forEach((l) => { liveByKey[liveKey(l.spec, l.peer)] = l; });

    const seenKeys = new Set();
    const items = stored.map((e) => {
      const k = liveKey(e.spec, e.peer);
      seenKeys.add(k);
      return { spec: e.spec, peer: e.peer, live: liveByKey[k] || null };
    });
    liveIntercepts.forEach((l) => {
      const k = liveKey(l.spec, l.peer);
      if (!seenKeys.has(k)) items.push({ spec: l.spec, peer: l.peer, live: l, orphan: true });
    });

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
      if (it.orphan) $head.append($('<span class="ax-pill ax-pill-pending">').text('unsaved'));

      const $meta = $('<span class="ax-intercept-meta">');
      if (parsed.length) {
        const bits = [];
        if (v4count) bits.push(`<span class="ax-meta-routes">${v4count} route${v4count === 1 ? '' : 's'}</span>`);
        if (v6count) bits.push(`<span class="ax-meta-reject">${v6count} reject</span>`);
        $meta.html(bits.join(' · '));
      }
      $head.append($meta);

      const $rm = $('<button class="ax-list-remove">remove</button>');
      $rm.on('click', (e) => { e.stopPropagation(); removeSpec(it.spec, it.peer, it.live); });
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

  function removeSpec(spec, peer, live) {
    removeStoredSpec(spec, peer);
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

  // reconcileStoredIntercepts retries POSTing any stored intercept that's
  // not currently live. Runs on every status refresh while the engine is
  // up. Silent failures are fine — they'll be retried next tick.
  function reconcileStoredIntercepts() {
    if (!engineRunning) return;
    const liveKey = (spec, peer) => spec + '\x00' + peer;
    const liveSet = new Set(liveIntercepts.map((l) => liveKey(l.spec, l.peer)));
    getStoredSpecs().forEach((e) => {
      if (liveSet.has(liveKey(e.spec, e.peer))) return;
      addOne(e.spec, e.peer).fail(() => { /* retry next refresh */ });
    });
  }


  function triggerStart() {
    const cfg = {
      camp_name: $('#camp-name').val().trim(),
      camp_id: $('#camp-id').val().trim(),
    };
    pendingOp = 'starting';
    setEngineState('loading', 'starting…', '');
    $.ajax({
      url: '/api/start',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify(cfg)
    })
      .always(() => { pendingOp = null; })
      .done(refreshStatus)
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

    // Save locally first — survives engine restarts; reconciliation
    // pushes anything stored-but-not-live into engine on next status tick.
    specs.forEach((spec) => addStoredSpec(spec, peer));
    $interceptInput.val('');
    renderIntercepts();

    if (!engineRunning) return;

    const errors = [];
    const requests = specs.map((spec) =>
      addOne(spec, peer).fail((xhr) => errors.push(`${spec}: ${errorOf(xhr)}`))
    );
    $.when(...requests).always(() => {
      refreshStatus();
      if (errors.length) alert('Some intercepts failed to apply live:\n' + errors.join('\n'));
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
      let dotClass;
      if (p.self) dotClass = 'self';
      else if (p.online === false) dotClass = 'offline';
      else if (p.reachable) dotClass = 'reachable';
      else dotClass = 'unreachable';
      const $row = $('<tr>')
        .addClass(p.self ? 'is-self' : '')
        .addClass(p.online === false ? 'is-offline' : '')
        .attr('data-tunnel-ip', p.tunnel_ip || '');
      $row.append(
        $('<td>').append($('<span>').addClass('ax-dot ' + dotClass)),
        $('<td>').text(p.name + (p.self ? ' (you)' : '')),
        $('<td>').text(p.tunnel_ip || '—'),
        $('<td>').text(endpoint || '—'),
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
  // localStorage is the source of truth — engine holds an in-memory copy
  // that gets blown away on restart, so on every refresh we re-push if
  // the runtime list lost entries we still have stored.
  const MY_DOMAINS_KEY = 'f2f:my-domains';
  function getStoredMyDomains() {
    try {
      const raw = localStorage.getItem(MY_DOMAINS_KEY);
      if (!raw) return [];
      const arr = JSON.parse(raw);
      return Array.isArray(arr) ? arr.filter((e) => e && typeof e === 'object' && typeof e.name === 'string') : [];
    } catch (_) { return []; }
  }
  function setStoredMyDomains(list) {
    localStorage.setItem(MY_DOMAINS_KEY, JSON.stringify(list));
  }
  let myDomains = getStoredMyDomains();
  function refreshMyDomains() {
    $.getJSON('/api/my-domains', (list) => {
      const fromEngine = Array.isArray(list) ? list : [];
      const stored = getStoredMyDomains();
      // Reconcile: if stored has entries that engine doesn't, re-push.
      // This survives engine restart while UI keeps running.
      const sameLen = fromEngine.length === stored.length;
      const sameAll = sameLen && stored.every((s) =>
        fromEngine.some((e) => e.name === s.name && (e.port || 0) === (s.port || 0))
      );
      if (!sameAll && stored.length > 0) {
        putMyDomains(stored, { silent: true });
        return; // putMyDomains calls refreshMyDomains on done — render via that
      }
      myDomains = fromEngine;
      setStoredMyDomains(myDomains);
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
      const campID = $('#camp-id').val() || '<camp_id>';
      const fqdn = d.name + '.' + campID + '.f2f';
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      $head.append(makeHealthDot(d));
      const $link = $('<a class="ax-intercept-spec ax-domain-link" target="_blank">')
        .attr('href', 'https://' + fqdn + '/')
        .text(fqdn);
      $head.append($link);
      if (d.port) $head.append($('<span class="ax-pill ax-pill-peer">').text(':' + d.port));
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
  function putMyDomains(list, opts) {
    opts = opts || {};
    setStoredMyDomains(list); // persist regardless of engine state
    $.ajax({
      url: '/api/my-domains',
      method: 'PUT',
      contentType: 'application/json',
      data: JSON.stringify(list),
    })
      .done(refreshMyDomains)
      .fail((xhr) => { if (!opts.silent) alert('Save failed: ' + errorOf(xhr)); });
  }
  $('#btn-add-my-domain').on('click', () => {
    const name = ($('#my-domain-name').val() || '').trim().toLowerCase();
    if (!name) return;
    if (!/^[a-z0-9-]+$/.test(name)) {
      alert('Name may contain only lowercase letters, digits, and "-".');
      return;
    }
    const port = parseInt($('#my-domain-port').val(), 10);
    const entry = { name };
    if (port > 0 && port < 65536) entry.port = port;
    const next = myDomains.filter((e) => e.name !== name).concat(entry);
    putMyDomains(next);
    $('#my-domain-name').val('');
    $('#my-domain-port').val('');
  });

  function renderKnownDomains() {
    const $list = $('#known-domains-list');
    $list.empty();
    // Collect from livePeers (all online & offline peers we know).
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
    const campID = $('#camp-id').val() || '<camp_id>';
    rows.forEach((r) => {
      const fqdn = r.name + '.' + campID + '.f2f';
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      $head.append(makeHealthDot(r));
      const $link = $('<a class="ax-intercept-spec ax-domain-link" target="_blank">')
        .attr('href', 'https://' + fqdn + '/')
        .text(fqdn);
      if (!r.online) $link.css('opacity', '0.5');
      $head.append($link);
      $head.append($('<span class="ax-pill ax-pill-peer">').text('via ' + r.peer));
      if (r.port) $head.append($('<span class="ax-pill ax-pill-peer">').text(':' + r.port));
      if (!r.online) $head.append($('<span class="ax-pill ax-pill-pending">').text('offline'));
      $head.append($('<span class="ax-intercept-meta">').text(r.peerTunnel));
      $row.append($head);
      $list.append($row);
    });
  }

  // ---- trusted peer CAs (DNS tab, bottom section) ----
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
        $head.append($('<span class="ax-intercept-spec">').text(f.name));
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
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      $head.append($('<span class="ax-intercept-spec">').text(r.name));
      $head.append($('<span class="ax-pill ax-pill-peer">').text('from ' + r.peer));
      $head.append($('<span class="ax-pill ax-pill-peer">').text(fmtBytes(r.size)));
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
      $head.append($('<span class="ax-intercept-meta">'));
      $head.append($dl);
      $row.append($head);
      $list.append($row);
    });
  }

  function refreshDownloads() {
    $.ajax({ url: '/api/files/downloads', dataType: 'json' })
      .done((list) => renderDownloads(list))
      .fail((xhr) => {
        if (xhr.status === 503) renderDownloads([], 'torrent client not running');
        else renderDownloads([]);
      });
  }
  function renderDownloads(list, errMsg) {
    const arr = Array.isArray(list) ? list : [];
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
      $head.append($('<span class="ax-intercept-spec">').text(d.name || d.info_hash.slice(0, 12)));
      if (d.size) {
        const total = d.size;
        const done = d.bytes_completed || 0;
        const pct = Math.floor((done / total) * 100);
        $head.append($('<span class="ax-pill ax-pill-active">').text(pct + '%'));
        $head.append($('<span class="ax-pill ax-pill-peer">').text(fmtBytes(done) + ' / ' + fmtBytes(total)));
      }
      $head.append($('<span class="ax-intercept-meta">'));
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
  refreshStatus();
  refreshCampPeers();
  refreshMyDomains();
  refreshTrustedPeers();
  refreshMyFiles();
  refreshDownloads();
  setInterval(refreshStatus, 3000);
  setInterval(refreshCampPeers, 3000);
  setInterval(refreshMyDomains, 5000);
  setInterval(refreshTrustedPeers, 5000);
  setInterval(refreshMyFiles, 5000);
  setInterval(refreshDownloads, 2000);
  setInterval(refreshLibrary, 5000);
  // Known-domains panel reads from livePeers, which is updated in
  // applyStatus. Trigger a render on each status refresh.
  setInterval(renderKnownDomains, 3000);
  startLogStream();
});
