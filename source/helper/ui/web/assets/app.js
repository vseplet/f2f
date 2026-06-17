$(function () {
  // Selected-peer state for the identity panel + last status sample. Declared
  // first because applyRoute() runs during init, before later code.
  let selectedPeer = '';   // peerKey of the peer whose details fill the identity panel
  let lastStatus = null;   // last /api/status sample, for re-rendering on route change
  let livePeers = [];      // last seen camp peers from /api/status (declared early:
                           // nameForPub/applyRoute reference it during init)
  // Profile state — declared up here because applyRoute() reads profileRequired
  // during init (a later `let` would be in the temporal dead zone → crash).
  let profile = null;          // {first, last, has_passkey} or null
  let profileChecked = false;  // /api/profile fetched this session
  let profileRequired = false; // router pins the user to the profile page until saved
  let onboardPkDone = false;   // passkey created during the onboarding page

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

  // Notifications arrive over the unified chat stream (type:'notif') — see
  // openChatStream — so there's no separate EventSource here.
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
  const GENERAL_ID = 'general'; // camp-wide channel everyone is in (no leave)
  // URL routes: a conversation is "channel:<key>" — there's no separate "dm:"
  // because a DM is just the degenerate channel. The key's SHAPE tells them
  // apart: a bare peer pub is a DM, while "general" or an "<owner>/<name>" id
  // is a room. Notes are "note:<id>". general's ownerless "*/general" id is
  // shown in the URL as the clean "general" alias ("*/" is plumbing).
  function convKey(id) { return id === GENERAL_ID ? 'general' : id; }
  function convRoute(id) { return 'channel:' + convKey(id); }
  function noteRoute(id) { return 'note:' + convKey(id); }
  // convKind infers a conversation's kind from its key: a channel key is the
  // channel block bid ("general" or "<fp16>-<rand>"), a DM key is the peer's
  // pub (64 hex, no dash). So a dash (or the general id) ⇒ channel.
  function convKind(key) { return (key === GENERAL_ID || key.includes('-')) ? 'channel' : 'dm'; }
  let chatChannels = [];      // /api/channels — channels we belong to
  let chatConv = null;        // { kind:'dm'|'channel', key } currently open
  let replyTarget = null;     // message the next send will quote, or null
  let editTarget = null;      // { id } of the message the next send will edit, or null
  let threadRoot = null;      // id of the message whose thread panel is open, or null
  let querySchemaLoaded = false;  // SQL console: schema fetched on first open
  const DEFAULT_QUERY =
    "SELECT scope, type, substr(author,1,8) AS author, seq, lamport\n  FROM frames\n ORDER BY lamport DESC\n LIMIT 50;";
  let chatMsgs = [];          // messages of the open conversation (cached for redraw)
  let chatPending = [];       // staged (not yet sent) attachment File objects
  let chatPendingUrls = [];   // object URLs for staged image thumbnails, revoked on clear
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
  // msgFromName is the sender's display name: the profile full name the server
  // resolved (from_name), else the peer/roster name.
  function msgFromName(m) { return (m && m.from_name) || nameForPub(m && m.from); }
  function authorOf(m) {
    return (m && (m.mine || (lastStatus && m.from === lastStatus.identity_pub))) ? 'you' : msgFromName(m);
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
  // replies>0 adds a "N replies" thread affordance under the bubble (main
  // timeline only); inThread=true renders inside the thread panel (no footer,
  // no thread action).
  function msgRow(m, grouped, replies, inThread) {
    if (m.type && m.type !== 'text') {
      const who = msgFromName(m);
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
    const author = mine ? 'you' : msgFromName(m);
    const edited = m.edited ? `<span class="ax-msg-edited">edited</span>` : '';
    const head = (grouped && !m.edited) ? '' :
      `<div class="ax-msg-head">`
        + `<span class="ax-msg-author">${esc(author)}</span>`
        + `<span class="ax-msg-time">${esc(hhmm(m.ts))}</span>`
        + edited
      + `</div>`;
    // An attachment and its caption share ONE bubble so they read as a single
    // message: a photo/clip previews above the caption; a file/torrent shows a
    // doc card above the caption. Plain text is just the text bubble.
    const mime = (m.file && m.file.mime) || '';
    const previewable = mime.indexOf('image/') === 0 || mime.indexOf('video/') === 0;
    const caption = m.body ? `<div class="ax-msg-caption">${richBody(m.body)}</div>` : '';
    let body;
    if (m.file && previewable) {
      body = `<div class="ax-msg-media">${attachHtml(m.file)}${caption}</div>`;
    } else if (m.file) {
      body = `<div class="ax-msg-filecard">${attachHtml(m.file)}${caption}</div>`;
    } else {
      body = `<div class="ax-msg-text">${richBody(m.body)}</div>`;
    }
    // Action row under the bubble, revealed on hover (aligned to the message's
    // side by the parent flex). "reply" = inline quote; "thread" opens the
    // thread panel (only on a root — a message not itself in a thread, and not
    // already shown inside the panel). "edit" only on my own text messages.
    const acts = `<div class="ax-msg-acts">`
      + `<button class="ax-msg-act" data-act="reply" title="reply"><i class="bi bi-reply-fill"></i> reply</button>`
      + ((!m.thread && !inThread) ? `<button class="ax-msg-act" data-act="thread" title="reply in thread"><i class="bi bi-chat-square-text-fill"></i> thread</button>` : '')
      + ((mine && m.body) ? `<button class="ax-msg-act" data-act="edit" title="edit"><i class="bi bi-pencil-fill"></i> edit</button>` : '')
      + `</div>`;
    // "N replies" affordance under a root message in the main timeline.
    const threadFooter = (replies > 0 && !inThread)
      ? `<button class="ax-msg-thread-open" data-root="${esc(m.id)}"><i class="bi bi-chat-square-text-fill"></i> ${replies} ${replies === 1 ? 'reply' : 'replies'}</button>`
      : '';
    return `<div class="ax-msg${mine ? ' is-mine' : ''}" data-id="${esc(m.id)}" data-author="${esc(m.from)}">`
      + head
      + quoteHtml(m.reply_to)
      + body
      + threadFooter
      + acts
      + `</div>`;
  }

  // Object URLs minted for the currently-rendered attachments. Revoked and
  // rebuilt on every full chat redraw so they don't leak across conversations.
  let chatBlobUrls = [];
  let noteBlobUrls = [];
  // attachUrl pushes minted object URLs into whichever bucket is active so the
  // right view's redraw can revoke them. Chat sets it in renderChat, notes in
  // renderNoteBlocks — both views reuse attachHtml.
  let attachBucket = chatBlobUrls;
  function attachUrl(f) {
    // Decode the base64 the backend sent (Go marshals []byte as std base64)
    // into a Blob and hand back an object URL. Unlike a data: URL, a blob:
    // URL can be opened in a new tab, downloaded, and plays reliably in
    // <video> — data: URLs are blocked for top-frame navigation in Chrome.
    const bin = atob(f.data);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    const url = URL.createObjectURL(new Blob([bytes], { type: f.mime || 'application/octet-stream' }));
    attachBucket.push(url);
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
      // Same doc card as an inline file, only a transfer icon — the size and
      // the live status/download action ride in the subtitle.
      return `<div class="ax-msg-doc ax-msg-torrent" data-infohash="${esc(f.info_hash)}" data-magnet="${esc(f.magnet || '')}">`
        + `<span class="ax-msg-doc-ic"><i class="bi bi-cloud-arrow-down-fill"></i></span>`
        + `<span class="ax-msg-doc-meta">`
          + `<span class="ax-msg-doc-name">${tn}</span>`
          + `<span class="ax-msg-doc-sub"><span class="ax-msg-doc-size">${esc(fmtBytes(f.size || 0))}</span><span class="ax-msg-torrent-status"></span></span>`
        + `</span>`
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
    return `<a class="ax-msg-doc" href="${url}" download="${name}" title="${name}">`
      + `<span class="ax-msg-doc-ic"><i class="bi bi-file-earmark-fill"></i></span>`
      + `<span class="ax-msg-doc-meta">`
        + `<span class="ax-msg-doc-name">${name}</span>`
        + `<span class="ax-msg-doc-sub">${esc(fmtBytes(f.size || 0))} · download</span>`
      + `</span></a>`;
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
    attachBucket = chatBlobUrls;
    const all = msgs || [];
    const edits = editsByRoot(all);
    // Known message ids (roots/normal); thread replies pointing at a known root
    // live in the thread panel, not the main timeline. Count replies per root.
    const ids = new Set(); for (const x of all) if (!x.edit_id) ids.add(x.id);
    const replyCount = {};
    for (const x of all) {
      if (x.edit_id) continue;
      if (x.thread && ids.has(x.thread)) replyCount[x.thread] = (replyCount[x.thread] || 0) + 1;
    }
    let html = '';
    for (const raw of all) {
      if (raw.edit_id) continue;                       // edits patch their original
      if (raw.thread && ids.has(raw.thread)) continue; // a thread reply → panel only
      const m = applyEdit(raw, edits);
      // Header grouping (collapsing consecutive same-author lines) is disabled
      // for now — every message shows its own author/time header.
      html += msgRow(m, false, replyCount[m.id] || 0, false);
    }
    const $m = $('#chat-messages');
    $m.html(html || '<div class="ax-msg-system">no messages yet</div>');
    $m.scrollTop($m[0].scrollHeight);
    if (window.f2fRich) f2fRich.renderDiagrams($m[0]); // turn ```mermaid into SVG
    mountSandboxes($m[0]); // ```sandbox fences → isolated iframes
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
    $.getJSON('/api/messages?kind=' + encodeURIComponent(chatConv.kind)
      + '&key=' + encodeURIComponent(chatConv.key), (msgs) => {
      if (!chatConv) return;
      chatMsgs = msgs || [];
      renderChat(chatMsgs);
      setChatTitle();
      // Fill the thread panel once history lands (e.g. a deep-link opened it
      // before messages arrived).
      if (threadRoot && !$('#chat-thread').hasClass('hidden')) renderThread();
    });
  }

  // fetchChannels refreshes the channel list, then rebuilds the sidebar and
  // (if open) the members panel.
  function fetchChannels() {
    $.getJSON('/api/channels', (list) => {
      chatChannels = Array.isArray(list) ? list : [];
      if (lastStatus && typeof renderSidebarTree === 'function') renderSidebarTree(lastStatus);
      if (!$('#chat-members-panel').hasClass('hidden')) renderMembersPanel();
      // Refresh the open notes editor once the channel list arrives — skipped
      // while editing (noteEditingNow) so it never eats in-progress text.
      if (noteConv && !$('#tab-note').hasClass('hidden') && !noteEditingNow()) loadNoteBlocks();
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
  function chatChannelAction(path, verb, body) {
    if (!chatConv || !confirm(verb + ' channel?')) return;
    $.ajax({
      url: path, method: 'POST', contentType: 'application/json',
      data: JSON.stringify(body),
    }).done(() => { location.hash = ''; fetchChannels(); })
      .fail((xhr) => alert(verb + ': ' + errorOf(xhr)));
  }
  $('#chat-members-panel').on('click', '#chat-delete-channel', () => chatChannelAction('/api/channels/delete', 'delete', { bid: chatConv.key }));
  // Leaving a channel = removing ourselves from its member list.
  $('#chat-members-panel').on('click', '#chat-leave-channel', () => chatChannelAction('/api/channels/members', 'leave', { bid: chatConv.key, remove: [lastStatus && lastStatus.identity_pub] }));

  // --- channel notes (the shared doc, opened in the main pane) ---
  //
  // A conversation's notes open as their own main-window view (note:<scope>),
  // not a dropdown — a clean full-pane editor (whitepaper-style). A DM is a
  // channel too, so DMs carry notes as well; the doc is fetched/saved by scope
  // (channel id or peer pub). Edits debounce-save (last-writer-wins).
  let noteConv = null;            // conversation key of the open note, or null
  let noteScope = null;           // resolved db scope ("note:<channelBid>") of the open note
  let noteBlocks = [];            // last loaded blocks for the open note
  const noteSaveTimers = {};      // bid → debounce timer for block edits
  const noteDeleting = {};        // bid → true while a delete+reload is in flight (dedupe)
  // Undo/redo stack for block deletions. Delete is a tombstone version, so undo
  // = write the prior content back (revives the block), redo = re-tombstone.
  // Each entry: {bid, content}. Cleared when switching conversations.
  let noteUndo = [];
  let noteRedo = [];
  let notePage = '';              // bid of the open page within the note ('' = root)
  // The open note's db scope is resolved once per open (via /api/notes/scope,
  // which runs the SAME chanBID messages use — so a DM resolves to the symmetric
  // "note:dm-<hash>" both peers share, not each peer's "note:<other-pub>").
  // Everything operates on the currently-open note, so this returns that scope.
  function noteScopeOf() { return noteScope; }

  // A note is a tree of blocks (Notion-style): text blocks are content, `page`
  // blocks are containers; both nest via the block's parent. The open page shows
  // only its direct children; sub-pages render as links you drill into.
  function pageKids() { return noteBlocks.filter((b) => (b.parent || '') === notePage); }
  function noteBlockOf(bid) { return noteBlocks.find((b) => b.bid === bid); }
  function blockType(b) { return b.type || 'text'; }
  // pageTitle reads a page block's {title} (latest head).
  function pageTitle(b) {
    const head = (b && b.heads && b.heads.length) ? b.heads[b.heads.length - 1] : null;
    return (head && head.content && head.content.title) || 'untitled';
  }
  // notePageRoute builds the hash for a page within a conversation.
  function notePageRoute(conv, page) {
    return 'note:' + convKey(conv) + (page ? ':page:' + page : '');
  }
  // breadcrumb chain from the root down to the open page (array of page blocks).
  function pageTrail() {
    const trail = [];
    let p = notePage;
    while (p) {
      const b = noteBlockOf(p);
      if (!b) break;
      trail.unshift(b);
      p = b.parent || '';
    }
    return trail;
  }

  // noteTitle resolves a scope to a header: a channel's name (# foo) or, for a
  // DM, the peer's nickname. Falls back to the scope's leaf if names aren't in.
  function noteTitle(scope) {
    if (scope === GENERAL_ID || scope.includes('-')) {
      const ch = chatChannels.find((c) => c.id === scope);
      return '# ' + ((ch && ch.name) || scope) + ' · notes';
    }
    return nameForPub(scope) + ' · notes';
  }

  function openNote(conv, page) {
    noteConv = conv;
    noteScope = null;
    notePage = page || '';
    noteBlocks = [];
    noteUndo = []; noteRedo = [];
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-note').removeClass('hidden');
    $('#note-title').text('notes'); // location is shown by the breadcrumbs
    $('#note-status').text('loading…');
    $('#note-crumbs').empty();
    $('#note-blocks').empty();
    // Resolve the db scope (channel bid) first, then load — a DM keys off the
    // shared dm bid, not the peer's pub, so both sides see the same notes.
    resolveNoteScope(conv, () => { if (noteConv === conv) loadNoteBlocks(); });
  }
  // resolveNoteScope asks the server for the conversation's note scope (chanBID),
  // falling back to the legacy "note:<conv>" if the call fails.
  function resolveNoteScope(conv, cb) {
    const kind = convKind(conv);
    $.getJSON('/api/notes/scope?kind=' + encodeURIComponent(kind) + '&key=' + encodeURIComponent(conv))
      .done((r) => { if (noteConv !== conv) return; noteScope = (r && r.scope) || ('note:' + conv); cb(); })
      .fail(() => { if (noteConv !== conv) return; noteScope = 'note:' + conv; cb(); });
  }

  // loadNoteBlocks fetches the conversation's note blocks and paints them.
  function loadNoteBlocks(cb) {
    if (!noteConv) return;
    const conv = noteConv;
    $.getJSON('/api/notes?channel=' + encodeURIComponent(noteScopeOf(conv)), (list) => {
      if (noteConv !== conv) return; // navigated away
      noteBlocks = (list || []).filter((b) => !b.deleted);
      renderNoteBlocks();
      $('#note-status').text(noteBlocks.length + (noteBlocks.length === 1 ? ' block' : ' blocks'));
      if (typeof cb === 'function') cb();
    }).fail(() => { if (noteConv === conv) $('#note-status').text('load failed'); });
  }

  // refreshPreservingEdit reloads the note WITHOUT disturbing the block the
  // user is editing: it snapshots the focused editor (which block / inline
  // draft, its text and caret), repaints everything else, then puts the editor
  // back. This is what lets a peer's edits to OTHER blocks show up live while
  // you sit in an open block — the coarse noteEditingNow() guard would just
  // drop the refresh until you blur or reload.
  let noteRefreshTimer = null;
  function refreshPreservingEdit() {
    clearTimeout(noteRefreshTimer); // coalesce bursts (peer adds several blocks)
    noteRefreshTimer = setTimeout(() => {
      const a = document.activeElement;
      const $ta = a && $(a).hasClass('ax-nb-in') && $(a).closest('#note-blocks').length ? $(a) : null;
      let snap = null;
      if ($ta) {
        const $row = $ta.closest('.ax-nb');
        snap = {
          bid: $row.length ? $row.attr('data-bid') : null,
          idraft: $ta.hasClass('ax-nb-idraft'),
          pos: $ta.attr('data-pos') || null,
          val: $ta.val(), start: a.selectionStart, end: a.selectionEnd,
        };
      }
      loadNoteBlocks(() => {
        if (!snap) return;
        let el = null;
        if (snap.bid) {
          const $row = $('#note-blocks .ax-nb[data-bid="' + snap.bid + '"]');
          if ($row.length) { editBlock($row); el = $row.find('.ax-nb-text')[0]; }
        } else if (snap.idraft) {
          insertDraftAtPos(snap.pos);
          el = $('#note-blocks .ax-nb-idraft')[0];
        }
        if (el) { // restore the user's in-progress text + caret untouched
          el.value = snap.val; autogrow(el); el.focus();
          try { el.setSelectionRange(snap.start, snap.end); } catch (_) {}
        }
      });
    }, 120);
  }

  // noteEditingNow guards the background refresh: don't repaint while a block
  // is focused or a save is pending (would eat the cursor / in-flight text).
  function noteEditingNow() {
    if (Object.keys(noteSaveTimers).length) return true;
    const a = document.activeElement;
    return !!(a && $(a).closest('#note-blocks').length);
  }

  function autogrow(el) { el.style.height = 'auto'; el.style.height = el.scrollHeight + 'px'; }

  function renderNoteBlocks() {
    const $c = $('#note-blocks');
    // #note-blocks is the scroll container; emptying it drops scrollTop to 0, so
    // a rebuild (create/edit/sync) would jump to the top. Keep the position.
    const prevScroll = $c.scrollTop();
    // Orphan the previous render's attachment blobs (see attachUrl).
    noteBlobUrls.forEach((u) => { try { URL.revokeObjectURL(u); } catch (_) {} });
    noteBlobUrls = [];
    attachBucket = noteBlobUrls;
    $c.empty();
    renderCrumbs(); // breadcrumbs live in the header (#note-crumbs)
    const kids = pageKids();
    const pages = kids.filter((b) => blockType(b) === 'page');
    const texts = kids.filter((b) => blockType(b) !== 'page');
    if (pages.length) $c.append(renderToC(pages)); // auto table of contents
    for (const b of texts) $c.append(noteBlockRow(b));
    // Trailing draft line — type + Enter to add a text block (Notion-style).
    const ph = texts.length ? 'type to add a block…' : 'empty — type to start…';
    const $d = $('<textarea rows="1" spellcheck="false"></textarea>').addClass('ax-nb-in ax-nb-draft').attr('placeholder', ph);
    $c.append(editorWrap($d));
    $c.find('textarea').each(function () { autogrow(this); });
    if (window.f2fRich) f2fRich.renderDiagrams($c[0]);
    $c.scrollTop(prevScroll); // restore scroll after the full rebuild
    updateTorrentChips(); // paint torrent file-block status, then refresh
    pollTorrents();
  }

  // renderCrumbs fills the header breadcrumb slot (root › page › …) — the note's
  // only location indicator — with a trailing + that creates a sub-page here.
  function renderCrumbs() {
    const $c = $('#note-crumbs').empty();
    const root = noteTitle(noteConv).replace(/ · notes$/, '');
    $c.append($('<a class="ax-nb-crumb"></a>').text(root).attr('data-page', ''));
    for (const b of pageTrail()) {
      $c.append('<span class="ax-nb-crumb-sep">›</span>');
      $c.append($('<a class="ax-nb-crumb"></a>').text(pageTitle(b)).attr('data-page', b.bid));
    }
    $c.append('<button class="ax-nb-crumb-add" title="new sub-page">+</button>');
  }

  // renderToC builds the automatic table of contents — every sub-page of the
  // open page as a link (drill in). Sub-pages live here, not inline among text.
  function renderToC(pages) {
    const $toc = $('<div class="ax-nb-toc"></div>');
    $toc.append('<div class="ax-nb-toc-head">Pages</div>');
    for (const b of pages) {
      const $row = $('<div class="ax-nb ax-nb-tocrow"></div>').attr('data-bid', b.bid);
      const $link = $('<div class="ax-nb-pagelink"></div>').attr('data-page', b.bid)
        .html('<span class="ax-nb-pageicon">▤</span> ' + esc(pageTitle(b)));
      const $del = $('<button class="ax-nb-del" title="delete">✕</button>');
      $toc.append($row.append($link, $del));
    }
    return $toc;
  }

  // noteBlockRow builds one editable block + its meta line (type · author ·
  // version · variant count). The latest head is the default shown.
  function noteBlockRow(b) {
    const head = (b.heads && b.heads.length) ? b.heads[b.heads.length - 1] : null;
    const content = (head && head.content) || {};
    const $row = $('<div class="ax-nb"></div>').attr('data-bid', b.bid);
    const isFile = blockType(b) === 'file';
    // Text/heading blocks are markdown (rendered by default, click → edit). A
    // file block renders its attachment (image/video/chip) + optional caption,
    // not an editor.
    let $ed;
    if (isFile) {
      $ed = fileBlockBody(content);
    } else {
      $ed = $('<div class="ax-nb-body"></div>');
      renderView($ed, content);
    }
    const hist = b.history || [];
    if (isFile && hist.length && selfPubHex() && hist[0].author === selfPubHex()) {
      $row.attr('data-mine', '1'); // I shared this file → I'm the seeder
    }
    const creator = hist.length ? nameForPub(hist[0].author) : (head ? nameForPub(head.author) : '?');
    const editor = head ? nameForPub(head.author) : (hist.length ? nameForPub(hist[hist.length - 1].author) : '?');
    const nver = hist.length || (head ? 1 : 0);
    const editedTs = head ? head.ts : (hist.length ? hist[hist.length - 1].ts : 0);
    // Show the last editor only once a block has been revised (otherwise it's
    // just the creator). Created-by always shows; version history is the vN btn.
    const editedBy = nver > 1
      ? '<span class="ax-nb-editor" title="last edited by">✎ ' + esc(editor) + '</span>' : '';
    const variants = (b.heads && b.heads.length > 1)
      ? '<span class="ax-nb-variants" title="concurrent versions">' + b.heads.length + ' variants</span>' : '';
    const vbtn = isFile ? '' : '<span class="ax-nb-vbtn" title="version history">v' + nver + '</span>';
    const $meta = $(
      '<div class="ax-nb-meta">' +
        '<span class="ax-nb-author" title="created by">' + esc(creator) + '</span>' +
        vbtn +
        editedBy +
        '<span class="ax-nb-time" title="last edited">' + esc(editedTs ? notifWhen(editedTs) : '') + '</span>' +
        variants +
        '<span class="ax-nb-ctl">' +
          '<button class="ax-nb-up" title="move up">↑</button>' +
          '<button class="ax-nb-down" title="move down">↓</button>' +
          '<button class="ax-nb-del" title="delete">✕</button>' +
        '</span></div>');
    return $row.append($ed, $meta);
  }

  function selfPubHex() { return (lastStatus && lastStatus.identity_pub) || ''; }

  // fileBlockBody renders a block.file: the attachment (image/video preview or
  // download chip, via the shared attachHtml) plus an optional markdown caption.
  function fileBlockBody(content) {
    const f = content || {};
    const $body = $('<div class="ax-nb-body ax-nb-file"></div>');
    const mime = f.mime || '';
    const previewable = mime.indexOf('image/') === 0 || mime.indexOf('video/') === 0;
    const cap = f.caption
      ? '<div class="ax-nb-filecap ax-md">' + (window.f2fRich ? f2fRich.markdown(f.caption) : esc(f.caption)) + '</div>'
      : '';
    const cls = previewable ? 'ax-msg-media' : 'ax-msg-filecard';
    $body.html('<div class="' + cls + '">' + attachHtml(f) + cap + '</div>');
    return $body;
  }

  // uploadNoteFile attaches a file to the open note as a block.file: small files
  // inline (base64), large ones seeded over torrent (multipart). Appended at the
  // end of the current page.
  // uploadingPlaceholder is a transient in-place card shown at the insertion
  // spot while a file uploads, so it's clear both THAT something is happening
  // and WHERE the file will land. Replaced by the real block on completion.
  function uploadingPlaceholder(name) {
    return $('<div class="ax-nb ax-nb-uploading"><div class="ax-nb-body">' +
      '<div class="ax-msg-filecard"><div class="ax-up-row">' +
      '<span class="ax-up-spin"></span><span class="ax-up-name"></span>' +
      '<span class="ax-up-tag">uploading…</span></div></div></div></div>')
      .find('.ax-up-name').text(name).end();
  }
  // placeUpload drops the placeholder at the chosen spot: after a block, before
  // a draft, or (fallback) before the trailing draft line.
  function placeUpload($ph, place) {
    if (place && place.after && place.after.length) place.after.after($ph);
    else if (place && place.before && place.before.length) place.before.before($ph);
    else $('#note-blocks .ax-nb-draft').last().closest('.ax-nb-editwrap').before($ph);
  }

  // postNoteFile uploads the bytes and creates the block.file, returning a
  // promise. Small files go inline (base64), large ones over torrent.
  function postNoteFile(conv, parent, pos, file) {
    if (file.size > (8 << 20)) {
      const fd = new FormData();
      fd.append('channel', noteScopeOf(conv));
      fd.append('parent', parent);
      fd.append('pos', pos);
      fd.append('file', file);
      return $.ajax({ url: '/api/notes/share', method: 'POST', data: fd, processData: false, contentType: false });
    }
    const d = $.Deferred();
    const reader = new FileReader();
    reader.onload = () => {
      const b64 = String(reader.result).split(',')[1] || '';
      $.ajax({
        url: '/api/notes/attach', method: 'POST', contentType: 'application/json',
        data: JSON.stringify({ channel: noteScopeOf(conv), parent, pos, file: { name: file.name, mime: file.type, data: b64 } }),
      }).done(d.resolve).fail(d.reject);
    };
    reader.onerror = () => d.reject(reader.error);
    reader.readAsDataURL(file);
    return d.promise();
  }

  function uploadNoteFile(file, opts) {
    if (!noteConv || !file) return;
    const conv = noteConv;
    const parent = (opts && opts.parent != null) ? opts.parent : notePage;
    const pos = (opts && opts.pos) ? opts.pos : posEnd();
    // Preserve whatever is typed in the draft across the reload — regardless of
    // how the attach was triggered (button/paste/drop). Use the explicit value
    // if a handler captured it, else read the focused/trailing draft from the DOM.
    let draftText = (opts && opts.draftText != null) ? opts.draftText : '';
    if (!draftText) {
      const a = document.activeElement;
      const $d = (a && $(a).hasClass('ax-nb-draft')) ? $(a) : $('#note-blocks .ax-nb-draft').last();
      if ($d && $d.length) draftText = $d.val() || '';
    }
    const $ph = uploadingPlaceholder(file.name);
    placeUpload($ph, opts && opts.place);
    $('#note-status').text('attaching ' + file.name + '…');
    // Attaching must not discard what's typed: the reload would wipe the draft
    // textarea, so put the unsaved text back into it afterwards. It still only
    // becomes a block on Enter.
    const done = () => loadNoteBlocks(() => {
      $('#note-status').text('');
      if (!draftText.trim()) return;
      const $d = $('#note-blocks .ax-nb-draft').last();
      if ($d.length) { $d.val(draftText); autogrow($d[0]); $d.focus(); placeCaretEnd($d[0]); updateGutter($d[0]); }
    });
    const fail = (x) => { $ph.remove(); $('#note-status').text('attach failed: ' + errorOf(x)); };
    // Flush pending edits to existing blocks so the reload doesn't drop them.
    flushNoteSaves()
      .then(() => postNoteFile(conv, parent, pos, file))
      .done(done).fail(fail);
  }

  // renderView paints a text/heading block as RENDERED markdown (full
  // markdown incl. code highlighting + mermaid via f2fRich) — the default
  // when not editing. Stashes the raw md for the click-to-edit swap.
  function renderView($body, content) {
    const md = (content && (content.md || content.text)) || '';
    $body.attr('data-md', md);
    const inner = md
      ? (window.f2fRich ? f2fRich.markdown(md) : esc(md))
      : '<span class="ax-nb-ph">empty — click to edit</span>';
    $body.html('<div class="ax-nb-view ax-md">' + inner + '</div>');
    if (window.f2fRich && f2fRich.renderDiagrams) f2fRich.renderDiagrams($body[0]); // code + mermaid
    mountSandboxes($body[0]); // ```sandbox fences → isolated iframes (notes only)
  }

  // sandboxDoc wraps a ```sandbox cell's HTML/JS so it runs in a locked-down
  // iframe: null origin (no allow-same-origin → no access to /api, cookies, the
  // parent DOM) and a CSP that blocks ALL network (no exfiltration, no remote
  // code). A tiny reporter posts its height out so the iframe can auto-size.
  function sandboxDoc(code) {
    const csp = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
      "img-src data: blob:; media-src data: blob:; font-src data:;";
    const reporter = '(function(){function r(){try{parent.postMessage({__sbHeight:' +
      'document.documentElement.scrollHeight},"*")}catch(e){}}' +
      'if(window.ResizeObserver){new ResizeObserver(r).observe(document.documentElement)}' +
      'window.addEventListener("load",r);setTimeout(r,60);setTimeout(r,400);r()})();';
    return '<!doctype html><html><head><meta charset="utf-8">' +
      '<meta http-equiv="Content-Security-Policy" content="' + csp + '">' +
      '<style>html,body{margin:0;padding:8px;font:14px/1.5 system-ui,sans-serif;color:#e6e6e6;background:transparent}' +
      '*{box-sizing:border-box}</style></head><body>' + code +
      '<scr' + 'ipt>' + reporter + '</scr' + 'ipt></body></html>';
  }
  // mountSandboxes converts ```sandbox placeholders (emitted by richtext) into
  // sandboxed iframes. Only ever called for notes — messages keep the source.
  function mountSandboxes(root) {
    if (!root || !root.querySelectorAll) return;
    root.querySelectorAll('pre.ax-sandbox:not([data-done])').forEach((pre) => {
      pre.setAttribute('data-done', '1');
      const f = document.createElement('iframe');
      f.className = 'ax-nb-sandbox';
      f.setAttribute('sandbox', 'allow-scripts'); // NO allow-same-origin
      f.setAttribute('referrerpolicy', 'no-referrer');
      f.srcdoc = sandboxDoc(pre.textContent || ''); // raw code; iframe is isolated
      pre.replaceWith(f);
    });
  }
  // Height auto-size: each sandbox posts its scrollHeight; match by contentWindow
  // (the iframe is cross-origin, so origin is "null" — don't check it).
  window.addEventListener('message', function (e) {
    const h = e.data && e.data.__sbHeight;
    if (!h) return;
    document.querySelectorAll('iframe.ax-nb-sandbox').forEach((f) => {
      if (f.contentWindow === e.source) f.style.height = Math.min(Math.max(h, 24) + 2, 4000) + 'px';
    });
  });

  // editBlock swaps a rendered block to a raw-markdown textarea (with a line-
  // number gutter, code-editor style), focused.
  function editBlock($row) {
    const $body = $row.find('.ax-nb-body');
    const md = $body.attr('data-md') || '';
    const $ta = $('<textarea rows="1" spellcheck="false"></textarea>').addClass('ax-nb-in ax-nb-text').val(md);
    $body.empty().append(editorWrap($ta));
    $ta.focus();
    autogrow($ta[0]);
    placeCaretEnd($ta[0]);
    updateGutter($ta[0]);
  }

  // editorWrap wraps an editing textarea with a line-number gutter (the gutter
  // is hidden by CSS until the textarea is focused). Used for both editing an
  // existing block and typing a new one (draft), so the experience matches.
  function editorWrap($ta) {
    return $('<div class="ax-nb-editwrap"><div class="ax-nb-gutter"></div></div>')
      .append($ta)
      // Attach affordance for THIS block/draft — shown on focus (like the
      // gutter). tabindex=-1 + mousedown-preventDefault keep the editor focused
      // so the file lands at this position, not at the end of the page.
      .append('<button type="button" class="ax-nb-attach" tabindex="-1" title="attach file"><i class="bi bi-paperclip"></i></button>');
  }

  // Line-number gutter for the editing textarea. A hidden mirror (same width +
  // font) measures each logical line's wrapped height so numbers align even when
  // lines soft-wrap (one number per logical line).
  let noteMirror = null;
  function updateGutter(ta) {
    const wrap = ta.closest && ta.closest('.ax-nb-editwrap');
    if (!wrap) return;
    const gut = wrap.querySelector('.ax-nb-gutter');
    if (!gut) return;
    if (!noteMirror) {
      noteMirror = document.createElement('div');
      noteMirror.style.cssText = 'position:absolute;left:-9999px;top:-9999px;visibility:hidden;white-space:pre-wrap;word-wrap:break-word;';
      document.body.appendChild(noteMirror);
    }
    const cs = getComputedStyle(ta);
    noteMirror.style.fontFamily = cs.fontFamily;
    noteMirror.style.fontSize = cs.fontSize;
    noteMirror.style.lineHeight = cs.lineHeight;
    noteMirror.style.width = ta.clientWidth + 'px';
    const lines = ta.value.split('\n');
    let html = '';
    for (let i = 0; i < lines.length; i++) {
      noteMirror.textContent = lines[i] || ' ';
      html += '<div class="ax-nb-lnum" style="height:' + noteMirror.offsetHeight + 'px">' + (i + 1) + '</div>';
    }
    gut.innerHTML = html;
  }

  // toggleHistory shows/hides a block's version list under its meta line. Each
  // row is one immutable version (newest first): vK · author · time. Clicking a
  // row previews that version with restore/current actions.
  function toggleHistory($row) {
    const $existing = $row.find('.ax-nb-hist');
    if ($existing.length) { $existing.remove(); return; }
    // close any other open history panel first
    $('#note-blocks .ax-nb-hist').remove();
    const bid = $row.attr('data-bid');
    const b = noteBlocks.find((x) => x.bid === bid);
    const hist = (b && b.history) || [];
    const $h = $('<div class="ax-nb-hist"></div>');
    for (let i = hist.length - 1; i >= 0; i--) {
      const v = hist[i];
      const head = (b.heads && b.heads.length) ? b.heads[b.heads.length - 1] : null;
      const isCurrent = head && head.entry_id === v.entry_id;
      const $r = $('<div class="ax-nb-hrow"></div>')
        .attr('data-entry', v.entry_id)
        .toggleClass('ax-nb-hcur', !!isCurrent);
      $r.html(
        '<span class="ax-nb-hver">v' + (i + 1) + '</span>' +
        '<span class="ax-nb-hauthor">' + esc(nameForPub(v.author)) + '</span>' +
        '<span class="ax-nb-hop">' + esc(v.op || '') + '</span>' +
        '<span class="ax-nb-htime">' + esc(v.ts ? notifWhen(v.ts) : '') + '</span>' +
        (isCurrent ? '<span class="ax-nb-hcur-tag">current</span>' : ''));
      $h.append($r);
    }
    $row.find('.ax-nb-meta').after($h);
  }

  // previewVersion paints a chosen historical version into the block body with
  // a banner offering restore (write it as a new head) or back-to-current.
  function previewVersion($row, entryId) {
    const bid = $row.attr('data-bid');
    const b = noteBlocks.find((x) => x.bid === bid);
    if (!b) return;
    const v = (b.history || []).find((x) => x.entry_id === entryId);
    if (!v) return;
    const $body = $row.find('.ax-nb-body');
    $row.attr('data-viewing', entryId);
    renderView($body, v.content || {});
    $body.find('.ax-nb-view').prepend(
      '<div class="ax-nb-vbanner">viewing an old version · ' +
        '<button class="ax-nb-restore">restore</button>' +
        '<button class="ax-nb-current">back to current</button></div>');
  }

  // restoreVersion writes the previewed version's content as a new head.
  function restoreVersion($row) {
    const bid = $row.attr('data-bid');
    const entryId = $row.attr('data-viewing');
    const b = noteBlocks.find((x) => x.bid === bid);
    const v = b && (b.history || []).find((x) => x.entry_id === entryId);
    if (!v) return;
    $.ajax({
      url: '/api/notes/update', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: noteScopeOf(noteConv), bid, content: v.content || {} }),
    }).done(() => loadNoteBlocks());
  }

  // showCurrent drops the preview and repaints the latest head.
  function showCurrent($row) {
    const bid = $row.attr('data-bid');
    const b = noteBlocks.find((x) => x.bid === bid);
    $row.removeAttr('data-viewing');
    const head = (b && b.heads && b.heads.length) ? b.heads[b.heads.length - 1] : null;
    renderView($row.find('.ax-nb-body'), (head && head.content) || {});
  }

  // saveBlock debounce-saves one block (update over current heads; concurrent
  // edits resolve as the engine merges heads).
  function saveBlock(bid, content) {
    const conv = noteConv;
    clearTimeout(noteSaveTimers[bid]);
    noteSaveTimers[bid] = setTimeout(() => {
      delete noteSaveTimers[bid];
      $.ajax({
        url: '/api/notes/update', method: 'POST', contentType: 'application/json',
        data: JSON.stringify({ channel: noteScopeOf(conv), bid, content }),
      });
    }, 500);
  }

  // fractional positions over zero-padded numbers (gap 1000 leaves room to
  // insert between neighbours without re-indexing).
  function posNum(s) { return parseInt(s || '0', 10) || 0; }
  function posPad(n) { return String(n).padStart(8, '0'); }
  // pos helpers operate within the OPEN page's siblings (pageKids), so order is
  // per-page, not across the whole note tree.
  function posEnd() { let m = 0; for (const b of pageKids()) { const n = posNum(b.pos); if (n > m) m = n; } return posPad(m + 1000); }
  function posAfter(bid) {
    const kids = pageKids();
    const i = kids.findIndex((b) => b.bid === bid);
    if (i < 0) return posEnd();
    const a = posNum(kids[i].pos);
    const b = i + 1 < kids.length ? posNum(kids[i + 1].pos) : a + 2000;
    return posPad(Math.floor((a + b) / 2));
  }
  function placeCaretEnd(el) { if (el && el.setSelectionRange) { const n = el.value.length; el.setSelectionRange(n, n); } }

  // flushNoteSaves sends any pending debounced block edits NOW (reading the
  // live textarea value) and returns a promise that resolves once they land.
  // Callers that reload the list must wait on it, else the GET races the save
  // and paints stale content (the edit only appears after a manual reload).
  function flushNoteSaves() {
    const reqs = [];
    for (const bid of Object.keys(noteSaveTimers)) {
      clearTimeout(noteSaveTimers[bid]);
      delete noteSaveTimers[bid];
      const $ta = $('#note-blocks .ax-nb[data-bid="' + bid + '"] .ax-nb-text');
      if (!$ta.length) continue;
      reqs.push($.ajax({
        url: '/api/notes/update', method: 'POST', contentType: 'application/json',
        data: JSON.stringify({ channel: noteScopeOf(noteConv), bid, content: { md: $ta.val() } }),
      }));
    }
    return reqs.length ? $.when.apply($, reqs) : $.Deferred().resolve().promise();
  }

  // keepDraftInView scrolls the draft into view now AND re-does it once any
  // not-yet-loaded images finish decoding. Inline images start at height ~0 and
  // grow when decoded, shifting everything above the draft — without this the
  // view appears to "jump up" after creating a block in an image-heavy note.
  function keepDraftInView(el) {
    if (!el) return;
    const f = () => { try { el.scrollIntoView({ block: 'nearest' }); } catch (_) {} };
    f();
    $('#note-blocks img').each(function () {
      if (!this.complete) this.addEventListener('load', f, { once: true });
    });
  }
  // insertDraftAfter drops a TRANSIENT, unsaved editing line right after a row
  // (or at the top if none). Empty blocks are never written to the log — only
  // when the draft gets text + Enter does a real block get created at its pos.
  // The pos is stashed on the element so the eventual create lands in place.
  function insertDraftAfter($afterRow, pos) {
    $('#note-blocks .ax-nb-idraft').closest('.ax-nb-editwrap').remove(); // at most one inline draft
    // Inserting at the very end? Reuse the always-present trailing draft instead
    // of stacking a second draft line right next to it.
    if ($afterRow && $afterRow.length && $afterRow.nextAll('.ax-nb').length === 0) {
      const $tail = $('#note-blocks .ax-nb-draft').last();
      if ($tail.length) {
        $tail[0].focus(); autogrow($tail[0]); placeCaretEnd($tail[0]);
        updateGutter($tail[0]); keepDraftInView($tail[0]);
        return;
      }
    }
    const $d = $('<textarea rows="1" spellcheck="false"></textarea>')
      .addClass('ax-nb-in ax-nb-draft ax-nb-idraft')
      .attr('placeholder', 'type…').attr('data-pos', pos);
    const $wrap = editorWrap($d);
    if ($afterRow && $afterRow.length) $afterRow.after($wrap);
    else $('#note-blocks').prepend($wrap);
    $d[0].focus();
    autogrow($d[0]);
    updateGutter($d[0]);
    keepDraftInView($d[0]);
  }

  // insertDraftAtPos re-inserts an inline draft at a fractional pos after a
  // re-render (blocks are pos-sorted): place it after the last block whose pos
  // is below the draft's, else at the top.
  function insertDraftAtPos(pos) {
    let $after = null;
    for (const b of pageKids()) {
      if (posNum(b.pos) < posNum(pos)) {
        const $r = $('#note-blocks .ax-nb[data-bid="' + b.bid + '"]');
        if ($r.length) $after = $r;
      } else break;
    }
    insertDraftAfter($after, pos);
  }

  // createNoteBlock POSTs a new markdown block, then reloads and opens a fresh
  // draft line after it (Notion-style: keep typing on a new line). Pending
  // edits are flushed first so the reload sees them.
  let noteCreating = false; // guards against a second Enter while a create is in flight
  function createNoteBlock(content, pos) {
    if (!noteConv || noteCreating) return;
    noteCreating = true;
    const conv = noteConv;
    const parent = notePage;
    $('#note-status').text('saving…');
    flushNoteSaves().always(() => {
      $.ajax({
        url: '/api/notes', method: 'POST', contentType: 'application/json',
        data: JSON.stringify({ channel: noteScopeOf(conv), type: 'text', content, parent, pos }),
      }).done((r) => {
        const bid = r && r.bid;
        loadNoteBlocks(() => {
          noteCreating = false;
          const $row = $('#note-blocks .ax-nb[data-bid="' + bid + '"]');
          if ($row.length) insertDraftAfter($row, posAfter(bid));
          else $('#note-blocks .ax-nb-draft').last().focus();
        });
      }).fail((x) => { noteCreating = false; $('#note-status').text('add failed: ' + errorOf(x)); });
    });
  }

  // createPage adds a sub-page block under the open page, then drills into it.
  function createPage() {
    if (!noteConv) return;
    const title = (prompt('page title') || '').trim();
    if (!title) return;
    const conv = noteConv, parent = notePage, pos = posEnd();
    $.ajax({
      url: '/api/notes', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: noteScopeOf(conv), type: 'page', content: { title }, parent, pos }),
    }).done((r) => {
      if (r && r.bid) location.hash = notePageRoute(conv, r.bid); // open the new page
    }).fail((x) => $('#note-status').text('add page failed: ' + errorOf(x)));
  }

  function delNoteBlock(bid, fromRedo) {
    // Guard against a double-delete: deleting reloads the list, which removes
    // the focused textarea and fires blur → the blur handler would delete the
    // (empty) block again. Likewise ✕ blurs then clicks. Stay locked until the
    // reload's render is done, so the spurious blur is ignored.
    if (!noteConv || noteDeleting[bid]) return;
    noteDeleting[bid] = true;
    // Snapshot current content so the delete can be undone (revived).
    const b = noteBlocks.find((x) => x.bid === bid);
    const head = (b && b.heads && b.heads.length) ? b.heads[b.heads.length - 1] : null;
    if (b) {
      noteUndo.push({ bid, content: (head && head.content) || {} });
      if (!fromRedo) noteRedo = [];
    }
    $.ajax({
      url: '/api/notes/delete', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: noteScopeOf(noteConv), bid }),
    }).done(() => loadNoteBlocks(() => { delete noteDeleting[bid]; }))
      .fail(() => { delete noteDeleting[bid]; });
  }

  // undoDelete revives the last deleted block (writes its prior content over the
  // tombstone head). redoDelete re-tombstones it. Both keep the stacks paired.
  function undoDelete() {
    const e = noteUndo.pop();
    if (!e) { $('#note-status').text('nothing to undo'); return; }
    noteRedo.push(e);
    $.ajax({
      url: '/api/notes/update', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ channel: noteScopeOf(noteConv), bid: e.bid, content: e.content }),
    }).done(() => loadNoteBlocks());
  }
  function redoDelete() {
    const e = noteRedo.pop();
    if (!e) { $('#note-status').text('nothing to redo'); return; }
    delNoteBlock(e.bid, true);
  }

  // moveNoteBlock reorders by swapping pos with the neighbour.
  function moveNoteBlock(bid, dir) {
    const kids = pageKids(); // reorder within the open page's siblings only
    const i = kids.findIndex((b) => b.bid === bid);
    if (i < 0) return;
    const j = dir < 0 ? i - 1 : i + 1;
    if (j < 0 || j >= kids.length) return;
    const conv = noteConv, a = kids[i], b = kids[j];
    const mv = (id, pos) => $.ajax({ url: '/api/notes/move', method: 'POST', contentType: 'application/json', data: JSON.stringify({ channel: noteScopeOf(conv), bid: id, pos }) });
    $.when(mv(a.bid, b.pos), mv(b.bid, a.pos)).always(loadNoteBlocks);
  }

  // --- block editor wiring (Notion-style: keyboard-driven, no buttons) ---
  // Every block is markdown text (types come later). Edit → debounce-save.
  $('#note-blocks').on('input', '.ax-nb-text', function () {
    autogrow(this);
    updateGutter(this);
    saveBlock($(this).closest('.ax-nb').attr('data-bid'), { md: $(this).val() });
  });
  $('#note-blocks').on('input', '.ax-nb-draft', function () { autogrow(this); updateGutter(this); });
  // Populate the gutter when an editor/draft gains focus (it's hidden until then).
  $('#note-blocks').on('focusin', '.ax-nb-in', function () { updateGutter(this); });
  // Click a rendered block → edit it; blur → flush save + render back. Don't
  // open the editor while previewing a historical version (banner buttons act).
  $('#note-blocks').on('click', '.ax-nb-view', function (e) {
    if ($(e.target).closest('.ax-nb-vbanner').length) return; // banner button
    const $row = $(this).closest('.ax-nb');
    if ($row.attr('data-viewing')) return;
    editBlock($row);
  });
  // Version history: toggle the panel, preview a version, restore / current.
  $('#note-blocks').on('click', '.ax-nb-vbtn', function () { toggleHistory($(this).closest('.ax-nb')); });
  $('#note-blocks').on('click', '.ax-nb-hrow', function () { previewVersion($(this).closest('.ax-nb'), $(this).attr('data-entry')); });
  $('#note-blocks').on('click', '.ax-nb-restore', function (e) { e.stopPropagation(); restoreVersion($(this).closest('.ax-nb')); });
  $('#note-blocks').on('click', '.ax-nb-current', function (e) { e.stopPropagation(); showCurrent($(this).closest('.ax-nb')); });
  $('#note-blocks').on('blur', '.ax-nb-text', function () {
    const $row = $(this).closest('.ax-nb');
    const bid = $row.attr('data-bid');
    const md = $(this).val();
    const pending = !!noteSaveTimers[bid];
    if (pending) { clearTimeout(noteSaveTimers[bid]); delete noteSaveTimers[bid]; }
    if (!md.trim()) { delNoteBlock(bid); return; } // empty block vanishes on blur
    if (pending) { // flush a pending debounced save now
      $.ajax({ url: '/api/notes/update', method: 'POST', contentType: 'application/json', data: JSON.stringify({ channel: noteScopeOf(noteConv), bid, content: { md } }) });
    }
    renderView($row.find('.ax-nb-body'), { md });
  });
  // Enter = new line below; Shift+Enter = newline within the block. We never
  // persist empties: Enter on a block collapses it back to a view and opens a
  // transient draft line below; the draft only becomes a real block once it
  // has text. Backspace on an empty block at the start deletes it.
  $('#note-blocks').on('keydown', '.ax-nb-text, .ax-nb-draft', function (e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if ($(this).hasClass('ax-nb-draft')) {
        const v = $(this).val().trim();
        const pos = $(this).attr('data-pos') || posEnd();
        if (v) createNoteBlock({ md: v }, pos);
        else if ($(this).hasClass('ax-nb-idraft')) $(this).remove(); // empty inline draft → discard
        return;
      }
      // Editing an existing block: flush it, render it back, open a draft below.
      const $row = $(this).closest('.ax-nb');
      const bid = $row.attr('data-bid');
      const md = $(this).val();
      flushNoteSaves();
      renderView($row.find('.ax-nb-body'), { md });
      insertDraftAfter($row, posAfter(bid));
      return;
    }
    // Backspace at the start of an empty block (whitespace-only counts) removes
    // it in one press — no need to first delete a stray newline.
    if (e.key === 'Backspace' && !$(this).hasClass('ax-nb-draft') && !$(this).val().trim() && this.selectionStart === 0) {
      e.preventDefault();
      delNoteBlock($(this).closest('.ax-nb').attr('data-bid'));
    }
  });
  // An inline draft left empty just vanishes — nothing is written to the log.
  $('#note-blocks').on('blur', '.ax-nb-idraft', function () {
    if (!$(this).val().trim()) $(this).closest('.ax-nb-editwrap').remove();
  });
  $('#note-blocks').on('click', '.ax-nb-del', function () { delNoteBlock($(this).closest('.ax-nb').attr('data-bid')); });
  // Undo/redo for deletions while the notes view is open.
  $(document).on('keydown', function (e) {
    if (noteConv === null || $('#tab-note').hasClass('hidden')) return;
    if (!(e.ctrlKey || e.metaKey) || e.key.toLowerCase() !== 'z') return;
    e.preventDefault();
    if (e.shiftKey) redoDelete(); else undoDelete();
  });
  $('#note-blocks').on('click', '.ax-nb-up', function () { moveNoteBlock($(this).closest('.ax-nb').attr('data-bid'), -1); });
  $('#note-blocks').on('click', '.ax-nb-down', function () { moveNoteBlock($(this).closest('.ax-nb').attr('data-bid'), 1); });
  // Pages: drill into a sub-page (ToC link in #note-blocks); hop via a breadcrumb
  // or create a sub-page (breadcrumb row lives in the header, #note-crumbs).
  $('#note-blocks').on('click', '.ax-nb-pagelink', function () {
    location.hash = notePageRoute(noteConv, $(this).attr('data-page'));
  });
  $('#note-crumbs').on('click', '.ax-nb-crumb', function () {
    location.hash = notePageRoute(noteConv, $(this).attr('data-page') || '');
  });
  $('#note-crumbs').on('click', '.ax-nb-crumb-add', createPage);

  // Open a channel's notes from the hover icon on its sidebar row.
  $('#ax-tree').on('click', '.ax-tree-note', function (e) {
    e.stopPropagation();
    const id = $(this).attr('data-note');
    if (id) location.hash = noteRoute(id);
  });
  // Back from the notes view to its conversation.
  $('#note-back').on('click', function () {
    if (noteConv) location.hash = convRoute(noteConv);
  });

  // Attach a file at a specific spot: the per-block/draft paperclip inserts
  // right after that block (or at the draft's pos); drag-drop inserts after the
  // block dropped on. The header has no global button — attaching is positional.
  let pendingAttach = null;
  // attachTargetFor resolves where a file goes from the editor it was triggered
  // on: after an existing block, or at a draft's stashed pos (placeholder shown
  // right there so it's clear where it lands).
  function attachTargetFor($wrap) {
    const $row = $wrap.closest('.ax-nb');
    if ($row.length && $row.attr('data-bid')) {
      return { parent: notePage, pos: posAfter($row.attr('data-bid')), place: { after: $row } };
    }
    const $d = $wrap.find('.ax-nb-draft'); // a draft line stashes its target pos
    return { parent: notePage, pos: ($d.attr('data-pos')) || posEnd(), place: { before: $wrap }, draftText: $d.val() };
  }
  $('#note-blocks').on('mousedown', '.ax-nb-attach', function (e) {
    e.preventDefault(); // keep the editor focused so focus-within stays open
    pendingAttach = attachTargetFor($(this).closest('.ax-nb-editwrap'));
    $('#note-file-input').val('').click();
  });
  $('#note-file-input').on('change', function () {
    if (this.files && this.files[0]) uploadNoteFile(this.files[0], pendingAttach);
    pendingAttach = null;
  });
  $('#note-blocks').on('dragover', function (e) { e.preventDefault(); $(this).addClass('ax-nb-dragover'); })
    .on('dragleave drop', function () { $(this).removeClass('ax-nb-dragover'); })
    .on('drop', function (e) {
      e.preventDefault();
      const dt = e.originalEvent && e.originalEvent.dataTransfer;
      if (!dt || !dt.files || !dt.files.length) return;
      const $row = $(e.target).closest('.ax-nb');
      const inBlock = $row.length && $row.attr('data-bid');
      const pos = inBlock ? posAfter($row.attr('data-bid')) : posEnd();
      uploadNoteFile(dt.files[0], { parent: notePage, pos, place: inBlock ? { after: $row } : null });
    });
  // Paste an image/file into a note block or draft → attach it at that spot.
  // Plain-text pastes fall through. The draft's text is preserved (see uploadNoteFile).
  $('#note-blocks').on('paste', '.ax-nb-in', function (e) {
    if (!noteConv) return;
    const files = clipboardFiles(e.originalEvent && e.originalEvent.clipboardData);
    if (!files.length) return;
    e.preventDefault();
    const target = attachTargetFor($(this).closest('.ax-nb-editwrap'));
    files.forEach((f) => uploadNoteFile(f, target));
  });

  // Toggle the members panel.
  $('#chat-members').on('click', function () {
    const $p = $('#chat-members-panel');
    if ($p.hasClass('hidden')) renderMembersPanel();
    else $p.addClass('hidden');
  });
  // Open the current conversation's shared notes (header button, mirrors the
  // sidebar hover icon) — routes to note:<key> so it's the same deep-link.
  $('#chat-notes').on('click', function () {
    if (chatConv) location.hash = noteRoute(chatConv.key);
  });
  // Clear the open conversation's messages — LOCAL only (memory + SQLite);
  // peers keep their copies.
  $('#chat-clear').on('click', function () {
    if (!chatConv) return;
    if (!confirm('Delete all messages here? This removes them for everyone.')) return;
    $.ajax({
      url: '/api/messages/clear', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ kind: chatConv.kind, key: chatConv.key }),
    }).done(() => {
      chatMsgs = [];
      renderChat(chatMsgs);
      closeThread(); // its messages are gone too
    }).fail((xhr) => alert('clear: ' + errorOf(xhr)));
  });
  // Remove a member (owner only).
  $('#chat-members-panel').on('click', '.ax-chat-member-rm', function () {
    const pub = $(this).attr('data-rm');
    if (!pub || !chatConv) return;
    $.ajax({
      url: '/api/channels/members', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ bid: chatConv.key, remove: [pub] }),
    }).done(fetchChannels).fail((xhr) => alert('remove member: ' + errorOf(xhr)));
  });
  // Add a member (owner only).
  $('#chat-members-panel').on('change', '#chat-add-member', function () {
    const pub = $(this).val();
    if (!pub || !chatConv) return;
    $.ajax({
      url: '/api/channels/members', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ bid: chatConv.key, add: [pub] }),
    }).done(fetchChannels).fail((xhr) => alert('add member: ' + errorOf(xhr)));
  });

  // Live message stream: append to the open conversation, else bump unread.
  function openChatStream() {
    const es = new EventSource('/api/events');
    es.onmessage = (e) => {
      let m; try { m = JSON.parse(e.data); } catch (_) { return; }
      // Block-change event (remote db sync) rides this stream.
      if (m.type === 'blocks') {
        // A channel-meta change ("channel:<bid>", e.g. we were just added) →
        // refresh the sidebar so the channel (dis)appears live.
        if (m.scope && m.scope.indexOf('channel:') === 0) { fetchChannels(); return; }
        // Otherwise it's a note scope — live-refresh the open note editor.
        if (m.scope && noteConv && !$('#tab-note').hasClass('hidden') && m.scope === noteScopeOf(noteConv)) refreshPreservingEdit();
        return;
      }
      // Notification events ride this stream too (one connection for all push).
      if (m.type === 'notif') {
        const n = m.n;
        notifications.unshift(n);
        if (notifications.length > 200) notifications.length = 200;
        renderNotifications();
        osNotify(n);
        return;
      }
      // peer is the conversation key for both kinds (channel bid / DM peer pub).
      const key = m.peer;
      // Legacy notes-over-bus messages (notes are now blocks synced via db):
      // never render them as a chat line; refresh the block editor if open.
      if (m.type === 'notes') {
        if (noteConv && noteConv === key && !$('#tab-note').hasClass('hidden') && !noteEditingNow()) loadNoteBlocks();
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
        // An edit patches a bubble in place, and a thread reply belongs to the
        // panel (hidden from the timeline) → full redraw. A normal message just
        // appends (cheaper, keeps scroll).
        if (m.edit_id || m.thread) {
          renderChat(chatMsgs); // full redraw also clears optimistic bubbles
          if (threadRoot && !$('#chat-thread').hasClass('hidden')) renderThread();
        } else {
          // Our own echo replaces its optimistic "sending…" bubble IN PLACE
          // (so it's visible until the message lands, with no flash/duplicate);
          // everyone else's just appends.
          const $slot = m.mine ? matchPending(m.body) : $();
          const $row = $(msgRow(m, false, 0, false));
          if ($slot.length) $slot.replaceWith($row);
          else $('#chat-messages').append($row);
          const $m = $('#chat-messages');
          if (window.f2fRich) f2fRich.renderDiagrams($m[0]);
          mountSandboxes($m[0]);
          $m.scrollTop($m[0].scrollHeight);
          if (m.file && m.file.info_hash) { updateTorrentChips(); pollTorrents(); }
        }
      } else if (!m.mine && !m.edit_id) {
        chatUnread[key] = (chatUnread[key] || 0) + 1;
      }
    };
    es.onerror = () => {}; // EventSource auto-reconnects
  }
  openChatStream(); // block-change events ride this same stream (type:'blocks')
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
    // Mandatory profile gate: until a profile exists, pin the user to the
    // profile page and bounce any attempt to navigate away.
    if (profileRequired && h !== 'profile') { location.hash = 'profile'; return; }
    if (h === 'profile') { activateTab('profile'); fillProfilePage(); return; }
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
    // query → the read-only SQL console over messenger.db.
    if (h === 'query') { activateTab('query'); openQuery(); return; }
    if (h === 'oidc') { activateTab('oidc'); openOIDC(); return; }
    // "channel:new" → prompt for a name, create EMPTY (just us), open it and
    // pop the members panel so the user adds people deliberately — no
    // surprise "everyone's already in".
    if (h === 'channel:new') {
      const name = (prompt('channel name (use / for hierarchy, e.g. dev/backend)') || '').trim();
      if (name === GENERAL_ID || name.startsWith(GENERAL_ID + '/')) {
        alert('"' + GENERAL_ID + '" is reserved — you can’t nest channels under it.');
        location.hash = '';
        return;
      }
      if (name) {
        $.ajax({
          url: '/api/channels', method: 'POST', contentType: 'application/json',
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
    // note:<id> → the conversation's block-based notes in the main pane.
    const nm = h.match(/^note:(.+)$/);
    if (nm) {
      let key = nm[1].replace(/:preview$/, ''); // tolerate old preview links
      let page = '';
      const pm = key.match(/^(.+):page:([0-9a-zA-Z-]+)$/); // optional page within the note
      if (pm) { key = pm[1]; page = pm[2]; }
      if (key === 'general') key = GENERAL_ID;
      if (noteConv === key && notePage === page && !$('#tab-note').hasClass('hidden')) return; // already open
      openNote(key, page);
      return;
    }
    // A conversation: channel:<key>, optionally with a thread suffix
    // ":thread:<rootId>" (deep-linkable). Everything is a channel — a DM is the
    // degenerate one, keyed by the peer's pub; convKind() tells them apart by
    // the key's shape. general's clean "general" alias ↔ its internal
    // "*/general" id; a raw "*/general" in the hash (e.g. a notification deep-
    // link) is rewritten to the alias so URL + sidebar highlight stay in sync.
    const m = h.match(/^channel:(.+)$/);
    if (!m) return;
    let key = m[1], thread = '';
    const tt = key.match(/^(.+):thread:([0-9a-zA-Z-]+)$/); // ids contain '-' (fp16-rand)
    if (tt) { key = tt[1]; thread = tt[2]; }
    const kind = convKind(key);
    // Always switch to the chat tab (we may be coming back from the notes
    // view). Only RELOAD the conversation when it actually changed — toggling
    // the thread panel (same key) must not re-fetch/flicker the timeline.
    const sameConv = chatConv && chatConv.kind === kind && chatConv.key === key;
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-chat').removeClass('hidden');
    if (!sameConv) {
      chatConv = { kind, key };
      chatNamesPending = true; // names may not be loaded yet (e.g. hard reload)
      setChatTitle();
      $('#chat-call, #chat-clear, #chat-notes').show(); // call + clear + notes: both DMs and channels
      $('#chat-members').toggle(kind === 'channel'); // members button: channels only
      $('#chat-members-panel').addClass('hidden').empty();
      clearReplyTarget(); // a pending reply doesn't carry across conversations
      clearChatPending(); // nor a staged attachment
      loadConversation();
    }
    // Reconcile the thread panel with the URL.
    if (thread) showThread(thread); else closeThreadPanel();
  }
  // highlightActiveRoute marks the sidebar row matching the current hash
  // so the user can see where they are. Re-run after every tree rebuild
  // (the sidebar is regenerated from status each tick).
  function highlightActiveRoute() {
    let route = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    // An open thread keeps its conversation row highlighted — drop the suffix.
    route = route.replace(/:thread:[0-9a-zA-Z-]+$/, '');
    // A note view keeps its channel's row highlighted — drop the ":preview"
    // mode suffix and map note:<id> to the channel's own route so the row matches.
    const noteM = route.replace(/:preview$/, '').match(/^note:(.+)$/);
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
  // currentText resolves a message id to its latest content (original patched
  // with the newest edit) — what the edit button recalls into the composer.
  function currentText(id) {
    const orig = chatMsgs.find((x) => x.id === id && !x.edit_id);
    if (!orig) return '';
    return (editsByRoot(chatMsgs)[id] || orig).body || '';
  }
  // Message actions: edit (recall to composer), thread (open the thread panel),
  // reply (inline quote in the main composer).
  $('#chat-messages').on('click', '.ax-msg-act', function (e) {
    e.stopPropagation();
    const $msg = $(this).closest('.ax-msg');
    const id = $msg.attr('data-id');
    if (!id) return;
    const act = $(this).attr('data-act');
    if (act === 'edit') { startEdit({ id, text: currentText(id) }); return; }
    if (act === 'thread') { openThread(id); return; }
    const ref = chatMsgs.find((x) => x.id === id);
    const author = ref ? authorOf(ref) : nameForPub($msg.attr('data-author'));
    setReplyTarget({ id, author, snippet: msgSnippet(ref) });
  });
  // "N replies" footer opens the thread too.
  $('#chat-messages').on('click', '.ax-msg-thread-open', function (e) {
    e.stopPropagation();
    openThread($(this).attr('data-root'));
  });

  // --- threads (Slack-style side panel) ---
  // The open thread lives in the URL (channel:<key>:thread:<rootId>) so it's
  // deep-linkable and survives reload. openThread/closeThread just drive the
  // hash; applyRoute reacts and calls showThread/closeThreadPanel.
  function threadHash(rootId) {
    return chatConv ? convRoute(chatConv.key) + ':thread:' + rootId : '';
  }
  function openThread(rootId) {
    if (rootId && chatConv) location.hash = threadHash(rootId);
  }
  function closeThread() {
    if (chatConv) location.hash = convRoute(chatConv.key); // drop the thread suffix
    else closeThreadPanel();
  }
  $('#thread-close').on('click', closeThread);

  // showThread/closeThreadPanel do the actual panel work (no hash change) —
  // called by the router so open state always matches the URL.
  function showThread(rootId) {
    threadRoot = rootId;
    $('#chat-thread, #thread-resize').removeClass('hidden');
    renderThread();
    setTimeout(() => $('#thread-input').focus(), 0);
  }
  function closeThreadPanel() {
    threadRoot = null;
    $('#chat-thread, #thread-resize').addClass('hidden');
    $('#thread-messages').empty();
  }

  // The thread panel's width is drag-resizable (handle on its left edge) and
  // persisted, mirroring the sidebar. Dragging left widens it.
  const THREAD_W_KEY = 'f2f:thread-width';
  const THREAD_MIN = 280, THREAD_MAX = 900;
  function applyThreadWidth(px) {
    $('#chat-thread').css('width', Math.max(THREAD_MIN, Math.min(THREAD_MAX, px)) + 'px');
  }
  try { const s = parseInt(localStorage.getItem(THREAD_W_KEY) || '', 10); if (Number.isFinite(s)) applyThreadWidth(s); } catch (_) {}
  $('#thread-resize').on('mousedown', function (e) {
    e.preventDefault();
    $(this).addClass('dragging');
    const startX = e.clientX, startW = $('#chat-thread').outerWidth();
    function onMove(ev) { applyThreadWidth(startW - (ev.clientX - startX)); }
    function onUp() {
      $('#thread-resize').removeClass('dragging');
      $(document).off('mousemove.thres mouseup.thres');
      try { localStorage.setItem(THREAD_W_KEY, String($('#chat-thread').outerWidth())); } catch (_) {}
    }
    $(document).on('mousemove.thres', onMove).on('mouseup.thres', onUp);
  });

  // --- SQL console (read-only over messenger.db) ---
  // openQuery shows the console; on first open it seeds a default query and
  // loads the schema (which is just another read-only query).
  function openQuery() {
    if (!$('#query-sql').val().trim()) $('#query-sql').val(DEFAULT_QUERY);
    if (!querySchemaLoaded) loadQuerySchema();
    setTimeout(() => $('#query-sql').focus(), 0);
  }
  // runQuery POSTs the editor's SQL and renders columns + rows as a table.
  function runQuery() {
    const sql = $('#query-sql').val().trim();
    if (!sql) return;
    $('#query-status').text('running…');
    $.ajax({
      url: '/api/db/query', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ sql }),
    }).done((res) => {
      renderQueryTable($('#query-results'), res);
      const n = (res.rows || []).length;
      $('#query-status').text(n + ' row' + (n === 1 ? '' : 's') + (res.truncated ? ' (capped)' : ''));
    }).fail((xhr) => {
      $('#query-results').empty();
      $('#query-status').html('<span class="ax-query-err">' + esc(errorOf(xhr)) + '</span>');
    });
  }
  $('#query-run').on('click', runQuery);
  // Cmd/Ctrl+Enter runs; the editor otherwise behaves as a plain textarea.
  $('#query-sql').on('keydown', function (e) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); runQuery(); }
  });
  // Clicking a schema table name drops a SELECT for it into the editor.
  $('#query-schema').on('click', '.ax-query-tbl', function () {
    $('#query-sql').val('SELECT * FROM ' + $(this).attr('data-tbl') + ' LIMIT 50;').focus();
  });

  // loadQuerySchema lists tables + columns via PRAGMA (read-only) and renders a
  // reference panel.
  function loadQuerySchema() {
    querySchemaLoaded = true;
    $.ajax({
      url: '/api/db/query', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ sql: "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name" }),
    }).done((res) => {
      const tables = (res.rows || []).map((r) => r[0]);
      // Pull columns per table (PRAGMA table_info) sequentially-ish.
      let html = '<div class="ax-query-schema-title">schema</div>';
      let pending = tables.length;
      if (!pending) { $('#query-schema').html(html + '<div class="ax-query-empty">no tables</div>'); return; }
      const cols = {};
      tables.forEach((t) => {
        $.ajax({
          url: '/api/db/query', method: 'POST', contentType: 'application/json',
          data: JSON.stringify({ sql: 'PRAGMA table_info(' + t + ')' }),
        }).always((res2) => {
          cols[t] = (res2 && res2.rows) ? res2.rows.map((r) => r[1]) : [];
          if (--pending === 0) {
            for (const t2 of tables) {
              html += '<div class="ax-query-tbl" data-tbl="' + esc(t2) + '"># ' + esc(t2) + '</div>';
              html += '<div class="ax-query-cols">' + (cols[t2] || []).map(esc).join(', ') + '</div>';
            }
            $('#query-schema').html(html);
          }
        });
      });
    }).fail(() => { $('#query-schema').html('<div class="ax-query-empty">schema unavailable</div>'); });
  }

  // renderQueryTable paints a QueryResult as a scrollable HTML table.
  function renderQueryTable($el, res) {
    const cols = (res && res.columns) || [];
    const rows = (res && res.rows) || [];
    if (!cols.length) { $el.html('<div class="ax-query-empty">no columns</div>'); return; }
    let html = '<table class="ax-query-table"><thead><tr>';
    for (const c of cols) html += '<th>' + esc(c) + '</th>';
    html += '</tr></thead><tbody>';
    for (const row of rows) {
      html += '<tr>';
      for (const v of row) {
        const s = v == null ? '' : String(v);
        html += '<td title="' + esc(s) + '">' + esc(s.length > 200 ? s.slice(0, 200) + '…' : s) + '</td>';
      }
      html += '</tr>';
    }
    html += '</tbody></table>';
    $el.html(html);
  }

  // --- OIDC provider admin ---
  // openOIDC loads this peer's issuer + client list into the tab.
  function openOIDC() {
    $('#oidc-err').text('');
    $('#oidc-new').addClass('hidden').empty();
    $.getJSON('/api/oidc').done(function (d) {
      $('#oidc-issuer').text(d.issuer || '(not in a camp)');
      $('#oidc-discovery').text(d.discovery || '—');
      $('#oidc-endsession').text(d.issuer ? d.issuer + '/end_session' : '—');
      oidcUsers = (d && d.users) || [];
      renderOIDCClients(d.clients || []);
      renderOIDCUsers(oidcUsers);
    }).fail(function (x) {
      $('#oidc-err').text('load failed: ' + (x.responseText || x.status));
    });
  }

  function renderOIDCClients(clients) {
    const $c = $('#oidc-clients');
    if (!clients.length) { $c.html('<div class="ax-oidc-empty">no applications yet</div>'); return; }
    $c.empty();
    for (const cl of clients) {
      const kind = cl.confidential ? 'confidential' : 'public';
      const pkceTag = cl.pkce ? '<span class="ax-oidc-ckind">PKCE</span>' : '';
      const $row = $(
        '<div class="ax-oidc-client">' +
          '<div class="ax-oidc-cmeta">' +
            '<span class="ax-oidc-cname"></span>' +
            '<span class="ax-oidc-ckind">' + kind + '</span>' + pkceTag +
            '<button class="ax-oidc-del" title="delete">✕</button>' +
          '</div>' +
          '<div class="ax-oidc-cid">client_id: <code></code></div>' +
          '<div class="ax-oidc-csecret">client_secret: <code></code></div>' +
          '<div class="ax-oidc-clabel">callback URLs</div>' +
          '<div class="ax-oidc-cred"></div>' +
          '<div class="ax-oidc-clabel ax-oidc-llabel">logout URLs</div>' +
          '<div class="ax-oidc-cred ax-oidc-lred"></div>' +
        '</div>');
      $row.find('.ax-oidc-cname').text(cl.client_name || '(unnamed)');
      $row.find('.ax-oidc-cid code').text(cl.client_id);
      if (cl.client_secret) {
        $row.find('.ax-oidc-csecret code').text(cl.client_secret);
      } else {
        $row.find('.ax-oidc-csecret').remove();
      }
      $row.find('.ax-oidc-cred').not('.ax-oidc-lred').text((cl.redirect_uris || []).join('\n'));
      const logouts = cl.logout_uris || [];
      if (logouts.length) {
        $row.find('.ax-oidc-lred').text(logouts.join('\n'));
      } else {
        $row.find('.ax-oidc-llabel, .ax-oidc-lred').remove();
      }
      $row.find('.ax-oidc-del').on('click', function () {
        if (!confirm('Delete client "' + (cl.client_name || cl.client_id) + '"?')) return;
        $.ajax({ url: '/api/oidc/clients/' + encodeURIComponent(cl.client_id), method: 'DELETE' })
          .done(openOIDC)
          .fail(function (x) { $('#oidc-err').text('delete failed: ' + (x.responseText || x.status)); });
      });
      $c.append($row);
    }
  }

  // renderOIDCUsers lists peers that have enrolled a passkey — the IdP's
  // notion of a "registered user" (there's no user table; identity is the
  // camp pub). Their app accounts live in each relying app's own DB.
  function renderOIDCUsers(users) {
    const $u = $('#oidc-users');
    if (!users.length) { $u.html('<div class="ax-oidc-empty">no passkeys enrolled yet</div>'); return; }
    $u.empty();
    for (const u of users) {
      const n = u.credentials || 0;
      const $row = $(
        '<div class="ax-oidc-client">' +
          '<div class="ax-oidc-cmeta">' +
            '<span class="ax-oidc-cname"></span>' +
            '<span class="ax-oidc-ckind"></span>' +
          '</div>' +
          '<div class="ax-oidc-cid">pub: <code></code></div>' +
        '</div>');
      $row.find('.ax-oidc-cname').text(u.name || '(unnamed)');
      $row.find('.ax-oidc-ckind').text(n + ' passkey' + (n === 1 ? '' : 's'));
      $row.find('.ax-oidc-cid code').text(u.pub);
      $u.append($row);
    }
  }

  function createOIDCClient() {
    $('#oidc-err').text('');
    const name = $('#oidc-name').val().trim();
    const lines = (id) => $(id).val().split('\n').map(s => s.trim()).filter(Boolean);
    const redirects = lines('#oidc-redirects');
    if (!redirects.length) { $('#oidc-err').text('at least one callback URL required'); return; }
    $.ajax({
      url: '/api/oidc/clients', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({
        name: name,
        redirect_uris: redirects,
        logout_uris: lines('#oidc-logout'),
        public: $('#oidc-public').is(':checked'),
        pkce: $('#oidc-pkce').is(':checked'),
      }),
    }).done(function (d) {
      // Show the credentials once — the secret is never retrievable again.
      let html = '<div class="ax-oidc-label">client registered</div>' +
        '<div>client_id: <code>' + esc(d.client_id) + '</code></div>';
      if (d.client_secret) {
        html += '<div>client_secret: <code>' + esc(d.client_secret) + '</code></div>';
      }
      $('#oidc-new').removeClass('hidden').html(html);
      $('#oidc-name').val(''); $('#oidc-redirects').val(''); $('#oidc-logout').val('');
      // Refresh only the list — don't clear the one-time secret panel.
      $.getJSON('/api/oidc').done(function (d) { renderOIDCClients(d.clients || []); });
    }).fail(function (x) {
      $('#oidc-err').text('create failed: ' + (x.responseText || x.status));
    });
  }

  $('#oidc-create').on('click', createOIDCClient);
  $('#oidc-copy').on('click', function () {
    const t = $('#oidc-issuer').text();
    if (t && navigator.clipboard) navigator.clipboard.writeText(t);
  });

  // renderThread paints the open thread: the root message, an "N replies"
  // divider, then the replies (oldest-first), all with edits applied. If the
  // root isn't loaded yet (deep-link before history lands) it shows a stub and
  // is re-rendered once loadConversation completes.
  function renderThread() {
    if (!threadRoot) return;
    const $tm = $('#thread-messages');
    const edits = editsByRoot(chatMsgs);
    const rootRaw = chatMsgs.find((x) => x.id === threadRoot && !x.edit_id);
    if (!rootRaw) { $tm.html('<div class="ax-msg-system">loading thread…</div>'); $('#thread-title').text('Thread'); return; }
    const replies = chatMsgs
      .filter((x) => !x.edit_id && x.thread === threadRoot)
      .sort((a, b) => (a.ts - b.ts) || (a.id < b.id ? -1 : 1));
    let html = msgRow(applyEdit(rootRaw, edits), false, 0, true);
    html += `<div class="ax-thread-divider">${replies.length} ${replies.length === 1 ? 'reply' : 'replies'}</div>`;
    for (const r of replies) html += msgRow(applyEdit(r, edits), false, 0, true);
    $tm.html(html);
    if (window.f2fRich) f2fRich.renderDiagrams($tm[0]);
    mountSandboxes($tm[0]);
    // Scroll to the latest reply. Deferred to the next frame: the panel may
    // have just been shown (display flipped), so its flex layout — and thus
    // scrollHeight — isn't final until layout settles.
    requestAnimationFrame(() => { const el = $tm[0]; if (el) el.scrollTop = el.scrollHeight; });
    $('#thread-title').text('Thread · ' + replies.length);
  }
  // Thread composer: posts into the open thread (thread=<rootId>).
  $('#thread-form').on('submit', function (e) {
    e.preventDefault();
    if (!chatConv || !threadRoot) return;
    const $in = $('#thread-input');
    const text = $in.val().trim();
    if (!text) return;
    $in.val(''); $in.css('height', 'auto');
    $.ajax({
      url: '/api/messages', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ kind: chatConv.kind, key: chatConv.key, body: text, thread: threadRoot }),
    }).fail((xhr) => alert('send: ' + errorOf(xhr)));
  });
  $('#thread-input').on('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); $('#thread-form').trigger('submit'); }
  });
  $('#thread-input').on('input', function () {
    this.style.height = 'auto'; this.style.height = Math.min(this.scrollHeight, 160) + 'px';
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
  // appendPending shows an optimistic "sending…" bubble the instant you hit
  // Enter — so there's immediate feedback and you don't retype (the cause of
  // duplicate sends). The next full chat redraw (the echoed message arriving
  // over SSE) replaces it; a failed send marks it instead of silently vanishing.
  function appendPending(text) {
    const $m = $('#chat-messages');
    if (!$m.length) return null;
    const $b = $('<div class="ax-msg is-mine ax-msg-pending">' +
      '<div class="ax-msg-text"></div>' +
      '<div class="ax-msg-pendtag">отправляется…</div></div>').attr('data-pending-body', text);
    $b.find('.ax-msg-text').html(richBody(text));
    $m.append($b);
    $m.scrollTop($m[0].scrollHeight);
    return $b;
  }
  // appendPendingFile is the optimistic bubble for an attachment send: a local
  // thumbnail (image) or file card + "sending…", replaced in place by the echoed
  // message. data-pending-body carries the caption so matchPending pairs them.
  function appendPendingFile(file, caption) {
    const $m = $('#chat-messages');
    if (!$m.length) return null;
    const cap = caption ? '<div class="ax-msg-caption">' + richBody(caption) + '</div>' : '';
    let inner;
    if ((file.type || '').indexOf('image/') === 0) {
      const url = URL.createObjectURL(file); chatBlobUrls.push(url);
      inner = '<div class="ax-msg-media"><img class="ax-msg-img" src="' + url + '">' + cap + '</div>';
    } else {
      inner = '<div class="ax-msg-filecard"><a class="ax-msg-doc">' +
        '<span class="ax-msg-doc-ic"><i class="bi bi-file-earmark-fill"></i></span>' +
        '<span class="ax-msg-doc-meta"><span class="ax-msg-doc-name">' + esc(file.name || 'file') + '</span>' +
        '<span class="ax-msg-doc-sub">' + esc(fmtBytes(file.size || 0)) + '</span></span></a>' + cap + '</div>';
    }
    const $b = $('<div class="ax-msg is-mine ax-msg-pending">' + inner +
      '<div class="ax-msg-pendtag">отправляется…</div></div>').attr('data-pending-body', caption || '');
    $m.append($b);
    $m.scrollTop($m[0].scrollHeight);
    return $b;
  }
  // matchPending finds the optimistic bubble an arriving own-message replaces:
  // the one whose text matches, else the oldest (body may be normalised in
  // transit) — one own-echo = one staged bubble. Returns an empty set if none.
  function matchPending(body) {
    const $all = $('#chat-messages .ax-msg-pending');
    if (!$all.length) return $();
    const want = (body || '').trim();
    const $hit = $all.filter(function () { return ($(this).attr('data-pending-body') || '').trim() === want; }).first();
    return $hit.length ? $hit : $all.first();
  }

  $('#chat-form').on('submit', function (e) {
    e.preventDefault();
    if (!chatConv) return;
    const $in = $('#chat-input');
    const text = $in.val().trim();
    const files = chatPending.slice();
    if (!text && !files.length) return;
    const reply_to = replyId(), thread = threadId(), edit_id = editId();
    // Staged attachments: send each (first carries the typed text as caption).
    if (files.length) {
      $in.val(''); autoGrowInput();
      clearReplyTarget();
      clearChatPending();
      files.forEach((f, i) => {
        const cap = i === 0 ? text : '';
        appendPendingFile(f, cap); // optimistic "sending…" with a local thumbnail
        attachFrom(f, cap, { reply_to, thread, edit_id: i === 0 ? edit_id : '' });
      });
      return;
    }
    $in.val('');
    autoGrowInput();
    clearReplyTarget();
    const $pending = edit_id ? null : appendPending(text); // optimistic, new sends only
    $.ajax({
      url: '/api/messages', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ kind: chatConv.kind, key: chatConv.key, body: text, reply_to, thread, edit_id }),
    }).fail((xhr) => {
      if ($pending) $pending.addClass('ax-msg-failed').find('.ax-msg-pendtag').text('не отправлено');
      alert('send: ' + errorOf(xhr));
    });
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
  // (base64) on the message; larger ones are seeded and shared over torrent.
  // sendInline/sendShare/attachFrom take a context {reply_to,thread,edit_id} so
  // the main composer and the thread composer share ONE code path.
  const MAX_ATTACH = 8 * 1024 * 1024; // matches the backend's inline cap
  function sendInline(file, caption, ctx) {
    const reader = new FileReader();
    reader.onload = function () {
      // readAsDataURL gives "data:<mime>;base64,<payload>" — keep the payload;
      // Go decodes the []byte field straight from that base64 string.
      const b64 = String(reader.result).split(',', 2)[1] || '';
      $.ajax({
        url: '/api/messages', method: 'POST', contentType: 'application/json',
        data: JSON.stringify(Object.assign({
          kind: chatConv.kind, key: chatConv.key, body: caption,
          file: { name: file.name, mime: file.type || 'application/octet-stream', data: b64 },
        }, ctx)),
      }).fail((xhr) => alert('send: ' + errorOf(xhr)));
    };
    reader.onerror = function () { alert('could not read file'); };
    reader.readAsDataURL(file);
  }
  // Seed a large file and post it as a torrent message (multipart upload). The
  // backend pins the seed to this conversation (private to it) and echoes the
  // message back, where it renders with a download affordance + live status.
  function sendShare(file, caption, ctx) {
    const fd = new FormData();
    fd.append('kind', chatConv.kind);
    fd.append('key', chatConv.key);
    fd.append('body', caption);
    fd.append('file', file);
    if (ctx.reply_to) fd.append('reply_to', ctx.reply_to);
    if (ctx.thread) fd.append('thread', ctx.thread);
    if (ctx.edit_id) fd.append('edit_id', ctx.edit_id);
    $.ajax({ url: '/api/messages/share', method: 'POST', data: fd, processData: false, contentType: false })
      .fail((xhr) => alert('share: ' + errorOf(xhr)));
  }
  function attachFrom(file, caption, ctx) {
    if (file.size > MAX_ATTACH) sendShare(file, caption, ctx);
    else sendInline(file, caption, ctx);
  }

  // Staged attachments for the main composer: picking/pasting a file doesn't
  // send — it stages a preview chip, and the typed text becomes the caption on
  // Enter. (The thread composer still sends immediately.) State declared up top
  // so clearChatPending (called from applyRoute at startup) isn't in the TDZ.
  function stageChatFiles(files) {
    if (!chatConv || !files || !files.length) return;
    for (const f of files) if (f) chatPending.push(f);
    renderChatPreview();
    $('#chat-input').focus();
  }
  function clearChatPending() {
    chatPendingUrls.forEach((u) => { try { URL.revokeObjectURL(u); } catch (_) {} });
    chatPendingUrls = [];
    chatPending = [];
    $('#chat-attach-preview').addClass('hidden').empty();
  }
  function renderChatPreview() {
    const $p = $('#chat-attach-preview');
    chatPendingUrls.forEach((u) => { try { URL.revokeObjectURL(u); } catch (_) {} });
    chatPendingUrls = [];
    if (!chatPending.length) { $p.addClass('hidden').empty(); return; }
    $p.empty().removeClass('hidden');
    chatPending.forEach((f, i) => {
      const $chip = $('<div class="ax-chat-att-chip"></div>').attr('data-i', i);
      if ((f.type || '').indexOf('image/') === 0) {
        const url = URL.createObjectURL(f); chatPendingUrls.push(url);
        $chip.append($('<img class="ax-chat-att-thumb">').attr('src', url));
      } else {
        $chip.append('<span class="ax-chat-att-ic"><i class="bi bi-file-earmark-fill"></i></span>');
      }
      $chip.append($('<span class="ax-chat-att-name"></span>').text(f.name || 'file'));
      $chip.append($('<span class="ax-chat-att-sz"></span>').text(fmtBytes(f.size || 0)));
      $chip.append('<button type="button" class="ax-chat-att-x" title="remove">✕</button>');
      $p.append($chip);
    });
  }
  $('#chat-attach-preview').on('click', '.ax-chat-att-x', function () {
    const i = parseInt($(this).closest('.ax-chat-att-chip').attr('data-i'), 10);
    if (i >= 0) chatPending.splice(i, 1);
    renderChatPreview();
  });

  // Telegram-style drag-and-drop: drop a file anywhere over the chat frame to
  // stage it (preview + send on Enter), with an overlay while dragging files.
  // dragenter/over/leave fire per child element, so count depth to avoid flicker.
  let chatDragDepth = 0;
  function dragHasFiles(e) {
    const t = e.originalEvent && e.originalEvent.dataTransfer && e.originalEvent.dataTransfer.types;
    return !!t && Array.prototype.indexOf.call(t, 'Files') !== -1;
  }
  $('#chat-frame')
    .on('dragenter', function (e) {
      if (!chatConv || !dragHasFiles(e)) return;
      e.preventDefault(); chatDragDepth++; $('#chat-drop-overlay').removeClass('hidden');
    })
    .on('dragover', function (e) { if (chatConv && dragHasFiles(e)) e.preventDefault(); })
    .on('dragleave', function () { if (--chatDragDepth <= 0) { chatDragDepth = 0; $('#chat-drop-overlay').addClass('hidden'); } })
    .on('drop', function (e) {
      chatDragDepth = 0; $('#chat-drop-overlay').addClass('hidden');
      const dt = e.originalEvent && e.originalEvent.dataTransfer;
      if (!chatConv || !dt || !dt.files || !dt.files.length) return;
      e.preventDefault();
      stageChatFiles(Array.from(dt.files));
    });

  // Main composer attach → stage (don't send).
  $('#chat-attach').on('click', function () { if (chatConv) $('#chat-file').trigger('click'); });
  $('#chat-file').on('change', function () {
    const files = Array.from(this.files || []);
    this.value = '';
    stageChatFiles(files);
  });
  // Thread composer attach — same flow, scoped to the open thread.
  $('#thread-attach').on('click', function () { if (chatConv && threadRoot) $('#thread-file').trigger('click'); });
  $('#thread-file').on('change', function () {
    const file = this.files && this.files[0];
    this.value = '';
    if (!file || !chatConv || !threadRoot) return;
    const caption = $('#thread-input').val().trim();
    $('#thread-input').val('');
    attachFrom(file, caption, { thread: threadRoot });
  });

  // clipboardFiles pulls any files off a paste/drop event. Pasted screenshots
  // arrive with no filename, so synthesize one from the mime type.
  function clipboardFiles(cd) {
    if (!cd) return [];
    const out = [];
    if (cd.files && cd.files.length) {
      for (let i = 0; i < cd.files.length; i++) out.push(cd.files[i]);
    } else if (cd.items) {
      for (let i = 0; i < cd.items.length; i++) {
        const it = cd.items[i];
        if (it.kind === 'file') { const f = it.getAsFile(); if (f) out.push(f); }
      }
    }
    return out.map((f) => {
      if (f.name) return f;
      const ext = ((f.type || '').split('/')[1] || 'bin').replace('+xml', '');
      try { return new File([f], 'pasted-' + Date.now() + '.' + ext, { type: f.type }); } catch (_) { return f; }
    });
  }
  // Paste images/files straight into the composer (Ctrl/Cmd+V). Plain-text
  // pastes fall through untouched. The first file carries the typed caption.
  $('#chat-input').on('paste', function (e) {
    if (!chatConv) return;
    const files = clipboardFiles(e.originalEvent && e.originalEvent.clipboardData);
    if (!files.length) return;
    e.preventDefault();
    stageChatFiles(files); // stage; the typed text stays as the caption, send on Enter
  });
  $('#thread-input').on('paste', function (e) {
    if (!chatConv || !threadRoot) return;
    const files = clipboardFiles(e.originalEvent && e.originalEvent.clipboardData);
    if (!files.length) return;
    e.preventDefault();
    const caption = $('#thread-input').val().trim();
    $('#thread-input').val('');
    files.forEach((f, i) => attachFrom(f, i === 0 ? caption : '', { thread: threadRoot }));
  });

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
    // Note file-block: the seeder is the block's creator (first version author).
    const $nb = $chip.closest('.ax-nb');
    if ($nb.length) {
      const b = noteBlocks.find((x) => x.bid === $nb.attr('data-bid'));
      if (b && b.history && b.history.length) add(b.history[0].author);
    }
    if (chatConv && chatConv.kind === 'channel') {
      const ch = chatChannels.find((c) => c.id === chatConv.key);
      ((ch && ch.members) || []).forEach(add);
    }
    return ips;
  }
  function updateTorrentChips() {
    // Torrent chips live in chat messages and in note file-blocks alike.
    $('.ax-msg-torrent').each(function () {
      const $c = $(this);
      const mine = $c.closest('.ax-msg').hasClass('is-mine') || $c.closest('[data-mine="1"]').length > 0;
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
    if (!$('.ax-msg-torrent').length) return;
    $.getJSON('/api/files/downloads', function (list) {
      const m = {};
      (Array.isArray(list) ? list : []).forEach((d) => { m[d.info_hash] = d; });
      torrentStatus = m;
      updateTorrentChips();
    });
  }
  $('#tab-chat, #tab-note').on('click', '.ax-msg-torrent-dl', function () {
    const $c = $(this).closest('.ax-msg-torrent');
    const magnet = $c.attr('data-magnet');
    if (!magnet) return;
    $c.find('.ax-msg-torrent-status').html('<span class="t-wait">starting…</span>');
    $.ajax({
      url: '/api/files/download', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ magnet: magnet, peers: torrentPeers($c) }),
    }).done(pollTorrents).fail((xhr) => alert('download: ' + errorOf(xhr)));
  });
  $('#tab-chat, #tab-note').on('click', '.ax-msg-torrent-open', function () {
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
  let oidcClients = [];    // /api/oidc clients, for the sidebar list
  let oidcUsers = [];      // /api/oidc passkey users, for the OIDC tab
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
  // renderAccount fills the sidebar header plaque (avatar + name + camp/peer) —
  // the messenger-style account chip. Avatar is initials on a colour derived
  // from the identity pub (stable per identity); a real avatar comes later.
  function avatarColor(seed) {
    let h = 0; const str = String(seed || '');
    for (let i = 0; i < str.length; i++) h = (h * 31 + str.charCodeAt(i)) >>> 0;
    return 'hsl(' + (h % 360) + ', 42%, 46%)';
  }
  function avatarInitials(name) {
    const p = String(name || '?').trim().split(/\s+/);
    return ((p[0] || '?')[0] + (p[1] ? p[1][0] : '')).toUpperCase();
  }
  function profileName() {
    if (!profile) return '';
    return [profile.first, profile.last].filter(Boolean).join(' ').trim();
  }
  function renderAccount(s) {
    const $av = $('#ax-account-avatar'), $nm = $('#ax-account-name'),
      $sub = $('#ax-account-sub'), $dev = $('#ax-account-dev');
    if (!s || !s.running) {
      $nm.text('no camp');
      $sub.text('engine stopped');
      $dev.text('');
      $av.text('·').css('background', 'var(--panel-2)');
      return;
    }
    // Prefer the profile's full name; fall back to the peer name (= username).
    const name = profileName() || s.camp_name || 'me';
    const camp = s.camp_label || s.camp_id || '';
    // Device line: peer name @ overlay ip — this machine = this peer (= user).
    const self = (s.peers || []).find((p) => p.self);
    const ip = (self && self.overlay_v4) || s.local_ip || '';
    const dev = [s.camp_name, ip].filter(Boolean).join('@');
    $nm.text(name);
    $sub.text(camp ? 'camp ' + camp : '');
    $dev.text(dev);
    $av.text(avatarInitials(name)).css('background', avatarColor(s.identity_pub || name));
  }

  // Profile: a page (#tab-profile) in the main pane, reached via the account
  // plaque. Creating one is optional — we just load it if it exists (to fill
  // the plaque) and otherwise leave it empty; the page works for create or edit.
  function ensureProfile(s) {
    if (!s || !s.running || profileChecked) return;
    profileChecked = true;
    $.getJSON('/api/profile').done((p) => {
      if (p && p.exists) {
        profile = p;
        hideOnboarding();
        renderAccount(lastStatus);
        // If the profile page is already open (deep-link / it rendered before
        // the fetch landed), repaint it now that we have the data.
        if (!$('#tab-profile').hasClass('hidden')) fillProfilePage();
      } else {
        // No profile yet → mandatory full-screen onboarding (name + passkey).
        showOnboarding(p);
      }
    }).fail(() => { profileChecked = false; }); // retry next tick on error
  }
  // fillProfilePage paints the form for create (no profile) or edit (existing):
  // pre-fills first/last and the device section.
  function fillProfilePage() {
    const editing = !!profile;
    $('#profile-title').text(editing ? 'Профиль' : 'Создайте профиль');
    $('#profile-err').text('');
    $('#profile-first').val(editing ? (profile.first || '') : '');
    $('#profile-last').val(editing ? (profile.last || '') : '');
    $('#profile-save').text(editing ? 'Сохранить' : 'Создать');
    renderProfileDevices();
  }
  // renderProfileDevices fills the "this device" section from /api/status —
  // name (editable, live rename), overlay IP, key fingerprint, passkey state.
  // Peer = user, so there's no multi-device list.
  function renderProfileDevices() {
    const s = lastStatus || {};
    const self = (s.peers || []).find((p) => p.self) || {};
    const ip = self.overlay_v4 || s.local_ip || '—';
    const name = s.camp_name || '—';
    const fp = s.identity_fp || (s.identity_pub || '').slice(0, 16) || '—';
    const row = (k, v, mono) => '<div class="ax-prof-row"><span class="ax-prof-k">' + esc(k) +
      '</span><span class="ax-prof-v' + (mono ? ' ax-prof-mono' : '') + '">' + esc(v) + '</span></div>';
    // Device name is editable (live rename → re-announce); value set via .val()
    // afterwards to avoid attribute-injection.
    $('#profile-device').html(
      '<div class="ax-prof-row"><span class="ax-prof-k">Имя устройства</span>' +
      '<span class="ax-prof-v ax-prof-edit"><input id="device-name" class="ax-prof-input" maxlength="64">' +
      '<button type="button" id="device-rename" class="ax-prof-mini" title="переименовать">✓</button></span></div>' +
      '<div class="ax-prof-err" id="device-err"></div>' +
      row('Overlay IP', ip, true) + row('Отпечаток ключа', fp, true) +
      '<div class="ax-prof-row"><span class="ax-prof-k">Passkey</span>' +
      '<span class="ax-prof-v" id="passkey-cell"></span></div>' +
      '<div class="ax-prof-err" id="passkey-err"></div>');
    $('#device-name').val(name);
    if (profile && profile.has_passkey) $('#passkey-cell').html('<span class="ax-prof-ok">✓ создан</span>');
    else $('#passkey-cell').html('<button type="button" id="passkey-create" class="ax-prof-mini">Создать passkey</button>');
  }
  // Clicking the account plaque opens the profile page (edit). Leaving without
  // saving = just navigate away via the sidebar (no explicit cancel needed).
  $('#ax-account').on('click', function () { if (lastStatus && lastStatus.running) location.hash = 'profile'; });
  // Rename this device (live: re-announced to peers).
  $('#tab-profile').on('click', '#device-rename', function () {
    const name = $('#device-name').val().trim();
    $('#device-err').text('');
    if (!name) { $('#device-err').text('Имя устройства обязательно'); return; }
    const $btn = $('#device-rename').prop('disabled', true);
    $.ajax({
      url: '/api/profile/device', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ name }),
    }).done((r) => {
      if (lastStatus) lastStatus.camp_name = (r && r.name) || name; // instant feedback
      renderAccount(lastStatus);
      renderProfileDevices();
    }).fail((x) => { $('#device-err').text(errorOf(x)); })
      .always(() => { $btn.prop('disabled', false); });
  });

  // WebAuthn base64url ↔ ArrayBuffer (the server speaks base64url per the spec).
  function b64urlToBuf(s) {
    s = String(s).replace(/-/g, '+').replace(/_/g, '/');
    const pad = s.length % 4 ? '='.repeat(4 - (s.length % 4)) : '';
    const bin = atob(s + pad), b = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) b[i] = bin.charCodeAt(i);
    return b.buffer;
  }
  function bufToB64url(buf) {
    const b = new Uint8Array(buf); let s = '';
    for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  // Create a passkey for this device (registers with the local IdP credstore).
  $('#tab-profile').on('click', '#passkey-create', function () {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      $('#passkey-err').text('Этот браузер не поддерживает passkey'); return;
    }
    $('#passkey-err').text('');
    const $b = $('#passkey-create').prop('disabled', true).text('…');
    $.ajax({ url: '/api/profile/passkey/begin', method: 'POST' }).then((opts) => {
      const pk = opts.publicKey;
      pk.challenge = b64urlToBuf(pk.challenge);
      pk.user.id = b64urlToBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach((c) => { c.id = b64urlToBuf(c.id); });
      return navigator.credentials.create({ publicKey: pk });
    }).then((cred) => {
      const body = {
        id: cred.id, type: cred.type, rawId: bufToB64url(cred.rawId),
        response: {
          attestationObject: bufToB64url(cred.response.attestationObject),
          clientDataJSON: bufToB64url(cred.response.clientDataJSON),
        },
      };
      return $.ajax({
        url: '/api/profile/passkey/finish', method: 'POST',
        contentType: 'application/json', data: JSON.stringify(body),
      });
    }).then(() => {
      if (profile) profile.has_passkey = true;
      renderProfileDevices();
    }, (e) => {
      const msg = (e && e.name === 'NotAllowedError') ? 'отменено или истекло время'
        : ((e && e.responseText) || (e && e.message) || 'не удалось');
      $('#passkey-err').text(msg);
      $b.prop('disabled', false).text('Создать passkey');
    });
  });
  $('#profile-form').on('submit', function (e) {
    e.preventDefault();
    const first = $('#profile-first').val().trim();
    const last = $('#profile-last').val().trim();
    $('#profile-err').text('');
    if (!first) { $('#profile-err').text('Имя обязательно'); return; }
    const $btn = $('#profile-save').prop('disabled', true).text('Сохраняю…');
    $.ajax({
      url: '/api/profile', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ first, last }),
    }).done((p) => {
      profile = p;
      profileRequired = false;
      renderAccount(lastStatus);
      location.hash = ''; // leave the profile page
    }).fail((x) => {
      $('#profile-err').text(errorOf(x));
    }).always(() => { $btn.prop('disabled', false).text(profile ? 'Сохранить' : 'Создать'); });
  });

  // --- Onboarding: mandatory full-screen profile + passkey on first run ---
  function showOnboarding(p) {
    // A passkey may already exist (created, then reloaded before saving the
    // profile) — pre-mark it so the user isn't asked to register a duplicate.
    onboardPkDone = !!(p && p.has_passkey);
    if (onboardPkDone) $('#onboard-pk-cell').html('<span class="ax-prof-ok">✓ создан</span>');
    $('#onboarding').removeClass('hidden');
    setTimeout(() => $('#onboard-first').trigger('focus'), 0);
    updateOnboardSave();
  }
  function hideOnboarding() { $('#onboarding').addClass('hidden'); }
  function updateOnboardSave() {
    const ok = $('#onboard-first').val().trim() && onboardPkDone;
    $('#onboard-save').prop('disabled', !ok);
  }
  $('#onboard-first').on('input', updateOnboardSave);
  $('#onboard-pk-create').on('click', function () {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      $('#onboard-err').text('Этот браузер не поддерживает passkey'); return;
    }
    $('#onboard-err').text('');
    const $b = $('#onboard-pk-create').prop('disabled', true).text('…');
    $.ajax({ url: '/api/profile/passkey/begin', method: 'POST' }).then((opts) => {
      const pk = opts.publicKey;
      pk.challenge = b64urlToBuf(pk.challenge);
      pk.user.id = b64urlToBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach((c) => { c.id = b64urlToBuf(c.id); });
      return navigator.credentials.create({ publicKey: pk });
    }).then((cred) => {
      const body = {
        id: cred.id, type: cred.type, rawId: bufToB64url(cred.rawId),
        response: {
          attestationObject: bufToB64url(cred.response.attestationObject),
          clientDataJSON: bufToB64url(cred.response.clientDataJSON),
        },
      };
      return $.ajax({
        url: '/api/profile/passkey/finish', method: 'POST',
        contentType: 'application/json', data: JSON.stringify(body),
      });
    }).then(() => {
      onboardPkDone = true;
      $('#onboard-pk-cell').html('<span class="ax-prof-ok">✓ создан</span>');
      updateOnboardSave();
    }, (e) => {
      const msg = (e && e.name === 'NotAllowedError') ? 'отменено или истекло время'
        : ((e && e.responseText) || (e && e.message) || 'не удалось');
      $('#onboard-err').text(msg);
      $b.prop('disabled', false).text('Создать passkey');
    });
  });
  $('#onboard-form').on('submit', function (e) {
    e.preventDefault();
    const first = $('#onboard-first').val().trim();
    const last = $('#onboard-last').val().trim();
    $('#onboard-err').text('');
    if (!first) { $('#onboard-err').text('Имя обязательно'); return; }
    if (!onboardPkDone) { $('#onboard-err').text('Сначала создайте passkey'); return; }
    const $btn = $('#onboard-save').prop('disabled', true).text('Сохраняю…');
    $.ajax({
      url: '/api/profile', method: 'POST', contentType: 'application/json',
      data: JSON.stringify({ first, last }),
    }).done((p) => {
      profile = p;
      hideOnboarding();
      renderAccount(lastStatus);
    }).fail((x) => {
      $('#onboard-err').text(errorOf(x));
      $btn.prop('disabled', false).text('Готово');
    });
  });

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
    renderAccount(s);
    ensureProfile(s); // force profile creation if there's none yet
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
    blob:     'bi-database-fill',
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

  let lastTreeHtml = ''; // memo: skip the tree DOM swap when nothing changed
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
        // NB: no live rtt here — it churns every poll and would defeat the
        // tree's "skip rebuild when unchanged" memo, making the sidebar flicker.
        const meta = ip;
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

    // CHANNELS = the rooms we belong to (from /api/channels). A name may
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
      + section('direct')  + directsBody
      + addRow('query history →', 'query'); // read-only SQL console over messenger.db

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

    const treeHtml = (
      category('peers',     'network',   peers.length, peersBody)
      + category('shells',    'terminals', shellList.length || null, shellsBody)
      + category('desktops',  'desktops',  vncList.length || null, desktopsBody)
      + category('messages',  'channels',  totalUnread || null, messagingBody)
      + category('drop',      'drop',      allFiles.length,
          section('available') + peerFilesBody
          + section('sharing') + addRow('add/remove file', 'drop') + myFilesBody)
      + category('domains',   'domains',   allDomains.length,
          addRow('add/remove domain', 'dns') + domainsBody
          + section('certificates') + trustedBody)
      + category('tunnel',    'tunnel',    (intercepts.length + allPorts.length) || null, tunnelBody)
      + category('blob',      'Blob Storage', null, empty('coming soon'))
      + category('oidc',      'OIDC',      oidcClients.length || null,
          addRow('manage applications →', 'oidc')
          + (oidcClients.length
              ? oidcClients.map(cl => row('online',
                  cl.client_name || (cl.client_id || '').slice(0, 8),
                  cl.confidential ? '' : 'public', null, 'oidc')).join('')
              : empty('no applications')))
      + category('secrets',   'secrets',   null, empty('coming soon'))
      + category('policies',  'policies',  null, empty('not configured'))
      + category('apps',      'apps',      null, empty('coming soon'))
    );
    // These timers fire several times a second; only touch the DOM when the
    // tree actually changed, else the wholesale rebuild makes rows/selects
    // flicker and resets hover/scroll. Route highlight is cheap & idempotent.
    if (treeHtml !== lastTreeHtml) { lastTreeHtml = treeHtml; $tree.html(treeHtml); }
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
        $('<td>').addClass(p.profile_name ? '' : 'muted').text(p.profile_name || '—'),
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
  function refreshOIDC() {
    $.getJSON('/api/oidc', (d) => {
      oidcClients = (d && d.clients) || [];
      oidcUsers = (d && d.users) || [];
      if (lastStatus) renderSidebarTree(lastStatus);
    }).fail(() => {});
  }
  refreshShellPeers();
  refreshVncPeers();
  refreshOIDC();
  setInterval(refreshShellPeers, 5000);
  setInterval(refreshVncPeers, 5000);
  setInterval(refreshOIDC, 5000);
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
