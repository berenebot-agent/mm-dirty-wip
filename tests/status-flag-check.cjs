// Dependency-free checks for the header Live/Loading status flag state machine.
// Run: node tests/status-flag-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const start = source.indexOf('// Global Live/Loading status flag in the header.');
assert.ok(start >= 0, 'status flag module comment not found');
const iifeStart = source.lastIndexOf('(function () {', start);
const iifeEnd = source.indexOf('\n})();', start);
assert.ok(iifeEnd > iifeStart, 'status flag module end not found');
const moduleSource = source.slice(iifeStart, iifeEnd + '\n})();'.length);

class Element {
  constructor() {
    this.attrs = {};
    this.textContent = '';
  }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return this.attrs[name]; }
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
  return { status: window.mailmooseStatus, node, label: node.label, flush() { const pending = timers.splice(0); pending.forEach(fn => fn()); }, timers };
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

console.log('Status flag state machine checks passed');
