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
      const $row = $('<div class="ax-list-item">');
      $row.append('<span class="ax-list-icon">›</span>');
      const $main = $('<div class="ax-list-main">');
      const $spec = $('<div class="ax-list-spec">').text(it.spec);
      $spec.append('<span class="ax-pill ax-pill-peer">via ' + escapeHtml(it.peer) + '</span>');
      if (it.live) $spec.append('<span class="ax-pill ax-pill-active">active</span>');
      else         $spec.append('<span class="ax-pill ax-pill-pending">pending</span>');
      if (it.orphan) $spec.append('<span class="ax-pill ax-pill-pending">unsaved</span>');
      $main.append($spec);
      const prefixes = it.live ? (it.live.prefixes || []) : [];
      if (prefixes.length) {
        $main.append($('<div class="ax-list-meta">').text(prefixes.join(', ')));
      }
      $row.append($main);
      const $btn = $('<button class="ax-list-remove">remove</button>');
      $btn.on('click', () => removeSpec(it.spec, it.peer, it.live));
      $row.append($btn);
      $list.append($row);
    });
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    })[c]);
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
      .forEach((p) => $sel.append($('<option>').val(p.name).text(p.name)));
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

  // -- Topology graph (d3 force-directed) --
  const svgEl = document.getElementById('topology');
  const svg = d3.select('#topology');
  const width = svgEl.clientWidth || 800;
  const height = 360;

  const g = svg.append('g');
  const zoomBehavior = d3.zoom().scaleExtent([0.3, 3])
    .filter((e) => e.type !== 'wheel' || e.ctrlKey)
    .on('zoom', (e) => g.attr('transform', e.transform));
  svg.call(zoomBehavior);
  // Start zoomed out a bit so 3-4 bubbles fit without scrolling.
  const initialScale = 0.7;
  svg.call(
    zoomBehavior.transform,
    d3.zoomIdentity
      .translate(width * (1 - initialScale) / 2, height * (1 - initialScale) / 2)
      .scale(initialScale),
  );
  const linksLayer = g.append('g').attr('class', 'links');
  const nodesLayer = g.append('g').attr('class', 'nodes');

  const sim = d3.forceSimulation()
    .force('link', d3.forceLink().id((d) => d.id).distance(130))
    .force('charge', d3.forceManyBody().strength(-450))
    .force('center', d3.forceCenter(width / 2, height / 2))
    .force('collide', d3.forceCollide().radius(44));

  // Keep a stable view of nodes so positions survive refresh.
  let nodeMap = new Map();
  let lastTopologyKey = '';

  function bubbleRadius(n) {
    if (n.kind === 'self') return 28;
    if (n.kind === 'peer') return 26;
    return 20;
  }
  function bubbleColor(n) {
    if (n.kind === 'self') return '#2563eb';
    if (n.kind === 'peer') return '#059669';
    return '#d97706';
  }
  function edgeThickness(e) {
    const bytes = (e.tx_bytes || 0) + (e.rx_bytes || 0);
    if (!bytes) return 1.5;
    return Math.min(7, 1 + Math.log10(bytes + 1) / 1.5);
  }
  function abbrev(s) {
    if (!s) return '';
    return s.length <= 36 ? s : s.slice(0, 34) + '…';
  }
  function fullLabel(n) {
    let t = n.label;
    if (n.ips && n.ips.length) t += '\n' + n.ips.join('\n');
    return t;
  }

  function refreshTopology() {
    $.getJSON('/api/topology', (data) => {
      const incoming = data.nodes || [];
      const incomingEdges = data.edges || [];

      // Detect whether the structure (not byte counts) actually changed.
      // If not, skip the d3 selection/simulation work — restarting alpha
      // every 2s on an unchanged graph keeps the physics loop running
      // forever and starves the main thread under heavy log volume.
      const structureKey = incoming.map((n) => n.id).sort().join(',') + '|' +
        incomingEdges.map((e) => e.source + '>' + e.target).sort().join(',');
      const structureChanged = structureKey !== lastTopologyKey;
      lastTopologyKey = structureKey;

      const newMap = new Map();
      incoming.forEach((n) => {
        const existing = nodeMap.get(n.id);
        if (existing) {
          // Update labels/ips but preserve position
          Object.assign(existing, n);
          newMap.set(n.id, existing);
        } else {
          newMap.set(n.id, Object.assign({}, n));
        }
      });
      nodeMap = newMap;

      const nodes = Array.from(nodeMap.values());
      const links = incomingEdges.map((e) => ({ ...e }));

      const linkSel = linksLayer.selectAll('line').data(links, (e) => e.source + '|' + e.target);
      linkSel.exit().remove();
      const linkEnter = linkSel.enter().append('line')
        .attr('stroke', '#94a3b8')
        .attr('stroke-opacity', 0.75);
      linkEnter.merge(linkSel).attr('stroke-width', edgeThickness);

      const nodeSel = nodesLayer.selectAll('g.bubble').data(nodes, (n) => n.id);
      nodeSel.exit().remove();
      const nodeEnter = nodeSel.enter().append('g').attr('class', 'bubble').style('cursor', 'grab');
      nodeEnter.append('circle')
        .attr('stroke', '#0f172a')
        .attr('stroke-width', 2);
      // Label sits BELOW the bubble — dark text on the panel background,
      // no truncation worries.
      nodeEnter.append('text')
        .attr('text-anchor', 'middle')
        .attr('font-size', '12px')
        .attr('fill', '#9a8e7a')
        .attr('font-weight', '500')
        .style('pointer-events', 'none');
      nodeEnter.append('title');
      nodeEnter.call(d3.drag()
        .on('start', (event, d) => {
          if (!event.active) sim.alphaTarget(0.3).restart();
          d.fx = d.x; d.fy = d.y;
        })
        .on('drag', (event, d) => { d.fx = event.x; d.fy = event.y; })
        .on('end', (event, d) => {
          if (!event.active) sim.alphaTarget(0);
          d.fx = null; d.fy = null;
        }));

      const allNodes = nodeEnter.merge(nodeSel);
      allNodes.select('circle').attr('r', bubbleRadius).attr('fill', bubbleColor);
      allNodes.select('text')
        .attr('y', (n) => bubbleRadius(n) + 14)
        .text((n) => abbrev(n.label));
      allNodes.select('title').text((n) => fullLabel(n));

      sim.nodes(nodes).on('tick', () => {
        linksLayer.selectAll('line')
          .attr('x1', (d) => d.source.x).attr('y1', (d) => d.source.y)
          .attr('x2', (d) => d.target.x).attr('y2', (d) => d.target.y);
        allNodes.attr('transform', (d) => `translate(${d.x},${d.y})`);
      });
      sim.force('link').links(links);
      if (structureChanged) sim.alpha(0.4).restart();
    });
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
    const active = data.active || '';
    for (const p of peers) {
      const endpoint = p.udp_endpoint || (p.public_ip ? p.public_ip + (p.udp_port ? ':' + p.udp_port : '') : '—');
      const dotClass = p.self ? 'self' : (p.reachable ? 'reachable' : 'unreachable');
      const isActive = !p.self && active && p.tunnel_ip === active;
      const $row = $('<tr>')
        .addClass(p.self ? 'is-self' : '')
        .addClass(isActive ? 'is-active' : '')
        .attr('data-tunnel-ip', p.tunnel_ip || '');
      $row.append(
        $('<td>').append($('<span>').addClass('ax-dot ' + dotClass)),
        $('<td>').text(p.name + (p.self ? ' (you)' : '')),
        $('<td>').text(p.tunnel_ip || '—'),
        $('<td>').text(endpoint || '—'),
        $('<td>').addClass('muted').text(p.joined_at ? humanAgo(p.joined_at) : '—'),
        $('<td>').addClass(isActive ? 'active-mark' : 'muted').text(isActive ? '✓' : ''),
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
      const label = p.name + (p.reachable ? '' : ' (unreachable)');
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

  restoreForm();
  refreshStatus();
  refreshTopology();
  refreshCampPeers();
  setInterval(refreshStatus, 3000);
  setInterval(refreshTopology, 2000);
  setInterval(refreshCampPeers, 3000);
  startLogStream();
});
