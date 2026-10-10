// Dependency-free interaction checks for the real wizard JavaScript.
// Run: node tests/antler-wizard-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const start = source.indexOf('(function () {\n  document.querySelectorAll(\'form[data-antler-domain]\')');
const end = source.indexOf('\n(function () {', start + 1);
class Element {
  constructor(tag = 'DIV') { this.tagName = tag; this.children = []; this.events = {}; this.dataset = {}; this.value = ''; this.hidden = false; this.nodes = {}; this.className = ''; const classes = new Set(); this.classList = { toggle: (name, on) => { if (on === undefined) { on = !classes.has(name); } if (on) { classes.add(name); } else { classes.delete(name); } }, contains: name => classes.has(name), add: name => classes.add(name), remove: name => classes.delete(name) }; }
  addEventListener(name, fn) { this.events[name] = fn; }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  append(...children) { children.forEach(child => this.appendChild(child)); }
  prepend(child) { this.children.unshift(child); }
  insertBefore(child) { this.children.unshift(child); return child; }
  replaceChildren() { this.children = []; }
  removeChild(child) { this.children = this.children.filter(c => c !== child); return child; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
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
  let now = 100000, tick, requests = [], statuses = [], saved, redirect, failRotation = false, dns = [], copied = [];
  let mxList = mode === 'custom' ? [] : [{ hostname: 'mx.example.com', priority: 10 }];
  // The status view exercises per-receiver remediation, so give it two connectors
  // to prove only the failing receiver's MX record is ever prompted.
  if (mode === 'status') { mxList = [{ hostname: 'mx.example.com', priority: 10 }, { hostname: 'mx2.example.com', priority: 20 }]; }
  const dlg = new Element(); dlg.open = true;
  // The live per-receiver status seeded on the status form, so the dialog opens
  // already matching the dashboard light instead of flashing "Pending".
  const seeded = mode === 'status' ? [{ state: 'ready', smtp_hostname: 'mx.example.com' }, { state: 'ready', smtp_hostname: 'mx2.example.com' }] : [];
  const form = new Element('FORM'); form.dataset.antlerDomain = 'domain-1'; form.dataset.antlerDomainName = mode === 'subdomain' ? 'mail.example.com' : 'example.com'; if (mode === 'subdomain') form.dataset.antlerParent = 'example.com'; form.dataset.antlerConnectors = JSON.stringify(mxList); form.dataset.antlerStatus = JSON.stringify(seeded); form.closest = () => dlg;
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
  // The receiving footer's danger cluster: a provider Worker-secret regenerate
  // (amber), the Antler key rotate and the Remove receiving form. The status
  // view must keep rotate + remove and hide the unrelated Worker regenerate.
  const workerRegenerate = new Element('FORM'); workerRegenerate.nodes.button = new Element('BUTTON'); workerRegenerate.nodes.button.className = 'amber';
  const removeReceiving = new Element('FORM'); removeReceiving.nodes.button = new Element('BUTTON'); removeReceiving.nodes.button.className = 'secondary danger';
  const danger = new Element('DIV'); danger.children = [workerRegenerate, rotate, removeReceiving];
  dlg.nodes['.dialog-danger'] = danger;
  class Clock extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  const response = () => ({ provider: mode === 'status' ? 'dialmx' : '', config: { enforcement: 'moderate', service: mode === 'custom' ? 'custom' : 'antler', ...(mode === 'status' ? { contact_email: 'ops@example.com' } : {}) }, status: statuses, dns: dns, instructions: { txt_name: '_mailmoose-mx.example.com', txt_value: 'public-key', mx: mxList } });
  const created = [];
  vm.runInNewContext(source.slice(start, end), {
    document: { querySelectorAll: () => [form], createElement: tag => { const el = new Element(tag.toUpperCase()); created.push(el); return el; }, createTextNode: text => ({ textContent: text }) },
    Date: Clock, navigator: { clipboard: { writeText: text => { copied.push(text); return Promise.resolve(); } } }, window: { setInterval(fn) { tick = fn; return 1; }, clearInterval() {}, addEventListener() {}, location: { assign(url) { redirect = url; } } },
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
    // Opening the saved status view checks immediately rather than waiting for
    // the first poll tick, and lays out the complete table from the embedded
    // connector names and live statuses, so it opens already matching the
    // dashboard light rather than flashing "Pending".
    assert.equal(requests.filter(r => r.options.method === 'GET').length, 1, 'status opens with an immediate check');
    assert.match(collect(wizard.nodes['.antler-records']), /Connector Status/, 'status table is laid out before the first poll returns');
    assert.match(collect(wizard.nodes['.antler-records']), /mx\.example\.com/, 'rows carry the connector names');
    assert.match(collect(wizard.nodes['.antler-records']), /Ready to receive/, 'a seeded live status paints its real light on open');
    assert.doesNotMatch(collect(wizard.nodes['.antler-records']), /Pending/, 'a seeded connector is never shown pending');
    assert.ok(wizard.nodes['.antler-records'].classList.contains('antler-checking'), 'pending skeleton animates while the first check is in flight');
    await flush();
    assert.equal(wizard.nodes['.antler-records'].classList.contains('antler-checking'), false, 'pending animation stops once the check returns');
    assert.equal(provider.hidden, true);
    // The status footer keeps key rotation and Remove receiving, and hides the
    // provider-specific Worker-secret regenerate that is not an Antler concern.
    assert.equal(workerRegenerate.hidden, true, 'Worker-secret regenerate is hidden on Antler status');
    assert.equal(rotate.hidden, false, 'Regenerate key stays available on status');
    assert.equal(removeReceiving.hidden, false, 'Remove receiving stays available on status');
    assert.equal(save.textContent, 'Save email');
    tick(); await flush();
    assert.equal(wizard.panels[2].hidden, true);
    assert.equal(save.hidden, true, 'Save email is hidden while the contact email is unchanged');
    email.value = 'changed@example.com'; email.events.input();
    assert.equal(save.hidden, false, 'Save email appears once the contact email changes');
    email.value = 'ops@example.com'; email.events.input();
    assert.equal(save.hidden, true, 'Save email hides again when the email is reverted');
    email.value = 'changed@example.com'; email.events.input();
    await submit();
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
    assert.equal(save.disabled, false, 'rotation Finish is never blocked on readiness');
    // A receiver that has not reconnected yet must not trap the operator: Finish
    // returns to status, which shows the receiver as in progress.
    statuses = [{ state: 'disconnected' }];
    await submit();
    assert.equal(save.textContent, 'Save email', 'Finish returns to status without waiting for a receiver');
    assert.match(collect(wizard.nodes['.antler-records']), /Connector Status/);
    assert.doesNotMatch(collect(wizard.nodes['.antler-records']), /Name Type Priority Value Status/);
    assert.equal(requests.filter(r => r.options.method === 'PUT').length, 1, 'Finish does not rewrite config or rotate again');
    dlg.open = false; now += 20000;
    const closedCount = requests.length; tick(); await flush(); assert.equal(requests.length, closedCount);
    dlg.open = true; tick(); await flush();
    assert.match(collect(wizard.nodes['.antler-records']), /Connector Status/, 'reopening refreshes the connector status table');
    // The saved status view is one light per receiver: its single state carries
    // both the domain-authority and MX-routing verdicts (D095), so the table is
    // two columns and each row lights on its own connector.
    const statusTableEl = () => wizard.nodes['.antler-records'].children.find(el => el.tagName === 'TABLE');
    const connLightClass = i => {
      const row = statusTableEl().children[1].children[i];
      return row.children[1].children[0].className;
    };
    const connLabel = i => {
      const row = statusTableEl().children[1].children[i];
      return row.children[1].children[1].textContent;
    };
    // A ready receiver paints green and names the state.
    statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }];
    now += 20000; tick(); await flush();
    assert.match(connLightClass(0), /green/, 'a ready connector is green');
    assert.equal(connLabel(0), 'Ready to receive');
    // A receiver the core cannot reach is a fact, not "in progress": its row must
    // paint red and name the failure. The server fills the configured
    // smtp_hostname even when the unreachable receiver advertised none, so the
    // row still resolves to its status instead of a bare amber "Waiting".
    statuses = [{ state: 'unreachable', smtp_hostname: 'mx.example.com', reason: 'unreachable' }];
    now += 20000; tick(); await flush();
    assert.match(connLightClass(0), /red/, 'an unreachable connector is red, not amber');
    assert.equal(connLabel(0), 'Receiver unreachable');
    statuses = [{ state: 'disconnected', smtp_hostname: 'mx.example.com' }];
    now += 20000; tick(); await flush();
    assert.match(connLightClass(0), /red/, 'a disconnected connector is red, not amber');
    // A connector with no live status yet reads amber "Pending".
    statuses = [];
    now += 20000; tick(); await flush();
    assert.match(connLightClass(0), /amber/, 'a connector with no status is amber pending');
    assert.equal(connLabel(0), 'Pending');
    // The inline remediation separates the record's name and value into their own
    // labelled rows with their own copy buttons, so neither part has to be picked
    // apart from a combined block.
    const remediation = () => wizard.nodes['.antler-records'].children.find(el => el.className === 'antler-dns-remediation') || new Element('DIV');
    const remediationRows = () => remediation().children.flatMap(box => (box.children || []).filter(el => el.className === 'dns-remediation-row'));
    statuses = [{ state: 'rejected', reason: 'key_unavailable', smtp_hostname: 'mx.example.com' }];
    now += 20000; tick(); await flush();
    let rows = remediationRows();
    assert.equal(rows.length, 2, 'a TXT fix shows a name row and a value row');
    assert.equal(rows[0].children[0].textContent, 'Record name');
    assert.equal(rows[0].children[1].textContent, '_mailmoose-mx.example.com');
    assert.equal(rows[1].children[0].textContent, 'Record value');
    assert.equal(rows[1].children[1].textContent, 'public-key');
    assert.ok(rows.every(row => row.children[2].textContent === 'Copy'), 'each row carries its own copy button');
    rows[1].children[2].events.click(); await flush();
    assert.deepEqual(copied, ['public-key'], 'the value copy button copies only the value');
    rows[0].children[2].events.click(); await flush();
    assert.deepEqual(copied, ['public-key', '_mailmoose-mx.example.com'], 'the name copy button copies only the name');
    // When only one of two receivers fails MX routing, prompt for that record
    // alone rather than telling the operator to republish every MX record. The
    // flag names the failing receiver and the row spells out its type, priority
    // and value.
    statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }, { state: 'rejected', reason: 'not_mx', smtp_hostname: 'mx2.example.com' }];
    now += 20000; tick(); await flush();
    let mxText = collect(remediation());
    assert.match(mxText, /mx2\.example\.com is not listed as an MX receiver/, 'the flag names the failing receiver');
    assert.doesNotMatch(mxText, /mx\.example\.com/, 'the healthy MX record is not prompted');
    let mxRows = remediationRows();
    assert.equal(mxRows.length, 4, 'an MX fix shows name, type, priority and value rows');
    assert.deepEqual(mxRows.map(row => row.children[0].textContent), ['Record name', 'Record type', 'Priority', 'Record value']);
    assert.equal(mxRows[1].children[1].textContent, 'MX');
    assert.equal(mxRows[2].children[1].textContent, '20');
    assert.equal(mxRows[3].children[1].textContent, 'mx2.example.com');
    // The type is informational, so it carries no copy button; the copyable
    // fields each do.
    assert.equal(mxRows[1].children[2], undefined, 'the record type row is not copyable');
    assert.ok(mxRows.filter((row, i) => i !== 1).every(row => row.children[2].textContent === 'Copy'), 'the copyable MX rows each carry a copy button');
    // Both a missing TXT and a failing MX ask for both records.
    statuses = [{ state: 'rejected', reason: 'key_unavailable', smtp_hostname: 'mx.example.com' }, { state: 'rejected', reason: 'not_mx', smtp_hostname: 'mx2.example.com' }];
    now += 20000; tick(); await flush();
    assert.match(collect(remediation()), /Domain not authorized/, 'the TXT fix is shown alongside the MX fix');
    assert.match(collect(remediation()), /is not listed as an MX receiver/, 'the MX fix is shown alongside the TXT fix');
    // A fully healthy domain shows no remediation block at all.
    statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }, { state: 'ready', smtp_hostname: 'mx2.example.com' }];
    now += 20000; tick(); await flush();
    assert.equal(remediationRows().length, 0, 'a healthy domain has no remediation rows');
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
  // Finish is never gated on readiness: a receiver that has not reconnected
  // must not bounce the operator back to the receiver step. It saves and
  // returns to status, which reports the receiver as in progress.
  statuses = [{ state: 'disconnected' }];
  assert.equal(save.disabled, false, 'enforcement Finish is never blocked on readiness');
  const finishPuts = requests.filter(r => r.options.method === 'PUT').length;
  enforcement.value = 'hard'; await submit();
  assert.equal(saved.config.enforcement, 'hard');
  assert.equal(redirect, undefined);
  assert.equal(save.textContent, 'Save email', 'Finish returns to status without waiting for a receiver');
  assert.match(collect(wizard.nodes['.antler-records']), /Connector Status/);
  assert.equal(wizard.panels[2].hidden, true, 'initial Finish returns to status');
  assert.equal(requests.filter(r => r.options.method === 'PUT').length, finishPuts + 1, 'Finish saves the config once and never rotates again');
  dlg.open = false; now += 20000;
  const closedCount = requests.length; tick(); await flush(); assert.equal(requests.length, closedCount);
}
(async () => {
  await check(); await check('subdomain'); await check('custom'); await check('status');
  console.log('Antler hosted/subdomain/custom wizard and status interaction checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
