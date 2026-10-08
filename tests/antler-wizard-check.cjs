// Dependency-free interaction checks for the real wizard JavaScript.
// Run: node tests/antler-wizard-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const start = source.indexOf('(function () {\n  document.querySelectorAll(\'form[data-antler-domain]\')');
const end = source.indexOf('\n(function () {', start + 1);
class Element {
  constructor(tag = 'DIV') { this.tagName = tag; this.children = []; this.events = {}; this.dataset = {}; this.value = ''; this.hidden = false; this.nodes = {}; }
  addEventListener(name, fn) { this.events[name] = fn; }
  appendChild(child) { this.children.push(child); return child; }
  append(...children) { children.forEach(child => this.appendChild(child)); }
  replaceChildren() { this.children = []; }
  setAttribute() {}
  removeAttribute() {}
  reportValidity() { return true; }
  querySelector(selector) { return this.nodes[selector]; }
  querySelectorAll(selector) {
    if (selector === '[data-antler-step]') return this.panels;
    return [];
  }
  set innerHTML(value) {
    this.panels = [0, 1, 2, 3].map(() => new Element('SECTION'));
    this.panels.forEach((panel, i) => { this.nodes['[data-antler-step="' + i + '"]'] = panel; });
    for (const name of ['progress', 'records', 'dns', 'receivers', 'checks', 'check-note', 'check', 'error', 'back']) this.nodes['.antler-' + name] = new Element();
  }
}
async function main() {
  let now = 100000, tick, requests = [], statuses = [], saved, redirect;
  const dlg = new Element(); dlg.open = true;
  const form = new Element('FORM'); form.dataset.antlerDomain = 'domain-1'; form.closest = () => dlg;
  const provider = new Element('SELECT'); provider.value = 'dialmx';
  const group = new Element();
  const service = new Element('SELECT'); service.value = 'antler'; service.previousElementSibling = new Element('LABEL');
  const email = new Element('INPUT'); email.value = 'ops@example.com'; email.previousElementSibling = new Element('LABEL');
  const enforcement = new Element('SELECT'); enforcement.value = 'moderate'; enforcement.previousElementSibling = new Element('LABEL');
  const save = new Element('BUTTON');
  form.nodes = { '.provider-select': provider, '[data-provider="dialmx"]': group, '[name="_csrf"]': { value: 'csrf' } };
  group.nodes = { '[name="cfg_dialmx_service"]': service, '[name="cfg_dialmx_contact_email"]': email, '[name="cfg_dialmx_enforcement"]': enforcement };
  group.children = [service.previousElementSibling, service];
  dlg.nodes = { '[data-save-provider]': save };
  class Clock extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  const response = () => ({ config: { enforcement: 'moderate' }, status: statuses, dns: [], instructions: { txt_name: '_mailmoose-mx.example.com', txt_value: 'public-key', mx: [{ hostname: 'mx.example.com', priority: 10 }] } });
  vm.runInNewContext(source.slice(start, end), {
    document: { querySelectorAll: () => [form], createElement: tag => new Element(tag.toUpperCase()) },
    Date: Clock, navigator: {}, window: { setInterval(fn) { tick = fn; return 1; }, clearInterval() {}, addEventListener() {}, location: { assign(url) { redirect = url; } } },
    fetch(url, options) { requests.push({ url, options }); if (options.method === 'PUT') saved = JSON.parse(options.body); return Promise.resolve({ ok: true, json: () => Promise.resolve(response()) }); }
  });
  const wizard = group.children.at(-1);
  const flush = () => new Promise(resolve => setImmediate(resolve));
  const submit = async () => { form.events.submit({ preventDefault() {} }); await flush(); };
  assert.equal(save.textContent, 'Next');
  await submit();
  assert.equal(saved.config.enforcement, 'moderate');
  assert.equal(requests.find(request => request.options.method === 'PUT').options.headers['X-CSRF-Token'], 'csrf');
  assert.match(requests[0].url, /^\/ui\/domains\/domain-1\/receiving\/setup$/);
  assert.equal(wizard.panels[1].hidden, false);
  tick(); await flush(); tick();
  assert.match(wizard.nodes['.antler-check-note'].textContent, /Refresh in 10s/);
  const count = requests.length;
  now += 9999; tick(); await flush(); assert.equal(requests.length, count);
  now += 1; tick(); await flush(); assert.equal(requests.length, count + 1);
  wizard.nodes['.antler-check'].events.click(); await flush();
  assert.equal(wizard.nodes['.antler-check'].disabled, true);
  const manualCount = requests.length;
  wizard.nodes['.antler-check'].events.click(); await flush(); assert.equal(requests.length, manualCount);
  now += 3000; tick(); assert.equal(wizard.nodes['.antler-check'].disabled, false);
  await submit(); assert.equal(wizard.panels[2].hidden, false); assert.equal(save.disabled, true);
  statuses = [{ state: 'ready', smtp_hostname: 'one.example.com' }, { state: 'connecting', smtp_hostname: 'two.example.com' }];
  wizard.nodes['.antler-check'].events.click(); await flush();
  assert.equal(save.disabled, false, 'one ready receiver must unlock Next');
  await submit(); assert.equal(wizard.panels[3].hidden, false); assert.equal(save.textContent, 'Finish');
  statuses = [{ state: 'disconnected' }];
  await submit(); assert.equal(redirect, undefined); assert.equal(wizard.panels[2].hidden, false);
  now += 3000; statuses = [{ state: 'ready' }, { state: 'rejected' }];
  wizard.nodes['.antler-check'].events.click(); await flush(); await submit();
  enforcement.value = 'hard'; await submit();
  assert.equal(saved.config.enforcement, 'hard'); assert.match(redirect, /Antler/);
  dlg.open = false; now += 20000;
  const closedCount = requests.length; tick(); await flush(); assert.equal(requests.length, closedCount);
  console.log('Antler wizard interaction checks passed');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
