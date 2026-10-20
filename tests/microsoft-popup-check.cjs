// Dependency-free checks for the Microsoft sign-in window and its paste prompt.
// Run: node tests/microsoft-popup-check.cjs
//
// The Microsoft flow is the one provider that cannot finish in the browser: the
// callback lands on http://localhost, nothing answers there, and the only way
// forward is to copy that URL out of the sign-in window and paste it back. These
// checks pin the two halves of that hand-off: the window opens partial-screen
// (so the wizard stays visible behind it, and the address bar stays reachable),
// and the paste prompt above the completion field flashes as soon as it does.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');

// The standalone wizard module owns every provider step in one IIFE.
const anchor = "  var root = document.querySelector('[data-standalone-wizard]');";
const anchorAt = source.indexOf(anchor);
assert.ok(anchorAt >= 0, 'standalone wizard module not found');
const start = source.lastIndexOf('(function () {', anchorAt);
const end = source.indexOf('\n})();', anchorAt);
assert.ok(start >= 0 && end > start, 'standalone wizard module bounds not found');
const moduleSource = source.slice(start, end + '\n})();'.length);

const AUTH_URL = 'https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=x';

class Element {
  constructor(tag = 'DIV') {
    this.tagName = tag.toUpperCase();
    this.attrs = {};
    this.value = '';
    this.textContent = '';
    this.hidden = false;
    this.disabled = false;
    this.offsetWidth = 0;
    this.scrolls = 0;
    this.events = {};
    this.nodes = {};
    this.lists = {};
    this.children = [];
    this.classes = new Set();
    this.classOps = [];
    const self = this;
    this.classList = {
      add(name) { self.classes.add(name); self.classOps.push('add:' + name); },
      remove(name) { self.classes.delete(name); self.classOps.push('remove:' + name); },
      contains: name => self.classes.has(name),
      toggle(name, on) {
        if (on === undefined) { on = !self.classes.has(name); }
        if (on) { self.classList.add(name); } else { self.classList.remove(name); }
        return on;
      }
    };
  }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return this.attrs[name]; }
  hasAttribute(name) { return name in this.attrs; }
  addEventListener(name, fn) { this.events[name] = fn; }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...kids) { this.children = kids; }
  select() {}
  focus() {}
  scrollIntoView() { this.scrolls += 1; }
  querySelector(selector) { return this.nodes[selector] || null; }
  querySelectorAll(selector) { return this.lists[selector] || []; }
}

// run() loads the wizard module against a stub page and hands back the pieces the
// checks drive: the begin form (whose submit opens the window), the finish form
// (which must never open one) and the paste prompt.
function run() {
  const opened = [];
  const fetched = [];
  const hint = new Element('P');
  hint.hidden = true; // server-rendered with the hidden attribute
  const status = new Element('P');

  const beginForm = new Element('FORM');
  beginForm.attrs['data-microsoft-begin'] = '';
  beginForm.action = '/ui/oauth/microsoft/begin';
  beginForm.lists['button[name=mailbox_type]'] = [new Element('BUTTON'), new Element('BUTTON')];
  beginForm.lists.button = beginForm.lists['button[name=mailbox_type]'];

  const finishForm = new Element('FORM');
  finishForm.attrs['data-microsoft-finish'] = '';
  finishForm.action = '/ui/oauth/microsoft/finish';
  finishForm.lists.button = [new Element('BUTTON')];

  const root = new Element('MAIN');
  root.nodes = {
    '[data-google-status]': new Element('P'),
    '[data-microsoft-status]': status,
    '[data-wizard-next]': new Element('BUTTON'),
    '[data-google-copy]': new Element('BUTTON'),
    '[data-microsoft-paste-hint]': hint
  };
  root.lists = {
    '[data-wizard-step]': [],
    '[data-wizard-back]': [],
    '[data-google-begin], [data-google-finish]': [],
    '[data-microsoft-begin], [data-microsoft-finish]': [beginForm, finishForm]
  };

  const popup = { opener: 'unset', location: 'about:blank', close() { this.closed = true; } };
  const window = {
    screenX: 0,
    screenY: 0,
    outerWidth: 1600,
    outerHeight: 900,
    location: { search: '', assign() {} },
    open(...args) { opened.push(args); return popup; }
  };

  class FormData { constructor() {} }

  vm.runInNewContext(moduleSource, {
    window,
    document: {
      querySelector: selector => (selector === '[data-standalone-wizard]' ? root : null),
      createElement: tag => new Element(tag)
    },
    navigator: {},
    URLSearchParams,
    FormData,
    fetch(url) {
      fetched.push(url);
      // The Google callback-configuration probe is not part of this flow.
      if (url === '/ui/oauth/google/info') { return Promise.resolve({ ok: false }); }
      // Completing the connection rejects a bad paste.
      if (url === finishForm.action) {
        return Promise.resolve({ ok: false, text: () => Promise.resolve('That URL is not a valid completion callback.') });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ authorization_url: AUTH_URL }) });
    }
  });

  return { hint, status, beginForm, finishForm, opened, popup, fetched };
}

