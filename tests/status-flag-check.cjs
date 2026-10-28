// Dependency-free checks for the header Live/Loading status flag state machine.
// Run: node tests/status-flag-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');

function sliceIIFE(comment, name) {
  const start = source.indexOf(comment);
  assert.ok(start >= 0, name + ' module comment not found');
  const iifeStart = source.lastIndexOf('(function () {', start);
  const ends = ['\n})();', '\n}());']
    .map(s => source.indexOf(s, start))
    .filter(i => i >= 0);
  assert.ok(iifeStart >= 0 && ends.length > 0, name + ' module end not found');
  return source.slice(iifeStart, Math.min(...ends) + '\n})();'.length);
}

const moduleSource = sliceIIFE('// Global Live/Loading status flag in the header.', 'status flag');
const navSource = sliceIIFE('Pages that carry the header status flag report navigation loading through', 'navigation loading');

class Element {
  constructor() {
    this.attrs = {};
    this.textContent = '';
    this.hidden = false;
    this.className = '';
    this.children = [];
    const classes = new Set();
    this.classList = {
      add: name => classes.add(name),
      remove: name => classes.delete(name),
      contains: name => classes.has(name)
    };
  }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return this.attrs[name]; }
  appendChild(child) { this.children.push(child); return child; }
  querySelector(selector) { return selector === '.conn-label' ? this.label : null; }
}

function run(withNode) {
  const timers = [];
  const node = new Element();
  node.label = new Element();
  const window = {
    setTimeout(fn) { timers.push(fn); return timers.length; },
    clearTimeout() {}
  };
  const context = {
    window,
    document: { querySelector(selector) { return withNode && selector === '[data-conn-status]' ? node : null; } }
  };
  vm.runInNewContext(moduleSource, context);
  return { status: window.mammothmailStatus, node, label: node.label, flush() { const pending = timers.splice(0); pending.forEach(fn => fn()); }, timers };
}

// Initial state: connecting, server-rendered into the same shape.
{
  const { status, node, label } = run(true);
  assert.equal(node.getAttribute('data-state'), 'connecting');
  assert.equal(label.textContent, 'Connecting…');
  status.connected();
  assert.equal(node.getAttribute('data-state'), 'live');
  assert.equal(label.textContent, 'Live');
}

// A dropped stream wins over any in-flight background action.
{
  const { status, node, flush } = run(true);
  status.connected();
  status.busy('list', true);
  flush();
  assert.equal(node.getAttribute('data-state'), 'loading');
  assert.equal(node.getAttribute('aria-busy'), 'true');
  status.disconnected();
  assert.equal(node.getAttribute('data-state'), 'disconnected');
  assert.equal(node.label.textContent, 'Disconnected');
}

// A short action that finishes inside the delay never shows the spinner.
{
  const { status, node, flush } = run(true);
  status.connected();
  status.busy('body', true);
  status.busy('body', false);
  flush();
  assert.equal(node.getAttribute('data-state'), 'live');
}

// Busy actions carry their own tooltip; a later action upgrades the label.
{
  const { status, node, flush } = run(true);
  status.connected();
  status.busy('list', true);
  flush();
  assert.match(node.getAttribute('title'), /Updating messages/);
  status.busy('sync', true);
  assert.match(node.getAttribute('title'), /Syncing messages/);
  status.busy('list', false);
  status.busy('sync', false);
  assert.equal(node.getAttribute('data-state'), 'live');
}

// Sync errors show amber until cleared.
{
  const { status, node } = run(true);
  status.connected();
  status.error('Mail sync failed.');
  assert.equal(node.getAttribute('data-state'), 'error');
  assert.equal(node.getAttribute('title'), 'Mail sync failed.');
  status.error('');
  assert.equal(node.getAttribute('data-state'), 'live');
}

// Reopening after a hidden-tab close shows connecting again.
{
  const { status, node } = run(true);
  status.connected();
  status.disconnected();
  status.connecting();
  assert.equal(node.getAttribute('data-state'), 'connecting');
}

// Pages without the flag get a harmless no-op API.
{
  const { status } = run(false);
  status.connecting();
  status.connected();
  status.disconnected();
  status.error('ignored');
  status.busy('list', true);
}

// Navigation loading drives the header flag where it exists, and the flag
// outranks it only while disconnected.
function runNav(withFlag) {
  const timers = new Map();
  let nextId = 1;
  const windowListeners = {};
  const documentListeners = {};
  const node = new Element();
  node.label = new Element();
  const document = {
    querySelector(selector) { return withFlag && selector === '[data-conn-status]' ? node : null; },
    createElement() { return new Element(); },
    head: new Element(),
    body: new Element(),
    addEventListener(name, fn) { documentListeners[name] = fn; }
  };
  const window = {
    setTimeout(fn, ms) { const id = nextId++; timers.set(id, { fn, ms }); return id; },
    clearTimeout(id) { timers.delete(id); },
    addEventListener(name, fn) { windowListeners[name] = fn; },
    location: { href: 'http://example.test/', origin: 'http://example.test', pathname: '/', search: '' }
  };
  const context = { window, document, URL };
  vm.runInNewContext(moduleSource, context);
  vm.runInNewContext(navSource, context);
  return {
    status: window.mammothmailStatus,
    node,
    document,
    documentListeners,
    windowListeners,
    runTimerMs(ms) {
      for (const [id, timer] of [...timers]) {
        if (timer.ms === ms) {
          timers.delete(id);
          timer.fn();
        }
      }
    }
  };
}

function clickLink(documentListeners, href) {
  documentListeners.click({
    target: { closest: () => ({ href, hasAttribute: () => false, target: '' }) },
    button: 0,
    defaultPrevented: false
  });
}

// On a flagged page a link click spins the flag, pageshow clears it, and no
// floating banner is created.
{
  const ctx = runNav(true);
  ctx.status.connected();
  clickLink(ctx.documentListeners, 'http://example.test/ui/inboxes/x');
  assert.equal(ctx.document.body.children.length, 0, 'flagged page must not create a banner');
  ctx.runTimerMs(250);
  assert.equal(ctx.node.getAttribute('data-state'), 'loading');
  assert.match(ctx.node.getAttribute('title'), /Loading/);
  ctx.windowListeners.pageshow();
  assert.equal(ctx.node.getAttribute('data-state'), 'live');
}

// A submission also spins the flag; the watchdog clears it if the server hangs.
{
  const ctx = runNav(true);
  ctx.status.connected();
  ctx.documentListeners.submit({ defaultPrevented: false, target: { target: '' } });
  ctx.runTimerMs(0);
  ctx.runTimerMs(250);
  assert.equal(ctx.node.getAttribute('data-state'), 'loading');
  ctx.runTimerMs(60000);
  assert.equal(ctx.node.getAttribute('data-state'), 'live');
}

// A dropped stream still outranks navigation loading.
{
  const ctx = runNav(true);
  ctx.status.connected();
  ctx.status.disconnected();
  clickLink(ctx.documentListeners, 'http://example.test/ui/inboxes/x');
  ctx.runTimerMs(250);
  assert.equal(ctx.node.getAttribute('data-state'), 'disconnected');
}

// Pages without the flag keep the floating banner.
{
  const ctx = runNav(false);
  clickLink(ctx.documentListeners, 'http://example.test/ui/inboxes/x');
  const banner = ctx.document.body.children[0];
  assert.ok(banner, 'flag-less page must create the banner');
  assert.match(banner.className, /navigation-load/);
  assert.equal(banner.textContent, 'Loading…');
  ctx.windowListeners.pageshow();
  assert.equal(banner.hidden, true);
}

console.log('Status flag state machine checks passed');
