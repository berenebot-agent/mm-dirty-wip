// Dependency-free interaction checks for the real Tusk MX dialog JavaScript.
// Run: node tests/tusk-wizard-check.cjs
//
// The Go suite cannot observe what actually breaks in this dialog: which timer
// refreshes which step, how many listeners end up on the shared SSE stream,
// whether a push repaints the right dialog, and what the footer button sends.
// This harness drives the real closure from internal/httpapp/assets/app.js
// against a minimal DOM stand-in and asserts exactly those behaviours.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');

// Capabilities that later steps turn on. Each is a behaviour the dialog does not
// have yet, so its assertions stay off until the implementing change lands rather
// than failing from the start.
const HAVE = {
  // S5: enforcement + contact email editable in place from the saved status view.
  statusSettings: true,
  // S7: wizard steps 1-2 poll every 10s with a visible countdown.
  wizardCountdown: true
};
if (process.env.ANTLER_CHECK_CAPS) {
  for (const cap of process.env.ANTLER_CHECK_CAPS.split(',')) {
    if (cap.trim()) { HAVE[cap.trim()] = true; }
  }
}
const start = source.indexOf('(function () {\n  document.querySelectorAll(\'form[data-antler-domain]\')');
const end = source.indexOf('\n(function () {', start + 1);
if (start < 0 || end < 0) {
  throw new Error('could not locate the Tusk MX dialog closure in app.js');
}

// Element is a small DOM stand-in: exactly the surface the closure touches.
class Element {
  constructor(tag = 'DIV') {
    this.tagName = tag;
    this.children = [];
    this.events = {};
    this.dataset = {};
    this.value = '';
    this.hidden = false;
    this.disabled = false;
    this.required = false;
    this.type = '';
    this.textContent = '';
    this.className = '';
    this.nodes = {};
    const classes = new Set();
    this.classList = {
      toggle: (name, on) => {
        if (on === undefined) { on = !classes.has(name); }
        if (on) { classes.add(name); } else { classes.delete(name); }
      },
      contains: name => classes.has(name),
      add: name => classes.add(name),
      remove: name => classes.delete(name)
    };
  }
  addEventListener(name, fn) { (this.events[name] = this.events[name] || []).push(fn); }
  removeEventListener(name, fn) {
    this.events[name] = (this.events[name] || []).filter(candidate => candidate !== fn);
  }
  emit(name, event = {}) { (this.events[name] || []).slice().forEach(fn => fn(event)); }
  appendChild(child) {
    if (child && child.parentNode) { child.parentNode.removeChild(child); }
    this.children.push(child);
    if (child) { child.parentNode = this; }
    return child;
  }
  append(...children) { children.forEach(child => this.appendChild(child)); }
  prepend(child) { this.children.unshift(child); if (child) { child.parentNode = this; } }
  insertBefore(child, ref) {
    const at = ref ? this.children.indexOf(ref) : -1;
    if (at < 0) { return this.appendChild(child); }
    this.children.splice(at, 0, child);
    if (child) { child.parentNode = this; }
    return child;
  }
  replaceChildren() {
    this.children.forEach(child => { if (child) { child.parentNode = null; } });
    this.children = [];
  }
  removeChild(child) {
    this.children = this.children.filter(candidate => candidate !== child);
    if (child) { child.parentNode = null; }
    return child;
  }
  remove() { if (this.parentNode) { this.parentNode.removeChild(this); } }
  setAttribute(name, value) { this.attrs = this.attrs || {}; this.attrs[name] = value; }
  getAttribute(name) { return (this.attrs || {})[name]; }
  removeAttribute(name) { if (this.attrs) { delete this.attrs[name]; } }
  reportValidity() { return true; }
  closest(selector) { return (this.closestMap && this.closestMap[selector]) || this.closestNode || null; }
  querySelector(selector) { return this.nodes[selector]; }
  querySelectorAll(selector) {
    if (selector === '[data-antler-step]') { return this.panels; }
    if (selector === 'h3, [data-antler-step] > p') { return this.headings; }
    return [];
  }
  get firstChild() { return this.children[0]; }
  set innerHTML(value) {
    this.panels = [0, 1, 2, 3, 4].map(() => new Element('SECTION'));
    this.headings = [];
    this.panels.forEach((panel, i) => {
      panel.nodes['h3'] = new Element('H3');
      this.nodes['[data-antler-step="' + i + '"]'] = panel;
      this.nodes['[data-antler-step="' + i + '"] h3'] = panel.nodes['h3'];
      this.headings.push(panel.nodes['h3']);
      // The contact step has no description paragraph in the real wizard
      // markup. Saved-status initialization must tolerate that optional node.
      if (i !== 0) {
        panel.nodes['p'] = new Element('P');
        this.nodes['[data-antler-step="' + i + '"] p'] = panel.nodes['p'];
        this.nodes['[data-antler-step="' + i + '"] > p'] = panel.nodes['p'];
        this.headings.push(panel.nodes['p']);
      }
    });
    this.nodes['.antler-custom-urls'] = new Element('INPUT');
    for (const name of ['progress', 'records', 'receivers', 'error']) {
      this.nodes['.antler-' + name] = new Element();
    }
    // The check row contains its note: the note carries the countdown text and the
    // row is what the dialog shows or hides. The manifest line lives in the same
    // row and reports where the receiver set came from, so the closure reads it on
    // every render and the stand-in must provide it.
    const checks = new Element('DIV');
    const note = new Element('P');
    checks.appendChild(note);
    const manifestNote = new Element('P');
    manifestNote.hidden = true;
    checks.appendChild(manifestNote);
    this.nodes['.antler-checks'] = checks;
    this.nodes['.antler-check-note'] = note;
    this.nodes['.antler-manifest-note'] = manifestNote;
  }
}

