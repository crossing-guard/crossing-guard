// Linkify safety + honesty tests — console-info-panel-impl-plan Step 1.3a.
//
// Run:  node internal/daemon/static/js/core.linkify.test.mjs
//
// NOT wired into CI: this repo has no JS test runner and no CI workflow, so
// nothing runs this for you. It is checked in because the first block below is a
// SECURITY gate, not a nicety — user turns render through linkify, transcript
// text is attacker-influenceable (prompt-injected tool output, pasted content),
// and before this change user turns were injection-safe only because they went
// through textContent (impl-plan R1). Re-run it after touching linkify/mdInline.

// Stub resolver mirroring the real states.
import { linkify, mdInline, mdToHtml, headingSlug } from './core.js';

const KNOWN = new Set(['internal/daemon/enrich.go', 'docs/guide.md']);
const resolve = tok => {
  if (/^https?:\/\//.test(tok)) return { state:'external', url:tok, token:tok };
  if (tok === 'D18') return { state:'resolved', kind:'id', path:'docs/status.md', line:72, token:tok };
  if (tok === 'D9999') return { state:'unresolved', reason:'D9999 is not defined in any doc in this repo' };
  if (tok === 'cmd/cp/main.go') return { state:'stale', reason:'referenced, no longer in the tree' };
  const bare = tok.replace(/:\d+$/,'');
  if (KNOWN.has(bare)) return { state:'resolved', kind:'path', path:bare, line:+(tok.split(':')[1]||0), token:tok };
  return { state:'missing', reason:'no file at this path in the current tree' };
};


// Mirrors refs.js: a slash token without a file extension is not a reference.
const proseResolve = tok => {
  if (/^https?:\/\//.test(tok)) return { state:'external', url:tok, token:tok };
  if (!/\.(?:go|js|css|md|json|sh)$/i.test(tok.replace(/:\d+$/,''))) return null;
  return resolve(tok);
};

let fail = 0;
const check = (name, cond, got) => { console.log((cond?'  PASS  ':'  FAIL  ')+name + (cond?'':'\n         got: '+got)); if(!cond) fail++; };

console.log('\n== R1: XSS / injection (the gate before wiring) ==');
let h = linkify('<script>alert(1)</script>', resolve);
check('script tag escaped', !h.includes('<script'), h);
h = linkify('<img src=x onerror="alert(1)">', resolve);
// The only tags allowed in output are the ones we emit ourselves.
const OURS = /<\/?(?:a|span|code|strong|em|p|h[1-3]|ul|ol|li|pre|blockquote|hr)[^>]*>/gi;
check('no foreign tag survives escaping', !/<[a-z!/]/i.test(h.replace(OURS, '')), h);
h = linkify('"><a href="evil">x</a>', resolve);
check('attribute break-out escaped', !/<a href="evil"/.test(h), h);
h = mdInline('[click](javascript:alert(1))', resolve);
check('javascript: URL never emitted', !/javascript:/i.test(h), h);
h = mdInline('[click](data:text/html;base64,PHN2Zz4=)', resolve);
check('data: URL never emitted', !/data:/i.test(h), h);

console.log('\n== R1: no linkify inside code / existing links ==');
h = mdInline('use `internal/daemon/enrich.go` here', resolve);
check('path inside inline code is NOT linkified', h.includes('<code>internal/daemon/enrich.go</code>') && !/<a[^>]*>internal/.test(h), h);
h = mdInline('[enrich](internal/daemon/enrich.go)', resolve);
check('md link not double-wrapped', (h.match(/<a /g)||[]).length === 1, h);
h = mdToHtml('```\ninternal/daemon/enrich.go\n```', resolve);
check('fenced code block untouched', !/<a /.test(h), h);

console.log('\n== §3 the original bug: relative md link ==');
h = mdInline('[x](docs/guide.md)', resolve);
check('relative link now renders as a link (was literal text)', /<a[^>]*ref-ok/.test(h) && !h.includes('[x]('), h);
h = mdInline('[y](https://example.com)', resolve);
check('http link still opens in a tab', /target="_blank"/.test(h), h);

console.log('\n== §5 honest states ==');
h = linkify('see internal/daemon/enrich.go:57 now', resolve);
check('resolved file:line -> anchor with line', /data-ref-line="57"/.test(h), h);
h = linkify('see nope/gone.go now', resolve);
check('missing -> dim span with reason, NOT a link', /<span class="ref ref-miss"/.test(h) && !/<a /.test(h), h);
h = linkify('see cmd/cp/main.go now', resolve);
check('stale -> ref-stale span', /ref-stale/.test(h), h);
h = linkify('item D18 and D9999', resolve);
check('D18 links, D9999 dims', /ref-ok/.test(h) && /ref-miss/.test(h), h);
h = linkify('done (see internal/daemon/enrich.go).', resolve);
check('trailing punctuation stays outside the link', /<\/a>\)\./.test(h), h);

console.log('\n== R2: indexing != missing ==');
h = linkify('see internal/daemon/enrich.go', () => ({ state:'indexing', reason:'building…' }));
check('indexing renders neutral (no link, no dim-miss)', !/<a /.test(h) && !/ref-miss/.test(h), h);

console.log('\n== no resolver -> no guesses ==');
h = linkify('see internal/daemon/enrich.go', null);
check('without resolver, plain escaped text', !/<a /.test(h) && !/<span/.test(h), h);

console.log('\n== R8: heading anchors ==');
check('D-item heading keeps its id', headingSlug('D5 auth rejects bearer') === 'D5', headingSlug('D5 auth rejects bearer'));
check('prose heading slugified', headingSlug('The console today (grounded)') === 'the-console-today-grounded', headingSlug('The console today (grounded)'));
check('mdToHtml emits heading id', /<h2 id="the-gaps">/.test(mdToHtml('## The gaps', resolve)), mdToHtml('## The gaps', resolve));


console.log('\n== regressions found during live GUI verification ==');
// Nested stash: a markdown link whose LABEL contains inline code. One restore
// pass dropped the code span and leaked a NUL into the DOM.
h = mdInline('see [`index.html`:504](internal/daemon/enrich.go:504)', resolve);
check('nested placeholder fully restored', h.includes('<code>index.html</code>'), h);
check('no NUL sentinel leaks into the DOM', !h.includes('\u0000'), JSON.stringify(h));

// Prose slashes are not paths. These must render as PLAIN TEXT, not dim spans:
// "not a reference" and "a broken reference" are different claims.
for (const prose of ['list/detail', 'account/settings', 'Chat/Cowork/Code', 'open/close']) {
  const out = linkify('the ' + prose + ' split', proseResolve);
  check('prose "' + prose + '" stays plain text', !/<span|<a /.test(out), out);
}
// A real-looking file that is absent still dims — the honest miss must survive.
check('a real path that is absent still dims',
  /ref-miss/.test(linkify('see nope/gone.go now', resolve)), linkify('see nope/gone.go now', resolve));

console.log(fail ? `\n${fail} FAILED\n` : '\nALL PASS\n');
process.exit(fail?1:0);
