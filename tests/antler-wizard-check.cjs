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
  prepend(child) { this.children.unshift(child); }
  insertBefore(child) { this.children.unshift(child); return child; }
  replaceChildren() { this.children = []; }
  setAttribute() {}
  removeAttribute() {}
  reportValidity() { return true; }
  closest() { return this.closestNode || null; }
  querySelector(selector) { return this.nodes[selector]; }
  querySelectorAll(selector) {
    if (selector === '[data-antler-step]') return this.panels;
    return [];
  }
  set innerHTML(value) {
    this.panels = [0, 1, 2, 3, 4].map(() => new Element('SECTION'));
    this.panels.forEach((panel, i) => {
      panel.nodes['h3'] = new Element('H3');
      panel.nodes['p'] = new Element('P');
      this.nodes['[data-antler-step="' + i + '"]'] = panel;
      this.nodes['[data-antler-step="' + i + '"] h3'] = panel.nodes['h3'];
      this.nodes['[data-antler-step="' + i + '"] p'] = panel.nodes['p'];
      this.nodes['[data-antler-step="' + i + '"] > p'] = panel.nodes['p'];
    });
    this.nodes['.antler-custom-urls'] = new Element('INPUT');
    for (const name of ['progress', 'records', 'note', 'receivers', 'checks', 'check-note', 'check', 'error']) this.nodes['.antler-' + name] = new Element();
  }
}
async function check(mode = 'hosted') {
  let now = 100000, tick, requests = [], statuses = [], saved, redirect, failRotation = false;
  const dlg = new Element(); dlg.open = true;
  const form = new Element('FORM'); form.dataset.antlerDomain = 'domain-1'; form.dataset.antlerDomainName = mode === 'subdomain' ? 'mail.example.com' : 'example.com'; if (mode === 'subdomain') form.dataset.antlerParent = 'example.com'; form.closest = () => dlg;
  const provider = new Element('SELECT'); provider.value = 'dialmx';
  const group = new Element();
  const service = new Element('SELECT'); service.value = 'antler'; service.previousElementSibling = new Element('LABEL');
  const email = new Element('INPUT'); email.value = 'ops@example.com'; email.previousElementSibling = new Element('LABEL');
  const enforcement = new Element('SELECT'); enforcement.value = 'moderate'; enforcement.previousElementSibling = new Element('LABEL');
  const footer = new Element();
  const save = new Element('BUTTON'); save.closestNode = footer;
  form.nodes = { '.provider-select': provider, '[data-provider="dialmx"]': group, '[name="_csrf"]': { value: 'csrf' } };
  group.nodes = { '[name="cfg_dialmx_service"]': service, '[name="cfg_dialmx_contact_email"]': email, '[name="cfg_dialmx_enforcement"]': enforcement };
  group.children = [service.previousElementSibling, service];
  dlg.nodes = { '[data-save-provider]': save };
  if (mode === 'status') dlg.nodes['.dialmx-setup'] = new Element();
  const rotate = new Element('FORM'); rotate.nodes.button = new Element('BUTTON');
  dlg.nodes['form[data-antler-regenerate]'] = rotate;
  class Clock extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  const response = () => ({ provider: mode === 'status' ? 'dialmx' : '', config: { enforcement: 'moderate', service: mode === 'custom' ? 'custom' : 'antler' }, status: statuses, dns: [], instructions: { txt_name: '_mailmoose-mx.example.com', txt_value: 'public-key', mx: mode === 'custom' ? [] : [{ hostname: 'mx.example.com', priority: 10 }] } });
  const created = [];
  vm.runInNewContext(source.slice(start, end), {
    document: { querySelectorAll: () => [form], createElement: tag => { const el = new Element(tag.toUpperCase()); created.push(el); return el; }, createTextNode: text => ({ textContent: text }) },
    Date: Clock, navigator: {}, window: { setInterval(fn) { tick = fn; return 1; }, clearInterval() {}, addEventListener() {}, location: { assign(url) { redirect = url; } } },
    fetch(url, options) { requests.push({ url, options }); if (options.method !== 'GET') saved = JSON.parse(options.body); if (failRotation && saved && options.method === 'PUT' && saved.regenerate_secret) return Promise.reject(new Error('network error')); return Promise.resolve({ ok: true, json: () => Promise.resolve(response()) }); }
  });
  const wizard = group.children.at(-1);
  const flush = () => new Promise(resolve => setImmediate(resolve));
  const submit = async () => { form.events.submit({ preventDefault() {} }); await flush(); };
  const collect = node => [node.textContent || '', ...(node.children || []).map(collect)].join(' ');
  const back = footer.children.find(child => child.className.includes('antler-back'));
  assert.ok(back, 'Back lives in the dialog footer, not the wizard form');
  assert.equal(back.hidden, true, 'Back is hidden before entering a wizard step');
  assert.equal(service.hidden, true, 'service selector is removed from every step');
  if (mode === 'status') {
    assert.equal(provider.hidden, true);
    assert.equal(save.textContent, 'Save email');
    tick(); await flush();
    assert.equal(wizard.panels[2].hidden, true);
    email.value = 'changed@example.com'; await submit();
    assert.equal(requests.at(-1).options.method, 'POST');
    assert.deepEqual(saved.config, { contact_email: 'changed@example.com' });
    assert.equal(wizard.panels[3].hidden, true);
    const beforeCancel = requests.length;
    rotate.events.submit({ defaultPrevented: true, preventDefault() {} }); await flush();
    assert.equal(requests.length, beforeCancel, 'cancelled confirmation cannot rotate');
    rotate.events.submit({ defaultPrevented: false, preventDefault() {} });
    rotate.events.submit({ defaultPrevented: false, preventDefault() {} }); await flush();
    assert.equal(requests.length, beforeCancel + 1, 'double submit rotates only once');
    assert.deepEqual(saved, { provider: 'dialmx', regenerate_secret: true });
    assert.equal(wizard.panels[1].hidden, false, 'rotation opens DNS step');
    assert.match(collect(wizard.nodes['.antler-records']), /public-key/);
    assert.match(collect(wizard.nodes['.antler-records']), /Name Type Priority Value Status/);
    assert.match(collect(wizard.nodes['.antler-records']), /Copy value/);
    assert.doesNotMatch(collect(wizard.nodes['.antler-records']), /Connection status/);
    assert.equal(back.hidden, true, 'rotation cannot return to contact/setup steps');
    await submit();
    assert.equal(wizard.panels[2].hidden, false);
    assert.equal(save.textContent, 'Finish');
    assert.equal(save.disabled, true, 'rotation waits for current readiness');
    statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }];
    wizard.nodes['.antler-check'].events.click(); await flush();
    assert.equal(save.disabled, false);
    statuses = [{ state: 'disconnected' }];
    await submit();
    assert.equal(wizard.panels[2].hidden, false, 'Finish rechecks readiness');
    statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }]; now += 3000;
    wizard.nodes['.antler-check'].events.click(); await flush(); await submit();
    assert.equal(save.textContent, 'Save email', 'Finish returns to status');
    assert.match(collect(wizard.nodes['.antler-records']), /Connector Connection status MX status/);
    assert.doesNotMatch(collect(wizard.nodes['.antler-records']), /Name Type Priority Value Status/);
    assert.equal(requests.filter(r => r.options.method === 'PUT').length, 1, 'Finish does not rewrite config or rotate again');
    dlg.open = false; now += 20000;
    const closedCount = requests.length; tick(); await flush(); assert.equal(requests.length, closedCount);
    dlg.open = true; tick(); await flush();
    assert.match(collect(wizard.nodes['.antler-records']), /Antler|TXT/, 'reopening refreshes status and DNS');
    failRotation = true;
    rotate.events.submit({ defaultPrevented: false, preventDefault() {} }); await flush();
    assert.match(wizard.nodes['.antler-error'].textContent, /key may already have changed/);
    const failedCount = requests.filter(r => r.options.method === 'PUT').length;
    now += 20000; tick(); await flush();
    assert.equal(requests.filter(r => r.options.method === 'PUT').length, failedCount, 'failed rotation is never automatically retried');
    return;
  }
  assert.equal(save.textContent, 'Next');
  if (mode !== 'status') {
    // Back is not shown before a receiver type has been chosen and entered.
    assert.equal(back.hidden, true);
  }
  if (mode === 'custom') {
    const adv = created.find(el => el.className === 'antler-advanced');
    assert.ok(adv, 'advanced checkbox exists');
    adv.checked = true; adv.events.change();
    await submit();
    assert.equal(wizard.panels[4].hidden, false, 'advanced path gets a URL screen');
    assert.equal(provider.hidden, true);
    assert.equal(back.hidden, false, 'Back is available on the URL screen');
    back.events.click();
    assert.equal(wizard.panels[0].hidden, false, 'Back from the URL screen returns to contact email');
    await submit();
    wizard.nodes['.antler-custom-urls'].value = 'https://custom.example.com';
  }
  await submit();
  assert.equal(provider.hidden, true, 'provider selector disappears inside wizard');
  assert.equal(saved.config.enforcement, 'moderate');
  assert.equal(saved.config.service, mode === 'custom' ? 'custom' : 'antler');
  if (mode === 'custom') assert.equal(saved.config.receiver_urls, 'https://custom.example.com');
  assert.equal(requests.find(request => request.options.method === 'PUT').options.headers['X-CSRF-Token'], 'csrf');
  assert.match(requests[0].url, /^\/ui\/domains\/domain-1\/receiving\/setup$/);
  assert.equal(wizard.panels[1].hidden, false);
  assert.equal(back.hidden, false, 'Back appears once inside the wizard');
  const table = collect(wizard.nodes['.antler-records']);
  assert.match(table, /Name Type Priority Value Status/, 'initial setup uses DNS record layout');
  if (mode === 'custom') {
    assert.match(table, /_mailmoose-mx\.example\.com/);
  } else {
    assert.match(table, /MX/);
    assert.match(table, /mx\.example\.com/);
    if (mode === 'subdomain') {
      assert.match(table, /mail\.example\.com/, 'subdomain rows spell out the full name');
      assert.doesNotMatch(table, /(^|\s)@(\s|$)/, 'subdomain rows do not use @');
    } else {
      assert.match(table, /@/, 'apex MX uses @');
      assert.doesNotMatch(table, /mail\.example\.com/);
    }
  }
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
}
(async () => {
  await check(); await check('subdomain'); await check('custom'); await check('status');
  console.log('Antler hosted/subdomain/custom wizard and status interaction checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