// harness builds one isolated dialog world (its own DOM, clock, fetch log and SSE
// streams) so every scenario runs without leaking state into the next.
function harness(mode) {
  const world = {
    now: 100000,
    requests: [],
    statuses: [],
    dns: [],
    unexpected: [],
    copied: [],
    timers: [],
    nextTimer: 1,
    windowEvents: {},
    hidden: false,
    saved: null,
    rejectSave: false
  };
  let mxList = [{ hostname: 'mx.example.com', priority: 10 }];
  if (mode === 'status' || mode === 'status-two') {
    mxList = [{ hostname: 'mx.example.com', priority: 10 }, { hostname: 'mx2.example.com', priority: 20 }];
  }
  if (mode === 'custom') { mxList = []; }
  const seedStatus = (mode === 'status' || mode === 'status-two')
    ? [{ state: 'ready', smtp_hostname: 'mx.example.com' }, { state: 'ready', smtp_hostname: 'mx2.example.com' }]
    : [];

  const dlg = new Element('DIALOG');
  dlg.open = true;
  dlg.close = () => { dlg.open = false; dlg.emit('close'); };
  const form = new Element('FORM');
  form.dataset.antlerDomain = 'domain-1';
  form.dataset.antlerDomainName = mode === 'subdomain' ? 'mail.example.com' : 'example.com';
  // A subdomain added standalone (not linked as an inherited child) carries no
  // parent domain. The record-name logic must not depend on that link: it is
  // exactly the case that used to render "@" on a subdomain.
  if (mode === 'subdomain') { form.dataset.antlerParent = ''; }
  form.dataset.antlerConnectors = JSON.stringify(mxList);
  form.dataset.antlerStatus = JSON.stringify(seedStatus);
  form.dataset.antlerDns = JSON.stringify([{ kind: 'mx', state: 'not_found' }, { kind: 'txt', state: 'not_found' }]);
  form.dataset.antlerInstructions = JSON.stringify({ txt_name: '_mail-proof.example.com', txt_value: 'public-key', mx: mxList });
  form.closestMap = { dialog: dlg };
  const provider = new Element('SELECT');
  provider.value = 'dialmx';
  const group = new Element('DIV');
  const service = new Element('SELECT');
  service.value = mode === 'custom' ? 'custom' : 'hosted';
  service.previousElementSibling = new Element('LABEL');
  const email = new Element('INPUT');
  email.value = 'ops@example.com';
  email.previousElementSibling = new Element('LABEL');
  const enforcement = new Element('SELECT');
  enforcement.value = 'moderate';
  enforcement.previousElementSibling = new Element('LABEL');
  const footer = new Element('DIV');
  const save = new Element('BUTTON');
  save.closestNode = footer;
  const close = new Element('BUTTON');
  form.nodes = { '.provider-select': provider, '[data-provider="dialmx"]': group, '[name="_csrf"]': { value: 'csrf' } };
  group.nodes = {
    '[name="cfg_dialmx_service"]': service,
    '[name="cfg_dialmx_contact_email"]': email,
    '[name="cfg_dialmx_enforcement"]': enforcement
  };
  group.children = [service.previousElementSibling, service];
  dlg.nodes = { '[data-save-provider]': save, '[data-close-dialog]': close };
  if (mode === 'status' || mode === 'status-two') { dlg.nodes['.dialmx-setup'] = new Element('SECTION'); }
  const rotate = new Element('FORM');
  rotate.nodes.button = new Element('BUTTON');
  dlg.nodes['form[data-antler-regenerate]'] = rotate;
  const workerRegenerate = new Element('FORM');
  workerRegenerate.nodes.button = new Element('BUTTON');
  workerRegenerate.nodes.button.className = 'amber';
  const removeReceiving = new Element('FORM');
  removeReceiving.nodes.button = new Element('BUTTON');
  removeReceiving.nodes.button.className = 'secondary danger';
  const danger = new Element('DIV');
  danger.children = [workerRegenerate, rotate, removeReceiving];
  // The server draws the danger block for every receiving dialog but hides it
  // when the domain had no receiving config when the page was rendered: the
  // wizard reveals it once it stores one of its own. The status modes render it
  // visible, because there the config already exists.
  danger.hidden = !(mode === 'status' || mode === 'status-two');
  dlg.nodes['.dialog-danger'] = danger;

  class Clock extends Date {
    constructor(...args) { super(...(args.length ? args : [world.now])); }
    static now() { return world.now; }
  }

  const response = () => ({
    provider: mode === 'status' || mode === 'status-two' ? 'dialmx' : '',
    config: {
      enforcement: enforcement.value || 'moderate',
      service: service.value === 'custom' ? 'custom' : 'hosted',
      contact_email: 'ops@example.com'
    },
    status: world.statuses,
    dns: world.dns,
    dns_stale: !!world.stale,
    unexpected_mx: world.unexpected,
    instructions: { txt_name: '_mail-proof.example.com', txt_value: 'public-key', mx: mxList }
  });

  const document = {
    querySelectorAll: () => [form],
    createElement: tag => new Element(tag.toUpperCase()),
    createTextNode: text => ({ textContent: text }),
    head: new Element('HEAD'),
    body: new Element('BODY'),
    hidden: false,
    addEventListener: (name, fn) => { document[name] = fn; }
  };
  const window = {
    setTimeout(fn, delay) {
      const id = world.nextTimer++;
      world.timers.push({ id, fn, delay, at: world.now + (delay || 0), interval: false });
      return id;
    },
    clearTimeout(id) { world.timers = world.timers.filter(t => t.id !== id); },
    setInterval(fn, delay) {
      const id = world.nextTimer++;
      world.timers.push({ id, fn, delay, at: world.now + (delay || 0), interval: true });
      return id;
    },
    clearInterval(id) { world.timers = world.timers.filter(t => t.id !== id); },
    addEventListener(name, fn) { (world.windowEvents[name] = world.windowEvents[name] || []).push(fn); },
    removeEventListener(name, fn) { world.windowEvents[name] = (world.windowEvents[name] || []).filter(f => f !== fn); },
    dispatchEvent(event) { (world.windowEvents[event.type] || []).slice().forEach(fn => fn(event)); return true; },
    location: { assign() {} },
    Event: function (type) { this.type = type; },
     mammothmailEventsSource: new Element(),
    // The real app.js installs window.mammothmailCopy (assets/app.js) and every
    // copy control routes through it. The closure under test calls
    // window.mammothmailCopy.wire(btn, opts) while building the Antler
    // remediation block, so the harness must provide the same surface or that
    // path throws before the block is built. Behaviour mirrors the real wire:
    // a function opts.text wins, then opts.text, then btn[data-copy]; a click
    // records the copied value.
    mammothmailCopy: {
      text: value => String(value == null ? '' : value),
      announce: text => { world.copied.push(text); },
      select: target => target,
      wire(btn, opts) {
        if (!btn) { return; }
        opts = opts || {};
        btn.addEventListener('click', () => {
          let text = typeof opts.text === 'function' ? opts.text() : opts.text;
          if (text == null) { text = btn.getAttribute('data-copy') || ''; }
          world.copied.push(text);
        });
      }
    }
  };

  const sandbox = {
    document,
    Date: Clock,
    navigator: { clipboard: { writeText: text => { world.copied.push(text); return Promise.resolve(); } } },
    window,
    fetch(url, options) {
      world.requests.push({ url, options });
      if (options.method !== 'GET') { world.saved = JSON.parse(options.body); }
      if (world.rejectSave && options.method !== 'GET') { return Promise.reject(new Error('network error')); }
      if (world.holdGet && options.method === 'GET') {
        return new Promise(resolve => { world.releaseGet = () => resolve({ ok: true, json: () => Promise.resolve(response()) }); });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(response()) });
    }
  };
  vm.runInNewContext(source.slice(start, end), sandbox);

  const wizard = group.children.at(-1);
  const back = footer.children.find(child => child.className.includes('antler-back'));
  const flush = () => new Promise(resolve => setImmediate(resolve));
  const settle = async () => { for (let i = 0; i < 6; i++) { await flush(); } };
  const submit = async () => { form.emit('submit', { preventDefault() {} }); await settle(); };
  const collect = node => [node.textContent || '', ...(node.children || []).map(collect)].join(' ');
  const gets = () => world.requests.filter(r => r.options.method === 'GET').length;
  // fireTimer runs the single pending timer matching a predicate, as the browser
  // would when its delay elapses.
  const fireTimer = async (predicate = () => true) => {
    const timer = world.timers.filter(predicate)[0];
    if (!timer) { return false; }
    if (!timer.interval) {
      world.timers = world.timers.filter(t => t !== timer);
    } else {
      // An interval reschedules itself; advance its next due time.
      timer.at += timer.delay;
    }
    world.now = timer.at;
    timer.fn();
    await settle();
    return true;
  };
  const pends = (delay, predicate = () => true) => world.timers.some(t => t.delay === delay && predicate(t));
  return { world, dlg, form, group, provider, service, email, enforcement, footer, save, close,
           rotate, workerRegenerate, removeReceiving, danger, wizard, back, mxList, seedStatus,
            flush, settle, submit, collect, fireTimer, gets, pends, document, window };
}

