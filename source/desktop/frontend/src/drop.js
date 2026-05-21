// drop.js — UDP-only BitTorrent file sharing for desktop lite client.
//
// Ports the mac drop-tab UI one-to-one. Differences from mac:
//   - no /api/* HTTP; everything goes through Wails bindings
//     (MyFiles, AddMyFileBytes, RemoveMyFile, Library, StartDownload,
//     Downloads, Reveal).
//   - cross-peer catalog discovery uses 0xF3 state frames over the
//     hole-punched UDP socket, not HTTP polling.

import {
  MyFiles, AddMyFileBytes, RemoveMyFile,
  Library, StartDownload, Downloads, Reveal,
} from '../wailsjs/go/main/App';

export function startDrop() {
  const $ = (sel) => document.querySelector(sel);

  const $myList = $('#drop-my-list');
  const $myMeta = $('#drop-my-meta');
  const $libList = $('#drop-lib-list');
  const $libMeta = $('#drop-lib-meta');
  const $dlList = $('#drop-dl-list');
  const $dlMeta = $('#drop-dl-meta');
  const $zone = $('#drop-dropzone');
  const $input = $('#drop-fileinput');

  // ---- helpers ----
  function fmtBytes(n) {
    if (!Number.isFinite(n) || n <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    let v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + ' ' + units[i];
  }
  // Used everywhere a filename is shown: clicking it opens Finder
  // with the file selected. Falls through to plain text if there's no
  // local path (e.g. library entry not yet downloaded).
  function makeFileLink(name, path) {
    if (!path) {
      const span = document.createElement('span');
      span.className = 'ax-intercept-spec';
      span.textContent = name;
      return span;
    }
    const a = document.createElement('a');
    a.className = 'ax-intercept-spec ax-domain-link';
    a.href = '#';
    a.textContent = name;
    a.addEventListener('click', (e) => {
      e.preventDefault();
      Reveal(path).catch((err) => alert('Open in Finder failed: ' + (err?.message || err)));
    });
    return a;
  }

  // ---- my shared files ----
  async function refreshMyFiles() {
    let list;
    try { list = await MyFiles(); }
    catch { renderMyFiles([], 'torrent client not running'); return; }
    renderMyFiles(Array.isArray(list) ? list : []);
  }
  function renderMyFiles(list, errMsg) {
    $myMeta.textContent = list.length;
    $myList.replaceChildren();
    if (errMsg) {
      const empty = document.createElement('div');
      empty.className = 'ax-list-empty';
      empty.textContent = errMsg;
      $myList.appendChild(empty);
      return;
    }
    if (list.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'ax-list-empty';
      empty.textContent = 'nothing shared yet.';
      $myList.appendChild(empty);
      return;
    }
    list.forEach((f) => {
      const row = document.createElement('div');
      row.className = 'ax-intercept';
      const head = document.createElement('div');
      head.className = 'ax-intercept-head';
      head.style.cursor = 'default';
      const caret = document.createElement('span');
      caret.className = 'ax-intercept-caret'; caret.textContent = ' ';
      head.appendChild(caret);
      head.appendChild(makeFileLink(f.name, f.path));
      const size = document.createElement('span');
      size.className = 'ax-pill ax-pill-peer'; size.textContent = fmtBytes(f.size);
      head.appendChild(size);
      const hash = document.createElement('span');
      hash.className = 'ax-pill ax-pill-peer';
      hash.textContent = (f.info_hash || '').slice(0, 12);
      head.appendChild(hash);
      const meta = document.createElement('span');
      meta.className = 'ax-intercept-meta';
      head.appendChild(meta);
      const rm = document.createElement('button');
      rm.className = 'ax-list-remove';
      rm.textContent = 'remove';
      rm.addEventListener('click', async () => {
        try { await RemoveMyFile(f.info_hash); }
        catch (e) { alert('Remove failed: ' + (e?.message || e)); }
        refreshMyFiles();
      });
      head.appendChild(rm);
      row.appendChild(head);
      $myList.appendChild(row);
    });
  }

  // ---- camp library + active downloads ----
  // localDownloads is the latest Downloads() snapshot keyed by
  // info_hash so the library section knows what we already have on
  // disk and can render an "open" link instead of a download button.
  let localDownloads = {};
  let livePeers = [];

  async function refreshDownloads() {
    let list;
    try { list = await Downloads(); }
    catch { renderDownloads([], 'torrent client not running'); return; }
    const all = Array.isArray(list) ? list : [];
    localDownloads = {};
    for (const d of all) localDownloads[d.info_hash] = d;
    renderDownloads(all);
    refreshLibrary();
  }
  function renderDownloads(all, errMsg) {
    const arr = all.filter((d) => !d.complete);
    $dlMeta.textContent = arr.length;
    $dlList.replaceChildren();
    if (errMsg) {
      const empty = document.createElement('div');
      empty.className = 'ax-list-empty'; empty.textContent = errMsg;
      $dlList.appendChild(empty);
      return;
    }
    if (arr.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'ax-list-empty'; empty.textContent = 'no active downloads.';
      $dlList.appendChild(empty);
      return;
    }
    arr.forEach((d) => {
      const row = document.createElement('div');
      row.className = 'ax-intercept';
      const head = document.createElement('div');
      head.className = 'ax-intercept-head';
      head.style.cursor = 'default';
      const caret = document.createElement('span');
      caret.className = 'ax-intercept-caret'; caret.textContent = ' ';
      head.appendChild(caret);
      const name = document.createElement('span');
      name.className = 'ax-intercept-spec';
      name.textContent = d.name || (d.info_hash || '').slice(0, 12);
      head.appendChild(name);
      if (d.size) {
        const total = d.size;
        const done = d.bytes_completed || 0;
        const pct = Math.floor((done / total) * 100);
        const pctPill = document.createElement('span');
        pctPill.className = 'ax-pill ax-pill-active'; pctPill.textContent = pct + '%';
        head.appendChild(pctPill);
        const sizePill = document.createElement('span');
        sizePill.className = 'ax-pill ax-pill-peer';
        sizePill.textContent = fmtBytes(done) + ' / ' + fmtBytes(total);
        head.appendChild(sizePill);
      }
      const meta = document.createElement('span');
      meta.className = 'ax-intercept-meta';
      head.appendChild(meta);
      row.appendChild(head);
      $dlList.appendChild(row);
    });
  }

  async function refreshLibrary() {
    let lib;
    try { lib = await Library(); }
    catch { lib = []; }
    const rows = Array.isArray(lib) ? lib : [];
    $libMeta.textContent = rows.length;
    $libList.replaceChildren();
    if (rows.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'ax-list-empty';
      empty.textContent = 'no files shared by any peer yet.';
      $libList.appendChild(empty);
      return;
    }
    rows.forEach((r) => {
      const local = localDownloads[r.info_hash];
      const row = document.createElement('div');
      row.className = 'ax-intercept';
      const head = document.createElement('div');
      head.className = 'ax-intercept-head';
      head.style.cursor = 'default';
      const caret = document.createElement('span');
      caret.className = 'ax-intercept-caret'; caret.textContent = ' ';
      head.appendChild(caret);
      if (local && local.complete && local.path) {
        head.appendChild(makeFileLink(r.name, local.path));
      } else {
        const name = document.createElement('span');
        name.className = 'ax-intercept-spec';
        name.textContent = r.name;
        head.appendChild(name);
      }
      const fromPill = document.createElement('span');
      fromPill.className = 'ax-pill ax-pill-peer'; fromPill.textContent = 'from ' + r.peer;
      head.appendChild(fromPill);
      const sizePill = document.createElement('span');
      sizePill.className = 'ax-pill ax-pill-peer'; sizePill.textContent = fmtBytes(r.size);
      head.appendChild(sizePill);
      const meta = document.createElement('span');
      meta.className = 'ax-intercept-meta';
      head.appendChild(meta);
      if (local && local.complete) {
        const label = local.seeding ? 'seeding' : 'downloaded';
        const pill = document.createElement('span');
        pill.className = 'ax-pill ax-pill-active';
        pill.style.background = '#86b86b'; pill.style.color = '#000';
        pill.textContent = label;
        head.appendChild(pill);
      } else if (local && !local.complete) {
        const pct = local.size ? Math.floor(((local.bytes_completed || 0) / local.size) * 100) : 0;
        const pill = document.createElement('span');
        pill.className = 'ax-pill ax-pill-active'; pill.textContent = pct + '%';
        head.appendChild(pill);
      } else {
        const dl = document.createElement('button');
        dl.className = 'ax-list-remove'; dl.style.color = '#86b86b';
        dl.textContent = 'download';
        dl.addEventListener('click', async () => {
          const peerEndpoint = lookupPeerEndpoint(r.peer_tunnel);
          try {
            await StartDownload(r.magnet, peerEndpoint);
          } catch (e) {
            alert('Download failed: ' + (e?.message || e));
            return;
          }
          refreshDownloads();
        });
        head.appendChild(dl);
      }
      row.appendChild(head);
      $libList.appendChild(row);
    });
  }

  // The library row knows the peer's tunnel_ip but anacrolix needs an
  // actual UDP endpoint to dial. We pull the live udp_endpoint from
  // the global peer list main.js stashes (window.__livePeers).
  function lookupPeerEndpoint(tunnelIP) {
    livePeers = window.__livePeers || [];
    const p = livePeers.find((x) => !x.self && x.tunnel_ip === tunnelIP);
    return p ? (p.udp_endpoint || '') : '';
  }

  // ---- dropzone ----
  $zone.addEventListener('click', () => $input.click());
  $input.addEventListener('change', () => {
    if (!$input.files || $input.files.length === 0) return;
    upload($input.files[0]);
    $input.value = '';
  });
  $zone.addEventListener('dragover', (e) => {
    e.preventDefault();
    $zone.classList.add('is-drag');
  });
  $zone.addEventListener('dragleave', () => $zone.classList.remove('is-drag'));
  $zone.addEventListener('drop', (e) => {
    e.preventDefault();
    $zone.classList.remove('is-drag');
    const f = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
    if (f) upload(f);
  });

  // Read the dropped File client-side, base64-encode, ship to Go via
  // the AddMyFileBytes binding. Wails serialises method arguments as
  // JSON so binary bytes need to be encoded. For multi-GB files this
  // would be inefficient — open-file dialog would be better there —
  // but for everyday-sized drops it's fine.
  async function upload(file) {
    try {
      const buf = await file.arrayBuffer();
      const b64 = arrayBufferToBase64(buf);
      await AddMyFileBytes(file.name, b64);
      refreshMyFiles();
    } catch (e) {
      alert('Upload failed: ' + (e?.message || e));
    }
  }
  function arrayBufferToBase64(buf) {
    const bytes = new Uint8Array(buf);
    let bin = '';
    const chunk = 0x8000;
    for (let i = 0; i < bytes.length; i += chunk) {
      bin += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
    }
    return btoa(bin);
  }

  // ---- init ----
  refreshMyFiles();
  refreshDownloads();
  setInterval(refreshMyFiles, 5000);
  setInterval(refreshDownloads, 2000);
  setInterval(refreshLibrary, 5000);
}
