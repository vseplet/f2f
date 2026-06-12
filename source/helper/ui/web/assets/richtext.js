// richtext.js — message body rendering. A message is plain text by default,
// with two layers of opt-in formatting:
//
//   - inline markdown in plain text: `code`, **bold**, *italic*, [links](url)
//   - fenced blocks select a renderer by language tag:
//       ```js / ```python / …  → syntax-highlighted code (highlight.js)
//       ```md                  → rendered markdown (marked)
//       ```mermaid             → a diagram (mermaid, rendered post-insert)
//
// Peer content is UNTRUSTED, so every produced fragment is run through
// DOMPurify before it reaches the DOM; mermaid renders under securityLevel
// 'strict'. Exposes window.f2fRich = { render, renderDiagrams }.
(function () {
  'use strict';

  if (window.marked && marked.setOptions) {
    marked.setOptions({ gfm: true, breaks: true });
  }
  if (window.mermaid) {
    // startOnLoad off — we drive rendering ourselves after inserting nodes.
    mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'dark' });
  }

  // A fenced block: ```<lang>\n<body>``` (lang optional, no backticks in it).
  const FENCE = /```([^\n`]*)\r?\n([\s\S]*?)```/g;

  // escape covers BOTH text and double-quoted-attribute contexts (we stash the
  // mermaid source in a data- attribute), so quotes are escaped too.
  function escape(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  // inline renders a plain segment: inline markdown per line, <br> between
  // lines. Falls back to escaped text if marked isn't loaded.
  function inline(text) {
    if (!text) return '';
    if (!window.marked || !marked.parseInline) {
      return escape(text).replace(/\r?\n/g, '<br>');
    }
    return text.split(/\r?\n/).map(function (line) {
      try { return marked.parseInline(line); } catch (_) { return escape(line); }
    }).join('<br>');
  }

  function codeBlock(lang, code) {
    var inner, cls = 'hljs';
    if (window.hljs && lang && hljs.getLanguage(lang)) {
      try { inner = hljs.highlight(code, { language: lang, ignoreIllegals: true }).value; }
      catch (_) { inner = escape(code); }
    } else if (window.hljs) {
      try { var r = hljs.highlightAuto(code); inner = r.value; } catch (_) { inner = escape(code); }
    } else {
      inner = escape(code);
    }
    var label = lang ? '<span class="ax-code-lang">' + escape(lang) + '</span>' : '';
    return '<div class="ax-code">' + label + '<pre><code class="' + cls + '">' + inner + '</code></pre></div>';
  }

  function mdBlock(src) {
    if (!window.marked) return inline(src);
    try { return marked.parse(src); } catch (_) { return inline(src); }
  }

  // A mermaid block is emitted as a placeholder carrying its (escaped) source;
  // renderDiagrams turns it into an SVG once it's in the DOM.
  function mermaidBlock(src) {
    return '<pre class="ax-mermaid" data-src="' + escape(src) + '"></pre>';
  }

  // plain emits an inter-fence text chunk, dropping the single newline that
  // hugged an adjacent fence so blocks don't get a blank line around them.
  function plain(s, trimStart, trimEnd) {
    if (trimStart) s = s.replace(/^\r?\n/, '');
    if (trimEnd) s = s.replace(/\r?\n$/, '');
    return inline(s);
  }

  function render(text) {
    text = text || '';
    var out = '', last = 0, m;
    FENCE.lastIndex = 0;
    while ((m = FENCE.exec(text)) !== null) {
      if (m.index > last) out += plain(text.slice(last, m.index), last > 0, true);
      var lang = (m[1] || '').trim().toLowerCase();
      var code = m[2];
      if (lang === 'mermaid') out += mermaidBlock(code);
      else if (lang === 'md' || lang === 'markdown') out += mdBlock(code);
      else out += codeBlock(lang, code);
      last = m.index + m[0].length;
    }
    if (last < text.length) out += plain(text.slice(last), last > 0, false);
    if (window.DOMPurify) {
      // Allow the structural bits we emit; mermaid placeholders keep data-src.
      return DOMPurify.sanitize(out, { ADD_ATTR: ['data-src', 'target'] });
    }
    return out;
  }

  var mid = 0;
  // renderDiagrams turns every not-yet-rendered mermaid placeholder inside
  // container into an SVG. Async (mermaid.render is async); a parse error
  // falls back to showing the source so nothing silently vanishes.
  function renderDiagrams(container) {
    if (!window.mermaid || !container) return;
    var nodes = container.querySelectorAll('.ax-mermaid[data-src]:not([data-done])');
    nodes.forEach(function (el) {
      el.setAttribute('data-done', '1');
      var src = el.getAttribute('data-src') || '';
      mermaid.render('ax-mmd-' + (++mid), src).then(function (res) {
        el.innerHTML = res.svg;
        el.classList.add('ax-mermaid-ok');
      }).catch(function () {
        el.textContent = src;
        el.classList.add('ax-mermaid-err');
      });
    });
  }

  window.f2fRich = { render: render, renderDiagrams: renderDiagrams };
})();