// --- status view ---------------------------------------------------------

async function statusView() {
  const h = harness('status');
  await h.settle();

  // It opens complete from the embedded seed, not a blank table that fills later.
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /Connector Status/, 'status table is laid out on open');
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /mx\.example\.com/, 'connector names come from the form seed');

  // The footer keeps key rotation and Remove receiving and drops the unrelated
  // provider Worker-secret regenerate.
  assert.equal(h.workerRegenerate.hidden, true, 'Worker-secret regenerate is hidden on Antler status');
  assert.equal(h.rotate.hidden, false, 'Regenerate key stays available');
  assert.equal(h.removeReceiving.hidden, false, 'Remove receiving stays available');
  assert.equal(h.back.hidden, true, 'the saved status view has no Back');
  assert.equal(h.provider.hidden, true, 'the provider selector is hidden');

  // D107: the saved status view is buttonless and counter-free. There is no
  // "Check now", no countdown: it reconciles on SSE and a slow fallback poll.
  assert.doesNotMatch(h.collect(h.wizard.nodes['.antler-checks']), /Check now/, 'no Check now button');
  assert.doesNotMatch(h.collect(h.wizard.nodes['.antler-checks']), /Checking again in/, 'no countdown on the status view');

  // A receiver the core cannot reach is a fact, not "in progress": the row must
  // read red and name the failure.
  h.world.statuses = [{ state: 'unreachable', smtp_hostname: 'mx.example.com', reason: 'unreachable' }];
  h.world.dns = [];
  await h.fireTimer(t => t.delay === 60000);
  const table = () => h.wizard.nodes['.antler-records'].children.find(el => el.tagName === 'TABLE');
  const cellOf = i => table().children[1].children[i].children[1];
  assert.match(cellOf(0).children[0].className, /red/, 'an unreachable connector is red, not amber');
  assert.equal(cellOf(0).children[1].textContent, 'Receiver unreachable');

  // A ready receiver paints green and names the state.
  h.world.statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }];
  await h.fireTimer(t => t.delay === 60000);
  assert.match(cellOf(0).children[0].className, /green/, 'a ready connector is green');
  assert.equal(cellOf(0).children[1].textContent, 'Ready to receive');

  // A connector with no live status yet reads amber "Pending".
  h.world.statuses = [];
  await h.fireTimer(t => t.delay === 60000);
  assert.match(cellOf(0).children[0].className, /amber/, 'a connector with no status is amber pending');
  assert.equal(cellOf(0).children[1].textContent, 'Pending');

  // Repair prompts follow the receiver refusal, not an independent DNS cache.
  h.world.dns = [
    { kind: 'txt', state: 'mismatch', reason: 'stale' },
    { kind: 'mx', state: 'not_found', reason: 'none' }
  ];
  h.world.statuses = [{ state: 'rejected', reason: 'key_unavailable', smtp_hostname: 'mx.example.com' }];
  await h.fireTimer(t => t.delay === 60000);
  const remediation = () => h.wizard.nodes['.antler-records'].children.find(el => el.className === 'antler-dns-remediation') || new Element('DIV');
  assert.match(h.collect(remediation()), /Domain not authorized/, 'the TXT fix is shown');
   assert.doesNotMatch(h.collect(remediation()), /MX records/, 'a local DNS failure does not invent a receiver routing refusal');

  // Local DNS cannot erase a receiver's explicit refusal either.
  h.world.dns = [{ kind: 'txt', state: 'ok' }, { kind: 'mx', state: 'ok' }];
  await h.fireTimer(t => t.delay === 60000);
   assert.match(h.collect(remediation()), /Domain not authorized/, 'receiver refusal remains actionable even if local DNS matches');

  // A local lookup error does not alter the receiver's actionable key refusal.
  h.world.dns = [{ kind: 'txt', state: 'dns_error', reason: 'TXT lookup failed; retrying' }, { kind: 'mx', state: 'ok' }];
  h.world.statuses = [{ state: 'rejected', reason: 'key_unavailable', smtp_hostname: 'mx.example.com' }];
  await h.fireTimer(t => t.delay === 60000);
  const unverified = h.collect(remediation());
   assert.match(unverified, /Domain not authorized/, 'receiver key refusal shows the TXT repair');
  assert.match(unverified, /Record value/, 'a dns_error check still shows the record to copy');
}

