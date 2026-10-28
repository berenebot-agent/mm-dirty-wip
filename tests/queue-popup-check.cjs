// Dependency-free checks for the queue popup's rows.
// Run: node tests/queue-popup-check.cjs
//
// The Go suite asserts the queue itself. This harness drives the real app.js
// closure against a DOM stand-in to assert what only the drawer can show: a
// row names the mailbox it came from, the Clear all control appears only when
// the drawer holds a flagged item, it takes the flagged rows off the drawer,
// it leaves the live work alone, and a later poll does not bring the cleared
// rows back.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const marker = '// The existing header status flag is also the durable action queue entry point.';
const markerAt = source.indexOf(marker);
assert.ok(markerAt >= 0, 'queue module comment not found');
const start = source.lastIndexOf('(function () {', markerAt);
const end = source.indexOf('\n})();', markerAt);
assert.ok(start >= 0 && end >= 0, 'queue module bounds not found');
const moduleSource = source.slice(start, end + '\n})();'.length);

class Element {
  constructor(tag = 'DIV') {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.events = {};
    this.style = { setProperty() {} };
    this.textContent = '';
    this.className = '';
    this.id = '';
    this.hidden = false;
    this.disabled = false;
    this.open = false;
    this.type = '';
    this.value = '';
    this.tabIndex = 0;
    this.dataset = {};
    this.ownerDocument = null;
    this.childNodes = [];
    this.style = { setProperty() {} };
    const classes = new Set();
    this.classList = {
      add: name => classes.add(name),
      remove: name => classes.delete(name),
      contains: name => classes.has(name)
    };
  }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return this.attrs[name]; }
  removeAttribute(name) { delete this.attrs[name]; }
  append(...children) { children.forEach(child => this.appendChild(child)); }
  appendChild(child) {
    this.children.push(child);
    this.childNodes = this.children;
    return child;
  }
  prepend(child) { this.children.unshift(child); this.childNodes = this.children; return child; }
  replaceChildren() { this.children = []; this.childNodes = this.children; }
  addEventListener(name, fn) { (this.events[name] = this.events[name] || []).push(fn); }
  querySelector() { return null; }
  querySelectorAll() { return []; }
  contains() { return false; }
  closest() { return null; }
  focus() {}
  show() { this.open = true; }
  showModal() { this.open = true; }
  close() { this.open = false; }
  getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
}

function find(root, predicate, found = []) {
  if (!root) { return found; }
  if (predicate(root)) { found.push(root); }
  (root.children || []).forEach(child => find(child, predicate, found));
  return found;
}

function run() {
  const body = new Element('BODY');
  const head = new Element('HEAD');
  const menu = new Element('DIV');
  const listeners = {};
  const document = {
    head,
    body,
    hidden: false,
    documentElement: { clientWidth: 1280 },
    createElement: tag => new Element(tag.toUpperCase()),
    querySelector: selector => (selector === 'header .menu-right' ? menu : null),
    querySelectorAll: () => [],
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
    // open drives only the flag's own keydown handler. The flag's click handler
    // is paired with a document-level click-away listener the module installs,
    // and dispatching a synthetic click would trip that listener and immediately
    // re-hide the popup.
    open() {
      const flag = find(menu, el => el.attrs['aria-haspopup'] === 'dialog')[0];
      (flag.events.keydown || []).forEach(fn => fn({ key: 'Enter', preventDefault() {} }));
      return flag;
    }
  };
  const window = {
    mammothmailStatus: { busy() {} },
    addEventListener() {},
    setInterval() { return 0; },
    setTimeout() { return 0; },
    clearTimeout() {},
    pageXOffset: 0,
    pageYOffset: 0,
    location: { href: '/' }
  };
  const context = {
    window,
    document,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    EventSource: class { addEventListener() {} },
    setInterval: window.setInterval,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    console
  };
  vm.runInNewContext(moduleSource, context);
  assert.equal(typeof window.mammothmailActionUpdateForTest, 'function', 'test seam is exported');
  document.open();
  return { window, body, menu, document };
}

function byClass(root, className) {
  return find(root, el => (el.className || '').split(/\s+/).includes(className));
}

function clearButton(body) {
  const found = byClass(body, 'aq-clear');
  assert.equal(found.length, 1, 'the drawer head carries one Clear all control');
  return found[0];
}

function rows(body) {
  const list = byClass(body, 'aq-list')[0];
  // A sync row carries a second class (aq-sync), so match the token.
  return list.children.filter(child => (child.className || '').split(/\s+/).includes('aq-row'));
}

