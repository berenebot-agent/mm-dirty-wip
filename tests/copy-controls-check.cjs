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
    this.parentNode = null;
    this.nodes = {};
    this.children = [];
    this.style = {};
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
  removeAttribute(name) { delete this.attributes[name]; }
  closest(selector) { return selector === 'p' ? this.parent : null; }
  querySelector(selector) { return this.nodes[selector] || null; }
  // appendChild moves an already-appended node, as the DOM does.
  appendChild(node) {
    const at = this.children.indexOf(node);
    if (at >= 0) { this.children.splice(at, 1); }
    this.children.push(node);
    node.parentNode = this;
    return node;
  }
  remove() {}
}

// makeWorld runs the copy modules against a DOM stand-in. secure:false is a
// page served over plain HTTP, where the Clipboard API does not exist.
function makeWorld(options) {
  const secure = !options || options.secure !== false;
  const copied = [];
  const notices = [];
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

  const selected = { node: null };
  const secret = new Element('PRE');
  secret.textContent = 'mmm_one-time-secret';
  const secretCopy = new Element('BUTTON');
  secretCopy.textContent = 'Copy';
  secretCopy.setAttribute('data-copy-value', secret.textContent);
  secretCopy.previousElementSibling = secret;
  // The open modal dialog. A toast mounted in the body sits behind the
  // dialog's grey backdrop, so the helper mounts it in the dialog instead.
  const modal = new Element('DIALOG');
  const document = {
    body: { appendChild: node => notices.push(node) },
    createElement: tag => new Element(tag.toUpperCase()),
    querySelectorAll(selector) {
      if (selector === '[data-copy-value]') { return [secretCopy]; }
      return selector === '[data-copy]' ? controls.flatMap(c => [c.name, c.button]) : [];
    },
    createRange() { return { selectNodeContents(node) { selected.node = node; }, selectNode() {} }; },
    querySelector(selector) { return selector === 'dialog:modal' ? modal : null; }
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
    isSecureContext: secure,
    navigator,
    getSelection() { return { removeAllRanges() {}, addRange() {} }; },
    setTimeout() { return 1; },
    clearTimeout() {},
    mammothmailCopy: null
  };
  const context = vm.createContext({ window, document, navigator, Promise });

  vm.runInContext(extractIIFE(helperStart, helperEnd), context);
  vm.runInContext(extractIIFE(dataCopyStart, dataCopyEnd), context);
  const valueCopyStart = source.lastIndexOf('(function () {', source.indexOf("document.querySelectorAll('[data-copy-value]')"));
  vm.runInContext(extractIIFE(valueCopyStart, source.length), context);

  return { copied, notices, controls, secret, secretCopy, modal, selected, window, navigator };
}

async function flushPromises() {
  await new Promise(resolve => setImmediate(resolve));
}

async function secureContext() {
  const world = makeWorld({ secure: true });

  for (const control of world.controls) {
    control.name.setAttribute('data-copy', control.value);
    control.button.setAttribute('data-copy', control.value);

    control.button.dispatch('click');
    await flushPromises();
    assert.equal(world.copied.at(-1), control.value, control.id + ' delete copy button copies its current value');

    control.name.dispatch('click');
    await flushPromises();
    assert.equal(world.copied.at(-1), control.value, control.id + ' confirmation name copies its current value');
  }

  const ordinaryButton = new Element('BUTTON');
  ordinaryButton.textContent = 'Copy';
  world.window.mammothmailCopy.wire(ordinaryButton, { text: 'ordinary copy value' });
  ordinaryButton.dispatch('click');
  await flushPromises();
  assert.equal(world.copied.at(-1), 'ordinary copy value', 'ordinary copy control writes its value');
  assert.equal(ordinaryButton.textContent, 'Copied!', 'ordinary copy control keeps its label feedback');

  world.secretCopy.dispatch('click');
  await flushPromises();
  assert.equal(world.copied.at(-1), world.secret.textContent, 'secret Copy button copies only the displayed value');

  // The toast must be mounted in the open modal dialog: mounted in the body it
  // is painted behind the dialog's grey backdrop and is never seen.
  assert.equal(world.notices.length, 0, 'no toast is mounted in the body');
  assert.equal(world.modal.children.length, 1, 'the toast is mounted in the open dialog');
  assert.equal(world.modal.children[0].textContent, 'Copied to clipboard', 'the toast announces success in the dialog');

  world.navigator.clipboard.writeText = () => Promise.reject(new Error('permission denied'));
  world.selected.node = null;
  world.secretCopy.dispatch('click');
  await flushPromises();
  assert.equal(world.selected.node, world.secret, 'refused clipboard write selects the secret');
  assert.match(world.modal.children.at(-1).textContent, /value is selected.*Ctrl\+C/, 'refused write gives manual copy guidance');
  assert.match(world.modal.children.at(-1).textContent, /refused/, 'the manual copy message no longer blames HTTPS');
}

async function insecureContext() {
  const world = makeWorld({ secure: false });

  // A copy control that cannot work must not be offered.
  assert.equal(world.secretCopy.hidden, true, 'the copy button is hidden without a clipboard');
  assert.equal(world.controls[0].button.hidden, true, 'the delete copy button is hidden without a clipboard');
  assert.equal(world.controls[0].name.getAttribute('role'), '', 'an inline value loses the button role');
  assert.equal(world.controls[0].name.getAttribute('tabindex'), '', 'an inline value loses the tab stop');
  assert.equal(world.controls[0].name.getAttribute('title'), '', 'an inline value loses the click-to-copy title');
  assert.equal(world.controls[0].name.style.cursor, 'text', 'an inline value is no longer clickable');

  world.secretCopy.dispatch('click');
  world.controls[0].button.dispatch('click');
  world.controls[0].name.dispatch('click');
  await flushPromises();
  assert.equal(world.copied.length, 0, 'no clipboard write is attempted without a clipboard');
  assert.equal(world.notices.length, 0, 'no toast is raised without a clipboard');
  assert.equal(world.modal.children.length, 0, 'no toast is raised in the dialog without a clipboard');
}

async function run() {
  await secureContext();
  await insecureContext();
  console.log('copy-controls-check: all checks passed');
}

run().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