// --- edit settings in place ---------------------------------------------

async function statusSettings() {
  const h = harness('status-two');
  await h.settle();

  // Settings live below the records: the status picture reads first.
  const settings = h.wizard.querySelector('[data-antler-settings]');
  assert.ok(settings, 'the status view carries a settings section');
  assert.ok(
    h.wizard.children.indexOf(h.wizard.querySelector('.antler-records')) < h.wizard.children.indexOf(settings),
    'settings sit below the status records'
  );
  assert.equal(settings.children.includes(h.email.previousElementSibling), true, 'the contact email label is in settings');
  assert.equal(settings.children.includes(h.email), true, 'the contact email field is in settings');
  assert.equal(settings.children.includes(h.enforcement), true, 'the enforcement field is in settings');

  // The button is hidden until something actually changes.
  assert.equal(h.save.hidden, true, 'Save is hidden while both settings are unchanged');
  h.email.value = 'changed@example.com';
  h.email.emit('input');
  assert.equal(h.save.hidden, false, 'Save appears once the contact email changes');
  if (HAVE.statusSettings) { assert.equal(h.save.textContent, 'Save settings'); }

  // Enforcement alone also counts as dirty.
  h.email.value = 'ops@example.com';
  h.email.emit('input');
  h.enforcement.value = 'hard';
  h.enforcement.emit('input');
  assert.equal(h.save.hidden, false, 'Save appears once enforcement changes');

  await h.submit();
  assert.equal(h.world.requests.at(-1).options.method, 'POST');
  assert.deepEqual(h.world.saved, { contact_email: 'ops@example.com', enforcement: 'hard' },
    'settings save both fields in one request');
  assert.equal(h.save.hidden, true, 'Save hides again after a successful save');

  // A failed save keeps the values and surfaces the error rather than pretending.
  h.world.rejectSave = true;
  h.enforcement.value = 'moderate';
  h.enforcement.emit('input');
  await h.submit();
  assert.match(h.wizard.nodes['.antler-error'].textContent, /Unable to save/, 'a failed settings save reports the error');
}

