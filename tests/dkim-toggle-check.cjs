// Dependency-free check for the provider-editor dependent-field behaviour: that
// switching a field another field depends on (Direct MX "DKIM signing") shows or
// hides the dependent block immediately, and keeps hidden inputs disabled so a
// switched-off block is not submitted.
//
// Run: node tests/dkim-toggle-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');

// Locate the provider-editor closure by the first statement it runs.
const startMarker = '(function () {\n  // syncDialMXService materialises';
const start = source.indexOf(startMarker);
assert.ok(start >= 0, 'could not locate the provider editor closure in app.js');
const end = source.indexOf('\n})();', start);
assert.ok(end >= 0, 'could not locate the end of the provider editor closure');
const moduleSource = source.slice(start, end + '\n})();'.length);

// Element is a minimal DOM stand-in covering exactly what the closure touches.
// Children are searched for querySelector/querySelectorAll so the closure's
// closest()-then-querySelector() lookup resolves naturally.
class Element {
  constructor(tag = 'DIV') {
    this.tagName = tag;
    this.children = [];
    this.events = {};
    this.attrs = {};
    this.value = '';
    this.hidden = false;
    this.disabled = false;
    this.textContent = '';
    this.parentNode = null;
    this.options = [];
    this.selectedIndex = 0;
  }
  addEventListener(name, fn) { (this.events[name] = this.events[name] || []).push(fn); }
  emit(name, event = {}) { (this.events[name] || []).slice().forEach(fn => fn(event)); }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  setAttribute(name, value) { this.attrs[name] = value; }
  getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null; }
  removeAttribute(name) { delete this.attrs[name]; }
  hasAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name); }
  remove() {}
  // matches applies the handful of attribute/name/descendant selectors this
  // closure uses.
  matches(selector) {
    // Descendant selectors: the last token must match this element and every
    // earlier token must match an ancestor, walking upwards.
    if (selector.indexOf(' ') !== -1) {
      const parts = selector.trim().split(/\s+/);
      if (!this.matches(parts[parts.length - 1])) { return false; }
      let node = this.parentNode;
      for (let i = parts.length - 2; i >= 0; i--) {
        let matched = false;
        while (node) {
          const current = node;
          node = node.parentNode;
          if (current.matches(parts[i])) { matched = true; break; }
        }
        if (!matched) { return false; }
      }
      return true;
    }
    // Attribute-presence selectors: [attr] matches any element carrying it.
    const presence = /^\[([a-zA-Z0-9_-]+)\]$/.exec(selector);
    if (presence) {
      return this.hasAttribute(presence[1]);
    }
    // Class selectors: .name matches membership in the class attribute.
    if (selector.charAt(0) === '.') {
      return (this.attrs['class'] || '').split(/\s+/).indexOf(selector.slice(1)) !== -1;
    }
    // Tag selectors: a bare tag name.
    if (/^[a-z]+$/.test(selector)) {
      return this.tagName === selector.toUpperCase();
    }
    // Tag + attribute: select[name="x"].
    const tagName = /^([a-z]+)\[name="([^"]+)"\]$/.exec(selector);
    if (tagName) {
      return this.tagName === tagName[1].toUpperCase() && this.getAttribute('name') === tagName[2];
    }
    // Bare attribute-equals: [name="x"].
    const nameMatch = /^\[name="([^"]+)"\]$/.exec(selector);
    if (nameMatch) {
      return this.getAttribute('name') === nameMatch[1];
    }
    return false;
  }
  // Each branch of a comma-separated selector list is tried.
  matchesAny(selector) {
    return selector.split(',').some(part => this.matches(part.trim()));
  }
  walk(out = []) {
    for (const child of this.children) {
      out.push(child);
      child.walk(out);
    }
    return out;
  }
  querySelectorAll(selector) { return this.walk([]).filter(el => el.matchesAny(selector)); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) {
    let node = this;
    while (node) {
      if (node.matchesAny(selector)) { return node; }
      node = node.parentNode;
    }
    return null;
  }
}

function el(tag, attrs = {}) {
  const e = new Element(tag);
  Object.assign(e.attrs, attrs);
  return e;
}

