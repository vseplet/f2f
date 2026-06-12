$(function () {
  // Selected-peer state for the identity panel + last status sample. Declared
  // first because applyRoute() runs during init, before later code.
  let selectedPeer = '';   // peerKey of the peer whose details fill the identity panel
  let lastStatus = null;   // last /api/status sample, for re-rendering on route change
  let livePeers = [];      // last seen camp peers from /api/status (declared early:
                           // nameForPub/applyRoute reference it during init)

  // Tab switching. The terminal-styled tabbar at the top is the only UI:
  // we toggle .ax-tab-active on the clicked button and swap visible panels.
  $('.ax-tab').on('click', function () {
    const tab = $(this).data('tab');
    if (!tab) return;
    $('.ax-tab').removeClass('ax-tab-active');
    $(this).addClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-' + tab).removeClass('hidden');
    $('#status-diag').removeClass('active'); // leaving diagnostics
    $(document).trigger('f2f:tab-changed', [tab]);
  });

  // Left sidebar: width persists in localStorage, drag handle on the
  // right edge resizes it. Drag below the collapse threshold and it
  // becomes a thin strip — click that strip to expand it back. No button.
  const $sidebar = $('#ax-sidebar');
  const $sidebarResize = $('#ax-sidebar-resize');
  const SIDEBAR_KEY = 'f2f:sidebar-width';
  const LAST_EXPANDED_KEY = 'f2f:sidebar-expanded-width';
  const SIDEBAR_COLLAPSE_THRESHOLD = 80; // px
  const SIDEBAR_MIN = 32;
  const SIDEBAR_MAX = 600;
  const sidebarDefault = 240;

  // lastExpandedWidth survives reloads so clicking the strip restores the
  // user's preferred width.
  let lastExpandedWidth = parseInt(localStorage.getItem(LAST_EXPANDED_KEY) || '', 10);
  if (!Number.isFinite(lastExpandedWidth) || lastExpandedWidth < SIDEBAR_COLLAPSE_THRESHOLD) {
    lastExpandedWidth = sidebarDefault;
  }

  function applySidebarWidth(px) {
    const clamped = Math.max(SIDEBAR_MIN, Math.min(SIDEBAR_MAX, px));
    $sidebar.css('width', clamped + 'px');
    const collapsed = clamped < SIDEBAR_COLLAPSE_THRESHOLD;
    $sidebar.toggleClass('ax-collapsed', collapsed);
    if (!collapsed) {
      lastExpandedWidth = clamped;
      try { localStorage.setItem(LAST_EXPANDED_KEY, String(clamped)); } catch (_) {}
    }
  }
  try {
    const saved = parseInt(localStorage.getItem(SIDEBAR_KEY) || '', 10);
    applySidebarWidth(Number.isFinite(saved) ? saved : sidebarDefault);
  } catch (_) {
    applySidebarWidth(sidebarDefault);
  }

  $sidebarResize.on('mousedown', function (e) {
    e.preventDefault();
    $sidebarResize.addClass('dragging');
    const startX = e.clientX;
    const startW = $sidebar.outerWidth();
    function onMove(ev) {
      applySidebarWidth(startW + (ev.clientX - startX));
    }
    function onUp() {
      $sidebarResize.removeClass('dragging');
      $(document).off('mousemove.sbres mouseup.sbres');
      try { localStorage.setItem(SIDEBAR_KEY, String($sidebar.outerWidth())); } catch (_) {}
    }
    $(document).on('mousemove.sbres', onMove).on('mouseup.sbres', onUp);
  });

  // Click the collapsed strip (anywhere but the resize handle) to expand.
  $sidebar.on('click', function (e) {
    if (!$sidebar.hasClass('ax-collapsed')) return;
    if ($(e.target).closest('#ax-sidebar-resize').length) return;
    applySidebarWidth(lastExpandedWidth);
    try { localStorage.setItem(SIDEBAR_KEY, String($sidebar.outerWidth())); } catch (_) {}
  });

  // ---- Right notifications sidebar ----
  // Symmetric to the left tree but feeds off a moving log of events.
  // Width + collapsed state persist independently. Mock data for now;
  // a real notification service will push entries through the same
  // renderNotifications() function once it ships.
  const $notif = $('#ax-notifications');
  const $notifResize = $('#ax-notifications-resize');
  const NOTIF_KEY = 'f2f:notif-width';
  const NOTIF_EXPANDED_KEY = 'f2f:notif-expanded-width';
  const notifDefault = 260;
  function applyNotifWidth(px) {
    const clamped = Math.max(SIDEBAR_MIN, Math.min(SIDEBAR_MAX, px));
    $notif.css('width', clamped + 'px');
    $notif.toggleClass('ax-collapsed', clamped < SIDEBAR_COLLAPSE_THRESHOLD);
  }
  try {
    const saved = parseInt(localStorage.getItem(NOTIF_KEY) || '', 10);
    applyNotifWidth(Number.isFinite(saved) ? saved : notifDefault);
  } catch (_) { applyNotifWidth(notifDefault); }
  let lastNotifExpanded = parseInt(localStorage.getItem(NOTIF_EXPANDED_KEY) || '', 10);
  if (!Number.isFinite(lastNotifExpanded) || lastNotifExpanded < SIDEBAR_COLLAPSE_THRESHOLD) {
    lastNotifExpanded = notifDefault;
  }
  $notifResize.on('mousedown', function (e) {
    e.preventDefault();
    $notifResize.addClass('dragging');
    const startX = e.clientX;
    const startW = $notif.outerWidth();
    function onMove(ev) {
      // Resize handle is on the LEFT of the right panel — drag right
      // shrinks the panel, drag left grows it. Sign flips.
      applyNotifWidth(startW - (ev.clientX - startX));
    }
    function onUp() {
      $notifResize.removeClass('dragging');
      $(document).off('mousemove.nres mouseup.nres');
      try { localStorage.setItem(NOTIF_KEY, String($notif.outerWidth())); } catch (_) {}
    }
    $(document).on('mousemove.nres', onMove).on('mouseup.nres', onUp);
  });
  $('#ax-notifications-toggle').on('click', function () {
    if ($notif.hasClass('ax-collapsed')) {
      applyNotifWidth(lastNotifExpanded);
    } else {
      lastNotifExpanded = $notif.outerWidth();
      try { localStorage.setItem(NOTIF_EXPANDED_KEY, String(lastNotifExpanded)); } catch (_) {}
      applyNotifWidth(SIDEBAR_MIN);
    }
    try { localStorage.setItem(NOTIF_KEY, String($notif.outerWidth())); } catch (_) {}
  });
  function updateNotifGlyph() {
    // Mirror of the left toggle: › when open (click to collapse right),
    // ‹ when collapsed (click to expand back).
    $('#ax-notifications-toggle').text($notif.hasClass('ax-collapsed') ? '‹' : '›');
  }
  new MutationObserver(updateNotifGlyph)
    .observe($notif[0], { attributes: true, attributeFilter: ['class'] });
  updateNotifGlyph();

  // Notifications mock. kind ∈ {ok, warn, info, muted} drives the
  // accent bar colour: ok = something good happened, warn = needs
  // attention, info = passive, muted = stale/closed. group is the
  // day-bucket header the cards live under.
  // Each notification has a stable `id` so we can find/remove it after
  // render. Real notification service will mint these server-side; for
  // now we generate from the index at module load time.
  // Live notifications from the backend (/api/notifications + SSE). Newest
  // first. Sources: inbound messages, calls, and peer presence (up/down).
  let notifications = [];
  let selectedNotifId = null;

  function notifPeerName(pub) {
    if (!pub) return '';
    const p = ((lastStatus && lastStatus.peers) || []).find(x => x.pub === pub);
    return p ? (p.name || pub.slice(0, 12)) : pub.slice(0, 12);
  }
  function notifAccent(n) {
    const t = (n.title || '').toLowerCase();
    if (/fail|down|denied|blocked|offline|error/.test(t)) return 'warn';
    return ({ message: 'info', call: 'ok', cert: 'warn', peer: 'info', system: 'muted' })[n.kind] || 'info';
  }
  function notifWhen(ts) {
    if (!ts) return '';
    const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (s < 5) return 'now';
    if (s < 60) return s + 's';
    const m = Math.floor(s / 60); if (m < 60) return m + 'm';
    const h = Math.floor(m / 60); if (h < 24) return h + 'h';
    return Math.floor(h / 24) + 'd';
  }

  function renderNotifications() {
    const banner = notifPermPrompt();
    const q = ($('#ax-notifications-search').val() || '').trim().toLowerCase();
    const items = q
      ? notifications.filter(n => (n.title + ' ' + (n.body || '') + ' ' + notifPeerName(n.from)).toLowerCase().includes(q))
      : notifications;
    if (!items.length) {
      $('#ax-notifications-list').html(banner + empty('no notifications'));
      return;
    }
    const parts = items.map(n => {
      const selected = n.id === selectedNotifId ? ' selected' : '';
      const meta = n.body || notifPeerName(n.from);
      return `<div class="ax-notif ${esc(notifAccent(n))}${selected}" data-id="${esc(n.id)}" title="${esc(n.title)}">`
        + `<div class="ax-notif-accent"></div>`
        + `<div class="ax-notif-body">`
          + `<div class="ax-notif-title">${esc(n.title)}</div>`
          + (meta ? `<div class="ax-notif-meta">${esc(meta)}</div>` : '')
        + `</div>`
        + `<div class="ax-notif-time">${esc(notifWhen(n.ts))}</div>`
        + `<button type="button" class="ax-notif-close" title="dismiss" aria-label="dismiss">×</button>`
      + `</div>`;
    });
    $('#ax-notifications-list').html(banner + parts.join(''));
  }
  // Clear all — wipes the backend store for the active camp, then the local
  // list. New notifications keep streaming in over SSE afterwards.
  $('#ax-notifications-clear').on('click', function () {
    $.ajax({ url: '/api/notifications', method: 'DELETE' }).always(function () {
      notifications = [];
      selectedNotifId = null;
      renderNotifications();
    });
  });
  $('#ax-notifications-search').on('input', renderNotifications);
  $('#ax-notifications-search').on('keydown', function (e) {
    if (e.key === 'Escape') { $(this).val('').trigger('input').blur(); }
  });
  // Dismiss (×) — local-only: drop from the live list.
  $('#ax-notifications-list').on('click', '.ax-notif-close', function (e) {
    e.stopPropagation();
    const $card = $(this).closest('.ax-notif');
    const id = $card.data('id');
    $card.addClass('removing');
    setTimeout(function () {
      notifications = notifications.filter(n => String(n.id) !== String(id));
      if (selectedNotifId === id) selectedNotifId = null;
      renderNotifications();
    }, 180);
  });
  $('#ax-notifications-list').on('click', '.ax-notif', function () {
    const id = $(this).data('id');
    selectedNotifId = (selectedNotifId === String(id)) ? null : String(id);
    const n = notifications.find(x => String(x.id) === String(id));
    if (n && n.route) location.hash = n.route;
    renderNotifications();
  });

  // Seed from the buffer, then stream new ones over SSE.
  $.getJSON('/api/notifications', function (list) {
    notifications = (Array.isArray(list) ? list : []).slice().reverse(); // newest-first
    renderNotifications();
  });
  // Native OS notifications (Web Notifications API). Chrome silently drops a
  // permission request that isn't tied to a user gesture, so we don't ask on
  // load — instead notifPermPrompt() renders an "enable" banner the user
  // clicks (a real gesture), handled below. A toast is only raised when the
  // tab is hidden; an in-view sidebar already shows the entry.
  const canNotify = 'Notification' in window;
  function notifPermPrompt() {
    if (!canNotify || Notification.permission === 'granted') return '';
    if (Notification.permission === 'denied') {
      return `<div class="ax-notif-perm muted">${esc('desktop notifications blocked — allow them in the browser site settings')}</div>`;
    }
    return `<div class="ax-notif-perm" id="ax-notif-enable">enable desktop notifications</div>`;
  }
  // Gesture-driven permission request (reliable, unlike an on-load ask).
  $('#ax-notifications-list').on('click', '#ax-notif-enable', function () {
    if (!canNotify) return;
    Promise.resolve(Notification.requestPermission()).then(renderNotifications);
  });
  function osNotify(n) {
    if (!canNotify || Notification.permission !== 'granted') return;
    if (!document.hidden) return; // tab is focused — the sidebar already shows it
    let note;
    try {
      note = new Notification(n.title || 'f2f', {
        body: n.body || '',
        tag: 'f2f:' + (n.id || ''), // collapse rapid repeats from the same event
      });
    } catch (_) { return; }
    note.onclick = function () {
      window.focus();
      if (n.route) location.hash = n.route;
      note.close();
    };
  }

  (function notifStream() {
    let es;
    try { es = new EventSource('/api/notifications/stream'); } catch (_) { return; }
    es.onmessage = function (e) {
      let n; try { n = JSON.parse(e.data); } catch (_) { return; }
      notifications.unshift(n);
      if (notifications.length > 200) notifications.length = 200;
      renderNotifications();
      osNotify(n);
    };
  })();
  setInterval(renderNotifications, 30000); // refresh relative timestamps

  // Category collapse: click the row toggles .collapsed on the category;
  // the CSS adjacent-sibling selector hides .ax-tree-children.
  $('#ax-tree').on('click', '.ax-tree-category', function () {
    $(this).toggleClass('collapsed');
  });

  // Channel sub-tree collapse — the caret is a separate control: it toggles
  // the node's children and does NOT open the channel (stopPropagation keeps
  // the row's data-route handler from firing).
  $('#ax-tree').on('click', '.ax-chan-caret', function (e) {
    e.stopPropagation();
    toggleChanPath($(this).attr('data-chan-path'));
  });
  // A virtual folder routes nowhere, so clicking its body toggles it too.
  $('#ax-tree').on('click', '.ax-tree-row.ax-tree-folder', function () {
    toggleChanPath($(this).attr('data-chan-path'));
  });

  // Rows carrying data-url (e.g. domains) open their target in a new tab.
  $('#ax-tree').on('click', '.ax-tree-row[data-url]', function () {
    const url = $(this).attr('data-url');
    if (url) window.open(url, '_blank', 'noopener');
  });

  // Rows carrying data-route drive the main window via the URL hash:
  // the sidebar only sets location.hash, the router below reacts. This
  // is how sidebar selections (chats, …) open content in the main pane
  // without the two being directly coupled.
  $('#ax-tree').on('click', '.ax-tree-row[data-route]', function () {
    const route = $(this).attr('data-route');
    if (route) location.hash = route;
  });

  // Forget-peer button (offline ghosts only). Stops the row's own
  // navigation, DELETEs the peer, then refreshes the tree.
  $('#ax-tree').on('click', '.ax-tree-remove[data-remove-peer]', function (e) {
    e.stopPropagation();
    const pub = $(this).attr('data-remove-peer');
    if (!pub) return;
    $.ajax({ url: '/api/peers/' + encodeURIComponent(pub), method: 'DELETE' })
      .always(() => { if (typeof refreshStatus === 'function') refreshStatus(); });
  });


  // --- messaging (services/messenger over the bus) ---
  const GENERAL_ID = '*/general'; // camp-wide channel everyone is in (no leave)
  // URL routes: a conversation is "channel:<key>" — there's no separate "dm:"
  // because a DM is just the degenerate channel. The key's SHAPE tells them
  // apart: a bare peer pub is a DM, while "general" or an "<owner>/<name>" id
  // is a room. Notes are "note:<id>". general's ownerless "*/general" id is
  // shown in the URL as the clean "general" alias ("*/" is plumbing).
  function convKey(id) { return id === GENERAL_ID ? 'general' : id; }
  function convRoute(id) { return 'channel:' + convKey(id); }
  function noteRoute(id) { return 'note:' + convKey(id); }
  // convKind infers a conversation's kind from a (de-aliased) key.
  function convKind(key) { return (key === GENERAL_ID || key.includes('/')) ? 'channel' : 'dm'; }
  let chatChannels = [];      // /api/chat/channels — channels we belong to
  let chatConv = null;        // { kind:'dm'|'channel', key } currently open
  let replyTarget = null;     // message the next send will quote, or null
  let editTarget = null;      // { id } of the message the next send will edit, or null
  let chatMsgs = [];          // messages of the open conversation (cached for redraw)
  let chatNamesPending = false; // redraw the open chat once /api/status lands so
                                // authors/title resolve to names, not pub prefixes
  const chatUnread = {};      // conversation key → unread count

  // nameForPub renders a peer pub as its display name (falls back to a fp-ish
  // prefix). Self resolves to "you". Drives message authorship in the UI.
  function nameForPub(pub) {
    if (!pub) return '?';
    const self = lastStatus && lastStatus.identity_pub;
    if (pub === self) return 'you';
    const p = (livePeers || []).find((x) => x.pub === pub);
    return (p && p.name) || pub.slice(0, 8);
  }
  function hhmm(ts) {
    const d = new Date(ts || Date.now());
    return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  }

  // richBody renders a message body through the rich-text pipeline (inline
  // markdown + fenced code/markdown/mermaid, all sanitised). Falls back to
  // plain escaped text if richtext.js didn't load.
  function richBody(text) {
    return window.f2fRich ? f2fRich.render(text || '') : esc(text);
  }

  // msgSnippet is a one-line plain-text gist of a message — for reply quotes
  // and the reply compose bar. Attachments collapse to an emoji + name.
  function msgSnippet(m) {
    if (!m) return 'message';
    if (m.body) return m.body.replace(/\s+/g, ' ').trim().slice(0, 90);
    if (m.file) {
      const mime = m.file.mime || '';
      if (mime.indexOf('image/') === 0) return '📷 photo';
      if (mime.indexOf('video/') === 0) return '🎬 video';
      return '📎 ' + (m.file.name || 'file');
    }
    return 'message';
  }
  function authorOf(m) {
    return (m && (m.mine || (lastStatus && m.from === lastStatus.identity_pub))) ? 'you' : nameForPub(m && m.from);
  }
  // quoteHtml renders the "replying to" preview shown above a message that
  // quotes another. Resolves the target from the loaded history; click scrolls
  // to it (handled below). Falls back gracefully if the target isn't loaded.
  function quoteHtml(replyToId) {
    if (!replyToId) return '';
    const ref = chatMsgs.find((x) => x.id === replyToId);
    const who = ref ? authorOf(ref) : '';
    const snip = ref ? msgSnippet(ref) : 'message';
    return `<div class="ax-msg-quote" data-target="${esc(replyToId)}">`
      + (who ? `<span class="ax-quote-author">${esc(who)}</span>` : '')
      + `<span class="ax-quote-text">${esc(snip)}</span></div>`;
  }

  // msgRow renders one message. A type other than "text" is a channel
  // lifecycle event (create/add/remove) → a muted system line. grouped=true
  // omits the author/time header for a consecutive line from the same author.
  function msgRow(m, grouped) {
    if (m.type && m.type !== 'text') {
      const who = nameForPub(m.from);
      const names = (m.targets || []).map(nameForPub).join(', ');
      let line;
      if (m.type === 'create') line = who + ' created the channel';
      else if (m.type === 'add') line = who + ' added ' + (names || 'members');
      else if (m.type === 'remove') line = who + ' removed ' + (names || 'members');
      else if (m.type === 'call_start') line = '☎ ' + who + ' started a call';
      else if (m.type === 'call_end') line = '☎ call ended';
      else line = who + ' ' + m.type;
      // A channel call announcement doubles as the join affordance.
      const join = (m.type === 'call_start' && m.kind === 'channel')
        ? ` <a href="#call:group:${esc(m.peer)}" class="ax-msg-join">join</a>` : '';
      return `<div class="ax-msg ax-msg-system">${esc(line)}${join}</div>`;
    }
    // Own messages bubble on the right, everyone else's on the left.
    const mine = m.mine || (lastStatus && m.from === lastStatus.identity_pub);
    const author = mine ? 'you' : nameForPub(m.from);
    const edited = m.edited ? `<span class="ax-msg-edited">edited</span>` : '';
    const head = (grouped && !m.edited) ? '' :
      `<div class="ax-msg-head">`
        + `<span class="ax-msg-author">${esc(author)}</span>`
        + `<span class="ax-msg-time">${esc(hhmm(m.ts))}</span>`
        + edited
      + `</div>`;
    // A previewable attachment (image/video) and its caption share one bubble
    // so they read as a single message. A non-previewable file is its own
    // download chip with the caption as a separate text bubble. Plain text is
    // just the text bubble.
    const mime = (m.file && m.file.mime) || '';
    const previewable = mime.indexOf('image/') === 0 || mime.indexOf('video/') === 0;
    const caption = m.body ? `<div class="ax-msg-caption">${richBody(m.body)}</div>` : '';
    let body;
    if (m.file && previewable) {
      body = `<div class="ax-msg-media">${attachHtml(m.file)}${caption}</div>`;
    } else if (m.file) {
      body = attachHtml(m.file) + (m.body ? `<div class="ax-msg-text">${richBody(m.body)}</div>` : '');
    } else {
      body = `<div class="ax-msg-text">${richBody(m.body)}</div>`;
    }
    // Action row under the bubble, revealed on hover (aligned to the message's
    // side by the parent flex). "reply" always; "thread" only on a message that
    // isn't itself in a thread yet (an empty thread id → a potential root).
    const acts = `<div class="ax-msg-acts">`
      + `<button class="ax-msg-act" data-act="reply" title="reply"><i class="bi bi-reply-fill"></i> reply</button>`
      + (!m.thread ? `<button class="ax-msg-act" data-act="thread" title="reply in thread"><i class="bi bi-chat-square-text-fill"></i> thread</button>` : '')
      // Edit only on my own text messages (you can't edit someone else's).
      + ((mine && m.body) ? `<button class="ax-msg-act" data-act="edit" title="edit"><i class="bi bi-pencil-fill"></i> edit</button>` : '')
      + `</div>`;
    return `<div class="ax-msg${mine ? ' is-mine' : ''}" data-id="${esc(m.id)}" data-author="${esc(m.from)}">`
      + head
      + quoteHtml(m.reply_to)
      + body
      + acts
      + `</div>`;
  }

  // Object URLs minted for the currently-rendered attachments. Revoked and
  // rebuilt on every full chat redraw so they don't leak across conversations.
  let chatBlobUrls = [];
  function attachUrl(f) {
    // Decode the base64 the backend sent (Go marshals []byte as std base64)
    // into a Blob and hand back an object URL. Unlike a data: URL, a blob:
    // URL can be opened in a new tab, downloaded, and plays reliably in
    // <video> — data: URLs are blocked for top-frame navigation in Chrome.
    const bin = atob(f.data);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    const url = URL.createObjectURL(new Blob([bytes], { type: f.mime || 'application/octet-stream' }));
    chatBlobUrls.push(url);
    return url;
  }

  // attachHtml renders the attachment element itself (the enclosing .ax-msg-media
  // bubble in msgRow handles sizing/alignment). Images and clips preview in
  // place; anything else is a download chip. Built from a blob: URL (see above).
  function attachHtml(f) {
    if (!f) return '';
    // Torrent attachment: no inline bytes, carries a magnet. Rendered as a
    // chip with a download button + live status (updated by updateTorrentChips).
    if (f.info_hash) {
      const tn = esc(f.name || 'file');
      return `<div class="ax-msg-torrent" data-infohash="${esc(f.info_hash)}" data-magnet="${esc(f.magnet || '')}">`
        + `<div class="ax-msg-torrent-main">`
          + `<i class="bi bi-file-earmark-arrow-down"></i>`
          + `<span class="ax-msg-torrent-name">${tn}</span>`
          + `<span class="ax-msg-torrent-size">${esc(fmtBytes(f.size || 0))}</span>`
        + `</div>`
        + `<div class="ax-msg-torrent-status"></div>`
      + `</div>`;
    }
    if (!f.data) return '';
    let url;
    try { url = attachUrl(f); } catch (_) { return ''; }
    const mime = f.mime || '';
    const name = esc(f.name || 'file');
    if (mime.indexOf('image/') === 0) {
      return `<a href="${url}" target="_blank" rel="noopener" title="${name}">`
        + `<img class="ax-msg-img" src="${url}" alt="${name}"></a>`;
    }
    if (mime.indexOf('video/') === 0) {
      // Always offer a download link too: a codec Chrome can't decode (e.g. an
      // HEVC .mov) shows no picture, but the file is still saveable.
      return `<video class="ax-msg-video" src="${url}" controls preload="metadata"></video>`
        + `<a class="ax-msg-video-dl" href="${url}" download="${name}">⤓ ${name}</a>`;
    }
    return `<a class="ax-msg-file" href="${url}" download="${name}">`
      + `<i class="bi bi-paperclip"></i> <span>${name}</span>`
      + `<span class="ax-msg-file-size">${esc(fmtBytes(f.size || 0))}</span></a>`;
  }

  // editsByRoot maps an original message id → its latest edit (newest ts) made
  // by the SAME author. An edit from anyone else is ignored (you can't edit
  // someone else's message). Built per render from the conversation history.
  function editsByRoot(msgs) {
    const byId = {};
    for (const m of msgs) if (!m.edit_id) byId[m.id] = m;
    const edits = {};
    for (const m of msgs) {
      if (!m.edit_id) continue;
      const orig = byId[m.edit_id];
      if (!orig || orig.from !== m.from) continue; // only the author may edit
      const cur = edits[m.edit_id];
      if (!cur || m.ts >= cur.ts) edits[m.edit_id] = m;
    }
    return edits;
  }
  // applyEdit returns the message to display: the original patched with its
  // latest edit's body/file (keeping the original's position, author, ts).
  function applyEdit(m, edits) {
    const e = edits[m.id];
    if (!e) return m;
    return Object.assign({}, m, { body: e.body, file: e.file || m.file, edited: true });
  }

  function renderChat(msgs) {
    // A full redraw replaces the DOM wholesale, orphaning the previous blobs.
    chatBlobUrls.forEach((u) => { try { URL.revokeObjectURL(u); } catch (_) {} });
    chatBlobUrls = [];
    const edits = editsByRoot(msgs || []);
    let html = '';
    for (const raw of (msgs || [])) {
      if (raw.edit_id) continue; // an edit patches its original; not its own bubble
      const m = applyEdit(raw, edits);
      // Header grouping (collapsing consecutive same-author lines) is disabled
      // for now — every message shows its own author/time header.
      html += msgRow(m, false);
    }
    const $m = $('#chat-messages');
    $m.html(html || '<div class="ax-msg-system">no messages yet</div>');
    $m.scrollTop($m[0].scrollHeight);
    if (window.f2fRich) f2fRich.renderDiagrams($m[0]); // turn ```mermaid into SVG
    updateTorrentChips(); // paint cached status, then refresh from the backend
    pollTorrents();
  }

  // setChatTitle resolves the open conversation's header (channel name or
  // peer nickname). Re-callable so it updates once /api/status lands.
  function setChatTitle() {
    if (!chatConv) return;
    const title = chatConv.kind === 'channel'
      ? '# ' + ((chatChannels.find((c) => c.id === chatConv.key) || {}).name || chatConv.key.split('/').pop())
      : nameForPub(chatConv.key);
    $('#chat-title').text(title);
  }

  // loadConversation fetches history for the open conversation and renders.
  function loadConversation() {
    if (!chatConv) return;
    chatUnread[chatConv.key] = 0;
    $.getJSON('/api/chat/messages?kind=' + encodeURIComponent(chatConv.kind)
      + '&key=' + encodeURIComponent(chatConv.key), (msgs) => {
      if (!chatConv) return;
      chatMsgs = msgs || [];
      renderChat(chatMsgs);
      setChatTitle();
    });
  }

  // fetchChannels refreshes the channel list, then rebuilds the sidebar and
  // (if open) the members panel.
  function fetchChannels() {
    $.getJSON('/api/chat/channels', (list) => {
      chatChannels = Array.isArray(list) ? list : [];
      if (lastStatus && typeof renderSidebarTree === 'function') renderSidebarTree(lastStatus);
      if (!$('#chat-members-panel').hasClass('hidden')) renderMembersPanel();
      // Refresh the open notes editor — renderNote keeps unsaved edits (so
      // this fills the editor on first paint after a reload, once the list
      // arrives, and reflects remote edits otherwise).
      if (noteConv && !$('#tab-note').hasClass('hidden')) renderNote();
    });
  }

  // renderMembersPanel lists a channel's members. The owner gets remove
  // buttons and an "add member" dropdown of peers not yet in; others see a
  // read-only roster. Reads from the materialised channel in chatChannels.
  function renderMembersPanel() {
    const $p = $('#chat-members-panel');
    if (!chatConv || chatConv.kind !== 'channel') { $p.addClass('hidden').empty(); return; }
    const ch = chatChannels.find((c) => c.id === chatConv.key);
    if (!ch) { $p.empty(); return; }
    const selfPub = lastStatus && lastStatus.identity_pub;
    const isGeneral = ch.id === GENERAL_ID;
    const isOwner = !isGeneral && ch.owner === selfPub;
    let html = '<div class="ax-chat-members-list">';
    (ch.members || []).forEach((pub) => {
      const rm = (isOwner && pub !== selfPub)
        ? `<button class="ax-chat-member-rm" data-rm="${esc(pub)}" title="remove">×</button>` : '';
      html += `<span class="ax-chat-member">${esc(nameForPub(pub))}${rm}</span>`;
    });
    html += '</div>';
    if (isOwner) {
      const inChan = ch.members || [];
      const candidates = (livePeers || []).filter((p) => !p.self && p.pub && inChan.indexOf(p.pub) === -1);
      if (candidates.length) {
        html += '<div class="ax-chat-members-add"><select id="chat-add-member">'
          + '<option value="">+ add member…</option>'
          + candidates.map((p) => `<option value="${esc(p.pub)}">${esc(p.name || p.pub.slice(0, 8))}</option>`).join('')
          + '</select></div>';
      }
    } else if (isGeneral) {
      html += '<div class="ax-chat-members-note">everyone in the camp is a member</div>';
    } else {
      html += '<div class="ax-chat-members-note">only the owner can change members</div>';
    }
    // Owner tears the channel down for everyone; a member just leaves. The
    // general channel is permanent — no delete or leave.
    if (!isGeneral) {
      html += '<div class="ax-chat-members-actions">'
        + (isOwner
            ? '<button class="ax-chat-danger" id="chat-delete-channel">delete channel</button>'
            : '<button class="ax-chat-danger" id="chat-leave-channel">leave channel</button>')
        + '</div>';
    }
    $p.html(html).removeClass('hidden');
  }

  // Delete (owner) / leave (member) the open channel.
  function chatChannelAction(path, verb) {
    if (!chatConv || !confirm(verb + ' channel?')) return;
    $.ajax({
      url: path, method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: chatConv.key }),
    }).done(() => { location.hash = ''; fetchChannels(); })
      .fail((xhr) => alert(verb + ': ' + errorOf(xhr)));
  }
  $('#chat-members-panel').on('click', '#chat-delete-channel', () => chatChannelAction('/api/chat/channels/delete', 'delete'));
  $('#chat-members-panel').on('click', '#chat-leave-channel', () => chatChannelAction('/api/chat/channels/leave', 'leave'));

  // --- channel notes (the shared doc, opened in the main pane) ---
  //
  // A conversation's notes open as their own main-window view (note:<scope>),
  // not a dropdown — a clean full-pane editor (whitepaper-style). A DM is a
  // channel too, so DMs carry notes as well; the doc is fetched/saved by scope
  // (channel id or peer pub). Edits debounce-save (last-writer-wins).
  let noteConv = null;       // scope of the open note, or null
  let noteSaveTimer = null;
  let noteDirty = false;     // unsaved local edits — guards against clobbering
  let noteDoc = null;        // last doc from the server: {scope, body, ts, by}

  // noteTitle resolves a scope to a header: a channel's name (# foo) or, for a
  // DM, the peer's nickname. Falls back to the scope's leaf if names aren't in.
  function noteTitle(scope) {
    if (scope === GENERAL_ID || scope.includes('/')) {
      const ch = chatChannels.find((c) => c.id === scope);
      return '# ' + ((ch && ch.name) || scope.split('/').pop()) + ' · notes';
    }
    return nameForPub(scope) + ' · notes';
  }

  function openNote(scope) {
    noteConv = scope;
    noteDirty = false;
    noteDoc = null;
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-note').removeClass('hidden');
    $('#note-title').text(noteTitle(scope));
    $('#note-status').text('loading…');
    $('#note-text').val('');
    fetchNote(scope);
    setTimeout(() => $('#note-text').focus(), 0);
  }

  // fetchNote loads the doc for a scope from the backend, then paints it.
  function fetchNote(scope) {
    $.getJSON('/api/chat/notes?key=' + encodeURIComponent(scope), (doc) => {
      if (noteConv !== scope) return; // navigated away mid-flight
      noteDoc = doc || {};
      renderNote();
    }).fail(() => { if (noteConv === scope) $('#note-status').text('load failed'); });
  }

  // renderNote paints the editor from the fetched doc. It mirrors the server
  // copy UNLESS there are unsaved local edits — so it reflects remote edits
  // without ever eating what you're typing.
  function renderNote() {
    if (!noteConv) return;
    $('#note-title').text(noteTitle(noteConv));
    $('#note-status').text(noteDoc && noteDoc.by ? 'last edit · ' + nameForPub(noteDoc.by) : '');
    if (!noteDirty) $('#note-text').val((noteDoc && noteDoc.body) || '');
  }

  function saveNote(scope, body) {
    $('#note-status').text('saving…');
    $.ajax({
      url: '/api/chat/notes', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ key: scope, body }),
    }).done((doc) => {
      if (noteConv !== scope) return;
      noteDoc = doc || noteDoc;
      // Clear dirty only if no edit happened during the request (the textarea
      // still holds exactly what we sent) — otherwise a newer edit is pending.
      if ($('#note-text').val() === body) noteDirty = false;
      $('#note-status').text('saved');
    }).fail((xhr) => { if (noteConv === scope) $('#note-status').text('save failed: ' + errorOf(xhr)); });
  }

  // Debounced auto-save on every edit (whitepaper-style — no save button).
  $('#note-text').on('input', function () {
    if (!noteConv) return;
    noteDirty = true;
    const body = $(this).val();
    $('#note-status').text('editing…');
    clearTimeout(noteSaveTimer);
    noteSaveTimer = setTimeout(() => saveNote(noteConv, body), 600);
  });

  // Open a channel's notes from the hover icon on its sidebar row.
  $('#ax-tree').on('click', '.ax-tree-note', function (e) {
    e.stopPropagation();
    const id = $(this).attr('data-note');
    if (id) location.hash = noteRoute(id);
  });

  // Toggle the members panel.
  $('#chat-members').on('click', function () {
    const $p = $('#chat-members-panel');
    if ($p.hasClass('hidden')) renderMembersPanel();
    else $p.addClass('hidden');
  });
  // Remove a member (owner only).
  $('#chat-members-panel').on('click', '.ax-chat-member-rm', function () {
    const pub = $(this).attr('data-rm');
    if (!pub || !chatConv) return;
    $.ajax({
      url: '/api/chat/members', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: chatConv.key, remove: [pub] }),
    }).done(fetchChannels).fail((xhr) => alert('remove member: ' + errorOf(xhr)));
  });
  // Add a member (owner only).
  $('#chat-members-panel').on('change', '#chat-add-member', function () {
    const pub = $(this).val();
    if (!pub || !chatConv) return;
    $.ajax({
      url: '/api/chat/members', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: chatConv.key, add: [pub] }),
    }).done(fetchChannels).fail((xhr) => alert('add member: ' + errorOf(xhr)));
  });

  // Live message stream: append to the open conversation, else bump unread.
  function openChatStream() {
    const es = new EventSource('/api/chat/stream');
    es.onmessage = (e) => {
      let m; try { m = JSON.parse(e.data); } catch (_) { return; }
      const key = m.kind === 'channel' ? m.peer : (m.mine ? m.to : m.from);
      // A channel lifecycle event may have created/changed/removed a channel
      // — refresh the list so it (dis)appears.
      if (m.kind === 'channel' && m.type && m.type !== 'text') fetchChannels();
      // A notes edit mutates the conversation's shared doc, not the feed:
      // re-fetch if its scope is the one we're viewing, and never render it as
      // a chat line. (key, computed above, is the conversation scope.)
      if (m.type === 'notes') {
        if (noteConv && noteConv === key && !$('#tab-note').hasClass('hidden')) fetchNote(noteConv);
        return;
      }
      // The open channel was deleted (or we left) → close the view.
      if ((m.type === 'delete' || m.type === 'leave')
          && chatConv && chatConv.kind === 'channel' && chatConv.key === m.peer) {
        chatConv = null;
        location.hash = '';
        return;
      }
      // System lifecycle lines (create/add/remove) belong in the channel
      // view; render them too, not just text.
      if (chatConv && chatConv.kind === m.kind && chatConv.key === key) {
        chatMsgs.push(m);
        // An edit patches an existing bubble in place → full redraw. A normal
        // message just appends (cheaper, keeps scroll).
        if (m.edit_id) {
          renderChat(chatMsgs);
        } else {
          $('#chat-messages').append(msgRow(m, false));
          const $m = $('#chat-messages');
          if (window.f2fRich) f2fRich.renderDiagrams($m[0]);
          $m.scrollTop($m[0].scrollHeight);
          if (m.file && m.file.info_hash) { updateTorrentChips(); pollTorrents(); }
        }
      } else if (!m.mine && !m.edit_id) {
        chatUnread[key] = (chatUnread[key] || 0) + 1;
      }
    };
    es.onerror = () => {}; // EventSource auto-reconnects
  }
  openChatStream();
  fetchChannels();

  // Hash router. Handles conversation routes (channel:<key>, where a bare pub
  // key is a DM), notes (note:<id>), and resource tabs (peer:/tunnel:/…);
  // unknown hashes are ignored so normal tab switching keeps working.
  function activateTab(tab) {
    $('.ax-tab').removeClass('ax-tab-active');
    $('.ax-tab[data-tab="' + tab + '"]').addClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-' + tab).removeClass('hidden');
  }
  function applyRoute() {
    const h = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    // peer:<key> → open the (hidden) camp tab with that peer's details.
    const pm = h.match(/^peer:(.+)$/);
    if (pm) {
      selectedPeer = pm[1];
      activateTab('camp');
      renderIdentity(lastStatus);
      $('#status-diag').removeClass('active');
      return;
    }
    // leaving a peer route clears the selection so identity shows self again.
    if (selectedPeer) { selectedPeer = ''; renderIdentity(lastStatus); }
    // diag → open the (hidden) diagnostics tab.
    if (h === 'diag') {
      activateTab('diagnostics');
      $('#status-diag').addClass('active');
      return;
    }
    $('#status-diag').removeClass('active');
    // invite ("add peer") → camp tab; the invite flow will live there.
    if (h === 'invite') { activateTab('camp'); return; }
    // tunnel:/dns:/drop: rows open their (hidden) tab, like peers open camp.
    const tm = h.match(/^(tunnel|dns|drop)(?::.*)?$/);
    if (tm) { activateTab(tm[1]); return; }
    // "channel:new" → prompt for a name, create EMPTY (just us), open it and
    // pop the members panel so the user adds people deliberately — no
    // surprise "everyone's already in".
    if (h === 'channel:new') {
      const name = (prompt('channel name (use / for hierarchy, e.g. dev/backend)') || '').trim();
      if (name) {
        $.ajax({
          url: '/api/chat/channels', method: 'POST', contentType: 'application/json',
          data: JSON.stringify({ name, members: [] }),
        }).done((ch) => {
          fetchChannels();
          if (ch && ch.id) {
            location.hash = convRoute(ch.id);
            setTimeout(renderMembersPanel, 0); // open the panel to add members
          }
        }).fail((xhr) => alert('create channel: ' + errorOf(xhr)));
      }
      location.hash = '';
      return;
    }
    // note:<id> → open the channel's shared notes in the main pane.
    const nm = h.match(/^note:(.+)$/);
    if (nm) {
      let key = nm[1];
      if (key === GENERAL_ID) { location.hash = noteRoute(GENERAL_ID); return; }
      if (key === 'general') key = GENERAL_ID;
      openNote(key);
      return;
    }
    // A conversation: channel:<key>. Everything is a channel — a DM is the
    // degenerate one, keyed by the peer's pub; convKind() tells them apart by
    // the key's shape. general's clean "general" alias ↔ its internal
    // "*/general" id; a raw "*/general" in the hash (e.g. a notification deep-
    // link) is rewritten to the alias so URL + sidebar highlight stay in sync.
    const m = h.match(/^channel:(.+)$/);
    if (!m) return;
    let key = m[1];
    if (key === GENERAL_ID) { location.hash = convRoute(GENERAL_ID); return; }
    if (key === 'general') key = GENERAL_ID;
    const kind = convKind(key);
    chatConv = { kind, key };
    chatNamesPending = true; // names may not be loaded yet (e.g. hard reload)
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-chat').removeClass('hidden');
    setChatTitle();
    $('#chat-call').show(); // call available in both DMs (1:1) and channels (group)
    // Members button only for channels; collapse the panel on switch.
    $('#chat-members').toggle(kind === 'channel');
    $('#chat-members-panel').addClass('hidden').empty();
    clearReplyTarget(); // a pending reply doesn't carry across conversations
    loadConversation();
  }
  // highlightActiveRoute marks the sidebar row matching the current hash
  // so the user can see where they are. Re-run after every tree rebuild
  // (the sidebar is regenerated from status each tick).
  function highlightActiveRoute() {
    let route = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    // A note view keeps its channel's row highlighted — map note:<id> to the
    // channel's own route so the row matches.
    const noteM = route.match(/^note:(.+)$/);
    if (noteM) route = 'channel:' + noteM[1];
    // An ongoing call stays flagged in the sidebar even when we navigate
    // away to a chat. Mark both its meet row and the peer's DM row.
    const a = window.f2fCall && window.f2fCall.active;
    const callRoutes = [];
    if (a) {
      // dm routes are keyed by pub (display name lives in a.id/a.title).
      const key = a.kind === 'dm' ? (a.pub || a.id) : a.id;
      callRoutes.push('call:' + (a.kind === 'dm' ? 'dm' : a.kind) + ':' + key);
      callRoutes.push(convRoute(a.kind === 'dm' ? key : a.id));
    }
    $('#ax-tree .ax-tree-row').each(function () {
      const r = $(this).attr('data-route');
      $(this).toggleClass('active', !!route && r === route);
      $(this).toggleClass('in-call', !!r && callRoutes.indexOf(r) !== -1);
    });
  }
  window.addEventListener('hashchange', function () {
    applyRoute();
    highlightActiveRoute();
  });
  applyRoute();
  highlightActiveRoute();

  // --- replies ---
  // replyTarget (declared up top) is what the next send will reference, or
  // null: { id, author, snippet, thread }. thread is "" for a plain quoted
  // reply, or the root message id for a threaded reply. Set from a message's
  // action buttons, shown in the compose bar, cleared on send or cancel.
  function replyId() { return replyTarget ? replyTarget.id : ''; }
  function threadId() { return (replyTarget && replyTarget.thread) || ''; }
  function editId() { return editTarget ? editTarget.id : ''; }
  function setReplyTarget(t) {
    editTarget = null; // reply and edit are mutually exclusive
    replyTarget = t;
    const inThread = !!t.thread;
    $('#chat-reply-bar').html(
      `<div class="ax-replybar-in">`
        + `<i class="bi ${inThread ? 'bi-chat-square-text-fill' : 'bi-reply-fill'}"></i>`
        + `<span class="ax-replybar-author">${esc(inThread ? 'in thread · ' : '')}${esc(t.author)}</span>`
        + `<span class="ax-replybar-snip">${esc(t.snippet)}</span>`
        + `<button type="button" class="ax-replybar-x" title="cancel" aria-label="cancel">×</button>`
      + `</div>`
    ).removeClass('hidden');
    $('#chat-input').focus();
  }
  // startEdit recalls a message into the composer to edit it: the next send
  // becomes an edit (edit_id) that supersedes it. text is the current content.
  function startEdit(t) {
    replyTarget = null;
    editTarget = { id: t.id };
    $('#chat-reply-bar').html(
      `<div class="ax-replybar-in ax-replybar-edit">`
        + `<i class="bi bi-pencil-fill"></i>`
        + `<span class="ax-replybar-author">editing</span>`
        + `<span class="ax-replybar-snip">your message · Esc to cancel</span>`
        + `<button type="button" class="ax-replybar-x" title="cancel edit" aria-label="cancel">×</button>`
      + `</div>`
    ).removeClass('hidden');
    const $in = $('#chat-input');
    $in.val(t.text);
    autoGrowInput();
    $in.focus();
    const el = $in[0]; if (el.setSelectionRange) el.setSelectionRange(el.value.length, el.value.length);
  }
  // clearCompose drops any reply/thread/edit context and the composer text if
  // we were editing (so Esc fully backs out of an edit).
  function clearReplyTarget() {
    const wasEditing = !!editTarget;
    replyTarget = null;
    editTarget = null;
    $('#chat-reply-bar').addClass('hidden').empty();
    if (wasEditing) { $('#chat-input').val(''); autoGrowInput(); }
  }
  $('#chat-reply-bar').on('click', '.ax-replybar-x', clearReplyTarget);
  // lastMineText finds my most recent text message in the open conversation and
  // its CURRENT content (after edits) — the up-arrow recalls it for editing.
  function lastMineText() {
    const me = lastStatus && lastStatus.identity_pub;
    const edits = editsByRoot(chatMsgs);
    for (let i = chatMsgs.length - 1; i >= 0; i--) {
      const m = chatMsgs[i];
      if (m.edit_id) continue;
      if (m.type && m.type !== 'text') continue;
      if (!(m.mine || (me && m.from === me))) continue;
      const cur = edits[m.id] || m;
      if (!cur.body) continue; // nothing editable (attachment-only)
      return { id: m.id, text: cur.body };
    }
    return null;
  }
  // Reply / reply-in-thread from a message's action buttons. "thread" carries
  // the root id so the sent message is tagged into that thread (the threaded
  // view is a later pass; the quote still shows it answers the root).
  // currentText resolves a message id to its latest content (original patched
  // with the newest edit) — what the edit button recalls into the composer.
  function currentText(id) {
    const orig = chatMsgs.find((x) => x.id === id && !x.edit_id);
    if (!orig) return '';
    return (editsByRoot(chatMsgs)[id] || orig).body || '';
  }
  $('#chat-messages').on('click', '.ax-msg-act', function (e) {
    e.stopPropagation();
    const $msg = $(this).closest('.ax-msg');
    const id = $msg.attr('data-id');
    if (!id) return;
    const act = $(this).attr('data-act');
    if (act === 'edit') { startEdit({ id, text: currentText(id) }); return; }
    const ref = chatMsgs.find((x) => x.id === id);
    const author = ref ? authorOf(ref) : nameForPub($msg.attr('data-author'));
    const thread = act === 'thread' ? id : '';
    setReplyTarget({ id, author, snippet: msgSnippet(ref), thread });
  });
  // Click a quote to jump to (and briefly flash) the message it answers.
  $('#chat-messages').on('click', '.ax-msg-quote', function () {
    const id = $(this).attr('data-target') || '';
    const $t = $('#chat-messages .ax-msg').filter(function () { return $(this).attr('data-id') === id; });
    if (!$t.length) return;
    $t[0].scrollIntoView({ behavior: 'smooth', block: 'center' });
    $t.addClass('ax-msg-flash');
    setTimeout(() => $t.removeClass('ax-msg-flash'), 1200);
  });

  // Send: POST to the messenger service; the message comes back on the SSE
  // stream (the backend echoes our own copy), which appends it — so we don't
  // append here, avoiding a duplicate.
  $('#chat-form').on('submit', function (e) {
    e.preventDefault();
    if (!chatConv) return;
    const $in = $('#chat-input');
    const text = $in.val().trim();
    if (!text) return;
    $in.val('');
    autoGrowInput();
    const reply_to = replyId(), thread = threadId(), edit_id = editId();
    clearReplyTarget();
    $.ajax({
      url: '/api/chat/send', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ kind: chatConv.kind, key: chatConv.key, body: text, reply_to, thread, edit_id }),
    }).fail((xhr) => alert('send: ' + errorOf(xhr)));
  });

  // Composer is a textarea: Enter sends, Shift+Enter inserts a newline (so a
  // multi-line message — e.g. a whole ```js fenced block — fits in ONE send).
  // isComposing guards IME input (Russian/CJK) mid-composition. Up-arrow on an
  // empty composer recalls my last message to edit it; Esc cancels.
  $('#chat-input').on('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $('#chat-form').trigger('submit');
      return;
    }
    if (e.key === 'ArrowUp' && !e.shiftKey && !editTarget && !replyTarget && $(this).val() === '') {
      const last = lastMineText();
      if (last) { e.preventDefault(); startEdit(last); }
      return;
    }
    if (e.key === 'Escape' && (editTarget || replyTarget)) {
      e.preventDefault();
      clearReplyTarget();
    }
  });
  // Auto-grow the composer with its content, up to the CSS max-height (then it
  // scrolls). Reset after a send.
  function autoGrowInput() {
    const el = $('#chat-input')[0];
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 160) + 'px';
  }
  $('#chat-input').on('input', autoGrowInput);

  // Attachments: the + button opens a file picker. Small files ride inline
  // (base64) on the message; larger ones are seeded and shared over torrent
  // (the message carries the magnet, the recipient downloads from the chat).
  const MAX_ATTACH = 8 * 1024 * 1024; // matches the backend's inline cap
  $('#chat-attach').on('click', function () {
    if (chatConv) $('#chat-file').trigger('click');
  });
  $('#chat-file').on('change', function () {
    const file = this.files && this.files[0];
    this.value = ''; // allow re-picking the same file later
    if (!file || !chatConv) return;
    if (file.size > MAX_ATTACH) { shareViaTorrent(file); return; } // large → torrent
    const reader = new FileReader();
    reader.onload = function () {
      // readAsDataURL gives "data:<mime>;base64,<payload>" — keep the payload;
      // Go decodes the []byte field straight from that base64 string.
      const b64 = String(reader.result).split(',', 2)[1] || '';
      const $in = $('#chat-input');
      const caption = $in.val().trim();
      $in.val('');
      const reply_to = replyId(), thread = threadId(), edit_id = editId();
      clearReplyTarget();
      $.ajax({
        url: '/api/chat/send', method: 'POST', contentType: 'application/json',
        data: JSON.stringify({
          kind: chatConv.kind, key: chatConv.key, body: caption, reply_to, thread, edit_id,
          file: { name: file.name, mime: file.type || 'application/octet-stream', data: b64 },
        }),
      }).fail((xhr) => alert('send: ' + errorOf(xhr)));
    };
    reader.onerror = function () { alert('could not read file'); };
    reader.readAsDataURL(file);
  });

  // Seed a large file and post it as a torrent message (multipart upload).
  // The backend pins the seed to this conversation (private to it) and echoes
  // the message back over the chat stream, where it renders with a download
  // affordance + live transfer status.
  function shareViaTorrent(file) {
    const $in = $('#chat-input');
    const caption = $in.val().trim();
    $in.val('');
    const fd = new FormData();
    fd.append('kind', chatConv.kind);
    fd.append('key', chatConv.key);
    fd.append('body', caption);
    fd.append('reply_to', replyId());
    fd.append('thread', threadId());
    fd.append('edit_id', editId());
    fd.append('file', file);
    clearReplyTarget();
    $.ajax({ url: '/api/chat/share', method: 'POST', data: fd, processData: false, contentType: false })
      .fail((xhr) => alert('share: ' + errorOf(xhr)));
  }

  // --- torrent transfers surfaced in chat ---
  let torrentStatus = {}; // info_hash → /api/files/downloads row
  // The BT client listens on the overlay v4 alias at the default torrent port;
  // anacrolix needs a host:port (a bare IP is not a dialable peer address).
  function overlayForPub(pub) {
    const p = (livePeers || []).find((x) => x.pub === pub);
    return (p && p.overlay_v4) ? p.overlay_v4 + ':6881' : '';
  }
  // Source addresses to feed a download: the sender (a guaranteed seeder) plus
  // the other conversation members (potential seeders). anacrolix only dials
  // fed peers — there's no DHT — so we hand it everyone who might have it.
  function torrentPeers($chip) {
    const ips = [];
    const add = (pub) => { const ip = overlayForPub(pub); if (ip && ips.indexOf(ip) === -1) ips.push(ip); };
    add($chip.closest('.ax-msg').data('author'));
    if (chatConv && chatConv.kind === 'channel') {
      const ch = chatChannels.find((c) => c.id === chatConv.key);
      ((ch && ch.members) || []).forEach(add);
    }
    return ips;
  }
  function updateTorrentChips() {
    $('#chat-messages .ax-msg-torrent').each(function () {
      const $c = $(this);
      const mine = $c.closest('.ax-msg').hasClass('is-mine');
      const st = torrentStatus[$c.attr('data-infohash')];
      let html;
      if (mine) {
        html = '<span class="t-seed">● seeding</span>';
      } else if (!st) {
        html = '<button type="button" class="ax-msg-torrent-dl">⤓ download</button>';
      } else if (st.complete) {
        html = '<span class="t-done">✓ downloaded</span>'
          + (st.path ? ' <a class="ax-msg-torrent-open">open</a>' : '');
      } else if (st.fetching_metadata) {
        html = '<span class="t-wait">' + (st.source_online ? 'connecting…' : 'source offline') + '</span>';
      } else {
        const pct = st.size > 0 ? Math.floor((st.bytes_completed / st.size) * 100) : 0;
        html = '<span class="t-prog">' + pct + '% · '
          + fmtBytes(st.bytes_completed || 0) + ' / ' + fmtBytes(st.size || 0) + '</span>';
      }
      $c.find('.ax-msg-torrent-status').html(html);
    });
  }
  function pollTorrents() {
    if (!$('#chat-messages .ax-msg-torrent').length) return;
    $.getJSON('/api/files/downloads', function (list) {
      const m = {};
      (Array.isArray(list) ? list : []).forEach((d) => { m[d.info_hash] = d; });
      torrentStatus = m;
      updateTorrentChips();
    });
  }
  $('#chat-messages').on('click', '.ax-msg-torrent-dl', function () {
    const $c = $(this).closest('.ax-msg-torrent');
    const magnet = $c.attr('data-magnet');
    if (!magnet) return;
    $c.find('.ax-msg-torrent-status').html('<span class="t-wait">starting…</span>');
    $.ajax({
      url: '/api/files/download', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ magnet: magnet, peers: torrentPeers($c) }),
    }).done(pollTorrents).fail((xhr) => alert('download: ' + errorOf(xhr)));
  });
  $('#chat-messages').on('click', '.ax-msg-torrent-open', function () {
    const st = torrentStatus[$(this).closest('.ax-msg-torrent').attr('data-infohash')];
    if (st && st.path) {
      $.ajax({ url: '/api/files/reveal', method: 'POST', contentType: 'application/json', data: JSON.stringify({ path: st.path }) });
    }
  });
  setInterval(pollTorrents, 2500);

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
  // Shell/VNC discovery is STICKY: bus links flap, so we keep a peer listed
  // for a short TTL after it was last seen reachable instead of dropping it
  // the instant one poll comes back without it. {pub → {name, ts}}.
  const PEER_TTL_MS = 35000;
  let shellSeen = {};      // /api/shell/peers
  let vncSeen = {};        // /api/vnc/peers
  function markSeen(seen, list) {
    if (!Array.isArray(list)) return;
    const now = Date.now();
    for (const p of list) { if (p && p.pub) seen[p.pub] = { name: p.name, ts: now }; }
  }
  function freshPeers(seen) {
    const now = Date.now();
    return Object.keys(seen)
      .filter((pub) => now - seen[pub].ts < PEER_TTL_MS)
      .map((pub) => ({ pub, name: seen[pub].name }));
  }
  const expandedIntercepts = new Set(); // keys (spec|peer) currently expanded

  // Camp identity is loaded from the backend (/api/status) on render;
  // the UI no longer creates or switches camps (that's the CLI's job).
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
      running: 'engine running — manage camps via the CLI',
      stopped: 'engine stopped — run `sudo f2f` to bring up a camp',
      loading: 'loading…',
      error: 'API error',
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

  // Camp lifecycle (create / join / switch / stop) lives in the CLI
  // now — the UI is read-only for camps. It just reflects whatever camp
  // the backend is running (or shows the "no camp" hint when stopped).

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
  // Diagnostics tab is opened from the status bar via a route (tab is hidden).
  $('#status-diag').on('click', function () { location.hash = 'diag'; });

  // Camp id lives in the status bar now — click it to copy the full id.
  $('#status-camp').on('click', function () {
    const $el = $(this);
    const id = $el.data('camp-id');
    if (!id) return;
    navigator.clipboard.writeText(id).then(() => {
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
  // Tracked from /api/status — drives `<name>.<camp_id>.f2f` rendering
  // in the domains panels.
  // campLabelFromID mirrors identity.CampLabel in Go: new-format camp_ids
  // look like "<64-hex-pub>_<label>", legacy ones are free-form. Split
  // only when the prefix is exactly 64 hex chars; otherwise return the
  // whole id as the label.
  function campLabelFromID(id) {
    if (!id) return '';
    if (id.length > 65 && id[64] === '_' && /^[0-9a-f]{64}$/i.test(id.slice(0, 64))) {
      return id.slice(65);
    }
    return id;
  }
  let currentCampID = '';
  let currentCampLabel = '';
  function campIDOrPlaceholder() {
    return currentCampID || '<camp_id>';
  }
  // campLabelOrPlaceholder picks the DNS-zone-safe label (post-CampLabel
  // split server-side). Falls back to the same picker value as the id
  // placeholder when status hasn't arrived yet — for the legacy case
  // (no '_'), id and label are identical, so the placeholder is fine.
  function campLabelOrPlaceholder() {
    return currentCampLabel || campIDOrPlaceholder();
  }

  // renderIdentity fills the identity panel — with the selected peer's details
  // when a peer:<key> route is active, otherwise our own camp identity.
  function renderIdentity(s) {
    if (!s) return;
    const peer = selectedPeer
      ? (s.peers || []).find(p => !p.self && peerKey(p) === selectedPeer)
      : null;
    if (peer) {
      $('#identity-title').text('— peer');
      $('#identity-name').text(peerKey(peer));
      $('#identity-ip').text(peer.overlay_v4 || '—');
      $('#identity-reflex').text(peer.udp_endpoint || '—');
      $('#identity-pub').text(peer.pub || '—').data('pub', peer.pub || '');
      $('#identity-fp').text(peer.last_rtt_ms ? '· ' + peer.last_rtt_ms + 'ms' : '');
    } else {
      $('#identity-title').text('— identity');
      $('#identity-name').text(s.camp_name || '?');
      $('#identity-ip').text(s.local_ip || '—');
      $('#identity-reflex').text(s.camp_reflex || '—');
      const pub = s.identity_pub || '', fp = s.identity_fp || '';
      $('#identity-pub').text(pub || '—').data('pub', pub);
      $('#identity-fp').text(fp ? '· fp ' + fp : '');
    }
  }

  function applyStatus(s) {
    lastStatus = s;
    if (s.running) {
      setEngineState('running', 'running', '· ' + (s.utun_name || '?'));
      currentCampID = s.camp_id || '';
      currentCampLabel = s.camp_label || s.camp_id || '';
      // Running: show the key:value readout, hide the "no camp" hint.
      renderIdentity(s);
      $('#identity-status').removeClass('hidden');
      $('#identity-none').addClass('hidden');
    } else {
      setEngineState('stopped', 'stopped', '');
      currentCampID = '';
      $('#identity-status').addClass('hidden');
      $('#identity-none').removeClass('hidden');
    }
    // Intercept management is always available — list lives in the browser.
    $interceptInput.prop('disabled', false);
    $btnAdd.prop('disabled', false);

    liveIntercepts = s.intercepts || [];
    livePeers = s.peers || [];
    // Names just became available — redraw the open chat once so authors and
    // the title resolve to nicknames instead of pub prefixes (matters on a
    // hard reload where the chat renders before /api/status lands).
    if (chatNamesPending && chatConv) {
      chatNamesPending = false;
      setChatTitle();
      renderChat(chatMsgs);
    }
    refreshInterceptPeerSelect();
    refreshCallPeerSelect(s.active_peer_pub || '');
    renderIntercepts();

    $('#tx-packets').text(s.tx_packets || 0);
    $('#rx-packets').text(s.rx_packets || 0);
    $('#tx-bytes').text(fmtBytes(s.tx_bytes || 0));
    $('#rx-bytes').text(fmtBytes(s.rx_bytes || 0));

    renderCampHealth(s);
    renderDiagnostics(s);
    renderSidebarTree(s);
    updateStatusBar(s);
  }

  // Sidebar tree. Rebuilt from /api/status every tick — cheap because
  // the categories are tiny lists. Per-category collapsed-state lives
  // in localStorage so it survives re-renders.
  const SIDEBAR_TREE_KEY = 'f2f:sidebar-collapsed';
  function loadCollapsed() {
    try { return new Set(JSON.parse(localStorage.getItem(SIDEBAR_TREE_KEY) || '[]')); }
    catch (_) { return new Set(); }
  }
  function saveCollapsed(set) {
    try { localStorage.setItem(SIDEBAR_TREE_KEY, JSON.stringify([...set])); } catch (_) {}
  }
  let collapsedCats = loadCollapsed();

  // Channel sub-tree collapse — independent of category collapse, keyed by the
  // channel's name path ("dev", "dev/backend") so it survives re-renders.
  const CHAN_COLLAPSED_KEY = 'f2f:channels-collapsed';
  function loadCollapsedChans() {
    try { return new Set(JSON.parse(localStorage.getItem(CHAN_COLLAPSED_KEY) || '[]')); }
    catch (_) { return new Set(); }
  }
  let collapsedChans = loadCollapsedChans();
  function toggleChanPath(path) {
    if (!path) return;
    if (collapsedChans.has(path)) collapsedChans.delete(path);
    else collapsedChans.add(path);
    try { localStorage.setItem(CHAN_COLLAPSED_KEY, JSON.stringify([...collapsedChans])); } catch (_) {}
    if (lastStatus) renderSidebarTree(lastStatus);
  }

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // category(key, label, count, body) builds <div.ax-tree-category>
  // + <div.ax-tree-children>. key uniquely identifies the category in
  // localStorage so collapse state is stable across reloads.
  // catIcons maps a category key → its bootstrap-icons glyph (rendered next to
  // the label). Keep in sync with the category() calls below.
  const catIcons = {
    peers:    'bi-people-fill',
    shells:   'bi-terminal-fill',
    desktops: 'bi-display-fill',
    messages: 'bi-chat-dots-fill',
    drop:     'bi-folder-fill',
    domains:  'bi-globe2',
    tunnel:   'bi-hdd-network-fill',
    oidc:     'bi-person-badge-fill',
    secrets:  'bi-key-fill',
    policies: 'bi-shield-lock-fill',
    apps:     'bi-grid-3x3-gap-fill',
  };

  function category(key, label, count, body) {
    const collapsed = collapsedCats.has(key) ? ' collapsed' : '';
    const badge = count != null
      ? `<span class="ax-tree-badge">${count}</span>`
      : '';
    const icon = catIcons[key]
      ? `<i class="bi ${catIcons[key]} ax-tree-cat-icon"></i>`
      : '';
    return (
      `<div class="ax-tree-category${collapsed}" data-cat="${esc(key)}">`
        + `<span class="ax-tree-caret">▾</span>`
        + icon
        + `<span class="ax-tree-label">${esc(label)}</span>`
        + badge
      + `</div>`
      + `<div class="ax-tree-children" data-cat-children="${esc(key)}">${body}</div>`
    );
  }

  function row(state, label, extra, url, route, tab, removePeer) {
    let attrs = url ? ` data-url="${esc(url)}"` : '';
    if (route) attrs += ` data-route="${esc(route)}"`;
    if (tab) attrs += ` data-tab="${esc(tab)}"`;
    // state === null → render without a status dot (e.g. chat rows).
    const dot = state === null ? '' : `<span class="ax-tree-dot"></span>`;
    // removePeer (a pub) → a forget button at the end of the row. Used for
    // offline peer ghosts; the click handler below issues the DELETE.
    const rm = removePeer
      ? `<button class="ax-tree-remove" data-remove-peer="${esc(removePeer)}" title="forget peer">×</button>`
      : '';
    return `<div class="ax-tree-row ${state || ''}"${attrs} title="${esc(url || extra || label)}">`
      + dot
      + `<span class="ax-tree-label">${esc(label)}</span>`
      + (extra ? `<span class="ax-tree-badge">${esc(extra)}</span>` : '')
      + rm
      + `</div>`;
  }

  function empty(text) { return `<div class="ax-tree-empty">${esc(text)}</div>`; }

  // peerKey is a peer's stable display id (name, else a pub prefix). Used both
  // for the sidebar label and the peer:<key> route.
  function peerKey(p) { return (p && (p.name || (p.pub || '').slice(0, 12))) || '?'; }

  // peerDot maps a peer to a status-dot class — the SAME vocabulary the main
  // window's peers table uses, so sidebar and window colours match:
  //   self→accent, paired→green, half_paired→orange, in_camp(no pair)→red, else→grey.
  function peerDot(p) {
    if (!p) return 'offline';
    if (p.self) return 'self';
    if (p.paired) return 'reachable';
    if (p.half_paired) return 'degraded';
    if (p.in_camp) return 'unreachable';
    return 'offline';
  }

  // Bottom IDE-style status bar — engine state, camp, peer counts, I/O.
  function updateStatusBar(s) {
    const running = !!(s && s.running);
    $('#ax-statusbar').toggleClass('running', running);
    const label = (s && (s.camp_label || (s.camp_id || '').split('_').pop())) || '';
    const campID = (s && s.camp_id) || '';
    $('#status-camp')
      .text(running && label ? 'camp ' + label : '—')
      .data('camp-id', running ? campID : '')
      .attr('title', running && campID ? 'click to copy camp id' : '')
      .toggleClass('clickable', !!(running && campID));
    const peers = (s && s.peers) || [];
    const known = peers.length; // includes self, matching the sidebar count
    const online = peers.filter(p => p && (p.self || p.in_camp)).length;
    $('#status-peers').text(online + '/' + known + ' peers');
    $('#status-io').text('↑ ' + fmtBytes((s && s.tx_bytes) || 0) + '  ↓ ' + fmtBytes((s && s.rx_bytes) || 0));
    $('#status-fp').text((s && s.identity_fp) ? 'fp ' + s.identity_fp : '');
  }

  function renderSidebarTree(s) {
    const $tree = $('#ax-tree');
    if (!$tree.length) return;

    // camps — replaces the old "identity" category. The currently
    // active camp gets the online dot; the rest are offline rows.
    // The camp's display name is the label suffix after the last "_"
    // in its ID — that's what users actually call the camp ("xyz",
    // "test1"); KnownCamp.name is the local user's nickname inside
    // that camp, which we surface as a sub-line.
    // peers — flat list. Each row: dot + name + ip/rtt meta.
    const peers = (s && s.peers) || [];
    function peerLabel(p) {
      return peerKey(p) + (p.self ? ' (you)' : '');
    }
    // "add peer" (invite) sits at the top; peers follow.
    let peersBody = addRow('add peer', 'invite');
    if (!peers.length) {
      peersBody += empty('no peers');
    } else {
      for (const p of peers) {
        const state = peerDot(p);
        const ip = p.overlay_v4 || '';
        const rtt = (typeof p.last_rtt_ms === 'number' && p.last_rtt_ms > 0)
          ? `${p.last_rtt_ms}ms` : '';
        const meta = [ip, rtt].filter(Boolean).join(' · ');
        // Offline ghosts (no pairing, not in the camp roster) carry a
        // forget button — the camp only re-sends active peers, so removing
        // one of these sticks. Live peers have no button (would reappear).
        const removable = (!p.self && state === 'offline' && p.pub) ? p.pub : null;
        // clicking a peer opens the (hidden) camp tab with this peer's details
        peersBody += row(state, peerLabel(p), meta, null, 'peer:' + peerKey(p), null, removable);
      }
    }

    // Flat aggregations across all peers: each (domain/port/file)
    // gets a row, with the owning peer's name as the meta column.
    const allDomains = [];
    const allPorts = [];
    const allFiles = [];
    for (const p of peers) {
      const owner = peerLabel(p);
      const state = peerDot(p);
      (p.domains || []).forEach(d => allDomains.push({ d, owner, state }));
      (p.firewall || []).filter(x => x.enabled).forEach(x => allPorts.push({ x, owner, self: !!p.self }));
      (p.files || []).forEach(f => allFiles.push({ f, owner, self: !!p.self }));
    }

    // Clicking a domain opens its live page (as before). The dns tab is
    // reached via the "+ add domain" row below the list.
    const zone = (s && (s.camp_label || (s.camp_id || '').split('_').pop())) || '';
    const domainsBody = allDomains.length
      ? allDomains.map(({ d, owner, state }) => {
          const url = (zone && !d.name.includes('*')) ? `https://${d.name}.${zone}.f2f` : '';
          return row(state, d.name, owner, url);
        }).join('')
      : empty('no domains');

    const portsBody = allPorts.length
      ? allPorts.map(({ x, owner }) =>
          row('', `:${x.port} ${x.protocol || 'tcp'}`, owner, null, 'tunnel:p:' + x.port)).join('')
      : empty('no ports');

    // drop is split into two sections: files available from peers and
    // files we share ourselves. Any file row opens the drop tab.
    const peerFilesBody = allFiles.filter(x => !x.self).length
      ? allFiles.filter(x => !x.self).map(({ f, owner }) =>
          row('', f.name, `${owner} · ${fmtBytes(f.size || 0)}`, null, 'drop:' + f.name)).join('')
      : empty('none');
    const myFilesBody = allFiles.filter(x => x.self).length
      ? allFiles.filter(x => x.self).map(({ f }) =>
          row('', f.name, fmtBytes(f.size || 0), null, 'drop:' + f.name)).join('')
      : empty('none');

    // calls — group calls hosted in the camp. Owner is the peer
    // whose tunnel IP matches sfu_host (self if it's our LocalCall);
    // children list participant names so the user sees who's in.
    const calls = (s && s.calls) || [];
    function peerNameByIP(ip) {
      if (!ip) return '';
      const p = peers.find(p => p.overlay_v4 === ip);
      return p ? (p.name || p.pub.slice(0, 12)) + (p.self ? ' (you)' : '') : ip;
    }
    // MEET — joinable/active calls: live group calls (from status) plus
    // our current p2p call (from the CallManager). Routable + highlightable
    // like chats, so the active call is marked and opens the call window.
    const activeCall = (window.f2fCall && window.f2fCall.active) || null;
    // The SFU call we're currently in is shown as the active row below, so skip
    // its duplicate from the status list.
    const myGroupHost = (activeCall && activeCall.kind === 'group' && window.f2fGroup) ? window.f2fGroup.sfuHost : '';
    let meetRows = '';
    function channelNameById(cid) {
      if (!cid) return '';
      const ch = chatChannels.find(ch => ch.id === cid);
      return ch ? ch.name : '';
    }
    for (const c of calls) {
      if (myGroupHost && c.sfu_host === myGroupHost) continue;
      // A channel-bound call shows its channel name; fall back to the host
      // peer's name for unbound calls.
      const chName = channelNameById(c.channel);
      const title = chName ? '# ' + chName : (peerNameByIP(c.sfu_host) || 'group');
      const id = c.call_id || c.sfu_host || title;
      const n = (c.participants || []).length;
      meetRows += row('online', title, n + ' in · group', null, 'call:group:' + id);
    }
    // Our current call (p2p or group) — routable + highlightable like chats.
    // No state dot: the in-call pulse pip already marks it.
    if (activeCall && activeCall.kind === 'dm') {
      meetRows += row(null, activeCall.title, 'p2p', null, 'call:dm:' + (activeCall.pub || activeCall.id));
    } else if (activeCall && activeCall.kind === 'group') {
      meetRows += row(null, '# ' + activeCall.title, 'group', null, 'call:group:' + activeCall.id);
    }

    // DIRECT = every peer except ourselves; one row each, routed to a DM by
    // pub (the backend keys conversations by pub).
    const directs = peers.filter(p => !p.self && p.pub);
    const directsBody = directs.length
      ? directs.map(p => {
          const name = p.name || (p.pub || '').slice(0, 12);
          // The active dm call stores the peer's pub separately (id is the
          // display label) — match on pub.
          const inCall = activeCall && activeCall.kind === 'dm' && activeCall.pub === p.pub;
          const tags = [];
          if (inCall) tags.push('● in call');
          if (chatUnread[p.pub]) tags.push(`${chatUnread[p.pub]} new`);
          // A DM is a channel too → same hover notes affordance as a channel.
          const note = `<button class="ax-tree-note" data-note="${esc(p.pub)}" title="open notes" aria-label="notes"><i class="bi bi-journal-text"></i></button>`;
          const meta = tags.join(' · ');
          return `<div class="ax-tree-row" data-route="${esc(convRoute(p.pub))}" title="${esc(name)}">`
            + `<span class="ax-tree-label">${esc(name)}</span>`
            + (meta ? `<span class="ax-tree-badge">${esc(meta)}</span>` : '')
            + note
            + `</div>`;
        }).join('')
      : empty('no peers');

    // CHANNELS = the rooms we belong to (from /api/chat/channels). A name may
    // carry a "/" path ("dev/backend") which folds into a tree: each segment
    // nests under the channel — or a virtual folder — named by the prefix
    // above it. general (the camp-wide room) is pinned to the top, outside
    // the tree. Only unread / live-call markers ride in the meta column —
    // the member count was noise.
    function channelMeta(ch) {
      const tags = [];
      if (chatUnread[ch.id]) tags.push(`${chatUnread[ch.id]} new`);
      // Our group call is bound to a channel id — surface it on the row.
      if (activeCall && activeCall.kind === 'group' && activeCall.id === ch.id) tags.push('● live');
      return tags.join(' · ');
    }
    function treeIndent(depth) {
      return depth > 0 ? ` style="padding-left:${5 + depth * 13}px"` : '';
    }
    // A node with children opens with a caret that toggles its sub-tree; a
    // leaf gets a same-width spacer so labels line up. The caret is a SEPARATE
    // control from the row body — clicking the channel name still opens it,
    // only the caret collapses. path identifies the node for persisted state.
    function nodeCaret(path, hasKids) {
      if (!hasKids) return `<span class="ax-chan-spacer"></span>`;
      return `<span class="ax-tree-caret ax-chan-caret" data-chan-path="${esc(path)}">▾</span>`;
    }
    function channelRow(ch, depth, path, hasKids, collapsed) {
      const leaf = ((ch.name || ch.id).split('/').pop()) || ch.id;
      const meta = channelMeta(ch);
      // A notes affordance revealed on row hover (Zed-style) — opens the
      // channel's shared doc in the main pane without opening the chat.
      const note = `<button class="ax-tree-note" data-note="${esc(ch.id)}" title="open notes" aria-label="notes"><i class="bi bi-journal-text"></i></button>`;
      return `<div class="ax-tree-row${collapsed ? ' collapsed' : ''}" data-route="${esc(convRoute(ch.id))}"${treeIndent(depth)} title="${esc(ch.name || ch.id)}">`
        + nodeCaret(path, hasKids)
        + `<span class="ax-tree-label"># ${esc(leaf)}</span>`
        + (meta ? `<span class="ax-tree-badge">${esc(meta)}</span>` : '')
        + note
        + `</div>`;
    }
    function folderRow(seg, depth, path, collapsed) {
      // A path prefix with no channel of its own — clicking it (or its caret)
      // just toggles the sub-tree; it routes nowhere.
      return `<div class="ax-tree-row ax-tree-folder${collapsed ? ' collapsed' : ''}" data-chan-path="${esc(path)}"${treeIndent(depth)} title="${esc(seg)}">`
        + nodeCaret(path, true)
        + `<span class="ax-tree-label">${esc(seg)}/</span></div>`;
    }
    function buildChannelTree(chans) {
      const root = { kids: {}, order: [] };
      for (const ch of chans) {
        const parts = ((ch.name || ch.id.split('/').pop()) || '').split('/').filter(Boolean);
        let node = root;
        parts.forEach((seg, i) => {
          if (!node.kids[seg]) { node.kids[seg] = { kids: {}, order: [], ch: null }; node.order.push(seg); }
          node = node.kids[seg];
          if (i === parts.length - 1) node.ch = ch;
        });
      }
      return root;
    }
    function renderChannelTree() {
      const tree = buildChannelTree(chatChannels.filter(c => c.id !== GENERAL_ID));
      let html = '';
      const gen = chatChannels.find(c => c.id === GENERAL_ID);
      if (gen) html += channelRow(gen, 0, '', false, false); // general pinned to the top
      // Each node renders its row, then its children wrapped in a collapsible
      // block (.ax-chan-children, hidden when the row carries .collapsed).
      (function walk(node, depth, prefix) {
        node.order.sort((a, b) => a.localeCompare(b));
        for (const seg of node.order) {
          const kid = node.kids[seg];
          const path = prefix ? prefix + '/' + seg : seg;
          const hasKids = kid.order.length > 0;
          const collapsed = hasKids && collapsedChans.has(path);
          html += kid.ch
            ? channelRow(kid.ch, depth, path, hasKids, collapsed)
            : folderRow(seg, depth, path, collapsed);
          if (hasKids) {
            html += `<div class="ax-chan-children">`;
            walk(kid, depth + 1, path);
            html += `</div>`;
          }
        }
      })(tree, 0, '');
      return html;
    }
    const groupsBody = (chatChannels.length ? renderChannelTree() : empty('no groups'))
      + addRow('+ channel', 'channel:new');

    // intercepts — :port -> peer.
    const intercepts = (s && s.intercepts) || [];
    const interceptsBody = intercepts.length
      ? intercepts.map(i => row('', i.spec, i.peer || '', null, 'tunnel:i:' + i.spec)).join('')
      : empty('none');

    // trusted CAs.
    const trusted = (s && s.trusted_peers) || [];
    const trustedBody = trusted.length
      ? trusted.map(t => {
          const name = t.peer_name || t.common_name || t.fingerprint.slice(0, 12);
          return row(t.installed ? 'online' : 'half', name,
            t.installed ? 'installed' : 'pending', null, 'dns:cert:' + name);
        }).join('')
      : empty('none');

    // All messaging lives under a single "messages" group with three
    // section dividers (channels / direct / active calls) — not
    // separately collapsible. The unread badge on the outer "messages"
    // header sums new messages across both lists so a collapsed
    // sidebar still shows pending traffic.
    const totalUnread = Object.values(chatUnread).reduce((n, c) => n + (c || 0), 0);
    function section(label) {
      return `<div class="ax-tree-section">${esc(label)}</div>`;
    }
    // A manage affordance ("add/remove …") that opens a resource's tab — also
    // the only entry point when the list is empty (nothing else to click).
    function addRow(label, route) {
      return `<div class="ax-tree-row ax-tree-add" data-route="${esc(route)}">`
        + `<span class="ax-tree-label">${esc(label)}</span></div>`;
    }
    const meetsBody = meetRows || empty('no calls');
    const messagingBody =
      section('calls')   + meetsBody
      + section('groups')  + groupsBody
      + section('direct')  + directsBody;

    // tunnel — outbound intercepts + inbound open ports under one group
    // with section dividers (mirrors the app's "tunnel" tab).
    const tunnelBody =
      section('intercepts') + addRow('add/remove intercept', 'tunnel') + interceptsBody
      + section('ports')      + portsBody;

    // shells — peers whose remote shell (services/shell) is open to us.
    // Each row opens a terminal (term.js) over the bus. Populated by the
    // /api/shell/peers poll below.
    const shellList = freshPeers(shellSeen);
    const shellsBody = shellList.length
      ? shellList.map(p => row('online', p.name || (p.pub || '').slice(0, 12), '', null, 'term:' + p.pub)).join('')
      : empty('none');

    // desktops — peers with a reachable VNC server (services/vnc). Each row
    // opens a noVNC viewer (vnc.js) over the bus.
    const vncList = freshPeers(vncSeen);
    const desktopsBody = vncList.length
      ? vncList.map(p => row('online', p.name || (p.pub || '').slice(0, 12), '', null, 'vnc:' + p.pub)).join('')
      : empty('none');

    $tree.html(
      category('peers',     'peers',     peers.length, peersBody)
      + category('shells',    'terminals', shellList.length || null, shellsBody)
      + category('desktops',  'desktops',  vncList.length || null, desktopsBody)
      + category('messages',  'messages',  totalUnread || null, messagingBody)
      + category('drop',      'drop',      allFiles.length,
          section('available') + peerFilesBody
          + section('sharing') + addRow('add/remove file', 'drop') + myFilesBody)
      + category('domains',   'domains',   allDomains.length,
          addRow('add/remove domain', 'dns') + domainsBody
          + section('certificates') + trustedBody)
      + category('tunnel',    'tunnel',    (intercepts.length + allPorts.length) || null, tunnelBody)
      + category('oidc',      'OIDC',      null, empty('coming soon'))
      + category('secrets',   'secrets',   null, empty('coming soon'))
      + category('policies',  'policies',  null, empty('not configured'))
      + category('apps',      'apps',      null, empty('coming soon'))
    );
    highlightActiveRoute();
  }

  // Persist category collapsed state. The handler defined earlier
  // toggles .collapsed; we hook the same click to write the set.
  $('#ax-tree').on('click', '.ax-tree-category', function () {
    const key = $(this).data('cat');
    if (!key) return;
    if ($(this).hasClass('collapsed')) collapsedCats.add(key);
    else collapsedCats.delete(key);
    saveCollapsed(collapsedCats);
  });

  // Sidebar search/filter. Substring match against the visible text of
  // each .ax-tree-row and .ax-tree-category label. A category survives
  // the filter if it OR any of its descendants match — that way users
  // can type a peer name and still see its parent group. Empty query
  // restores everything.
  let sidebarQuery = '';
  function applySidebarFilter() {
    const q = sidebarQuery.trim().toLowerCase();
    const $tree = $('#ax-tree');
    if (!q) {
      $tree.find('.ax-tree-row, .ax-tree-category, .ax-tree-children, .ax-chan-children, .ax-tree-section, .ax-tree-empty')
        .css('display', '');
      return;
    }
    // Hide everything first.
    $tree.find('.ax-tree-row, .ax-tree-category, .ax-tree-children, .ax-chan-children, .ax-tree-section, .ax-tree-empty')
      .css('display', 'none');
    // Leaf rows that match show themselves.
    $tree.find('.ax-tree-row').each(function () {
      if ($(this).text().toLowerCase().includes(q)) {
        $(this).css('display', '');
      }
    });
    // Reveal channel sub-trees that contain a match: deepest-first so the
    // chain of parents up to the top cascades visible. Expand any collapsed
    // node for the search duration (not persisted — the next render restores
    // it from collapsedChans).
    const allChanChildren = $tree.find('.ax-chan-children').toArray().reverse();
    for (const cc of allChanChildren) {
      const $cc = $(cc);
      const hasVisible = $cc.children().filter(function () {
        return $(this).css('display') !== 'none';
      }).length > 0;
      if (hasVisible) {
        $cc.css('display', '');
        $cc.prev('.ax-tree-row').css('display', '').removeClass('collapsed');
      }
    }
    // Walk each .ax-tree-children: if any descendant is visible OR the
    // owning category's label matches, the children block + its
    // category header become visible. Iterate deepest-first so parent
    // visibility cascades up correctly.
    const allChildren = $tree.find('.ax-tree-children').toArray().reverse();
    for (const ch of allChildren) {
      const $ch = $(ch);
      const key = $ch.attr('data-cat-children');
      const $cat = $tree.find('.ax-tree-category[data-cat="' + (key || '').replace(/"/g, '\\"') + '"]');
      const labelMatch = $cat.find('> .ax-tree-label').text().toLowerCase().includes(q);
      const hasVisible = $ch.children().filter(function () {
        return $(this).css('display') !== 'none';
      }).length > 0;
      if (labelMatch || hasVisible) {
        $ch.css('display', '');
        $cat.css('display', '');
        // If the category was collapsed, expand it for the search
        // duration so the match is visible. We don't persist this.
        $cat.removeClass('collapsed');
      }
    }
  }
  $('#ax-sidebar-search').on('input', function () {
    sidebarQuery = $(this).val() || '';
    applySidebarFilter();
  });
  // Esc clears the filter when the input is focused.
  $('#ax-sidebar-search').on('keydown', function (e) {
    if (e.key === 'Escape') {
      $(this).val('').trigger('input').blur();
    }
  });
  // Re-apply filter after every status re-render — without this the
  // 2-second tick wipes search results.
  const _origRenderSidebar = renderSidebarTree;
  renderSidebarTree = function (s) {
    _origRenderSidebar(s);
    if (sidebarQuery) applySidebarFilter();
  };

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
  }

  function renderIntercepts() {
    $list.empty();
    const items = liveIntercepts.map((l) => ({ spec: l.spec, peer: l.peer, live: l }));

    $('#intercept-meta').text(items.length);

    if (items.length === 0) {
      $list.append('<div class="ax-list-empty">no intercepts. add one below.</div>');
      return;
    }

    // Group subdomains under their parent zone: B nests under A when
    // B.spec is a strict subdomain of A.spec (most specific parent wins).
    // On-demand entries (www resolved automatically under a myip.com
    // intercept) and hand-added subdomains both fold in this way.
    const parentOf = (it) => {
      let best = null;
      for (const o of items) {
        if (o === it) continue;
        if (it.spec.endsWith('.' + o.spec) && (!best || o.spec.length > best.spec.length)) best = o;
      }
      return best;
    };
    const childrenOf = new Map();
    const tops = [];
    items.forEach((it) => {
      const p = parentOf(it);
      if (p) {
        if (!childrenOf.has(p.spec)) childrenOf.set(p.spec, []);
        childrenOf.get(p.spec).push(it);
      } else {
        tops.push(it);
      }
    });
    const bySpec = (a, b) => a.spec.localeCompare(b.spec);
    const renderNode = (it, depth) => {
      $list.append(buildInterceptRow(it, depth));
      const kids = childrenOf.get(it.spec);
      if (kids) {
        kids.sort(bySpec);
        kids.forEach((k) => renderNode(k, depth + 1));
      }
    };
    tops.sort(bySpec).forEach((it) => renderNode(it, 0));
  }

  // buildInterceptRow renders one intercept as a collapsible row. depth>0
  // means it's a subdomain nested under a parent zone — indented, with an
  // "auto" pill when we resolved it on demand rather than the user typing it.
  function buildInterceptRow(it, depth) {
    const key = it.spec + '\x00' + it.peer;
    const prefixes = it.live ? (it.live.prefixes || []) : [];
    const parsed = prefixes.map(parsePrefixEntry);
    const v4count = parsed.filter((p) => p.kind === 'v4').length;
    const v6count = parsed.filter((p) => p.kind === 'v6').length;
    const expanded = expandedIntercepts.has(key);

    const $row = $('<div class="ax-intercept">').toggleClass('is-expanded', expanded);
    if (depth > 0) $row.addClass('is-child').css('margin-left', (depth * 16) + 'px');
    const $head = $('<div class="ax-intercept-head">');
    $head.append($('<span class="ax-intercept-caret">').text(expanded ? '▼' : '▶'));
    $head.append($('<span class="ax-intercept-spec">').text(it.spec));
    $head.append($('<span class="ax-pill ax-pill-peer">').text('via ' + it.peer));
    if (it.live && it.live.on_demand) $head.append($('<span class="ax-pill ax-pill-auto">').text('auto'));
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
    return $row;
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
    const sel = $sel[0];
    if (document.activeElement === sel) return;
    const current = $sel.val();
    const others = livePeers.filter((p) => !p.self);
    let html = '<option value="">— peer —</option>';
    others.forEach((p) => {
      html += '<option value="' + p.name + '">' + peerOptionLabel(p) + '</option>';
    });
    if (sel.innerHTML === html) return;
    sel.innerHTML = html;
    if (current && others.some((p) => p.name === current)) $sel.val(current);
  }

  // The engine status button is a read-only indicator now (running /
  // stopped). Starting, stopping and switching camps are CLI-only.

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
    $campIDMeta.text(campLabelFromID(data.camp_id || ''));
    const hasOthers = peers.some((p) => !p.self);
    if (!hasOthers) {
      $campStatus.text('waiting for someone to join').show();
    } else {
      $campStatus.hide();
    }
    $campBody.empty();
    for (const p of peers) {
      const endpoint = p.udp_endpoint || (p.public_ip ? p.public_ip + (p.udp_port ? ':' + p.udp_port : '') : '—');
      // Paired      = bidirectional crypto-attested via pair_req + pair_res.
      // HalfPaired  = exactly one direction of the pair handshake is fresh.
      // InCamp      = camp server sees peer's announce.
      // Color matrix:
      //   self                                  → yellow (you)
      //   paired                                → green  (bidirectional pair-handshake, RTT measured)
      //   half_paired                           → orange (one-way only; we hear them OR they hear us, not both)
      //   in_camp without paired/half_paired    → red    (in camp roster but no crypto signal — old version OR NAT blocked)
      //   neither                               → gray   (not in camp)
      let dotClass, dotTitle;
      if (p.self) {
        dotClass = 'self';
        dotTitle = 'you';
      } else if (p.paired) {
        dotClass = 'reachable';
        dotTitle = 'paired — bidirectional crypto-attested' + (p.rtt_ms ? ' — rtt ' + p.rtt_ms + 'ms' : '');
      } else if (p.half_paired) {
        dotClass = 'degraded';
        dotTitle = 'half-paired — one direction only (NAT-rebind or asymmetric path)';
      } else if (p.in_camp) {
        dotClass = 'unreachable';
        dotTitle = 'in camp roster but no pair handshake (old version without pair support, or NAT blocking us)';
      } else {
        dotClass = 'offline';
        dotTitle = 'not in camp roster';
      }
      const $row = $('<tr>')
        .addClass(p.self ? 'is-self' : '')
        .addClass(!p.online && !p.self ? 'is-offline' : '')
        .attr('data-pub', p.pub || '');
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
      // Actions: forget an offline ghost (same gate/semantics as the
      // sidebar). Live peers get no button — they'd reappear next poll.
      const $actions = $('<td>').addClass('ax-peers-actions');
      if (!p.self && dotClass === 'offline' && p.pub) {
        $actions.append(
          $('<button class="ax-peers-remove" title="forget peer">×</button>')
            .attr('data-remove-peer', p.pub),
        );
      }
      // overlay address cell: pub-derived per-camp v6. v4 is no longer
      // a peer-identifying address (every mac uses the same localV4Alias
      // on its own utun, so peer-to-peer addressing is v6 only).
      const $tipCell = $('<td>').text(p.overlay_v4 || '—');
      $row.append(
        $('<td>').append($('<span>').addClass('ax-dot ' + dotClass).attr('title', dotTitle)),
        $name,
        $tipCell,
        $('<td>').text(endpoint || '—'),
        $rtt,
        $('<td>').addClass('muted').text(p.joined_at ? humanAgo(p.joined_at) : '—'),
        $actions,
      );
      $campBody.append($row);
    }
    $campTable.removeClass('hidden');
  }

  // Forget-peer button in the main-window peers table (offline ghosts).
  $campBody.on('click', '.ax-peers-remove[data-remove-peer]', function () {
    const pub = $(this).attr('data-remove-peer');
    if (!pub) return;
    $.ajax({ url: '/api/peers/' + encodeURIComponent(pub), method: 'DELETE' })
      .always(() => { if (typeof refreshStatus === 'function') refreshStatus(); });
  });

  // Meet-tab peer selector: set the engine's active peer (the one
  // signalling/HTTP-forward in /api/signal/outbox goes to). Reflected
  // on every status refresh in refreshCallPeerSelect.
  $('#ax-call-peer').on('change', function () {
    const name = $(this).val();
    const peer = livePeers.find((p) => !p.self && p.name === name);
    const pub = peer ? (peer.pub || '') : '';
    $.ajax({
      url: '/api/peers/active',
      method: 'POST',
      contentType: 'application/json',
      data: JSON.stringify({ pub }),
    })
      .done(refreshStatus)
      .fail((xhr) => alert('Set active failed: ' + errorOf(xhr)));
  });

  // refreshCallPeerSelect mirrors live camp peers into the meet-tab
  // dropdown, preserving the currently-active selection.
  function refreshCallPeerSelect(activePub) {
    const $sel = $('#ax-call-peer');
    const sel = $sel[0];
    if (document.activeElement === sel) return;
    const others = livePeers.filter((p) => !p.self);
    let html = '<option value="">— peer —</option>';
    others.forEach((p) => {
      let label = p.name;
      if (p.online === false) label += ' (offline)';
      else if (!p.reachable) label += ' (unreachable)';
      html += '<option value="' + p.name + '">' + label + '</option>';
    });
    if (sel.innerHTML === html) return;
    sel.innerHTML = html;
    const activePeer = others.find((p) => p.pub && p.pub === activePub);
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
      const isWildcard = (d.name || '').startsWith('*.');
      const fqdn = d.name + '.' + campLabelOrPlaceholder() + '.f2f';
      const $row = $('<div class="ax-intercept">');
      const $head = $('<div class="ax-intercept-head" style="cursor:default">');
      $head.append($('<span class="ax-intercept-caret">').text(' '));
      if (!isWildcard) $head.append(makeHealthDot(d));
      const $link = isWildcard
        ? $('<span class="ax-intercept-spec">').text(fqdn)
        : $('<a class="ax-intercept-spec ax-domain-link" target="_blank">')
            .attr('href', 'https://' + fqdn + '/')
            .text(fqdn);
      $head.append($link);
      if (isWildcard) {
        $head.append($('<span class="ax-pill ax-pill-fp">').text('wildcard'));
      }
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
    // Allowed forms:
    //   - simple:  gitea
    //   - nested:  gitea.mini
    //   - wildcard catch-all: *.mini
    const isWildcard = name.startsWith('*.');
    const rest = isWildcard ? name.slice(2) : name;
    if (
      rest.length === 0 ||
      !/^[a-z0-9-]+(\.[a-z0-9-]+)*$/.test(rest)
    ) {
      alert('Name must be like "gitea", "gitea.mini", or "*.mini".');
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
      ds.forEach((d) => rows.push({ peer: p.name, peerTunnel: p.overlay_v4 || '', online: p.online !== false, ...d }));
    });
    $('#known-domains-meta').text(rows.length);
    if (rows.length === 0) {
      $list.append('<div class="ax-list-empty">no domains published by any peer yet.</div>');
      return;
    }
    const campLabel = campLabelOrPlaceholder();
    rows.forEach((r) => {
      const fqdn = r.name + '.' + campLabel + '.f2f';
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
        peer: p.name, peerTunnel: p.overlay_v4 || '', online: p.online !== false, ...fp,
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

  // ---- my CA (DNS tab) ----
  function refreshMyCA() {
    $.getJSON('/api/my-ca', (data) => {
      if (!data || !data.common_name) {
        $('#my-ca-info').text('not running');
        return;
      }
      $('#my-ca-info').html(
        '<strong>' + $('<span>').text(data.common_name).html() + '</strong>' +
        ' <span class="muted">fp ' + (data.fingerprint || '—') + '</span>'
      );
    }).fail(() => { $('#my-ca-info').text('—'); });
  }

  // ---- trusted peer CAs (DNS tab) ----
  function refreshTrustedPeers() {
    $.getJSON('/api/trusted-peers', (list) => {
      const rows = Array.isArray(list) ? list : [];
      rows.sort((a, b) => (a.peer_name || '').localeCompare(b.peer_name || ''));
      $('#trusted-peers-meta').text(rows.length);
      const $list = $('#trusted-peers-list');
      $list.empty();
      if (rows.length === 0) {
        $list.append('<div class="ax-list-empty">no peer CAs discovered yet · they appear automatically as peers join. click install to trust a peer (asks your macOS password once).</div>');
        return;
      }
      rows.forEach((r) => {
        const $row = $('<div class="ax-intercept">');
        const $head = $('<div class="ax-intercept-head" style="cursor:default">');
        $head.append($('<span class="ax-intercept-caret">').text(' '));
        $head.append($('<span class="ax-intercept-spec">').text(r.peer_name || '?'));
        if (r.common_name) {
          $head.append($('<span class="ax-pill ax-pill-peer">').text(r.common_name));
        }
        $head.append($('<span class="ax-pill ax-pill-fp">').text(r.fingerprint || ''));
        if (r.installed) {
          const when = r.installed_at ? humanAgo(r.installed_at * 1000) : '';
          $head.append($('<span class="ax-pill ax-pill-active" style="background:#86b86b;color:#000">').text('installed'));
          if (when) $head.append($('<span class="ax-intercept-meta">').text(when));
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
        } else {
          $head.append($('<span class="ax-intercept-meta">').text('not trusted'));
          const $install = $('<button class="ax-btn ax-btn-primary" style="padding:2px 10px">').text('install');
          $install.on('click', () => {
            $install.prop('disabled', true).text('installing…');
            $.ajax({
              url: '/api/trusted-peers/' + encodeURIComponent(r.fingerprint) + '/install',
              method: 'POST',
            })
              .done(refreshTrustedPeers)
              .fail((xhr) => {
                alert('Install failed: ' + errorOf(xhr));
                $install.prop('disabled', false).text('install');
              });
          });
          $head.append($install);
        }
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
      files.forEach((f) => rows.push({ peer: p.name, peerTunnel: p.overlay_v4 || '', ...f }));
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
          // BT peer endpoint: prefer overlay v6 ([fd…]:6881), fall back
          // to legacy v4 if the peer hasn't announced a pub yet. The
          // local BT client listens on v6 (per camp utun alias) and on
          // v4 (legacy tunnel_ip), so either form lands on the peer.
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
        // Magnet added, anacrolix hasn't fetched the .torrent yet. If
        // the source peer is offline there's nobody to fetch from —
        // say so instead of a perpetual "fetching…". It resumes by
        // itself when the source comes back online.
        if (d.source_online === false) {
          $head.append($('<span class="ax-pill ax-pill-pending">').text('source offline'));
        } else {
          $head.append($('<span class="ax-pill ax-pill-pending">').text('fetching metadata…'));
        }
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
  refreshStatus();
  refreshCampPeers();
  refreshMyDomains();
  refreshMyCA();
  refreshTrustedPeers();
  refreshMyFiles();
  refreshDownloads();
  refreshFirewall();
  function refreshShellPeers() {
    $.getJSON('/api/shell/peers', (list) => {
      markSeen(shellSeen, list); // sticky: never clears, only refreshes last-seen
      if (lastStatus) renderSidebarTree(lastStatus);
    }).fail(() => {});
  }
  function refreshVncPeers() {
    $.getJSON('/api/vnc/peers', (list) => {
      markSeen(vncSeen, list);
      if (lastStatus) renderSidebarTree(lastStatus);
    }).fail(() => {});
  }
  refreshShellPeers();
  refreshVncPeers();
  setInterval(refreshShellPeers, 5000);
  setInterval(refreshVncPeers, 5000);
  setInterval(refreshStatus, 3000);
  setInterval(refreshCampPeers, 3000);
  setInterval(refreshMyDomains, 5000);
  setInterval(refreshMyCA, 5000);
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