// --- wizard ---------------------------------------------------------------

async function wizard(mode) {
  const h = harness(mode);
  await h.settle();
  assert.equal(h.save.textContent, 'Next');
  assert.equal(h.back.hidden, true, 'Back is hidden before a step is entered');
  // Nothing is stored yet, so the footer's danger block stays hidden: Remove
  // receiving has nothing to remove.
  assert.equal(h.danger.hidden, true, 'the danger block is hidden before the first save');

  if (mode === 'custom') {
    const step0 = h.wizard.panels[0];
    const advanced = step0.children
      .filter(el => el && el.tagName === 'LABEL')
      .map(label => (label.children || []).find(el => el && el.type === 'checkbox'))
      .find(Boolean);
    assert.ok(advanced, 'the advanced checkbox exists');
    advanced.checked = true;
    advanced.emit('change');
    await h.submit();
    assert.equal(h.wizard.panels[4].hidden, false, 'the advanced path reaches the URL step');
    h.wizard.querySelector('.antler-custom-urls').value = 'https://custom.example.com';
    await h.submit();
  } else {
    await h.submit(); // contact email -> DNS records
  }

  // The wizard stores the receiving config as it goes, so the setup it has just
  // saved must be removable from here: the danger block is revealed with the same
  // Remove receiving the saved status view carries.
  assert.equal(h.danger.hidden, false, 'storing the config reveals Remove receiving');
  assert.equal(h.removeReceiving.hidden, false, 'Remove receiving is offered once something is stored');

  // Step 1 is the DNS record table with its own copy buttons.
  assert.equal(h.wizard.panels[1].hidden, false, 'the DNS step is shown');
  const table = h.collect(h.wizard.nodes['.antler-records']);
  assert.match(table, /Name Type Priority Value Status/, 'the wizard uses the DNS record layout');
  if (mode === 'custom') {
    // A custom receiver configuration supplies URLs, not hosted MX hostnames.
    // Only its TXT authorization instruction is generated by the core.
    assert.doesNotMatch(table, /mx\.example\.com/, 'custom receivers do not invent hosted MX records');
    assert.match(table, /_mail-proof\.example\.com/, 'custom receiver setup still lists the authorization TXT record');
  } else {
    assert.match(table, /mx\.example\.com/, 'the hosted MX record is listed');
  }

  // The MX "Name" column is the zone apex's "@" only for a two-label apex. A
  // subdomain must spell out its fully-qualified name: "@" there would publish
  // the MX on the parent zone and send mail to the wrong domain, which is what
  // a subdomain added standalone (no parent link) used to show.
  if (mode === 'subdomain') {
    assert.match(table, /mail\.example\.com/, 'a subdomain shows its fully-qualified record name');
    assert.doesNotMatch(table, /(^|\s)@(\s|$)/, 'a subdomain never shows "@" as its record name');
  } else if (mode !== 'custom') {
    assert.match(table, /(^|\s)@(\s|$)/, 'the apex shows "@" as its record name');
  }

  // The wizard, unlike the status view, shows a live countdown while a data step
  // is open, so the operator can see the check is still running.
  if (HAVE.wizardCountdown) {
    assert.equal(h.wizard.nodes['.antler-checks'].hidden, false, 'the check row is visible on the DNS step');
    assert.match(h.collect(h.wizard.nodes['.antler-checks']), /checking again in \d+s/, 'the DNS step shows a live countdown');
    assert.match(h.collect(h.wizard.nodes['.antler-checks']), /Checked /, 'the countdown names when the last check ran');
    assert.ok(h.pends(1000), 'the countdown ticks once a second');

    // A timer-driven refresh advances the check without any user action: the
    // per-second countdown reaches zero and the dialog re-reads the server.
    const before = h.gets();
    for (let i = 0; i < 10; i++) { await h.fireTimer(t => t.delay === 1000 && t.interval); }
    assert.equal(h.gets(), before + 1, 'the countdown reaching zero refreshes the check');
  }

  // The receivers step gates Next on one ready receiver, and is not gated on all
  // of them.
  await h.submit();
  assert.equal(h.save.disabled, true, 'without a ready receiver Next stays disabled');
  h.back.emit('click');
  assert.equal(h.wizard.panels[1].hidden, false, 'Back returns to DNS instructions');
  h.world.dns = [{ kind: 'mx', state: 'not_found' }, { kind: 'txt', state: 'not_found' }];
  h.world.statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com' }, { state: 'connecting', smtp_hostname: 'mx2.example.com' }];
  await h.submit();
  assert.equal(h.wizard.panels[2].hidden, false, 'the receivers step is shown');
  assert.equal(h.save.disabled, false, 'one ready receiver unlocks Next');
  assert.doesNotMatch(h.collect(h.wizard.nodes['.antler-receivers']), /No MX records|Domain not authorized/, 'wizard receivers do not contradict readiness');
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /Matching/, 'DNS instructions reflect the receiver proof');
  if (HAVE.wizardCountdown) {
    assert.match(h.collect(h.wizard.nodes['.antler-checks']), /checking again in \d+s/, 'the receivers step also counts down');
  }

  // Finish is never gated on readiness.
  await h.submit();
  assert.equal(h.wizard.panels[3].hidden, false, 'the enforcement step is shown');
  assert.equal(h.save.textContent, 'Finish');
  h.world.statuses = [{ state: 'disconnected' }];
  assert.equal(h.save.disabled, false, 'Finish is never blocked on readiness');
  h.enforcement.value = 'hard';
  h.world.rejectSave = true;
  await h.submit();
  assert.equal(h.dlg.open, true, 'failed Finish keeps the dialog open');
  assert.equal(h.wizard.panels[3].hidden, false, 'failed Finish stays on the policy step');
  assert.equal(h.enforcement.value, 'hard', 'failed Finish preserves the policy');
  h.world.rejectSave = false;
  await h.submit();

  // Finishing saves and closes; reopening presents the saved status view.
   assert.equal(h.dlg.open, false, 'Finish closes the dialog after saving');
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /Connector Status/, 'the status table is shown after Finish');
  // The footer's close control is relabelled as the wizard becomes the saved
  // status view. It was only set once at init, so a freshly-finished setup left
  // a stray "Cancel" beside an otherwise actionless status panel.
  assert.equal(h.close.textContent, 'Close', 'Finish relabels the footer control from Cancel to Close');
  const expectConfig = mode === 'custom'
    ? { service: 'custom', enforcement: 'hard', receiver_urls: 'https://custom.example.com' }
    : { service: 'hosted', enforcement: 'hard', contact_email: 'ops@example.com' };
  assert.deepEqual(h.world.saved, { provider: 'dialmx', config: expectConfig },
    'Finish writes the config with the chosen enforcement and never rotates');
  h.dlg.open = true;
  h.dlg.emit('open');
  await h.settle();
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /Connector Status/, 'reopening shows status rather than restarting setup');

  // Nothing polls once the dialog is closed.
  h.dlg.open = false;
  const closed = h.gets();
  await h.fireTimer(t => t.delay === 10000);
  assert.equal(h.gets(), closed, 'no refresh runs while the dialog is closed');
}

