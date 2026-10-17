// Dependency-free checks for the Empty Trash rows in the queue popup.
// Run: node tests/empty-trash-check.cjs
//
// The Go suite asserts the operation itself. This harness drives the real
// app.js closure against a DOM stand-in to assert the queue presentation the
// server cannot observe: the parent row stays visible after completion, shows
// discovery and purge progress, keeps its Dismiss control, and sorts first.
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
    mailmooseStatus: { busy() {} },
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
  assert.equal(typeof window.mailmooseActionUpdateForTest, 'function', 'test seam is exported');
  // The popup paints only while open; a header click opens it, but the module
  // also installs its own click-away handler that our synthetic click would
  // trigger, so open it through the control's dedicated activation path.
  document.open();
  return { window, body, document };
}

function queueList(body) {
  return find(body, el => el.className === 'aq-list')[0];
}

function rows(body) {
  return queueList(body).children.filter(child => child.className === 'aq-row');
}

// texts collects the text of every descendant carrying the class as one of its
// class tokens: a control may also carry a variant class (aq-btn aq-primary).
function texts(row, className) {
  return find(row, el => (el.className || '').split(/\s+/).includes(className)).map(el => el.textContent);
}

// A completed parent operation stays visible with its progress and Dismiss.
{
  const { window, body, document } = run();
  window.mailmooseActionUpdateForTest({
    csrf: 'token',
    items: [
      { id: 'child-id', message_id: 'msg_1', state: 'pending', input: { kind: 'purge' } },
      { id: 'done-id', message_id: 'empty-trash:box', state: 'done', input: { kind: 'empty_trash' }, empty_trash: { phase: 'purging', total: 501, purged: 501, queued: 501 } }
    ]
  });
  const rendered = rows(body);
  assert.equal(rendered.length, 2, 'both queue rows render');
  assert.equal(rendered[0].getAttribute('data-state'), 'done', 'parent sorts first');
  assert.ok(texts(rendered[0], 'aq-kind')[0].includes('Empty Trash'), 'parent row names the operation');
  assert.deepEqual(texts(rendered[0], 'aq-meta'), ['501 of 501 purged'], 'parent row shows purge progress');
  assert.deepEqual(texts(rendered[0], 'aq-btn'), ['Dismiss'], 'completed parent row can be dismissed');
}

// Discovery phase reports finding messages, not a purged count.
{
  const { window, body, document } = run();
  window.mailmooseActionUpdateForTest({
    csrf: 'token',
    items: [{ id: 'find-id', message_id: 'empty-trash:box', state: 'pending', input: { kind: 'empty_trash' }, empty_trash: { phase: 'discovering', total: 12, purged: 0, queued: 0 } }]
  });
  assert.deepEqual(texts(rows(body)[0], 'aq-meta'), ['Finding messages… (12 found)'], 'discovery progress is shown');
}

// Failed parents keep the retry path and surface the public detail.
{
  const { window, body, document } = run();
  window.mailmooseActionUpdateForTest({
    csrf: 'token',
    items: [{ id: 'fail-id', message_id: 'empty-trash:box', state: 'failed', error: 'No remote Trash folder mapped', input: { kind: 'empty_trash' }, empty_trash: { phase: 'discovering', total: 0, purged: 0, queued: 0 } }]
  });
  const row = rows(body)[0];
  assert.equal(row.getAttribute('data-state'), 'failed');
  assert.deepEqual(texts(row, 'aq-meta'), ['No remote Trash folder mapped'], 'failure detail is shown');
  assert.deepEqual(texts(row, 'aq-btn'), ['Retry', 'Dismiss'], 'failed parent can be retried or dismissed');
}

console.log('empty-trash-check: all checks passed');
