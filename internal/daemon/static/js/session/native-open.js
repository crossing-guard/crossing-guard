// The "open this session in its vendor's desktop app" control. The daemon
// publishes the link on the session (native_open: {url, app}) and the switch
// in the console config; nothing here names a runtime, a scheme or an app.

let enabled = true;

// setNativeOpenEnabled applies the console config's native_open_links. The
// config read can land after the first rows are drawn, so switching off also
// removes the controls already on the page.
export function setNativeOpenEnabled(value, doc = globalThis.document) {
  enabled = value !== false;
  if (!enabled) doc?.querySelectorAll?.('a.session-native').forEach(node => node.remove());
}

// The console hands this URL to the operating system. The daemon composes it
// from a fixed template, and this is the second lock: a web, file or script
// scheme is never a desktop-app link.
const REFUSED_SCHEMES = new Set(['http', 'https', 'file', 'javascript', 'data', 'blob', 'about']);
const SCHEME = /^([a-z][a-z0-9+.-]*):/i;

// nativeOpenModel returns {href, label, title} for a session that has a link,
// else null: an absent control is the whole statement that there is none.
export function nativeOpenModel(session) {
  const link = session?.native_open;
  if (!enabled || !link || typeof link.url !== 'string' || !link.app) return null;
  const scheme = SCHEME.exec(link.url)?.[1]?.toLowerCase();
  if (!scheme || REFUSED_SCHEMES.has(scheme)) return null;
  return { href: link.url, label: 'Open in ' + link.app, title: 'Opens this session in ' + link.app };
}

// nativeOpenControl builds the anchor. It carries its own class and no class a
// document-level handler consumes. A rail row is a button, and what a browser
// does with a link inside one varies, so the click hands the scheme to the
// operating system itself (as the editor hand-off does) and never reaches the
// row. The href stays real: it is what the status bar and copy-link show.
export function nativeOpenControl(session, doc = document, win = globalThis.window) {
  const model = nativeOpenModel(session);
  if (!model) return null;
  const anchor = doc.createElement('a');
  anchor.className = 'session-native modeltag';
  anchor.href = model.href;
  anchor.textContent = '\u2197 ' + model.label;
  anchor.title = model.title;
  anchor.addEventListener('click', event => {
    event.preventDefault();
    event.stopPropagation();
    openNative(session, win);
  });
  // The row owns right-click (its tag menu) and would swallow the link's own
  // menu, which is how the link is copied. A middle click would open an empty
  // tab on a scheme the browser cannot show.
  anchor.addEventListener('contextmenu', event => event.stopPropagation());
  anchor.addEventListener('auxclick', event => { event.preventDefault(); event.stopPropagation(); });
  return anchor;
}

// openNative is the same hand-off for a control that is not an anchor (a menu
// item). It reports whether there was a link to open.
export function openNative(session, win = window) {
  const model = nativeOpenModel(session);
  if (!model) return false;
  win.location.href = model.href;
  return true;
}