// --- key rotation ---------------------------------------------------------

// --- intro copy -----------------------------------------------------------

// A fresh setup opens on step 0 with a short Antler MX service intro and the
// mail-flow diagram. It is about the hosted service, so it reads once: moving on
// hides it, and neither custom-receiver mode nor the saved status view shows it.
async function introCopy() {
  const find = h => h.wizard.children.find(child => child.className === 'antler-intro');
  const hosted = harness('hosted');
  await hosted.settle();
  const intro = find(hosted);
  assert.ok(intro, 'the wizard builds the Antler MX intro');
  assert.equal(intro.hidden, false, 'the intro shows on the first step');
  await hosted.submit();
  assert.equal(intro.hidden, true, 'advancing past the first step hides the intro');

  const custom = harness('custom');
  await custom.settle();
  assert.equal(find(custom).hidden, true, 'custom receivers do not show the Antler MX intro');

  const status = harness('status');
  await status.settle();
  assert.equal(find(status).hidden, true, 'the saved status view does not show the intro');
}

async function rotation() {
  const h = harness('status');
  await h.settle();

  // The shared data-confirm handler runs first; cancellation must never reach the
  // rotation endpoint.
  const before = h.world.requests.filter(r => r.options.method === 'PUT').length;
  h.rotate.emit('submit', { defaultPrevented: true, preventDefault() {} });
  await h.settle();
  assert.equal(h.world.requests.filter(r => r.options.method === 'PUT').length, before, 'a cancelled confirmation cannot rotate');

  // A double submit rotates once.
  h.rotate.emit('submit', { defaultPrevented: false, preventDefault() {} });
  h.rotate.emit('submit', { defaultPrevented: false, preventDefault() {} });
  await h.settle();
  assert.equal(h.world.requests.filter(r => r.options.method === 'PUT').length, before + 1, 'double submit rotates once');
  assert.deepEqual(h.world.saved, { provider: 'dialmx', regenerate_secret: true });

  // Rotation opens the replace-the-TXT step and cannot go back to contact/setup.
  assert.equal(h.wizard.panels[1].hidden, false, 'rotation opens the DNS step');
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /public-key/, 'the new TXT value is shown');
  assert.equal(h.back.hidden, true, 'rotation cannot return to the pre-rotation steps');

  // A receiver that has not reconnected must not trap the operator.
  await h.submit();
  h.world.statuses = [{ state: 'disconnected' }];
  await h.submit();
  assert.equal(h.save.textContent, HAVE.statusSettings ? 'Save settings' : 'Save email', 'rotation Finish returns to the status view');
  assert.equal(h.world.requests.filter(r => r.options.method === 'PUT').length, before + 1, 'Finish does not rotate again');

  // A failed rotation is reported and never retried automatically.
  h.world.rejectSave = true;
  h.rotate.emit('submit', { defaultPrevented: false, preventDefault() {} });
  await h.settle();
  assert.match(h.wizard.nodes['.antler-error'].textContent, /key may already have changed/);
  const failed = h.world.requests.filter(r => r.options.method === 'PUT').length;
  await h.fireTimer(t => t.delay === 10000 || t.delay === 60000);
  assert.equal(h.world.requests.filter(r => r.options.method === 'PUT').length, failed, 'a failed rotation is not retried');
}

