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
  // Two persistent lists in localStorage: intercept specs (what we send
  // INTO the tunnel) and inbound-allow specs (what we let peer's traffic
  // reach OUT of the tunnel on our side). Mirror structures, same helpers.
  function makeList(key) {
    return {
      get() {
        try {
          const raw = localStorage.getItem(key);
          if (!raw) return [];
          const arr = JSON.parse(raw);
          return Array.isArray(arr) ? arr.filter((s) => typeof s === 'string') : [];
        } catch (_) { return []; }
      },
      set(arr) { localStorage.setItem(key, JSON.stringify(arr)); },
      add(spec) {
        const list = this.get();
        if (!list.includes(spec)) list.push(spec);
        this.set(list);
      },
      remove(spec) {
        this.set(this.get().filter((s) => s !== spec));
      },
    };
  }
  const intercepts = makeList('f2f:intercepts');
  const allows = makeList('f2f:allow');

  // Back-compat wrappers (some existing code uses these names).
  const getStoredSpecs = () => intercepts.get();
  const addStoredSpec = (s) => intercepts.add(s);
  const removeStoredSpec = (s) => intercepts.remove(s);

  let liveIntercepts = []; // last seen from /api/status
  let liveAllows = [];

  // Persist config form values across reloads. Each field has a localStorage
  // key; we restore on load and save on every change. Engine-driven updates
  // (when running) also write to localStorage so the form starts from the
  // last actual state next time.
  const FIELDS = [
    '#local-ip', '#peer-ip', '#listen', '#peer-udp',
    '#egress-iface', '#egress-subnet',
    '#camp-url', '#camp-stun', '#camp-name', '#camp-room',
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
  let autoStarted = false;
  function maybeAutoStart(s) {
    if (autoStarted || s.running) return;
    const name = $('#camp-name').val().trim();
    const room = $('#camp-room').val().trim();
    if (!name || !room) return;
    autoStarted = true;
    triggerStart();
  }

  function applyStatus(s) {
    maybeAutoStart(s);
    if (s.running) {
      setEngineState('running', 'running', '· ' + (s.utun_name || '?'));
      $('#local-ip, #peer-ip, #listen, #peer-udp, #egress-iface, #egress-subnet, #camp-url, #camp-stun, #camp-name, #camp-room').prop('disabled', true);
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
      setEngineState('stopped', 'start', '');
      $('#local-ip, #peer-ip, #listen, #peer-udp, #egress-iface, #egress-subnet, #camp-url, #camp-stun, #camp-name, #camp-room').prop('disabled', false);
    }
    // Camp status row — shown only when camp is actually active.
    const $campStatus = $('#camp-status');
    if (s.camp_active) {
      const lines = [
        `connected as ${s.camp_name}@${s.camp_room}`,
        s.camp_reflex ? `reflex ${s.camp_reflex}` : '',
        s.camp_peer_name ? `peer ${s.camp_peer_name} @ ${s.peer_addr || '?'}` : 'waiting for peer',
      ].filter(Boolean);
      $campStatus.text(lines.join(' · ')).addClass('live');
    } else {
      $campStatus.text('').removeClass('live');
    }
    // Identity meta in section title shows compact running indicator.
    $('#identity-meta').text(s.running ? (s.utun_name || '') : '');
    // Intercept management is always available — list lives in the browser.
    $interceptInput.prop('disabled', false);
    $btnAdd.prop('disabled', false);

    liveIntercepts = s.intercepts || [];
    liveAllows = s.inbound_allow || [];
    renderIntercepts();
    renderAllows();

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));
    $('#dropped-inbound').text(s.dropped_inbound || 0);
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
    liveIntercepts.forEach((l) => {
      if (!seen.has(l.spec)) items.push({ spec: l.spec, live: l, orphan: true });
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

  // -- Inbound whitelist (mirror of Intercepts) --
  const $allowList = $('#allow-list');
  const $allowInput = $('#allow-input');
  const $btnAddAllow = $('#btn-add-allow');

  function renderAllows() {
    $allowList.empty();
    const stored = allows.get();
    const liveBySpec = {};
    liveAllows.forEach((l) => { liveBySpec[l.spec] = l; });

    const seen = new Set();
    const items = stored.map((spec) => {
      seen.add(spec);
      return { spec, live: liveBySpec[spec] || null };
    });
    liveAllows.forEach((l) => {
      if (!seen.has(l.spec)) items.push({ spec: l.spec, live: l, orphan: true });
    });

    $('#allow-meta').text(items.length);

    if (items.length === 0) {
      $allowList.append('<div class="ax-list-empty">no filter — peer can reach any destination through us.</div>');
      return;
    }

    items.forEach((it) => {
      const $row = $('<div class="ax-list-item">');
      $row.append('<span class="ax-list-icon">›</span>');
      const $main = $('<div class="ax-list-main">');
      const $spec = $('<div class="ax-list-spec">').text(it.spec);
      if (it.live) $spec.append('<span class="ax-pill ax-pill-active">active</span>');
      else         $spec.append('<span class="ax-pill ax-pill-pending">pending</span>');
      $main.append($spec);
      const prefixes = it.live ? (it.live.prefixes || []) : [];
      if (prefixes.length) {
        $main.append($('<div class="ax-list-meta">').text(prefixes.join(', ')));
      }
      $row.append($main);
      const $btn = $('<button class="ax-list-remove">remove</button>');
      $btn.on('click', () => removeAllowSpec(it.spec, it.live));
      $row.append($btn);
      $allowList.append($row);
    });
  }

  function removeAllowSpec(spec, live) {
    allows.remove(spec);
    const after = () => refreshStatus();
    if (live && live.id) {
      $.ajax({ url: '/api/inbound-allow/' + encodeURIComponent(live.id), method: 'DELETE' })
        .done(after)
        .fail((xhr) => { alert('Remove failed: ' + errorOf(xhr)); after(); });
    } else {
      renderAllows();
    }
  }

  function addAllowOne(spec) {
    return $.ajax({
      url: '/api/inbound-allow',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ spec: spec }),
    });
  }

  $btnAddAllow.on('click', () => {
    const raw = $allowInput.val();
    const specs = raw.split(',').map((s) => s.trim()).filter(Boolean);
    if (specs.length === 0) return;

    specs.forEach((s) => allows.add(s));
    $allowInput.val('');
    renderAllows();

    if (!engineRunning) return;
    const errors = [];
    const requests = specs.map((spec) =>
      addAllowOne(spec).fail((xhr) => errors.push(`${spec}: ${errorOf(xhr)}`)),
    );
    $.when(...requests).always(() => {
      refreshStatus();
      if (errors.length) alert('Some allow entries failed to apply live:\n' + errors.join('\n'));
    });
  });

  $allowInput.on('keydown', (e) => { if (e.key === 'Enter') $btnAddAllow.click(); });

  function loadIfaces() {
    $.getJSON('/api/ifaces', (ifs) => {
      const $sel = $('#egress-iface');
      const current = $sel.val();
      const stored = localStorage.getItem(storageKey('#egress-iface'));
      $sel.empty();
      $sel.append($('<option>').val('').text('— disabled —'));
      let defaultName = '';
      (ifs || []).forEach((i) => {
        let label = i.name;
        if (i.ip) label += '  (' + i.ip + ')';
        if (i.is_default) {
          label += '  · default route';
          defaultName = i.name;
        }
        $sel.append($('<option>').val(i.name).text(label));
      });
      // Priority: existing form value → previously stored choice → default
      // route interface → "disabled".
      if (current) {
        $sel.val(current);
      } else if (stored) {
        $sel.val(stored);
      } else if (defaultName) {
        $sel.val(defaultName);
        persistField('#egress-iface');
      }
    });
  }

  function triggerStart() {
    const cfg = {
      local_ip: $('#local-ip').val().trim(),
      peer_ip: $('#peer-ip').val().trim(),
      listen: $('#listen').val().trim(),
      peer: $('#peer-udp').val().trim(),
      // Seed the engine with whatever the user has saved locally — that
      // way pending intercepts/allows become active immediately on Start.
      intercepts: getStoredSpecs(),
      inbound_allow: allows.get(),
      egress_iface: $('#egress-iface').val(),
      egress_subnet: $('#egress-subnet').val().trim(),
      camp_url: $('#camp-url').val().trim(),
      camp_stun: $('#camp-stun').val().trim(),
      camp_name: $('#camp-name').val().trim(),
      camp_room: $('#camp-room').val().trim(),
    };
    setEngineState('loading', 'starting…', '');
    $.ajax({
      url: '/api/start',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify(cfg)
    }).done(refreshStatus).fail((xhr) => {
      refreshStatus();
      alert('Start failed: ' + errorOf(xhr));
    });
  }

  function triggerStop() {
    setEngineState('loading', 'stopping…', '');
    $.ajax({ url: '/api/stop', method: 'POST' })
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
    if (!engineRunning) return;

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

  // Camp tab — list of peers in our current room. Polls our local proxy
  // (/api/camp/peers), which in turn fetches /api/rooms/<room> from the
  // configured camp server. Off-state ("engine not running") is the only
  // non-happy branch; once we're in a room there's always at least one
  // peer (us).
  const $campStatus = $('#camp-peers-status');
  const $campTable = $('#camp-peers-table');
  const $campBody = $('#camp-peers-tbody');
  const $campRoomMeta = $('#camp-room-meta');

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
      $campRoomMeta.text('');
      return;
    }
    const peers = Array.isArray(data.peers) ? data.peers : [];
    $campRoomMeta.text(data.room || '');
    if (peers.length === 0) {
      $campStatus.text('no peers in this room').show();
      $campTable.addClass('hidden');
      return;
    }
    $campStatus.hide();
    $campBody.empty();
    for (const p of peers) {
      const endpoint = p.udp_endpoint || (p.public_ip ? p.public_ip + (p.udp_port ? ':' + p.udp_port : '') : '—');
      const isYou = data.you && p.name === data.you;
      const $row = $('<tr>').css('color', isYou ? '#86b86b' : '');
      $row.append(
        $('<td>').css({padding: '4px 16px 4px 0'}).text(p.name + (isYou ? ' (you)' : '')),
        $('<td>').css({padding: '4px 16px 4px 0'}).text(p.tunnel_ip || '—'),
        $('<td>').css({padding: '4px 16px 4px 0'}).text(endpoint),
        $('<td>').css({padding: '4px 0', color: '#666'}).text(humanAgo(p.joined_at))
      );
      $campBody.append($row);
    }
    $campTable.removeClass('hidden');
  }

  function refreshCampPeers() {
    $.ajax({ url: '/api/camp/peers', dataType: 'json' })
      .done(renderCampPeers)
      .fail(() => {
        $campStatus.text('failed to fetch room state').show();
        $campTable.addClass('hidden');
      });
  }

  restoreForm();
  loadIfaces();
  refreshStatus();
  refreshTopology();
  refreshCampPeers();
  setInterval(refreshStatus, 3000);
  setInterval(refreshTopology, 2000);
  setInterval(refreshCampPeers, 3000);
  startLogStream();
});
