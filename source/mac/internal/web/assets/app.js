$(function () {
  const $status = $('#status-indicator');
  const $btnStart = $('#btn-start');
  const $btnStop = $('#btn-stop');
  const $btnAdd = $('#btn-add-intercept');
  const $btnClearLog = $('#btn-clear-log');
  const $list = $('#intercept-list');
  const $log = $('#log');
  const $interceptInput = $('#intercept-input');
  const $hint = $('#intercept-hint');

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
      $hint.addClass('hidden');
      $interceptInput.prop('disabled', false);
      $btnAdd.prop('disabled', false);
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
      $hint.removeClass('hidden');
      $interceptInput.prop('disabled', true);
      $btnAdd.prop('disabled', true);
    }

    renderIntercepts(s.intercepts || []);

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));
  }

  function renderIntercepts(items) {
    $list.empty();
    if (items.length === 0) {
      $list.append('<div class="text-sm text-gray-500">No intercepts.</div>');
      return;
    }
    items.forEach((it) => {
      const $row = $('<div class="flex items-center justify-between bg-gray-50 rounded p-3">');
      const $info = $('<div>');
      $info.append($('<div class="font-medium text-sm">').text(it.spec));
      $info.append($('<div class="text-xs text-gray-500 font-mono">').text((it.prefixes || []).join(', ')));
      $row.append($info);

      const $btn = $('<button class="text-rose-600 hover:text-rose-800 text-sm font-medium">Remove</button>');
      $btn.on('click', () => {
        $.ajax({ url: '/api/intercepts/' + encodeURIComponent(it.id), method: 'DELETE' })
          .done(refreshStatus)
          .fail((xhr) => alert('Remove failed: ' + errorOf(xhr)));
      });
      $row.append($btn);

      $list.append($row);
    });
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
      intercepts: [],
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

    const errors = [];
    const requests = specs.map((spec) =>
      addOne(spec).fail((xhr) => errors.push(`${spec}: ${errorOf(xhr)}`))
    );

    $.when(...requests).always(() => {
      $interceptInput.val('');
      refreshStatus();
      if (errors.length) alert('Some intercepts failed:\n' + errors.join('\n'));
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