// --- live updates ---------------------------------------------------------

async function liveUpdates() {
  const h = harness('status');
  await h.settle();

  // The dialog reuses the dashboard's single SSE connection rather than opening a
  // second stream, so it attaches to whatever window.mammothmailEventsSource is at
  // the time rather than creating its own.
  const open = h.dlg.events.open || [];
  assert.ok(open.length > 0, 'the dialog listens for its own open event');

  // The status view reconciles on its slow fallback poll (D107: no Check now).
  const before = h.gets();
  h.world.statuses = [{ state: 'ready', smtp_hostname: 'mx.example.com', reason: 'ok' }];
  await h.fireTimer(t => t.delay === 60000);
  assert.equal(h.gets(), before + 1, 'the status view reconciles on its slow fallback poll');

  // An extra-MX verdict reaching the fetch repaints the warning. Its source is
  // the shared verdict: the fetch's unexpected_mx, folded into the MX check.
  h.world.dns = [{ kind: 'mx', state: 'ok', matched: ['mx.example.com'], unexpected: ['rogue.example.com'] }];
  h.world.unexpected = ['rogue.example.com'];
  await h.fireTimer(t => t.delay === 60000);
  assert.match(h.collect(h.wizard.nodes['.antler-records']), /rogue\.example\.com/, 'the extra-MX warning names the rogue host');

  // The contact email only offers Save once it actually differs from the saved
  // value, so a stray keystroke cannot rewrite settings.
  h.email.value = 'ops@example.com';
  h.email.emit('input');
  assert.equal(h.save.hidden, true, 'an unchanged contact email offers no Save');
}

