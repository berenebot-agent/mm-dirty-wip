// Dependency-free checks of the real wizard shell controller, not CSS strings.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const marker = source.indexOf('// Share editor fields and persistence');
assert.ok(marker >= 0, 'wizard controller exists');
const start = source.indexOf('function prepareReceivingInstructions');
const end = source.indexOf('\n(function () {', marker);

class Element {
  constructor(tag = 'div', attrs = {}) {
    this.tagName = tag.toUpperCase(); this.attrs = { ...attrs }; this.children = [];
    this.dataset = {}; this.events = {}; this.disabled = false; this.hidden = false;
    this.classList = { add: name => { this.className = (this.className + ' ' + name).trim(); } };
  }
  get id() { return this.attrs.id || ''; }
  get name() { return this.attrs.name || ''; }
  set name(value) { this.attrs.name = value; }
  get className() { return this.attrs.class || ''; }
  set className(value) { this.attrs.class = value; }
  get firstChild() { return this.children[0]; }
  get previousElementSibling() { return this.parentNode?.children[this.parentNode.children.indexOf(this) - 1]; }
  get nextElementSibling() { return this.parentNode?.children[this.parentNode.children.indexOf(this) + 1]; }
  getAttribute(name) { return this.attrs[name] ?? null; }
  setAttribute(name, value) { this.attrs[name] = value; }
  removeAttribute(name) { delete this.attrs[name]; }
  appendChild(child) {
    if (child.parentNode) { child.remove(); }
    this.children.push(child); child.parentNode = this; return child;
  }
  remove() {
    if (this.parentNode) { this.parentNode.children = this.parentNode.children.filter(child => child !== this); }
    this.parentNode = null;
  }
  replaceChildren() { this.children.slice().forEach(child => child.remove()); }
  replaceWith(node) { const parent = this.parentNode; parent.insertBefore(node, this); this.remove(); }
  insertBefore(child, ref) {
    if (child.parentNode) { child.remove(); }
    const at = this.children.indexOf(ref);
    this.children.splice(at < 0 ? this.children.length : at, 0, child); child.parentNode = this;
  }
  insertAdjacentElement(where, node) {
    assert.equal(where, 'afterend');
    const parent = this.parentNode;
    parent.insertBefore(node, parent.children[parent.children.indexOf(this) + 1]);
  }
  matches(selector) {
    if (selector.includes(',')) { return selector.split(',').some(s => this.matches(s.trim())); }
    const parts = selector.trim().split(/\s+/);
    if (parts.length > 1) {
      if (!this.matches(parts.pop())) { return false; }
      let parent = this.parentNode;
      while (parts.length) {
        const wanted = parts.pop();
        while (parent && !parent.matches(wanted)) { parent = parent.parentNode; }
        if (!parent) { return false; }
        parent = parent.parentNode;
      }
      return true;
    }
    const attr = selector.match(/\[([^=\]]+)(?:="([^"]*)")?\]/);
    if (attr && (this.getAttribute(attr[1]) === null || (attr[2] !== undefined && this.getAttribute(attr[1]) !== attr[2]))) { return false; }
    const base = selector.replace(/\[[^\]]+\]/g, '');
    const tag = base.match(/^[a-z][\w-]*/i);
    const id = base.match(/#([\w-]+)/);
    const cls = base.match(/\.([\w-]+)/);
    return (!tag || this.tagName === tag[0].toUpperCase()) && (!id || this.id === id[1]) && (!cls || this.className.split(/\s+/).includes(cls[1]));
  }
  querySelectorAll(selector) {
    if (selector.startsWith(':scope > ')) { return this.children.filter(child => child.matches(selector.slice(9))); }
    const selectors = selector.split(',');
    const out = [];
    function visit(node) {
      node.children.forEach(child => {
        if (selectors.some(s => child.matches(s.trim()))) { out.push(child); }
        visit(child);
      });
    }
    visit(this); return out;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  addEventListener(name, fn) { (this.events[name] ||= []).push(fn); }
  emit(name) { (this.events[name] || []).forEach(fn => fn({ target: this, preventDefault() {} })); }
  set innerHTML(value) {
    this.replaceChildren();
    for (const match of value.matchAll(/<(button|span|a)([^>]*)>(.*?)<\/\1>/g)) {
      const attrs = {};
      for (const attr of match[2].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) { attrs[attr[1]] = attr[2] || ''; }
      const node = new Element(match[1], attrs); node.textContent = match[3]; this.appendChild(node);
    }
  }
}

function fixture(kind, options = {}) {
  const root = new Element();
  const state = root.appendChild(new Element('div', { id: 'domain-wizard-state' }));
  state.dataset = { domain: 'domain-1', step: kind, next: '/next', back: '/back', inherit: 'false', review: options.review ? 'true' : 'false' };
  const dlg = root.appendChild(new Element('dialog', { id: `domain-${kind}-dialog-domain-1` }));
  dlg.appendChild(new Element('h2'));
  const form = dlg.appendChild(new Element('form', { id: 'config-form', class: 'cfg-form' }));
  const provider = form.appendChild(new Element('select', { class: 'provider-select' }));
  provider.value = options.provider || 'resend';
  const group = form.appendChild(new Element('div', { class: 'provider-fields' })); group.dataset.provider = provider.value;
  const guide = group.appendChild(new Element('ol', { class: 'steps' })); guide.textContent = 'Provider instructions';
  const field = group.appendChild(new Element('input', { name: 'cfg_resend_api_key' }));
  const footer = dlg.appendChild(new Element('div', { class: 'dialog-actions' }));
  const danger = footer.appendChild(new Element('div', { class: 'dialog-danger' }));
  danger.appendChild(new Element('button')).textContent = 'Remove';
  footer.appendChild(new Element('button', { 'data-close-dialog': '' })).textContent = 'Cancel';
  const save = footer.appendChild(new Element('button', { 'data-save-provider': '', form: form.id })); save.textContent = 'Save';
  let receiverField;
  if (options.remote) {
    const panel = dlg.appendChild(new Element('section', { 'data-remote-mx-panel': '' }));
    const receiver = panel.appendChild(new Element('form', { action: '/ui/account/mx' }));
    receiver.appendChild(new Element('input', { name: '_csrf' }));
    receiverField = receiver.appendChild(new Element('input', { name: 'rx_url' }));
    const privateOption = receiver.appendChild(new Element('label', { class: 'inherit-option' }));
    privateOption.appendChild(new Element('input', { name: 'rx_allow_private' }));
    receiver.appendChild(new Element('p'));
    receiver.appendChild(new Element('label'));
    receiver.appendChild(new Element('textarea', { name: 'rx_ca' }));
    const receiverActions = receiver.appendChild(new Element('div', { class: 'dialog-actions' }));
    receiverActions.appendChild(new Element('button')).textContent = 'Save receiver';
    panel.appendChild(new Element('form', { action: '/ui/account/mx/clear' })).appendChild(new Element('button')).textContent = 'Remove receiver';
  }
  if (options.credential) {
    const credential = root.appendChild(new Element('dialog', { id: 'domain-credential-dialog' }));
    credential.appendChild(new Element('h2'));
    credential.appendChild(new Element('pre', { id: 'domain-credential-url' })).textContent = 'one-time-credential';
    const actions = credential.appendChild(new Element('div', { class: 'dialog-actions' }));
    actions.appendChild(new Element('button', { id: 'domain-credential-copy' })).textContent = 'Copy URL';
    actions.appendChild(new Element('button', { 'data-close-dialog': '' })).textContent = 'Done';
  }
  const destinations = [];
  const document = {
    getElementById: id => root.querySelector('#' + id),
    createElement: tag => new Element(tag)
  };
  vm.runInNewContext(source.slice(start, end), { document, window: { location: { assign: url => destinations.push(url) } } });
  return { root, dlg, form, footer, save, provider, receiverField, destinations };
}

for (const kind of ['receiving', 'sending']) {
  const h = fixture(kind);
  assert.equal(h.dlg.querySelectorAll(':scope > .dialog-actions').length, 1, 'there is exactly one navigation row');
  assert.equal(h.dlg.querySelectorAll('[data-save-provider]').length, 1, 'there is exactly one primary button');
  assert.equal(h.dlg.querySelectorAll('[data-wizard-next]').length, 1);
  assert.equal(h.save.textContent, 'Next');
  assert.ok(!h.dlg.querySelector('.dialog-danger'), 'settings actions are not part of addition');
  assert.equal(h.footer.querySelector('[data-wizard-skip]').href, '/next');
  h.footer.querySelector('[data-wizard-back]').emit('click');
  assert.deepEqual(h.destinations, ['/back']);
}
const remote = fixture('receiving', { remote: true, provider: 'remotemx' });
assert.equal(remote.receiverField.getAttribute('form'), remote.form.id, 'receiver fields submit with the domain form');
assert.equal(remote.receiverField.disabled, false);
assert.equal(remote.dlg.querySelectorAll('form').length, 1, 'no nested receiver save form remains');
assert.equal(remote.dlg.querySelectorAll('button').filter(button => button.textContent === 'Save receiver').length, 0);
assert.ok(remote.dlg.querySelector('details.remote-mx-advanced').querySelector('[name="rx_ca"]'), 'optional CA/private-network settings are grouped separately');
remote.provider.value = 'resend'; remote.provider.emit('change');
assert.equal(remote.receiverField.disabled, true, 'inactive receiver fields cannot validate or submit');
assert.equal(remote.form.querySelector('[name="wizard_receiver_settings"]').disabled, true);
const credential = fixture('receiving', { credential: true, provider: 'postmark' });
assert.equal(credential.root.querySelectorAll('dialog').length, 1, 'one-time credentials appear in the same shell, not a second modal');
assert.equal(credential.dlg.querySelectorAll(':scope > .dialog-actions').length, 1);
assert.equal(credential.dlg.querySelectorAll('[data-wizard-next]').length, 1);
assert.ok(credential.dlg.querySelector('#domain-credential-copy'), 'Copy URL is retained beside the secret');
credential.save.emit('click');
assert.deepEqual(credential.destinations, ['/next']);
const editing = fixture('receiving');
assert.equal(editing.dlg.querySelector('.receiving-instructions').hidden, true, 'configuration is a focused screen, not credentials plus a long guide');
editing.form.querySelector('[name="cfg_resend_api_key"]').value = 'unsaved-key';
editing.dlg.querySelector('.provider-guide').emit('click');
assert.equal(editing.form.hidden, true, 'credential setup help is its own focused screen');
editing.save.emit('click');
assert.equal(editing.form.hidden, false);
assert.equal(editing.form.querySelector('[name="cfg_resend_api_key"]').value, 'unsaved-key', 'reading instructions preserves unsaved edits');
assert.equal(editing.destinations.length, 0, 'instruction help returns to configuration without navigating or saving');
const review = fixture('receiving', { review: true });
assert.equal(review.form.hidden, true, 'after saving, instructions have their own screen');
assert.equal(review.dlg.querySelector('.receiving-instructions').hidden, false);
review.save.emit('click'); assert.deepEqual(review.destinations, ['/next']);
console.log('domain wizard shell checks passed: single footer, Remote MX save, inline generated credentials');
