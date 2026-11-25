const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const marker = '// Folder and label management (shared mailbox sidebar).';
const start = source.indexOf('(function () {', source.indexOf(marker));
const end = source.indexOf('\n}());', start);
assert.ok(start >= 0 && end > start, 'mailbox navigation module not found');
const script = source.slice(start, end + '\n}());'.length);

function checkPicker(activeLink, expectedName) {
  const text = { textContent: 'Folders' };
  const attributes = new Map([['aria-expanded', 'false']]);
  let click;
  const toggle = {
    hidden: true,
    querySelector: () => text,
    getAttribute: name => attributes.get(name),
    setAttribute: (name, value) => attributes.set(name, value),
    addEventListener: (type, handler) => {
      assert.equal(type, 'click');
      click = handler;
    }
  };
  const sidebar = {
    getAttribute: () => 'preview',
    querySelector: selector => ({
      '.mailnav-toggle': toggle,
      'a.folder.active': activeLink
    })[selector] || null
  };
  vm.runInNewContext(script, { document: {
    getElementById: () => null,
    querySelector: () => sidebar
  } });
  assert.equal(toggle.hidden, false);
  assert.equal(text.textContent, expectedName);
  assert.equal(attributes.get('aria-expanded'), 'false');
  click();
  assert.equal(attributes.get('aria-expanded'), 'true');
  click();
  assert.equal(attributes.get('aria-expanded'), 'false');
}

checkPicker({ firstChild: { textContent: 'Inbox ' }, querySelector: () => null }, 'Inbox');
checkPicker({ querySelector: () => ({ textContent: 'Customer follow-up' }) }, 'Customer follow-up');
checkPicker(null, 'Folders');
console.log('mailbox-nav check passed');