async function receiverTruth() {
  const h = harness('status');
  await h.settle();
  const records = () => h.collect(h.wizard.nodes['.antler-records']);
  h.world.dns = [{ kind: 'mx', state: 'not_found' }, { kind: 'txt', state: 'not_found' }];
  h.world.statuses = h.seedStatus;
  await h.fireTimer(t => t.delay === 60000);
  assert.doesNotMatch(records(), /Domain not authorized|Check these MX|No MX records/, 'ready receivers override negative local DNS');
  h.world.statuses = [h.seedStatus[0], { state: 'rejected', reason: 'not_mx', smtp_hostname: 'mx2.example.com' }];
  await h.fireTimer(t => t.delay === 60000);
  assert.match(records(), /Check these MX records/, 'a sibling routing refusal remains visible');
  for (const status of [{ state: 'connecting' }, { state: 'unreachable' }, { state: 'ready', expires_at: '1970-01-01T00:00:00Z' }]) {
    h.world.statuses = [Object.assign({ smtp_hostname: 'mx.example.com' }, status)];
    await h.fireTimer(t => t.delay === 60000);
    assert.doesNotMatch(records(), /Ready to receive|Check these MX|Domain not authorized/, 'unknown or expired readiness does not invent DNS failures');
  }
}

async function queuedRefresh() {
  const h = harness('status');
  await h.settle();
  const push = () => h.window.mammothmailEventsSource.emit('mx.dns_changed', { data: '{"payload":{"domain_id":"domain-1"}}' });
  h.world.holdGet = true;
  push();
  const before = h.gets();
  push(); push();
  h.world.holdGet = false;
  h.world.releaseGet();
  await h.settle();
  assert.equal(h.gets(), before + 1, 'busy pushes coalesce into one follow-up');
  h.world.stale = true;
  push();
  await h.settle();
  const stale = h.gets();
  assert.ok(h.pends(2000), 'stale results schedule a bounded retry');
  await h.settle();
  assert.equal(h.gets(), stale, 'stale responses do not spin');
  h.world.stale = false;
  await h.fireTimer(t => t.delay === 2000);
  assert.equal(h.gets(), stale + 1, 'the stale retry reads the completed check');
  h.document.hidden = true;
  push();
  assert.equal(h.gets(), stale + 1, 'hidden tabs defer pushes');
  h.document.hidden = false;
  h.document.visibilitychange();
  await h.settle();
  assert.equal(h.gets(), stale + 2, 'visible tabs consume the pending update');
  h.dlg.close();
  push();
  assert.equal(h.gets(), stale + 2, 'closed dialogs do not fetch');
}

const SUITES = {
  statusView,
  statusSettings,
  wizardHosted: () => wizard('hosted'),
  wizardSubdomain: () => wizard('subdomain'),
  wizardCustom: () => wizard('custom'),
  introCopy,
  rotation,
  liveUpdates,
  receiverTruth,
  queuedRefresh
};
(async () => {
  // The suites that pass against the shipped behaviour. statusSettings is the S5
  // feature suite; run it by name to see what it is waiting on.
   const defaultSuites = ['statusView', 'wizardHosted', 'wizardSubdomain', 'wizardCustom', 'introCopy', 'rotation', 'liveUpdates', 'receiverTruth', 'queuedRefresh'];
  const names = process.argv.slice(2).length ? process.argv.slice(2) : defaultSuites;
  for (const name of names) {
    const fn = SUITES[name];
    if (!fn) { throw new Error('unknown suite ' + name); }
    await fn();
  }
  console.log('Tusk MX wizard interaction checks passed: ' + names.join(', '));
})().catch(error => { console.error(error); process.exitCode = 1; });
