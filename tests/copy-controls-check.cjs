// Dependency-free checks for the shared clipboard helper and server-rendered
// data-copy controls. Run: node tests/copy-controls-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');

function extractIIFE(start, end) {
  const body = source.slice(start, end);
  const open = body.indexOf('(function () {');
  const close = body.indexOf('\n})();', open);
  assert.ok(open >= 0 && close >= 0, 'copy module IIFE boundaries exist');
  return body.slice(open, close + '\n})();'.length);
}

const helperMarker = source.indexOf('// Shared copy-to-clipboard helper.');
const helperStart = source.indexOf('(function () {', helperMarker);
const helperEnd = source.indexOf('\n})();', helperStart) + '\n})();'.length;
const dataCopyMarker = source.indexOf('These controls are server-rendered.');
const dataCopyStart = source.lastIndexOf('(function () {', dataCopyMarker);
const dataCopyEnd = source.indexOf('\n})();', dataCopyMarker) + '\n})();'.length;
assert.ok(helperMarker >= 0 && dataCopyMarker >= 0, 'copy modules are present');

const antlerStart = source.indexOf("document.querySelectorAll('form[data-antler-domain]')");
assert.ok(antlerStart > dataCopyStart, 'data-copy controls are wired before optional Antler setup');

class Element {
  constructor(tagName = 'DIV') {
    this.tagName = tagName;
    this.textContent = '';
    this.attributes = {};
    this.handlers = {};
    this.parent = null;
    this.nodes = {};
    const classes = new Set();
    this.classList = {
      add: name => classes.add(name),
      remove: name => classes.delete(name),
      contains: name => classes.has(name)
    };
  }
  addEventListener(name, fn) {
    (this.handlers[name] = this.handlers[name] || []).push(fn);
  }
  dispatch(name, event = {}) {
    (this.handlers[name] || []).slice().forEach(fn => fn(event));
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] || ''; }
  closest(selector) { return selector === 'p' ? this.parent : null; }
  querySelector(selector) { return this.nodes[selector] || null; }
  remove() {}
}

const copied = [];
const controls = [];
for (const [id, value] of [
  ['inbox', 'ASDASD@example.test'],
  ['domain', 'example.test'],
  ['client', 'ASDASD client']
]) {
  const paragraph = new Element('P');
  const name = new Element('B');
  name.setAttribute('data-copy', '');
  name.className = 'confirm-name';
  const button = new Element('BUTTON');
  button.setAttribute('data-copy', '');
  paragraph.nodes['.confirm-name'] = name;
  name.parent = paragraph;
  button.parent = paragraph;
  controls.push({ id, value, name, button });
}

const notices = [];
const secret = new Element('PRE');
secret.textContent = 'mmm_one-time-secret';
const secretCopy = new Element('BUTTON');
secretCopy.textContent = 'Copy';
secretCopy.setAttribute('data-copy-value', secret.textContent);
secretCopy.previousElementSibling = secret;
let selected = null;
const document = {
  body: { appendChild: node => notices.push(node) },
  createElement: tag => new Element(tag.toUpperCase()),
  querySelectorAll(selector) {
    if (selector === '[data-copy-value]') return [secretCopy];
    return selector === '[data-copy]' ? controls.flatMap(c => [c.name, c.button]) : [];
  },
  createRange() { return { selectNodeContents(node) { selected = node; }, selectNode() {} }; },
  querySelector() { return null; }
};
const navigator = {
  clipboard: {
    writeText(text) {
      copied.push(text);
      return Promise.resolve();
    }
  }
};
const window = {
  isSecureContext: true,
  navigator,
  getSelection() { return { removeAllRanges() {}, addRange() {} }; },
  setTimeout() { return 1; },
  clearTimeout() {},
  mailmooseCopy: null
};
const context = vm.createContext({ window, document, navigator, Promise });

vm.runInContext(extractIIFE(helperStart, helperEnd), context);
vm.runInContext(extractIIFE(dataCopyStart, dataCopyEnd), context);
const valueCopyStart = source.lastIndexOf('(function () {', source.indexOf("document.querySelectorAll('[data-copy-value]')"));
vm.runInContext(extractIIFE(valueCopyStart, source.length), context);

async function flushPromises() {
  await new Promise(resolve => setImmediate(resolve));
}

async function run() {
  for (const control of controls) {
    control.name.setAttribute('data-copy', control.value);
    control.button.setAttribute('data-copy', control.value);

    control.button.dispatch('click');
    await flushPromises();
    assert.equal(copied.at(-1), control.value, control.id + ' delete copy button copies its current value');
    assert.equal(notices.at(-1).textContent, 'Copied to clipboard', control.id + ' copy button announces success');

    control.name.dispatch('click');
    await flushPromises();
    assert.equal(copied.at(-1), control.value, control.id + ' confirmation name copies its current value');
    assert.equal(notices.at(-1).textContent, 'Copied to clipboard', control.id + ' name copy announces success');
  }

  const ordinaryButton = new Element('BUTTON');
  ordinaryButton.textContent = 'Copy';
  window.mailmooseCopy.wire(ordinaryButton, { text: 'ordinary copy value' });
  ordinaryButton.dispatch('click');
  await flushPromises();
  assert.equal(copied.at(-1), 'ordinary copy value', 'ordinary copy control writes its value');
  assert.equal(ordinaryButton.textContent, 'Copied!', 'ordinary copy control keeps its label feedback');
  assert.equal(notices.at(-1).textContent, 'Copied to clipboard', 'ordinary copy control announces success');

  secretCopy.dispatch('click');
  await flushPromises();
  assert.equal(copied.at(-1), secret.textContent, 'secret Copy button copies only the displayed value');

  window.isSecureContext = false;
  const writes = copied.length;
  secretCopy.dispatch('click');
  await flushPromises();
  assert.equal(copied.length, writes, 'HTTP fallback does not attempt a clipboard write');
  assert.equal(selected, secret, 'HTTP fallback selects the adjacent secret');
  assert.match(notices.at(-1).textContent, /value is selected.*Ctrl\+C/, 'HTTP fallback gives manual copy guidance');

  window.isSecureContext = true;
  selected = null;
  navigator.clipboard.writeText = () => Promise.reject(new Error('permission denied'));
  secretCopy.dispatch('click');
  await flushPromises();
  assert.equal(selected, secret, 'refused clipboard write selects the secret');
  assert.match(notices.at(-1).textContent, /value is selected.*Ctrl\+C/, 'refused write gives manual copy guidance');

  console.log('copy-controls-check: all checks passed');
}

run().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