// The queue is account-wide, so a row names the mailbox it came from, on the
// line it already has.
{
  const { window, body } = run();
  window.mammothmailActionUpdateForTest({
    csrf: 'token',
    items: [{ id: 'act-id', message_id: 'msg_abcdefghijklmnop', state: 'pending', inbox_name: 'Sales', input: { kind: 'move' } }]
  });
  const row = rows(body)[0];
  const meta = byClass(row, 'aq-meta')[0];
  assert.equal(meta.textContent, 'Sales · msg_abcdefgh…', 'the row names its mailbox beside the detail');
  assert.equal(byClass(row, 'aq-kind')[0].textContent, 'Move', 'the action keeps its own line');
}

// With nothing flagged there is nothing to clear, so the control stays away.
{
  const { window, body } = run();
  window.mammothmailActionUpdateForTest({
    csrf: 'token',
    items: [{ id: 'live-id', message_id: 'msg_1', state: 'pending', input: { kind: 'delete' } }]
  });
  assert.equal(clearButton(body).hidden, true, 'no flagged item, no Clear all control');
  assert.equal(rows(body).length, 1, 'the live row stays on the drawer');
}

// A failed action is flagged: the control appears and takes the row away.
{
  const { window, body, menu } = run();
  window.mammothmailActionUpdateForTest({
    csrf: 'token',
    items: [{ id: 'fail-id', message_id: 'msg_1', state: 'failed', error: 'provider refused', input: { kind: 'delete' } }]
  });
  const button = clearButton(body);
  assert.equal(button.hidden, false, 'a flagged item offers the Clear all control');
  const badge = byClass(menu, 'conn-badge')[0];
  assert.equal(badge.textContent, ' !', 'the flagged item marks the header');
  button.onclick();
  assert.equal(rows(body).length, 0, 'the flagged row leaves the drawer');
  assert.equal(button.hidden, true, 'the control goes away with the row it cleared');
  assert.equal(badge.textContent, '', 'the header settles once the drawer is clear');
  const summary = byClass(body, 'aq-summary')[0];
  assert.equal(summary.textContent, 'All clear', 'the drawer reports nothing left');
}

// A later poll must not bring a cleared row back.
{
  const { window, body } = run();
  const payload = {
    csrf: 'token',
    items: [{ id: 'fail-id', message_id: 'msg_1', state: 'failed', error: 'provider refused', input: { kind: 'delete' } }]
  };
  window.mammothmailActionUpdateForTest(payload);
  clearButton(body).onclick();
  window.mammothmailActionUpdateForTest(payload);
  assert.equal(rows(body).length, 0, 'the cleared row stays cleared across a poll');
  assert.equal(clearButton(body).hidden, true, 'the control stays away');
}

// Live work is not a notification: it stays on the drawer.
{
  const { window, body } = run();
  window.mammothmailActionUpdateForTest({
    csrf: 'token',
    items: [
      { id: 'fail-id', message_id: 'msg_1', state: 'failed', error: 'provider refused', input: { kind: 'delete' } },
      { id: 'live-id', message_id: 'msg_2', state: 'pending', input: { kind: 'move' } }
    ]
  });
  clearButton(body).onclick();
  const remaining = rows(body);
  assert.equal(remaining.length, 1, 'only the flagged row is cleared');
  assert.equal(remaining[0].getAttribute('data-state'), 'pending', 'the queued row keeps running');
}

// A metadata sync that stopped for a reason is flagged too.
{
  const { window, body } = run();
  window.mammothmailActionUpdateForTest({
    csrf: 'token',
    syncs: [
      { inbox_id: 'in_1', name: 'Sales', provider: 'IMAP', state: 'error', phase: 'initial', indexed: 3, total: 10, error: 'provider refused' },
      { inbox_id: 'in_2', name: 'Support', provider: 'IMAP', state: 'running', phase: 'initial', indexed: 3, total: 10 }
    ]
  });
  const button = clearButton(body);
  assert.equal(button.hidden, false, 'a stopped sync offers the Clear all control');
  button.onclick();
  const remaining = rows(body);
  assert.equal(remaining.length, 1, 'the stopped sync leaves the drawer');
  assert.equal(remaining[0].getAttribute('data-sync-inbox'), 'in_2', 'the running sync keeps going');
}

console.log('queue-popup-check: all checks passed');