// buildDialog assembles the provider-editor markup the server emits for Direct
// MX: a provider select, the DKIM toggle, one dependent field, and one plain
// field that must be unaffected.
function buildDialog(toggleValue) {
  const dlg = el('DIALOG', { class: 'domain-dialog' });

  const providerSel = el('SELECT', { name: 'provider', class: 'provider-select' });
  providerSel.value = 'mx';
  const opt = el('OPTION', { 'data-next': '1' });
  providerSel.options = [opt];
  providerSel.selectedIndex = 0;
  dlg.appendChild(providerSel);

  const group = el('DIV', { class: 'provider-fields', 'data-provider': 'mx' });
  dlg.appendChild(group);

  const toggle = el('SELECT', { name: 'cfg_mx_dkim_enabled' });
  toggle.value = toggleValue;
  group.appendChild(toggle);

  // The DKIM record panel, as the server renders it: hidden and disabled until
  // the toggle is on. data-show-field carries the controlling input's name.
  const depWrap = el('SECTION', { class: 'dkim-panel', 'data-dkim-panel': '1', 'data-show-field': 'cfg_mx_dkim_enabled', 'data-show-values': 'on' });
  depWrap.hidden = true;
  const depInput = el('BUTTON', { name: 'cfg_mx_dkim_copy' });
  depInput.disabled = true;
  depWrap.appendChild(depInput);
  group.appendChild(depWrap);

  // A field with no dependency, which must stay untouched.
  const plainWrap = el('DIV', { class: 'cfg-field' });
  const plainInput = el('INPUT', { name: 'cfg_mx_helo' });
  plainInput.disabled = false;
  plainWrap.appendChild(plainInput);
  group.appendChild(plainWrap);

  const save = el('BUTTON', { 'data-save-provider': '1' });
  save.textContent = 'Next';
  dlg.appendChild(save);

  return { dlg, toggle, depWrap, depInput, plainWrap, plainInput, save };
}

function run(dlg) {
  const document = {
    querySelectorAll: sel => (sel === '.domain-dialog' ? [dlg] : [])
  };
  const context = { document, console };
  vm.createContext(context);
  vm.runInContext(moduleSource, context);
}

// 1. Signing off: the dependent field is hidden and its input disabled.
{
  const d = buildDialog('off');
  run(d.dlg);
  assert.equal(d.depWrap.hidden, true, 'dependents must be hidden while signing is off');
  assert.equal(d.depInput.disabled, true, 'a hidden dependent input must be disabled so it is not submitted');
  assert.equal(d.save.textContent, 'Save', 'with signing off the Direct MX button must read Save');
}

// 2. Signing on: the dependent field appears immediately, enabled.
{
  const d = buildDialog('on');
  run(d.dlg);
  assert.equal(d.depWrap.hidden, false, 'dependents must be visible while signing is on');
  assert.equal(d.depInput.disabled, false, 'a visible dependent input must be enabled');
  assert.equal(d.save.textContent, 'Next', 'with signing on the Direct MX button must offer the next step');
}

// 3. Toggling off then on re-hides and re-shows, keeping disabled in step.
{
  const d = buildDialog('on');
  run(d.dlg);
  d.toggle.value = 'off';
  d.toggle.emit('change');
  assert.equal(d.depWrap.hidden, true, 'turning signing off must hide the dependents');
  assert.equal(d.depInput.disabled, true, 'turning signing off must disable the dependents');
  assert.equal(d.save.textContent, 'Save', 'turning signing off must revert the button to Save');

  d.toggle.value = 'on';
  d.toggle.emit('change');
  assert.equal(d.depWrap.hidden, false, 'turning signing back on must show the dependents again');
  assert.equal(d.depInput.disabled, false, 'turning signing back on must re-enable the dependents');
  assert.equal(d.save.textContent, 'Next', 'turning signing back on must restore the Next step');
}

// 4. The dependency only affects the fields that declare it.
{
  const d = buildDialog('off');
  run(d.dlg);
  assert.equal(d.plainWrap.hidden, false, 'an independent field must not be hidden');
  assert.equal(d.plainInput.disabled, false, 'an independent field must stay enabled');
}

console.log('dkim-toggle-check: all assertions passed');
