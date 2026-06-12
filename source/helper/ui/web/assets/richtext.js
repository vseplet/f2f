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
    // Route fenced code blocks INSIDE markdown (```md …) through our own
    // highlighter/diagram dispatch, so code in markdown is highlighted and a
    // mermaid fence in markdown still renders. marked's renderer.code receives
    // either a token object (v5+) or positional args (older) — handle both.
    if (marked.use) {
      marked.use({
        renderer: {
          code: function (codeOrTok, lang) {
            var code = typeof codeOrTok === 'string' ? codeOrTok : (codeOrTok && codeOrTok.text) || '';
            var language = typeof codeOrTok === 'string' ? lang : (codeOrTok && codeOrTok.lang) || '';
            return blockFor(language, code);
          },
        },
      });
    }
  }
  if (window.mermaid) {
    // startOnLoad off — we drive rendering ourselves after inserting nodes.
    mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'dark' });
  }

  // A fence line is 3+ backticks at the start of a line, optionally followed
  // by an info string (the language). A bare run of backticks closes a block.
  const OPEN_FENCE = /^(`{3,})([^`]*)$/;

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

  // blockFor dispatches a fenced block to its renderer by language tag. Shared
  // by the top-level fence parser AND marked's code renderer (so code inside a
  // ```md block is highlighted / a mermaid fence inside it still draws).
  function blockFor(lang, code) {
    lang = (lang || '').trim().toLowerCase().split(/\s+/)[0];
    if (lang === 'mermaid') return mermaidBlock(code);
    if (lang === 'md' || lang === 'markdown') return mdBlock(code);
    return codeBlock(lang, code);
  }

  // render parses the message line by line into HTML. Fenced blocks:
  //   - a code/mermaid fence closes at the FIRST bare fence of the same length
  //     (standard, non-greedy);
  //   - a markdown fence (```md / ```markdown) closes at the LAST such fence,
  //     so it can wrap same-length code fences inside it (marked then renders
  //     and highlights them). That's what makes ```md … ```js … ``` … ``` work
  //     without forcing a longer outer fence.
  // Plain runs between blocks get inline markdown; everything is sanitised.
  function render(text) {
    var lines = String(text || '').split('\n');
    var out = '', buf = [];
    function flush() { if (buf.length) { out += inline(buf.join('\n')); buf = []; } }
    for (var i = 0; i < lines.length; ) {
      var mm = lines[i].match(OPEN_FENCE);
      if (!mm) { buf.push(lines[i]); i++; continue; }
      var fence = mm[1];
      var lang = (mm[2] || '').trim().toLowerCase().split(/\s+/)[0];
      var greedy = lang === 'md' || lang === 'markdown';
      var bare = new RegExp('^`{' + fence.length + ',}\\s*$'); // a closing fence
      var close = -1;
      if (greedy) {
        for (var j = lines.length - 1; j > i; j--) { if (bare.test(lines[j])) { close = j; break; } }
      } else {
        for (var k = i + 1; k < lines.length; k++) { if (bare.test(lines[k])) { close = k; break; } }
      }
      if (close === -1) { buf.push(lines[i]); i++; continue; } // unterminated → plain
      flush();
      out += blockFor(lang, lines.slice(i + 1, close).join('\n'));
      i = close + 1;
    }
    flush();
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