const flush = () => new Promise(resolve => setImmediate(resolve));
const flashOps = ops => ops.filter(op => op.endsWith(':flash'));

(async () => {
  const page = run();

  // Nothing is revealed or opened until the operator starts a sign-in.
  assert.equal(page.hint.hidden, true, 'the paste prompt is hidden until the sign-in is launched');
  assert.equal(page.hint.classList.contains('flash'), false, 'the paste prompt does not flash before launch');
  assert.equal(page.opened.length, 0, 'loading the wizard opens no window');

  page.beginForm.events.submit({ preventDefault() {} });
  await flush();

  assert.equal(page.opened.length, 1, 'the begin step opens exactly one sign-in window');
  const [url, name, features] = page.opened[0];
  assert.equal(url, 'about:blank', 'the window opens blank first so the popup blocker still sees a user gesture');
  assert.notEqual(name, '_blank', 'the sign-in gets its own named window instead of a full tab');
  for (const want of ['popup=yes', 'width=520', 'height=720', 'left=540', 'top=90', 'location=yes', 'resizable=yes', 'scrollbars=yes']) {
    assert.ok(features.includes(want), 'the sign-in window features are missing ' + want);
  }
  assert.equal(page.popup.opener, null, 'the sign-in window is detached from the wizard');
  assert.equal(page.popup.location, AUTH_URL, 'the named window is navigated to Microsoft');

  // The flash is the cue that the URL to paste is now in the other window.
  assert.equal(page.hint.hidden, false, 'the paste prompt appears once the sign-in window is open');
  assert.equal(page.hint.classList.contains('flash'), true, 'the paste prompt flashes');
  assert.equal(page.hint.scrolls, 1, 'the paste prompt is scrolled into view');
  assert.deepEqual(flashOps(page.hint.classOps), ['remove:flash', 'add:flash'], 'the flash animation is started by a reflow, not left already-set');

  // A second attempt (personal, then shared, or after a failure) reuses the one
  // named window and flashes the prompt again.
  page.beginForm.events.submit({ preventDefault() {} });
  await flush();
  assert.equal(page.opened.length, 2, 'a second attempt opens the window again');
  assert.equal(page.opened[1][1], name, 'and reuses the window name, so no second sign-in window is left behind');
  assert.deepEqual(flashOps(page.hint.classOps), ['remove:flash', 'add:flash', 'remove:flash', 'add:flash'], 'the second launch restarts the flash');
  assert.equal(page.hint.scrolls, 2, 'the prompt is brought into view again');

  // Completing the connection never opens a window: there is nothing left to
  // sign in to, and a rejected paste just reports back in place.
  page.finishForm.events.submit({ preventDefault() {} });
  await flush();
  assert.equal(page.opened.length, 2, 'completing the connection opens no window');
  assert.equal(page.status.textContent, 'That URL is not a valid completion callback.');

  // Scope guard: only the Microsoft step needs the paste-back, so the Google
  // step keeps its plain tab.
  assert.ok(
    source.includes("data-google-begin') ? window.open('about:blank', '_blank')"),
    'the Google step still opens a plain tab'
  );
  assert.equal(
    source.includes("data-microsoft-begin') ? window.open('about:blank', '_blank')"),
    false,
    'the Microsoft step no longer opens a full tab'
  );

  console.log('Microsoft sign-in window and paste-prompt checks passed');
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
