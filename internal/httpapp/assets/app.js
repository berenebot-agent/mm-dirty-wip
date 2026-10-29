(function () {
  'use strict';
  // Global Live/Loading status flag in the header. It is only rendered on the
  // pages that hold an event stream (dashboard and inbox views), where it shows
  // the SSE connection state: green "Live" while the stream is open, red
  // "Disconnected" when it drops and reconnects, amber "Sync problem" when the
  // remote index reports an error, and a spinner "Loading…" while a UI
  // background action is in flight (a remote message body load, an IMAP sync, a
  // background list reconcile). A dropped connection outranks Loading: it is the
  // more important signal. Short actions are delayed briefly before the spinner
  // shows, so a fast background refresh never flashes it.
  var node = document.querySelector('[data-conn-status]');
  var noop = function () {};
  if (!node) {
    window.mammothmailStatus = { connecting: noop, connected: noop, disconnected: noop, error: noop, busy: noop };
    return;
  }
  var label = node.querySelector('.conn-label');
  var BUSY_DELAY_MS = 250;
  var BUSY_ORDER = ['nav', 'sync', 'body', 'list', 'metadata'];
  var BUSY_LABELS = { nav: 'Loading…', sync: 'Syncing messages…', body: 'Loading message body…', list: 'Updating messages…', metadata: 'Syncing mailbox metadata…' };
  var conn = 'connecting';
  var syncError = '';
  var busy = {};
  var busyLabel = '';
  var showBusy = false;
  var showTimer = null;

  function activeBusy() {
    for (var i = 0; i < BUSY_ORDER.length; i++) {
      if (busy[BUSY_ORDER[i]]) {
        return BUSY_ORDER[i];
      }
    }
    return '';
  }

  function render() {
    var state, text, title;
    if (conn === 'disconnected') {
      state = 'disconnected';
      text = 'Disconnected';
      title = 'Live updates disconnected — reconnecting.';
    } else if (conn === 'connecting') {
      state = 'connecting';
      text = 'Connecting…';
      title = 'Connecting to live updates…';
    } else if (syncError) {
      state = 'error';
      text = 'Sync problem';
      title = syncError;
    } else if (showBusy) {
      state = 'loading';
      text = 'Loading…';
      title = busyLabel || 'Loading…';
    } else {
      state = 'live';
      text = 'Live';
      title = 'Live updates connected.';
    }
    node.setAttribute('data-state', state);
    node.setAttribute('title', title);
    node.setAttribute('aria-busy', state === 'loading' ? 'true' : 'false');
    if (label) {
      label.textContent = text;
    }
  }

  function refreshBusy() {
    var key = activeBusy();
    busyLabel = key ? BUSY_LABELS[key] : '';
    if (key) {
      if (!showBusy && !showTimer) {
        showTimer = window.setTimeout(function () {
          showTimer = null;
          showBusy = !!activeBusy();
          render();
        }, BUSY_DELAY_MS);
      } else if (showBusy) {
        render();
      }
      return;
    }
    if (showTimer) {
      window.clearTimeout(showTimer);
      showTimer = null;
    }
    showBusy = false;
    render();
  }

  window.mammothmailStatus = {
    connecting: function () { conn = 'connecting'; render(); },
    connected: function () { conn = 'live'; render(); },
    disconnected: function () { conn = 'disconnected'; render(); },
    error: function (text) { syncError = text || ''; render(); },
    busy: function (key, on) { busy[key] = !!on; refreshBusy(); }
  };
  render();
})();

// Shared copy-to-clipboard helper. The async Clipboard API only exists in a
// secure context (HTTPS or localhost), so a page served over plain HTTP cannot
// copy. Every copy control routes through here: a copy control is offered only
// where copying works, and a refused write selects the value where it can and
// shows one shared message telling the operator how to copy it manually.
(function () {
  'use strict';
  var SELECTED_MESSAGE = 'Copying to the clipboard was refused. The value is selected — press Ctrl+C (⌘C on Mac) to copy it.';
  var PLAIN_MESSAGE = 'Copying to the clipboard was refused. Select the value and copy it manually.';
  var message = null;
  var messageTimer = null;

  function hideMessage() {
    window.clearTimeout(messageTimer);
    if (!message) {
      return;
    }
    var node = message;
    message = null;
    node.classList.add('dismissing');
    window.setTimeout(function () { node.remove(); }, 300);
  }

  // toastHost picks where the message is mounted. A modal dialog is painted in
  // the top layer, above every ordinary element, so a message mounted in the
  // body would sit behind the dialog's grey backdrop and never be seen. The
  // :modal pseudo-class names the topmost open dialog; without it (a browser
  // too old for the top layer) the body is the best host there is.
  function toastHost() {
    var dialog = null;
    try {
      dialog = document.querySelector('dialog:modal');
    } catch (e) {
      dialog = null;
    }
    return dialog || document.body;
  }

  // announce shows the shared transient message. tone 'ok' is the green success
  // toast; the default warns that the clipboard write was refused.
  function announce(text, tone) {
    if (!message) {
      message = document.createElement('div');
      message.setAttribute('role', 'status');
      message.setAttribute('aria-live', 'polite');
      message.addEventListener('click', hideMessage);
    }
    // Mount the message in the current host, moving it only when the host
    // changed: a toast left inside a closed dialog would be invisible, and a
    // live region that is re-inserted may not be announced again.
    var host = toastHost();
    if (message.parentNode !== host) {
      host.appendChild(message);
    }
    message.className = tone === 'ok' ? 'ok notice' : 'notice warn';
    message.textContent = text;
    message.classList.remove('dismissing');
    /* Force reflow so a repeated message restarts the fade-out animation. */
    void message.offsetWidth;
    window.clearTimeout(messageTimer);
    messageTimer = window.setTimeout(hideMessage, 5000);
  }

  // canCopy reports whether this origin can write to the clipboard at all. The
  // async Clipboard API exists only in a secure context (HTTPS or localhost),
  // so a page served over plain HTTP cannot copy.
  function canCopy() {
    return !!(window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText);
  }

  function copyText(text) {
    if (canCopy()) {
      return navigator.clipboard.writeText(text);
    }
    return Promise.reject(new Error('clipboard unavailable'));
  }

  // select highlights a node's contents so the operator can copy manually.
  // target may be an element, a selector, or a function returning either.
  function select(target) {
    var node = typeof target === 'function' ? target() : target;
    if (typeof node === 'string') {
      node = document.querySelector(node);
    }
    if (!node || !window.getSelection || !document.createRange) {
      return false;
    }
    var range = document.createRange();
    range.selectNodeContents(node);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    return true;
  }

  // wire gives a control the standard copy behaviour. opts.text is a string or a
  // function returning one; opts.select is an element, selector or function
  // identifying what to highlight when a copy is not possible; opts.onSuccess
  // overrides the default "Copied!" label flash and success notice; opts.label
  // overrides the label restored after the flash.
  function wire(btn, opts) {
    if (!btn) {
      return;
    }
    if (!canCopy()) {
      // A control that cannot copy must not be offered: hide the button, and
      // for an inline value keep the text but drop the click-to-copy
      // affordance, so nothing looks copyable that is not.
      if ((btn.tagName || '').toLowerCase() === 'button') {
        btn.hidden = true;
      } else {
        btn.removeAttribute('role');
        btn.removeAttribute('tabindex');
        btn.removeAttribute('title');
        btn.style.cursor = 'text';
      }
      return;
    }
    opts = opts || {};
    var label = opts.label || btn.textContent;
    function activate() {
      var text = typeof opts.text === 'function' ? opts.text() : opts.text;
      if (text == null) {
        text = btn.getAttribute('data-copy') || '';
      }
      copyText(text).then(function () {
        if (opts.onSuccess) {
          opts.onSuccess();
          return;
        }
        btn.textContent = 'Copied!';
        announce('Copied to clipboard', 'ok');
        window.setTimeout(function () { btn.textContent = label; }, 1500);
      }).catch(function () {
        announce(select(opts.select) ? SELECTED_MESSAGE : PLAIN_MESSAGE);
      });
    }
    btn.addEventListener('click', activate);
    // Native buttons already turn Enter/Space into a click; only the
    // role="button" spans need the explicit key handler.
    if ((btn.tagName || '').toLowerCase() !== 'button') {
      btn.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          activate();
        }
      });
    }
  }

  window.mammothmailCopy = { text: copyText, wire: wire, announce: announce, select: select };
})();

(function () {
  // These controls are server-rendered. Bind them immediately after the shared
  // helper so an error in a later, unrelated page module cannot leave inbox and
  // delete-dialog copy controls inert. Values are read on click because the
  // delete dialogs populate their data-copy attributes when they open.
  document.querySelectorAll('[data-copy]').forEach(function (el) {
    window.mammothmailCopy.wire(el, {
      text: function () { return el.getAttribute('data-copy') || ''; },
      select: function () {
        // The delete-dialog icon buttons have no text of their own; the nearby
        // name/address is the selectable value.
        if ((el.tagName || '').toLowerCase() === 'button') {
          var scope = el.closest('p');
          return scope ? scope.querySelector('.confirm-name') : null;
        }
        return el;
      },
      onSuccess: function () {
        el.classList.add('copied');
        window.setTimeout(function () { el.classList.remove('copied'); }, 1200);
        window.mammothmailCopy.announce('Copied to clipboard', 'ok');
      }
    });
  });
})();

// Combobox: a real, always-visible dropdown list attached to a text input,
// replacing the native <input list> datalist popup (which browsers render as a
// small autocomplete hint rather than a proper list). Typing filters the
// options and free text is always allowed, so a group name can be picked from
// the list or typed fresh. It is a light ARIA combobox: the input keeps
// role="combobox" with aria-expanded/aria-controls/aria-activedescendant and the
// popup is a role="listbox" of role="option" rows. wire attaches behaviour to
// one input; setOptions replaces the option source.
(function () {
  'use strict';

  var openCombo = null;
  var uid = 0;

  function closeOpen() {
    if (openCombo) {
      openCombo.close();
    }
  }

  function wire(input, opts) {
    if (!input || input._mammothmailCombo) {
      return input && input._mammothmailCombo;
    }
    opts = opts || {};
    var id = 'combo-' + (++uid);
    var list = document.createElement('ul');
    list.className = 'combo-list';
    list.setAttribute('role', 'listbox');
    list.id = id + '-list';
    list.hidden = true;
    var wrap = document.createElement('div');
    wrap.className = 'combo-wrap';
    input.parentNode.insertBefore(wrap, input);
    wrap.appendChild(input);
    wrap.appendChild(list);

    var options = [];
    var activeIndex = -1;
    var api = {};

    function optionValues() {
      return typeof opts.options === 'function' ? opts.options() : (opts.options || []);
    }

    function filter(query) {
      var all = optionValues();
      var q = String(query || '').trim().toLowerCase();
      if (!q) {
        return all.slice();
      }
      return all.filter(function (name) { return String(name).toLowerCase().indexOf(q) >= 0; });
    }

    function render() {
      var values = filter(input.value);
      options = values;
      list.textContent = '';
      if (!values.length) {
        var empty = document.createElement('li');
        empty.className = 'combo-empty';
        empty.textContent = input.value.trim() ? 'No matching group — press Enter to create it' : 'No groups yet — type a name';
        empty.setAttribute('role', 'presentation');
        list.appendChild(empty);
        activeIndex = -1;
        return;
      }
      values.forEach(function (name, index) {
        var li = document.createElement('li');
        li.className = 'combo-option';
        li.setAttribute('role', 'option');
        li.id = id + '-opt-' + index;
        li.textContent = name;
        li.setAttribute('aria-selected', index === activeIndex ? 'true' : 'false');
        if (index === activeIndex) {
          li.classList.add('active');
        }
        li.addEventListener('mousedown', function (event) {
          event.preventDefault();
          pick(name);
        });
        list.appendChild(li);
      });
    }

    function open() {
      if (openCombo && openCombo !== api) {
        openCombo.close();
      }
      openCombo = api;
      list.hidden = false;
      input.setAttribute('aria-expanded', 'true');
      render();
    }

    function close() {
      if (list.hidden) {
        return;
      }
      list.hidden = true;
      activeIndex = -1;
      input.setAttribute('aria-expanded', 'false');
      input.removeAttribute('aria-activedescendant');
      if (openCombo === api) {
        openCombo = null;
      }
    }

    function highlight(index) {
      options = filter(input.value);
      if (!options.length) {
        activeIndex = -1;
        input.removeAttribute('aria-activedescendant');
        return;
      }
      activeIndex = (index + options.length) % options.length;
      render();
      input.setAttribute('aria-activedescendant', id + '-opt-' + activeIndex);
      var node = list.children[activeIndex];
      if (node && node.scrollIntoView) {
        node.scrollIntoView({ block: 'nearest' });
      }
    }

    function pick(name) {
      input.value = name;
      close();
      input.dispatchEvent(new Event('change', { bubbles: true }));
    }

    input.setAttribute('role', 'combobox');
    input.setAttribute('aria-expanded', 'false');
    input.setAttribute('aria-controls', list.id);
    input.setAttribute('aria-autocomplete', 'list');

    input.addEventListener('focus', function () { open(); });
    input.addEventListener('click', function () { open(); });
    input.addEventListener('input', function () { open(); render(); });
    input.addEventListener('keydown', function (event) {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        open();
        highlight(activeIndex + 1);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        open();
        highlight(activeIndex - 1);
      } else if (event.key === 'Enter') {
        if (!list.hidden && activeIndex >= 0 && options[activeIndex] != null) {
          event.preventDefault();
          pick(options[activeIndex]);
        } else {
          close();
        }
      } else if (event.key === 'Escape') {
        close();
      } else if (event.key === 'Tab') {
        close();
      }
    });
    input.addEventListener('blur', function () {
      // Delay so a click on an option (mousedown) lands before the list closes.
      window.setTimeout(close, 120);
    });

    api.setOptions = function () {
      if (!list.hidden) {
        render();
      }
    };
    api.close = close;
    input._mammothmailCombo = api;
    return api;
  }

  function closeAll() { closeOpen(); }
  document.addEventListener('click', function (event) {
    if (openCombo && !event.target.closest('.combo-wrap')) {
      closeOpen();
    }
  });

  window.mammothmailCombo = { wire: wire, close: closeAll };
})();

// The existing header status flag is also the durable action queue entry point.
(function () {
  'use strict';
  var menu = document.querySelector('header .menu-right');
  if (!menu) { return; }
  var flag = document.querySelector('[data-conn-status]');
  if (!flag) {
    flag = document.createElement('span');
    flag.className = 'conn-status';
    flag.textContent = 'Ready';
    menu.prepend(flag);
  }
  flag.setAttribute('role', 'button');
  flag.setAttribute('tabindex', '0');
  flag.setAttribute('aria-haspopup', 'dialog');
  flag.setAttribute('aria-expanded', 'false');
  var connLabel = flag.querySelector('.conn-label');
  if (connLabel) { connLabel.setAttribute('aria-live', 'polite'); }
  var badge = document.createElement('span');
  badge.className = 'conn-badge';
  badge.setAttribute('aria-live', 'polite');
  flag.appendChild(badge);

  var style = document.createElement('style');
  style.textContent = [
    '#action-queue[open]{position:absolute;left:0;top:0;right:auto;bottom:auto;margin:0;z-index:60;display:flex;flex-direction:column;width:304px;max-height:min(65vh,380px);padding:10px 12px;border:1px solid #dadce0;border-radius:9px;background:#fff;color:#202124;box-shadow:0 6px 20px rgba(32,33,36,.16);font-size:13px;line-height:1.4;box-sizing:border-box;outline:none}',
    '#action-queue::before{content:"";position:absolute;top:-6px;left:var(--aq-caret,18px);width:10px;height:10px;background:#fff;border-left:1px solid #dadce0;border-top:1px solid #dadce0;transform:rotate(45deg)}',
    '.aq-head{display:flex;align-items:center;gap:8px;padding:0 0 7px;border-bottom:1px solid #eee;flex:0 0 auto}',
    '.aq-title{font-weight:600;flex:0 0 auto}',
    '.aq-summary{margin-left:auto;color:#5f6368;font-size:12px;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.aq-close{width:26px;height:26px;padding:0;flex:0 0 auto;border:0;border-radius:50%;background:transparent;color:#5f6368;font-size:18px;line-height:1}',
    '.aq-close:hover{background:#f1f3f4;color:#202124}',
    '.aq-clear{border:0;background:transparent;color:#174ea6;font-size:12px;line-height:1.4;padding:3px 6px;border-radius:5px;cursor:pointer;flex:0 0 auto}',
    '.aq-clear:hover{background:#e8f0fe}',
    '.aq-clear[hidden]{display:none}',
    '.aq-list{flex:1 1 auto;min-height:0;overflow-y:auto;overscroll-behavior:contain}',
    '.aq-row{display:grid;grid-template-columns:7px minmax(0,1fr) auto;gap:2px 8px;align-items:start;padding:8px 0;border-bottom:1px solid #f1f3f4}',
    '.aq-row:last-child{border-bottom:0}',
    '.aq-dot{grid-column:1;grid-row:1;width:7px;height:7px;margin-top:5px;border-radius:50%;background:#9aa0a6}',
    '.aq-row[data-state=running] .aq-dot{background:#1a73e8}',
    '.aq-row[data-state=failed] .aq-dot{background:#b3261e}',
    '.aq-row[data-state=ambiguous] .aq-dot{background:#b88700}',
    '.aq-main{grid-column:2;grid-row:1;min-width:0}',
    '.aq-kind{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:500;line-height:1.4}',
    '.aq-meta{display:block;overflow-wrap:anywhere;color:#6b6f73;font-size:12px;line-height:1.4}',
    '.aq-meta[hidden]{display:none}',
    '.aq-row[data-error] .aq-meta{color:#b3261e}',
    '.aq-state{grid-column:3;grid-row:1;margin-top:1px;color:#6b6f73;font-size:12px;line-height:1.4;white-space:nowrap}',
    '.aq-row[data-state=running] .aq-state{color:#174ea6}',
    '.aq-row[data-state=failed] .aq-state,.aq-row[data-state=ambiguous] .aq-state{color:#b3261e}',
    '.aq-acts{grid-column:2 / 4;grid-row:2;display:flex;gap:6px;padding-top:4px}',
    '.aq-btn{padding:2px 9px;border:1px solid #c6c6c6;border-radius:5px;background:#fff;color:#202124;font-size:12px}',
    '.aq-btn:hover{background:#f5f5f5}',
    '.aq-btn.aq-primary{border-color:#202124;background:#202124;color:#fff}',
    '.aq-btn.aq-primary:hover{background:#3c4043}',
    '.aq-empty,.aq-more,.aq-notice{margin:9px 0 2px;color:#5f6368;font-size:12px;text-align:center}',
    '.aq-notice[data-error]{color:#b3261e}',
    '.aq-notice[hidden]{display:none}',
    '.aq-btn:focus-visible,.aq-close:focus-visible,.aq-clear:focus-visible{outline:2px solid #1557b0;outline-offset:1px}',
    '.aq-row[data-state=queued] .aq-dot{background:#b88700}',
    '.aq-row[data-state=waiting] .aq-dot{background:#9aa0a6}',
    '.aq-row[data-state=paused] .aq-dot{background:#b88700}',
    '.aq-row[data-state=paused] .aq-state{color:#8a6d00}',
    '.aq-row[data-state=done] .aq-dot{background:#188038}',
    '.aq-row[data-state=done] .aq-state{color:#188038}',
    '.aq-row[data-state=error] .aq-dot{background:#b3261e}',
    '.aq-row[data-state=error] .aq-state{color:#b3261e}',
    '.sync-banner{display:block;background:#e8f0fe;border-color:#c6dafc;color:#174ea6}',
    '.sync-banner[hidden]{display:none}',
    '.sync-banner[data-state=waiting]{background:#f1f3f4;border-color:#dadce0;color:#3c4043}',
    '.sync-banner[data-state=queued]{background:#fef7e0;border-color:#f2d489;color:#8a6d00}',
    '.sync-banner[data-state=paused]{background:#fef7e0;border-color:#f2d489;color:#8a6d00}',
    '.sync-banner[data-state=error]{background:#fce8e6;border-color:#f0b4ae;color:#b3261e}',
    '.aq-sync[data-state=running] .aq-dot,.sync-banner[data-state=running]::before{border:2px solid #c6dafc;border-top-color:#1a73e8;border-radius:50%;background:transparent;animation:metadata-sync-spin .8s linear infinite}',
    '.aq-sync[data-state=running] .aq-dot{width:9px;height:9px;box-sizing:border-box}',
    '.sync-banner[data-state=running]::before{content:"";display:inline-block;width:12px;height:12px;margin-right:8px;vertical-align:-2px}',
    '@keyframes metadata-sync-spin{to{transform:rotate(360deg)}}',
    '@media(prefers-reduced-motion:reduce){.aq-sync[data-state=running] .aq-dot,.sync-banner[data-state=running]::before{animation:none}}'
  ].join('');
  document.head.appendChild(style);

  var MAX_ROWS = 6;
  var RANK = { ambiguous: 0, failed: 1, running: 2, pending: 3 };
  var STATE_TEXT = { pending: 'Queued', running: 'Working…', failed: 'Failed', ambiguous: 'Check provider', done: 'Complete' };
  // Live metadata-sync rows share the popup with message actions but carry their
  // own state vocabulary (queued/running/waiting/error, plus "paused" when a
  // provider stops for storage capacity) and never render a retry control: a
  // sync is driven by the worker, not by the user. "waiting" is the idle/
   // retry-pending state, so it sorts last and is counted without a spinner;
  // "error" and "paused" need attention.
  var SYNC_RANK = { error: 0, paused: 0, running: 1, queued: 2, waiting: 3 };
  var SYNC_STATE_TEXT = { queued: 'Queued', running: 'Downloading', waiting: 'Waiting', paused: 'Paused', error: 'Sync error' };
  var csrf = '', items = [], rawItems = [], syncs = [], displaySyncs = [], drawerSyncs = [], connections = {}, cleared = {}, active = false;
  var bubble = document.createElement('dialog');
  bubble.id = 'action-queue';
  bubble.tabIndex = -1;
  bubble.setAttribute('aria-label', 'Mailbox action queue');
  var head = document.createElement('div');
  head.className = 'aq-head';
  var heading = document.createElement('span');
  heading.className = 'aq-title';
  heading.textContent = 'Queue';
  var summary = document.createElement('span');
  summary.className = 'aq-summary';
  var clearAll = document.createElement('button');
  clearAll.type = 'button';
  clearAll.className = 'aq-clear';
  clearAll.textContent = 'Clear all';
  clearAll.setAttribute('aria-label', 'Clear all flagged items from the queue');
  clearAll.hidden = true;
  var close = document.createElement('button');
  close.type = 'button';
  close.className = 'aq-close';
  close.setAttribute('aria-label', 'Close queue');
  close.textContent = '\u00d7';
  var list = document.createElement('div');
  list.className = 'aq-list';
  var notice = document.createElement('p');
  notice.className = 'aq-notice';
  notice.hidden = true;
  head.append(heading, summary, clearAll, close);
  bubble.append(head, list, notice);
  document.body.appendChild(bubble);

  function kindText(a) {
    var input = a.input || {};
    if (input.kind === 'patch') {
      if (input.read === true) { return 'Mark read'; }
      if (input.read === false) { return 'Mark unread'; }
      if (input.spam === true) { return 'Move to spam'; }
      if (input.spam === false) { return 'Move to inbox'; }
      if (input.labels) { return 'Update labels'; }
      return 'Update message';
    }
    if (input.kind === 'delete') { return 'Move to trash'; }
    if (input.kind === 'purge') { return 'Delete permanently'; }
    if (input.kind === 'empty_trash') { return 'Empty trash'; }
    if (input.kind === 'restore') { return 'Restore'; }
    if (input.kind === 'move') { return 'Move'; }
    return input.kind || 'Action';
  }

  function shortID(id) {
    if (!id) { return ''; }
    return id.length > 12 ? id.slice(0, 12) + '…' : id;
  }

  // shouldDisplaySync decides whether a metadata sync is shown in the inbox
  // banner, the queue rows and the visible activity counts. Initial indexing
  // is user-visible progress; routine refreshes of an already-complete index
  // are background-only and surface only through the header spinner. Failures
  // and pauses stay visible so they never disappear silently.
  function shouldDisplaySync(s) {
    if (!s) { return false; }
    if (s.phase !== 'refresh') { return true; }
    return s.state === 'error' || s.state === 'paused';
  }

  // counts reports the rows the drawer shows: a cleared row stops counting,
  // because the person has already taken it off the drawer.
  function counts() {
    var queued = 0, attention = 0, syncing = 0, waiting = 0;
    items.forEach(function (a) {
      if (a.state === 'pending' || a.state === 'running') { queued++; }
      if (a.state === 'failed' || a.state === 'ambiguous') { attention++; }
    });
    drawerSyncs.forEach(function (s) {
      if (s.state === 'running' || s.state === 'queued') { syncing++; }
      if (s.state === 'waiting') { waiting++; }
      if (s.state === 'error' || s.state === 'paused') { attention++; }
    });
    return { queued: queued, attention: attention, syncing: syncing, waiting: waiting };
  }

  // flaggedAction is an action only a human can settle: it failed, or the
  // provider left it ambiguous.
  function flaggedAction(a) { return a.state === 'failed' || a.state === 'ambiguous'; }

  // flaggedSync is a metadata sync that stopped for a reason only a human can
  // fix: an error, or a pause.
  function flaggedSync(s) { return s.state === 'error' || s.state === 'paused'; }

  // applyFilter decides which rows the drawer shows. A completed action clears
  // itself, a row the person cleared stays cleared, and a cleared metadata sync
  // leaves the drawer but keeps the banner on its own inbox page.
  function applyFilter() {
    items = rawItems.filter(function (a) { return a.state !== 'done' && !cleared['act:' + a.id]; });
    drawerSyncs = displaySyncs.filter(function (s) { return !cleared['sync:' + s.inbox_id]; });
  }

  function control(a, label, action, showError) {
    var button = document.createElement('button');
    button.type = 'button';
    button.className = 'aq-btn' + (action === 'retry' ? ' aq-primary' : '');
    button.textContent = label;
    button.onclick = function () {
      button.disabled = true;
      fetch('/ui/actions/' + encodeURIComponent(a.id), {method:'POST',credentials:'same-origin',body:new URLSearchParams({_csrf:csrf,action:action})})
        .then(function (r) { if (!r.ok) { throw new Error('Could not update the action'); } return r.json(); })
        .then(update).catch(function (e) { button.disabled = false; showError(e.message); });
    };
    return button;
  }

  function actionRow(a) {
    var row = document.createElement('div');
    row.className = 'aq-row';
    row.setAttribute('data-state', a.state);
    var dot = document.createElement('span');
    dot.className = 'aq-dot';
    dot.setAttribute('aria-hidden', 'true');
    var main = document.createElement('div');
    main.className = 'aq-main';
    var kind = document.createElement('span');
    kind.className = 'aq-kind';
    kind.textContent = kindText(a);
    var meta = document.createElement('span');
    meta.className = 'aq-meta';
    var detail = a.error || shortID(a.message_id);
    if (!a.error && a.empty_trash) {
      detail = a.empty_trash.phase === 'discovering'
        ? 'Finding messages… (' + fmtCount(a.empty_trash.total) + ' found)'
        : fmtCount(a.empty_trash.purged) + ' of ' + fmtCount(a.empty_trash.total) + ' purged';
    }
    // The queue is account-wide, so a row names the mailbox it came from. The
    // name rides the detail line rather than taking a line of its own.
    var parts = [];
    if (a.inbox_name) { parts.push(a.inbox_name); }
    if (detail) { parts.push(detail); }
    meta.textContent = parts.join(' · ');
    if (!meta.textContent) { meta.hidden = true; }
    main.append(kind, meta);
    var state = document.createElement('span');
    state.className = 'aq-state';
    state.textContent = STATE_TEXT[a.state] || a.state;
    row.append(dot, main, state);
    function showError(message) {
      row.setAttribute('data-error', '1');
      meta.hidden = false;
      meta.textContent = message;
    }
    var acts = document.createElement('div');
    acts.className = 'aq-acts';
    if (a.state === 'failed') { acts.appendChild(control(a, 'Retry', 'retry', showError)); }
    if (a.state === 'failed' || a.state === 'ambiguous') {
      acts.appendChild(control(a, 'Dismiss', 'dismiss', showError));
    }
    if (acts.childNodes.length) { row.appendChild(acts); }
    return row;
  }

  // fmtCount renders an integer with locale thousands separators, matching the
  // rest of the UI's counts.
  function fmtCount(n) {
    return Number(n || 0).toLocaleString();
  }

  // syncDenominator renders the total with an estimate marker when the provider
  // count is approximate (Gmail/One count every label; we index a folder
  // subset). "~240" reads as an estimate; a plain "240" is authoritative.
  function syncDenominator(s) {
    return (s.estimated_total ? '~' : '') + fmtCount(s.total);
  }

  // syncProgress renders the progress fragment: "N / Total (P%)" when the
  // provider reports a total, else "N indexed" (Gmail, One and IMAP have no
  // trustworthy denominator). A percentage is always clamped to 0-100 and the
  // server never sends a total below the indexed count, so it cannot render a
  // nonsense fraction like 110/100.
  function syncProgress(s) {
    var indexed = s.indexed || 0;
    if (s.total && s.total > 0 && indexed <= s.total) {
      var pct = Math.round(indexed / s.total * 100);
      if (pct < 0) { pct = 0; }
      if (pct > 100) { pct = 100; }
      return fmtCount(indexed) + ' / ' + syncDenominator(s) + ' (' + pct + '%)';
    }
    return fmtCount(indexed) + ' indexed';
  }

  function syncLabel(s) {
    return s.name || s.address || shortID(s.inbox_id) || 'Mailbox';
  }

  function syncMeta(s) {
    var parts = [];
    if (s.provider) { parts.push(s.provider); }
    var progress = syncProgress(s);
    if (s.state === 'running') {
      // Provider pacing is shown as live progress, never as a separate
      // "waiting" state. When only a count is known, drop the " indexed" suffix
      // so it reads "Downloading metadata 18" rather than "... 18 indexed".
      parts.push('Downloading metadata ' + progress.replace(/ indexed$/, ''));
    } else {
      parts.push(s.phase === 'initial' ? 'Initial metadata sync' : 'Metadata refresh');
      parts.push(progress);
    }
    if (s.error) { parts.push(s.error); }
    return parts.join(' · ');
  }

  // syncRow renders one inbox's live metadata sync. It deliberately has no
  // action controls: a sync cannot be retried or dismissed from here.
  function syncRow(s) {
    var row = document.createElement('div');
    row.className = 'aq-row aq-sync';
    row.setAttribute('data-state', s.state || 'waiting');
    row.setAttribute('data-sync-inbox', s.inbox_id || '');
    if (s.updated_at) { row.title = 'Status updated: ' + new Date(s.updated_at).toLocaleString(); }
    if (s.state === 'error') { row.setAttribute('data-error', '1'); }
    var dot = document.createElement('span');
    dot.className = 'aq-dot';
    dot.setAttribute('aria-hidden', 'true');
    var main = document.createElement('div');
    main.className = 'aq-main';
    var kind = document.createElement('span');
    kind.className = 'aq-kind';
    kind.textContent = syncLabel(s);
    var meta = document.createElement('span');
    meta.className = 'aq-meta';
    meta.textContent = syncMeta(s);
    if (!meta.textContent) { meta.hidden = true; }
    main.append(kind, meta);
    var state = document.createElement('span');
    state.className = 'aq-state';
    state.textContent = SYNC_STATE_TEXT[s.state] || s.state || 'Syncing';
    row.append(dot, main, state);
    return row;
  }

  // syncBannerText is the one-line banner summary for the inbox on screen.
  function syncBannerText(s) {
    var head = {
      running: 'Downloading metadata…',
      queued: 'Mailbox sync queued',
      waiting: 'Mailbox sync waiting',
      paused: 'Mailbox sync paused',
      error: 'Mailbox sync problem'
    }[s.state] || ('Mailbox sync ' + (SYNC_STATE_TEXT[s.state] || s.state || 'pending'));
    var bits = [head, 'Inbox-wide'];
    var progress = syncProgress(s);
    if (progress) { bits.push(progress); }
    if (s.error) { bits.push(s.error); }
    return bits.join(' · ');
  }

  // updateBanner mirrors the current inbox's initial sync into the top-of-view
  // banner the standalone mailbox and folder pages render. It rides the shared
  // queue poll so it tracks the same refresh as the popup. Routine refreshes of
  // an already-complete index stay background-only (header spinner); only
  // initial indexing and attention states are shown here.
  function updateBanner() {
    var banner = document.querySelector('[data-sync-banner]');
    if (!banner) { return; }
    var inbox = document.body.getAttribute('data-inbox') || '';
    var match = null;
    for (var i = 0; i < displaySyncs.length; i++) {
      if (displaySyncs[i].inbox_id === inbox) { match = displaySyncs[i]; break; }
    }
    // A rejected authorization has its own banner with the reconnect control.
    // When this page was rendered before the verdict was recorded, build that
    // banner here so the operator can repair it without reloading.
    var conn = connections[inbox];
    if (conn && conn.state === 'disconnected') { syncReconnectBanner(conn, inbox); }
    // Once that banner is on the page it carries the verdict and the control, so
    // the strip never repeats the same line.
    if (match && conn && conn.state === 'disconnected' && match.state === 'error' && match.error && match.error === conn.detail) { match = null; }
    if (!match) {
      banner.hidden = true;
      banner.textContent = '';
      banner.removeAttribute('data-state');
      return;
    }
    banner.hidden = false;
    banner.setAttribute('data-state', match.state || '');
    if (match.updated_at) { banner.title = 'Status updated: ' + new Date(match.updated_at).toLocaleString(); } else { banner.removeAttribute('title'); }
    banner.textContent = syncBannerText(match);
  }

  function render() {
    list.replaceChildren();
    var syncOrder = drawerSyncs.slice().sort(function (a, b) {
      return (SYNC_RANK[a.state] === undefined ? 4 : SYNC_RANK[a.state]) - (SYNC_RANK[b.state] === undefined ? 4 : SYNC_RANK[b.state]);
    });
    var actionOrder = items.slice().sort(function (a, b) {
      var ap = a.input && a.input.kind === 'empty_trash' ? 0 : 1;
      var bp = b.input && b.input.kind === 'empty_trash' ? 0 : 1;
      if (ap !== bp) { return ap - bp; }
      return (RANK[a.state] === undefined ? 3 : RANK[a.state]) - (RANK[b.state] === undefined ? 3 : RANK[b.state]);
    });
    var shownSync = syncOrder;
    var shownActions = actionOrder.slice(0, MAX_ROWS);
    if (!shownSync.length && !shownActions.length) {
      var empty = document.createElement('p');
      empty.className = 'aq-empty';
      empty.textContent = 'No actions waiting.';
      list.appendChild(empty);
      return;
    }
    shownSync.forEach(function (s) { list.appendChild(syncRow(s)); });
    shownActions.forEach(function (a) { list.appendChild(actionRow(a)); });
    var rest = (syncOrder.length - shownSync.length) + (actionOrder.length - shownActions.length);
    if (rest > 0) {
      var more = document.createElement('p');
      more.className = 'aq-more';
      more.textContent = 'and ' + rest + ' more';
      list.appendChild(more);
    }
  }

  // paint refreshes the header badge, the drawer's summary and the Clear all
  // control from the rows the drawer currently shows.
  function paint() {
    var n = counts();
    var activity = n.queued + n.syncing + n.waiting;
    badge.textContent = n.attention ? ' !' : activity ? ' (' + activity + ')' : '';
    badge.style.color = n.attention ? '#b3261e' : '';
    badge.style.fontWeight = 'bold';
    var label = n.attention ? (n.attention === 1 ? '1 item needs attention' : n.attention + ' items need attention') : activity ? activity + ' queued, syncing or waiting' : 'No queued actions or syncs';
    badge.setAttribute('aria-label', label);
    var summaryParts = [];
    if (n.queued) { summaryParts.push(n.queued + ' queued'); }
    if (n.syncing) { summaryParts.push(n.syncing + ' syncing'); }
    if (n.waiting) { summaryParts.push(n.waiting + ' waiting'); }
    if (n.attention) { summaryParts.push(n.attention + ' need attention'); }
    summary.textContent = summaryParts.join(' · ') || 'All clear';
    clearAll.hidden = !items.some(flaggedAction) && !drawerSyncs.some(flaggedSync);
  }

  function update(data) {
    csrf = data.csrf || csrf;
    // Successful actions stay in durable history briefly, but leave the status
    // popup as soon as the worker confirms completion.
    rawItems = data.items || [];
    syncs = data.syncs || [];
    displaySyncs = syncs.filter(shouldDisplaySync);
    connections = data.connections || {};
    applyFilter();
    // The header spinner reads every status, so routine refreshes still show
    // there while staying out of the banner, rows and counts.
    if (window.mammothmailStatus) { window.mammothmailStatus.busy('metadata', syncs.some(function (s) { return s.state === 'running'; })); }
    paint();
    notice.hidden = true;
    notice.removeAttribute('data-error');
    // The banner tracks the poll even while the popup is closed.
    updateBanner();
    if (bubble.open) { render(); }
  }

  function refresh() {
    if (active || document.hidden) { return; }
    active = true;
    fetch('/ui/actions', {credentials:'same-origin'}).then(function (r) { if (!r.ok) { throw new Error('Queue unavailable'); } return r.json(); })
      .then(update).catch(function () {
        if (window.mammothmailStatus) { window.mammothmailStatus.busy('metadata', false); }
        notice.hidden = false;
        notice.setAttribute('data-error', '1');
        notice.textContent = 'Queue unavailable. Try again.';
      }).finally(function () { active = false; });
  }

  function place() {
    var width = Math.min(304, Math.max(220, document.documentElement.clientWidth - 24));
    var pageX = window.pageXOffset || 0;
    var pageY = window.pageYOffset || 0;
    var rect = flag.getBoundingClientRect();
    var left = Math.max(pageX + 8, Math.min(rect.right + pageX - width, pageX + document.documentElement.clientWidth - width - 8));
    var caret = Math.max(10, Math.min(width - 20, Math.round(rect.left + rect.width / 2 - (left - pageX) - 5)));
    bubble.style.width = width + 'px';
    bubble.style.left = Math.round(left) + 'px';
    bubble.style.top = Math.round(rect.bottom + pageY + 7) + 'px';
    bubble.style.setProperty('--aq-caret', caret + 'px');
  }

  function show() {
    if (bubble.open) { return; }
    place();
    render();
    bubble.show();
    flag.setAttribute('aria-expanded', 'true');
    refresh();
  }

  function hide() {
    if (!bubble.open) { return; }
    bubble.close();
    flag.setAttribute('aria-expanded', 'false');
  }

  function toggle() {
    if (bubble.open) { hide(); flag.focus(); return; }
    show();
  }

  close.onclick = function () { hide(); flag.focus(); };
  // Clear all takes the flagged rows off the drawer. It hides them here only:
  // nothing is dismissed in the store, so the queue keeps running and the
  // worker keeps retrying a failed job behind the drawer.
  clearAll.onclick = function () {
    items.forEach(function (a) { if (flaggedAction(a)) { cleared['act:' + a.id] = true; } });
    drawerSyncs.forEach(function (s) { if (flaggedSync(s)) { cleared['sync:' + s.inbox_id] = true; } });
    applyFilter();
    paint();
    render();
    // The control hides itself with the rows it cleared, so hand the focus to
    // the drawer rather than losing it to the page.
    if (clearAll.hidden) { bubble.focus(); }
  };
  flag.addEventListener('click', toggle);
  flag.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
  });
  document.addEventListener('keydown', function (e) {
    if (bubble.open && e.key === 'Escape') { e.preventDefault(); hide(); flag.focus(); }
  });
  document.addEventListener('click', function (e) {
    if (!bubble.open || bubble.contains(e.target) || flag.contains(e.target)) { return; }
    hide();
  });
  window.addEventListener('resize', function () { if (bubble.open) { place(); } });
  window.mammothmailRefreshActions = refresh;
  // Test-only seam: the harness needs to drive one poll with a fixed payload.
  window.mammothmailActionUpdateForTest = update;
  window.setInterval(refresh, 10000);
  document.addEventListener('visibilitychange', refresh);
  refresh();
})();

(function () {
  var dlg = document.getElementById('key-dialog');
  if (!dlg) {
    return;
  }
  var dialogStyle = document.createElement('style');
  dialogStyle.textContent = '#key-dialog[open]{display:flex;flex-direction:column;max-height:calc(100dvh - 32px);overflow:hidden}#key-dialog h2{flex:0 0 auto}#key-form:not([hidden]){display:flex;flex:1 1 auto;flex-direction:column;min-height:0;overflow:hidden}#key-form[hidden]{display:none}.key-form-scroll{flex:1 1 auto;min-height:0;overflow-y:auto;overscroll-behavior:contain}.key-fields{display:block;min-width:0;overflow:visible}.connector-config-row{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.connector-config-row>div{min-width:0}.connector-config-row label{display:block}.connector-config-row select{margin-bottom:6px}@media(max-width:520px){.connector-config-row{grid-template-columns:1fr}}#key-dialog #key-error{flex:0 0 auto}#key-dialog #key-form>.dialog-actions{position:static;flex:0 0 auto;padding:12px 0;margin-top:4px;background:#fff;border-top:1px solid #eee}';
  document.head.appendChild(dialogStyle);
  var sel = document.getElementById('key-type');
  var form = document.getElementById('key-form');
  var formScroll = document.createElement('div');
  formScroll.className = 'key-form-scroll';
  var formError = document.getElementById('key-error');
  while (form.firstChild && form.firstChild !== formError) {
    formScroll.appendChild(form.firstChild);
  }
  form.insertBefore(formScroll, formError);
  var title = document.getElementById('key-dialog-title');
  var createSource = 'client';
  var fixedInboxID = '';
  var returnInboxID = '';
  var idInput = form.querySelector('[name=id]');
  var nameInput = form.querySelector('[name=name]');
  var adminInput = form.querySelector('[name=admin]');
  var result = document.getElementById('key-result');
  var resultTitle = document.getElementById('key-result-title');
  var resultLabel = document.getElementById('key-result-label');
  var resultSecret = document.getElementById('key-result-secret');
  var copyBtn = document.getElementById('key-copy');
  var errorBox = document.getElementById('key-error');
  var doneBtn = document.getElementById('key-done');
  var matrix = document.getElementById('key-matrix');
  var submitBtn = document.getElementById('key-submit');
  var rotateBtn = document.getElementById('key-rotate');
  var hermesInbox = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
  var hermesWarning = document.getElementById('key-hermes-warning');
  var hermesAckRow = document.getElementById('key-hermes-ack-row');
  var hermesAck = document.getElementById('key-hermes-ack');
  var openclawInbox = form.querySelector('.key-fields[data-type=openclaw] select[name=inbox]');
  var openclawWarning = document.getElementById('key-openclaw-warning');
  var openclawAckRow = document.getElementById('key-openclaw-ack-row');
  var openclawAck = document.getElementById('key-openclaw-ack');
  var openclawAlpha = document.getElementById('key-openclaw-alpha');
  var adminSnapshot = null;
  // adminSendSnapshot holds the checked domain-wide sending boxes while the Admin
  // checkbox is ticked, so unticking restores the operator's staged state.
  var adminSendSnapshot = null;
  var currentKind = 'api';
  var webhookFields = form.querySelector('.key-fields[data-type=webhook]');
  var webhookAuth = webhookFields.querySelector('select[name=auth]');
  var bearerFields = document.createElement('div');
  bearerFields.hidden = true;
  bearerFields.innerHTML = '<label for="webhook-bearer-secret">Bearer secret</label><div class="row"><input id="webhook-bearer-secret" name="bearer_secret" type="password" autocomplete="new-password" placeholder="Paste your secret here, or click Generate for a random one" aria-describedby="webhook-bearer-hint"><button type="button" class="secondary btn-narrow" id="webhook-generate">Generate</button></div><p class="muted small" id="webhook-bearer-hint"></p>';
  webhookFields.insertBefore(bearerFields, webhookAuth.nextElementSibling);
  var payloadSelect = webhookFields.querySelector('select[name=mode]');
  var configRow = document.createElement('div');
  configRow.className = 'connector-config-row';
  [payloadSelect, webhookAuth].forEach(function (select) {
    var field = document.createElement('div');
    field.appendChild(select.previousElementSibling);
    field.appendChild(select);
    configRow.appendChild(field);
  });
  webhookFields.insertBefore(configRow, bearerFields);
  form.querySelectorAll('.connector-auto-actions').forEach(function (section) {
    section.remove();
  });
  var bearerInput = bearerFields.querySelector('input');
  var generateBtn = bearerFields.querySelector('button');

  // Relay connectors share the same outbound relay transport and the same
  // no-allow-list risk gate: Hermes and OpenClaw.
  function isRelayKind(kind) {
    return kind === 'hermes' || kind === 'openclaw';
  }

  function syncBearer() {
    var active = sel.value === 'webhook' && webhookAuth.value === 'bearer';
    var editing = form.getAttribute('action') !== '/ui/keys';
    bearerFields.hidden = !active;
    bearerInput.disabled = !active;
    bearerInput.required = active && !editing;
    document.getElementById('webhook-bearer-hint').textContent = editing
      ? 'Leave blank to keep the current secret. Paste only the token, without Bearer, or Generate a replacement. It changes when you save.'
      : 'Paste only the token, without Bearer, or click Generate. Copy it before saving if another app needs it.';
    if (rotateBtn) {
      var canRotate = editing && !!idInput.value && (sel.value === 'api' || (sel.value === 'webhook' && !active));
      rotateBtn.hidden = !canRotate;
      rotateBtn.style.display = canRotate ? '' : 'none';
      rotateBtn.textContent = sel.value === 'webhook' ? 'Rotate signing secret' : 'Rotate Key';
    }
  }

  webhookAuth.addEventListener('change', syncBearer);
  generateBtn.addEventListener('click', function () {
    errorBox.hidden = true;
    try {
      bearerInput.value = generateBearerSecret();
      bearerInput.type = 'text';
      bearerInput.focus();
    } catch (err) {
      errorBox.textContent = 'Could not generate a secret. Paste a secret from your other app.';
      errorBox.hidden = false;
    }
  });

  function sync() {
    document.querySelectorAll('.key-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-type') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
    dlg.classList.add('key-dialog--wide');
    var connectorCreate = form.getAttribute('action') === '/ui/keys' && sel.value !== 'api';
    if (title) {
      title.textContent = connectorCreate ? 'Add connector' : (form.getAttribute('action') === '/ui/keys' ? 'Add client' : (sel.value === 'api' ? 'Edit client' : 'Edit connector'));
    }
    if (submitBtn && form.getAttribute('action') === '/ui/keys') {
      submitBtn.textContent = connectorCreate ? 'Add Connector' : 'Add Client';
    }
    form.querySelectorAll('.connector-inbox-select').forEach(function (inboxSel) {
      var label = inboxSel.previousElementSibling;
      var hide = connectorCreate && createSource === 'connector' && !!fixedInboxID;
      inboxSel.hidden = hide;
      if (label && label.classList.contains('connector-inbox-label')) {
        label.hidden = hide;
      }
      if (fixedInboxID && connectorCreate) {
        inboxSel.value = fixedInboxID;
      }
    });
    syncHermesRisk();
    syncBearer();
    // The OpenClaw connector's early-alpha note sits between the Type and Name
    // fields, so it follows the selected type like the per-type field sets.
    if (openclawAlpha) {
      openclawAlpha.hidden = sel.value !== 'openclaw';
    }
  }

  function rolesRadios() {
    return matrix ? matrix.querySelectorAll('input[type=radio][name^=role_]') : [];
  }

  function setAllRoles(value) {
    rolesRadios().forEach(function (el) {
      el.checked = el.value === value;
    });
  }

  // scopeGroup resolves a seg's data-set-scope to the tbody it controls. A
  // domain scope is the domain id; the standalone inboxes share one tbody that
  // no domain id matches, so that scope is keyed by the literal "standalone".
  function scopeGroup(scope) {
    if (!matrix) {
      return null;
    }
    if (scope === 'standalone') {
      return matrix.querySelector('tbody[data-standalone]');
    }
    return matrix.querySelector('tbody[data-domain="' + scope + '"]');
  }

  function setScopeRoles(scope, value) {
    var group = scopeGroup(scope);
    if (!group) {
      return;
    }
    group.querySelectorAll('input[type=radio][name^=role_]').forEach(function (el) {
      el.checked = el.value === value;
    });
  }

  function scopeRadios(scope) {
    if (scope === 'all') {
      return rolesRadios();
    }
    var group = scopeGroup(scope);
    return group ? group.querySelectorAll('input[type=radio][name^=role_]') : [];
  }

  // The de-facto role is the single checked value shared by every inbox in the
  // scope, or undefined when the scope is mixed or has no inboxes.
  function uniformRole(radios) {
    var value;
    var seen = false;
    for (var i = 0; i < radios.length; i++) {
      if (!radios[i].checked) {
        continue;
      }
      if (!seen) {
        value = radios[i].value;
        seen = true;
      } else if (value !== radios[i].value) {
        return undefined;
      }
    }
    return seen ? value : undefined;
  }

  function syncScopeButtons() {
    if (!matrix) {
      return;
    }
    document.querySelectorAll('#key-matrix .seg[data-set-scope]').forEach(function (seg) {
      var role = uniformRole(scopeRadios(seg.getAttribute('data-set-scope')));
      seg.querySelectorAll('button[data-set-role]').forEach(function (btn) {
        var on = role !== undefined && btn.getAttribute('data-set-role') === role;
        btn.classList.toggle('active', on);
        btn.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    });
  }

  // matrixDomainSend holds the matrix's staged domain-wide sending state per
  // inbox id. It is written to the hidden field on every change so the value is
  // always submitted (an unticked checkbox would submit nothing, making "clear"
  // indistinguishable from "untouched").
  function matrixDomainSendField(inboxID) {
    return matrix ? matrix.querySelector('input[type=hidden][name="domainsend_' + inboxID + '"]') : null;
  }

  function matrixDomainSendButton(inboxID) {
    return matrix ? matrix.querySelector('button.key-matrix-domainsend-btn[data-inbox="' + inboxID + '"]') : null;
  }

  // setMatrixDomainSend records the state for one inbox: writes the hidden field
  // (1 or 0), lights the button, pins the ladder to Owner when on, and disables
  // the sub-owner rungs so the pair cannot be broken.
  function setMatrixDomainSend(inboxID, on) {
    var field = matrixDomainSendField(inboxID);
    if (field) {
      field.value = on ? '1' : '0';
    }
    var btn = matrixDomainSendButton(inboxID);
    if (btn) {
      // Toggle the class rather than replacing className: the button carries
      // key-matrix-domainsend-btn, which every lookup here relies on.
      btn.classList.toggle('active', on);
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    }
    var ownerRadio = matrix.querySelector('input[type=radio][name="role_' + inboxID + '"][value=owner]');
    if (on && ownerRadio) {
      ownerRadio.checked = true;
    }
    matrix.querySelectorAll('input[type=radio][name="role_' + inboxID + '"]').forEach(function (r) {
      r.disabled = on && r.value !== 'owner';
    });
  }

  // syncMatrixDomainSend re-derives every row's state from the radios and the
  // staged grant, after a bulk change. changed, when given, is the element the
  // operator just acted on.
  function syncMatrixDomainSend(changed) {
    if (!matrix) {
      return;
    }
    matrix.querySelectorAll('button.key-matrix-domainsend-btn').forEach(function (btn) {
      var inboxID = btn.getAttribute('data-inbox');
      var on = btn.classList.contains('active');
      if (changed && changed.type === 'radio' && changed.name === 'role_' + inboxID && changed.value !== 'owner') {
        // Choosing a role below Owner drops the grant.
        on = false;
      }
      setMatrixDomainSend(inboxID, on);
    });
    syncScopeButtons();
  }

  if (matrix) {
    // The Domain send control is a button plus a hidden field, so clicking it
    // always submits an explicit 1 or 0.
    matrix.addEventListener('click', function (e) {
      var btn = e.target && e.target.closest ? e.target.closest('button.key-matrix-domainsend-btn') : null;
      if (!btn) {
        return;
      }
      e.preventDefault();
      var inboxID = btn.getAttribute('data-inbox');
      setMatrixDomainSend(inboxID, !btn.classList.contains('active'));
      syncScopeButtons();
    });
    matrix.addEventListener('change', function (e) {
      var t = e.target;
      if (t && t.type === 'radio' && t.name && t.name.indexOf('role_') === 0) {
        syncMatrixDomainSend(t);
      }
    });
  }

  function syncAdmin() {
    if (!matrix) {
      return;
    }
    var on = !!(adminInput && adminInput.checked);
    if (on) {
      if (adminSnapshot === null) {
        adminSnapshot = [];
        rolesRadios().forEach(function (el) {
          if (el.checked) {
            adminSnapshot.push({ name: el.name, value: el.value });
          }
        });
        adminSendSnapshot = [];
        matrix.querySelectorAll('button.key-matrix-domainsend-btn').forEach(function (btn) {
          var inboxID = btn.getAttribute('data-inbox');
          if (btn.classList.contains('active')) {
            adminSendSnapshot.push(inboxID);
          }
          // Admin keys get domain-wide sending implicitly, so the per-inbox
          // control is cleared while Admin is ticked and restored on untick.
          setMatrixDomainSend(inboxID, false);
        });
      }
      setAllRoles('owner');
    } else if (adminSnapshot !== null) {
      rolesRadios().forEach(function (el) {
        el.checked = false;
      });
      adminSnapshot.forEach(function (s) {
        var el = form.querySelector('input[type=radio][name="' + s.name + '"][value="' + s.value + '"]');
        if (el) {
          el.checked = true;
        }
      });
      adminSnapshot = null;
      (adminSendSnapshot || []).forEach(function (inboxID) {
        setMatrixDomainSend(inboxID, true);
      });
      adminSendSnapshot = null;
    }
    matrix.disabled = on;
    syncScopeButtons();
  }

  // Gate creating a relay connection (Hermes or OpenClaw) for an inbox with no
  // allowed senders: show the red warning and keep Create disabled until the
  // operator acknowledges that the agent will reply to anyone. Skipped when
  // editing an existing connection (the inbox is fixed and already accepted).
  function syncHermesRisk() {
    var editing = form.getAttribute('action') !== '/ui/keys';
    if (sel.value === 'hermes' || sel.value === 'openclaw') {
      var openclaw = sel.value === 'openclaw';
      var inboxSel = openclaw ? openclawInbox : hermesInbox;
      var warning = openclaw ? openclawWarning : hermesWarning;
      var ackRow = openclaw ? openclawAckRow : hermesAckRow;
      var ack = openclaw ? openclawAck : hermesAck;
      var opt = inboxSel ? inboxSel.options[inboxSel.selectedIndex] : null;
      var warn = !editing && (!opt || opt.getAttribute('data-allowlist') !== '1');
      if (warning) {
        warning.hidden = !warn;
      }
      // The row keeps its inline display:flex for layout, so the hidden attribute
      // is the only thing that can hide it: the page CSS is
      // [hidden]{display:none!important}, which beats the inline style.
      if (ackRow) {
        ackRow.hidden = !warn;
      }
      if (!warn) {
        if (ack) {
          ack.checked = false;
        }
        if (submitBtn) {
          submitBtn.disabled = false;
        }
        return;
      }
      if (ack && submitBtn) {
        submitBtn.disabled = !ack.checked;
      }
    } else if (hermesWarning && hermesAckRow && hermesAck && submitBtn) {
      hermesWarning.hidden = true;
      hermesAckRow.hidden = true;
      if (openclawWarning && openclawAckRow && openclawAck) {
        openclawWarning.hidden = true;
        openclawAckRow.hidden = true;
      }
      submitBtn.disabled = false;
    }
  }

  function showForm() {
    form.hidden = false;
    result.hidden = true;
    errorBox.hidden = true;
    errorBox.textContent = '';
    resultSecret.textContent = '';
    document.getElementById('key-hermes-secret').textContent = '';
    if (copyBtn) {
      copyBtn.textContent = 'Copy';
    }
  }

  function showResult(data) {
    dlg.classList.add('key-dialog--wide');
    form.hidden = true;
    errorBox.hidden = true;
    resultTitle.textContent = data.notice || 'Key created';
    resultLabel.textContent = data.label || '';
    resultSecret.textContent = data.secret || '';
    document.getElementById('key-openclaw-guide').hidden = sel.value !== 'openclaw';
    document.getElementById('key-hermes-guide').hidden = sel.value !== 'hermes';
    document.getElementById('key-hermes-secret').textContent = sel.value === 'hermes' ? (data.secret || '') : '';
    resultSecret.parentElement.hidden = sel.value === 'hermes';
    resultLabel.hidden = sel.value === 'hermes';
    if (copyBtn) {
      copyBtn.textContent = 'Copy';
    }
    result.hidden = false;
  }

  function setAPIOptionVisible(visible) {
    var opt = sel.querySelector('option[value=api]');
    if (opt) {
      opt.hidden = !visible;
      opt.disabled = !visible;
    }
    var group = sel.querySelector('optgroup[data-group=clients]');
    if (group) {
      group.hidden = !visible;
      group.disabled = !visible;
    }
  }

  function openCreate() {
    form.reset();
    bearerInput.type = 'password';
    form.action = '/ui/keys';
    idInput.value = '';
    adminSnapshot = null;
    adminSendSnapshot = null;
    currentKind = 'api';
    createSource = 'client';
    fixedInboxID = '';
    returnInboxID = '';
    setAPIOptionVisible(true);
    sel.value = 'api';
    sel.disabled = false;
    if (submitBtn) {
      submitBtn.textContent = 'Add Client';
    }
    if (rotateBtn) {
      rotateBtn.hidden = true;
      rotateBtn.textContent = 'Rotate Key';
    }
    showForm();
    sync();
    syncAdmin();
    dlg.showModal();
  }

  function openCreateConnector(inboxID) {
    form.reset();
    bearerInput.type = 'password';
    form.action = '/ui/keys';
    idInput.value = '';
    adminSnapshot = null;
    adminSendSnapshot = null;
    currentKind = 'hermes';
    createSource = 'connector';
    fixedInboxID = inboxID || '';
    returnInboxID = fixedInboxID;
    setAPIOptionVisible(false);
    sel.value = 'hermes';
    sel.disabled = false;
    if (rotateBtn) {
      rotateBtn.hidden = true;
      rotateBtn.textContent = 'Rotate Key';
    }
    showForm();
    sync();
    syncAdmin();
    dlg.showModal();
  }

  function openEdit(btn) {
    form.reset();
    bearerInput.type = 'password';
    adminSnapshot = null;
    adminSendSnapshot = null;
    createSource = 'edit';
    fixedInboxID = '';
    returnInboxID = '';
    setAPIOptionVisible(true);
    var kind = isRelayKind(btn.dataset.kind) || btn.dataset.kind === 'webhook' ? btn.dataset.kind : 'api';
    currentKind = kind;
    var segment = isRelayKind(kind) ? kind : (kind === 'webhook' ? 'webhooks' : 'keys');
    form.action = '/ui/' + segment + '/' + btn.dataset.id + '/edit';
    idInput.value = btn.dataset.id;
    sel.value = kind;
    sel.disabled = true;
    if (submitBtn) {
      submitBtn.textContent = 'Save';
    }
    if (rotateBtn) {
      rotateBtn.hidden = isRelayKind(kind);
      rotateBtn.textContent = 'Rotate Key';
    }
    nameInput.value = btn.dataset.name || '';
    if (adminInput) {
      adminInput.checked = btn.dataset.admin === '1';
    }
    if (kind === 'api') {
      var roles = {};
      try {
        roles = JSON.parse(btn.dataset.roles || '{}');
      } catch (e) {
        roles = {};
      }
      rolesRadios().forEach(function (el) {
        el.checked = el.value === (roles[el.name.slice(5)] || '');
      });
      // Domain-wide sending. Only live grants are reported, so an inbox that was
      // never granted stays off.
      var sends = [];
      try {
        sends = JSON.parse(btn.dataset.domainsend || '[]') || [];
      } catch (e) {
        sends = [];
      }
      matrix.querySelectorAll('button.key-matrix-domainsend-btn').forEach(function (b) {
        setMatrixDomainSend(b.getAttribute('data-inbox'), sends.indexOf(b.getAttribute('data-inbox')) !== -1);
      });
    } else if (kind === 'webhook') {
      var wf = form.querySelector('.key-fields[data-type=webhook]');
      var wInbox = wf.querySelector('select[name=inbox]');
      if (wInbox && btn.dataset.inbox) {
        wInbox.value = btn.dataset.inbox;
      }
      wf.querySelector('input[name=url]').value = btn.dataset.url || '';
      wf.querySelector('select[name=mode]').value = btn.dataset.mode || 'notify';
      wf.querySelector('select[name=auth]').value = btn.dataset.auth || 'signature';
    } else {
      var inbox = form.querySelector('.key-fields[data-type=' + kind + '] select[name=inbox]');
      if (inbox && btn.dataset.inbox) {
        inbox.value = btn.dataset.inbox;
      }
      var roleSel = form.querySelector('.key-fields[data-type=' + kind + '] select[name=role]');
      if (roleSel) {
        roleSel.value = btn.dataset.role || 'owner';
      }
    }
    sync();
    syncAdmin();
    if (isRelayKind(kind) || kind === 'webhook') {
      var inbox2 = form.querySelector('.key-fields[data-type=' + kind + '] select[name=inbox]');
      if (inbox2) {
        inbox2.disabled = true;
      }
      var setupSel = form.querySelector('.key-fields[data-type=' + kind + '] select[name=setup]');
      if (setupSel) {
        setupSel.disabled = true;
      }
    }
    showForm();
    dlg.showModal();
  }

  form.addEventListener('submit', function (e) {
    if (form.getAttribute('action') !== '/ui/keys' || typeof window.fetch !== 'function') {
      return;
    }
    e.preventDefault();
    errorBox.hidden = true;
    errorBox.textContent = '';
    var body = new URLSearchParams();
    new FormData(form).forEach(function (value, key) {
      body.append(key, value);
    });
    fetch(form.action, {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) {
        return res.text().then(function (text) {
          throw new Error(text || 'Could not create client');
        });
      }
      return res.json();
    }).then(function (data) {
      if (sel.value !== 'api') {
        var activeInbox = form.querySelector('.key-fields[data-type=' + sel.value + '] select[name=inbox]');
        returnInboxID = activeInbox ? activeInbox.value : returnInboxID;
      }
      if (sel.value === 'webhook' && webhookAuth.value === 'bearer') {
        dlg.close();
        if (returnInboxID) {
          window.location.href = '/?inbox=' + encodeURIComponent(returnInboxID) + '&inbox_tab=connectors';
        } else {
          window.location.reload();
        }
        return;
      }
      showResult(data);
    }).catch(function (err) {
      errorBox.textContent = err.message || 'Could not create client';
      errorBox.hidden = false;
    });
  });

  if (copyBtn) {
    window.mammothmailCopy.wire(copyBtn, {
      text: function () { return resultSecret.textContent; },
      select: resultSecret
    });
  }

  // accessResultDone, when set, overrides the Done button's navigation so an
  // embedded flow (the Clients & Access tab) can return to its own view.
  var accessResultDone = null;

  if (doneBtn) {
    doneBtn.addEventListener('click', function () {
      var cb = accessResultDone;
      accessResultDone = null;
      dlg.close();
      if (cb) {
        cb();
        return;
      }
      if (returnInboxID) {
        window.location.href = '/?inbox=' + encodeURIComponent(returnInboxID) + '&inbox_tab=connectors';
      } else {
        window.location.reload();
      }
    });
  }

  // mammothmailShowKeyResult renders a one-time secret in the shared key dialog,
  // matching the Add/Edit client flow. onDone, when supplied, runs when the
  // user dismisses the result (used by the Clients & Access tab).
  window.mammothmailShowKeyResult = function (data, onDone) {
    if (!data || typeof data !== 'object') {
      return;
    }
    accessResultDone = typeof onDone === 'function' ? onDone : null;
    showResult(data);
    if (!dlg.open) {
      dlg.showModal();
    }
  };

  if (rotateBtn) {
    rotateBtn.addEventListener('click', function () {
      var id = idInput.value;
      if (!id) {
        return;
      }
      var webhook = currentKind === 'webhook';
      if (!window.confirm(webhook ? 'Rotate this webhook signing secret? The current secret stops working immediately.' : 'Rotate this API key? The current key stops working immediately.')) {
        return;
      }
      var csrfInput = form.querySelector('[name=_csrf]');
      var body = new URLSearchParams();
      if (csrfInput) {
        body.append('_csrf', csrfInput.value);
      }
      rotateBtn.disabled = true;
      var rotatePath = webhook ? '/ui/webhooks/' + encodeURIComponent(id) + '/rotate' : '/ui/keys/' + encodeURIComponent(id) + '/rotate';
      fetch(rotatePath, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
        },
        body: body.toString(),
        credentials: 'same-origin'
      }).then(function (res) {
        if (!res.ok) {
          return res.text().then(function (text) {
            throw new Error(text || 'Could not rotate key');
          });
        }
        return res.json();
      }).then(function (data) {
        rotateBtn.disabled = false;
        showResult(data);
      }).catch(function (err) {
        rotateBtn.disabled = false;
        errorBox.textContent = err.message || 'Could not rotate key';
        errorBox.hidden = false;
      });
    });
  }

  sel.addEventListener('change', function () {
    sync();
    syncAdmin();
  });
  if (adminInput) {
    adminInput.addEventListener('change', syncAdmin);
  }
  if (hermesInbox) {
    hermesInbox.addEventListener('change', syncHermesRisk);
  }
  if (hermesAck) {
    hermesAck.addEventListener('change', syncHermesRisk);
  }
  if (openclawInbox) {
    openclawInbox.addEventListener('change', syncHermesRisk);
  }
  if (openclawAck) {
    openclawAck.addEventListener('change', syncHermesRisk);
  }
  document.querySelectorAll('#key-matrix [data-set-role]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var scope = btn.getAttribute('data-domain');
      var role = btn.getAttribute('data-set-role');
      if (scope) {
        setScopeRoles(scope, role);
      } else {
        setAllRoles(role);
      }
      // A bulk role below Owner drops the matching domain-wide sending grants, so
      // the "grant and owner are one state" rule holds through the bulk controls.
      if (role !== 'owner') {
        var group = scope ? scopeGroup(scope) : matrix;
        if (group) {
          group.querySelectorAll('button.key-matrix-domainsend-btn').forEach(function (b) {
            setMatrixDomainSend(b.getAttribute('data-inbox'), false);
          });
        }
      }
      syncMatrixDomainSend();
      syncScopeButtons();
    });
  });
  if (matrix) {
    matrix.addEventListener('change', syncScopeButtons);
  }
  var add = document.getElementById('add-key');
  if (add) {
    add.addEventListener('click', openCreate);
  }
  document.querySelectorAll('.add-connector').forEach(function (btn) {
    btn.addEventListener('click', function () {
      openCreateConnector(btn.getAttribute('data-inbox') || '');
    });
  });
  var cancel = document.getElementById('key-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
  document.querySelectorAll('.edit-credential').forEach(function (btn) {
    btn.addEventListener('click', function () {
      openEdit(btn);
    });
  });
  // A dismissed result dialog drops any pending embedded-flow callback so it
  // cannot leak into a later normal Add/Edit client flow.
  dlg.addEventListener('close', function () {
    accessResultDone = null;
  });
  syncScopeButtons();
})();

(function () {
  document.querySelectorAll('form[data-confirm]').forEach(function (form) {
    form.addEventListener('submit', function (e) {
      if (!window.confirm(form.getAttribute('data-confirm'))) {
        e.preventDefault();
      }
    });
  });
})();
(function () {
  'use strict';

  var inboxCard = document.querySelector('.dashboard-inboxes-card[data-group-scope]');
  if (!inboxCard) {
    return;
  }

  var clientCard = inboxCard.nextElementSibling;
  var domainCard = clientCard && clientCard.nextElementSibling;
  var csrfToken = inboxCard.getAttribute('data-dashboard-csrf') || '';
  var preferencesReady = false;
  var pendingSave = null;
  var saving = false;
  var style = document.createElement('style');
  style.textContent = [
    '.dashboard-group-tools{display:flex;align-items:center;gap:8px;flex-wrap:wrap}',
    '.dashboard-groups-toggle{display:inline-flex;align-items:center;gap:7px}',
    '.dashboard-groups-box{width:15px;height:15px;flex:0 0 auto;box-sizing:border-box;border:1px solid #999;border-radius:3px;background:#fff;display:inline-flex;align-items:center;justify-content:center}',
    '.dashboard-groups-toggle[aria-pressed="true"] .dashboard-groups-box{background:#111;border-color:#111}',
    '.dashboard-groups-toggle[aria-pressed="true"] .dashboard-groups-box:after{content:"";width:3px;height:7px;border:solid #fff;border-width:0 2px 2px 0;transform:rotate(45deg);margin-top:-2px}',
    '.dashboard-row-group{display:inline-flex;align-items:center;gap:5px;max-width:160px;height:26px;padding:0 8px;border-radius:6px;font-size:12px;line-height:1;vertical-align:middle}',
    '.dashboard-row-group.empty{color:#80868b;border-style:dashed}',
    '.dashboard-row-group svg{width:13px;height:13px;flex:0 0 auto}',
    '.dashboard-row-group-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.dashboard-group-head td{background:#f7f8fa;padding:4px 8px;border-bottom:1px solid #eee}',
    '.dashboard-group-toggle{display:flex;align-items:center;gap:8px;width:100%;padding:3px 2px;border:0;border-radius:0;background:transparent;color:#202124;text-align:left;font:inherit;cursor:pointer}',
    '.dashboard-group-toggle:hover,.dashboard-group-toggle:focus-visible{border-color:transparent;background:transparent;color:#202124}',
    '.dashboard-group-toggle:focus-visible,.dashboard-sortable:focus-visible{outline:2px solid #1557b0;outline-offset:-2px}',
    '.dashboard-group-chevron{width:0;height:0;flex:0 0 auto;border-top:5px solid transparent;border-bottom:5px solid transparent;border-left:6px solid #5f6368;transition:transform .15s}',
    '.dashboard-group-toggle[aria-expanded="true"] .dashboard-group-chevron{transform:rotate(90deg)}',
    '.dashboard-group-name{font-weight:700}',
    '.dashboard-group-count{font-weight:400}',
    '.dashboard-sortable{cursor:pointer;user-select:none;white-space:nowrap}',
    '.dashboard-sortable:hover{background:#f6f9ff}',
    '.dashboard-sortable:after{content:"";display:inline-block;width:0;height:0;margin-left:6px;border-left:4px solid transparent;border-right:4px solid transparent;vertical-align:middle;opacity:0}',
    '.dashboard-sortable[aria-sort="ascending"]:after{border-bottom:5px solid #5f6368;opacity:1}',
    '.dashboard-sortable[aria-sort="descending"]:after{border-top:5px solid #5f6368;opacity:1}',
    '#dashboard-group-dialog{width:min(420px,92vw)}',
    '#dashboard-group-dialog h2{margin-top:0}'
  ].join('');
  document.head.appendChild(style);

  var tables = {
    inboxes: {
      name: 'inboxes',
      card: inboxCard,
      tbody: inboxCard.querySelector('tbody[data-inbox-rows]'),
      rowSelector: 'tr[data-inbox-id]',
      columns: { 0: 'name', 1: 'address', 3: 'unread', 4: 'pending', 6: 'size' },
      numeric: { unread: true, pending: true, size: true },
      grouped: true
    },
    clients: {
      name: 'clients',
      card: clientCard,
      tbody: clientCard && clientCard.querySelector('table.dense tbody'),
      columns: { 0: 'name', 1: 'type' },
      numeric: {},
      grouped: true
    },
    domains: {
      name: 'domains',
      card: domainCard,
      tbody: domainCard && domainCard.querySelector('tbody[data-domain-rows]'),
      rowSelector: 'tr[data-domain-id]',
      columns: { 0: 'domain', 1: 'catchall', 2: 'sending', 3: 'receiving' },
      numeric: {},
      grouped: false
    }
  };

  function isObject(value) {
    return !!value && typeof value === 'object' && !Array.isArray(value);
  }

  function initialState() {
    return {
      groups: { inboxes: Object.create(null), clients: Object.create(null) },
      collapsed: { inboxes: Object.create(null), clients: Object.create(null) },
      showGroups: { inboxes: false, clients: false },
      showConnectors: { clients: true },
      sort: Object.create(null)
    };
  }

  function loadState(saved) {
    var next = initialState();
    try {
      if (!isObject(saved)) {
        return next;
      }
      ['inboxes', 'clients'].forEach(function (table) {
        if (isObject(saved.groups) && isObject(saved.groups[table])) {
          Object.keys(saved.groups[table]).forEach(function (id) {
            if (typeof saved.groups[table][id] === 'string') {
              next.groups[table][id] = cleanGroup(saved.groups[table][id]);
            }
          });
        }
        if (isObject(saved.collapsed) && isObject(saved.collapsed[table])) {
          Object.keys(saved.collapsed[table]).forEach(function (name) {
            next.collapsed[table][name] = saved.collapsed[table][name] === true;
          });
        }
        if (isObject(saved.showGroups)) {
          next.showGroups[table] = saved.showGroups[table] === true;
        }
      });
      // Absent or null means checked: connectors show by default, including
      // for instances saved before this preference existed.
      if (isObject(saved.showConnectors)) {
        next.showConnectors.clients = saved.showConnectors.clients !== false;
      }
      if (isObject(saved.sort)) {
        Object.keys(tables).forEach(function (table) {
          var spec = saved.sort[table];
          var config = tables[table];
          if (isObject(spec) && Object.keys(config.columns).some(function (index) { return config.columns[index] === spec.key; }) && (spec.direction === 'asc' || spec.direction === 'desc')) {
            next.sort[table] = { key: spec.key, direction: spec.direction };
          }
        });
      }
    } catch (err) {
      // Ignore malformed preference fields without affecting mailbox content.
    }
    return next;
  }

  var state = initialState();
  var groupEdit = null;
  var groupDialog = null;
  var groupInput = null;
  var groupTarget = null;
  var groupCombo = null;
  var groupRemove = null;
  var folderIcon = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 4.2A1.2 1.2 0 0 1 3.2 3h3l1.4 1.6h5.2A1.2 1.2 0 0 1 14 5.8v6A1.2 1.2 0 0 1 12.8 13H3.2A1.2 1.2 0 0 1 2 11.8z"/></svg>';

  function cleanGroup(value) {
    return String(value || '').trim().slice(0, 64);
  }

  function saveState() {
    pendingSave = JSON.stringify(state);
    flushSave();
  }

  function flushSave() {
    if (saving || !pendingSave || !preferencesReady) {
      return;
    }
    var snapshot = pendingSave;
    pendingSave = null;
    saving = true;
    fetch('/ui/dashboard/preferences', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
      body: snapshot
    }).then(function (response) {
      if (response.status !== 204) {
        throw new Error('Could not save dashboard preferences');
      }
      saving = false;
      flushSave();
    }).catch(function () {
      saving = false;
      pendingSave = pendingSave || snapshot;
      window.alert('Could not save dashboard preferences. Your changes are not yet saved. Please try again.');
    });
  }

  function rowID(table, row) {
    if (table === 'inboxes') {
      return row.getAttribute('data-inbox-id') || '';
    }
    if (table === 'domains') {
      return row.getAttribute('data-domain-id') || '';
    }
    var edit = row.querySelector('.edit-credential');
    var id = edit && edit.getAttribute('data-id');
    if (!id) {
      var remove = row.querySelector('.open-delete-client');
      id = remove && remove.getAttribute('data-id');
    }
    if (id) {
      row.setAttribute('data-dashboard-client-id', id);
    }
    return id || '';
  }

  function rowsFor(table) {
    var config = tables[table];
    if (!config || !config.tbody) {
      return [];
    }
    var selector = config.rowSelector || 'tr:not([data-dashboard-group-head])';
    return Array.prototype.slice.call(config.tbody.querySelectorAll(selector)).filter(function (row) {
      return !!rowID(table, row);
    });
  }

  function groupFor(table, row) {
    var id = rowID(table, row);
    return id && state.groups[table][id] ? cleanGroup(state.groups[table][id]) : '';
  }

  function setSortHeaders(table) {
    var config = tables[table];
    if (!config || !config.tbody) {
      return;
    }
    var head = config.tbody.closest('table').querySelector('thead');
    if (!head || !head.rows.length) {
      return;
    }
    var spec = state.sort[table];
    Array.prototype.forEach.call(head.rows[0].cells, function (th, index) {
      var key = config.columns[index];
      if (!key) {
        return;
      }
      th.classList.add('dashboard-sortable');
      th.setAttribute('tabindex', '0');
      th.setAttribute('data-dashboard-sort-table', table);
      th.setAttribute('data-dashboard-sort-key', key);
      if (spec && spec.key === key) {
        th.setAttribute('aria-sort', spec.direction === 'desc' ? 'descending' : 'ascending');
      } else {
        th.removeAttribute('aria-sort');
      }
    });
  }

  function setupCardTools(table) {
    var config = tables[table];
    if (!config || !config.card || !config.tbody) {
      return;
    }
    var head = config.card.querySelector('.card-head');
    if (!head) {
      return;
    }
    var addButton = head.querySelector(table === 'inboxes' ? '#add-inbox' : '#add-key');
    var tools = head.querySelector('.dashboard-group-tools');
    if (!tools) {
      tools = document.createElement('div');
      tools.className = 'dashboard-group-tools';
      head.appendChild(tools);
    }
    if (addButton && addButton.parentNode !== tools) {
      tools.appendChild(addButton);
    }
    if (table === 'domains') {
      return;
    }

    var toggle = tools.querySelector('[data-dashboard-groups-toggle]');
    if (!toggle) {
      toggle = document.createElement('button');
      toggle.type = 'button';
      toggle.className = 'secondary btn-sm dashboard-groups-toggle';
      toggle.setAttribute('data-dashboard-groups-toggle', table);
      var box = document.createElement('span');
      box.className = 'dashboard-groups-box';
      box.setAttribute('aria-hidden', 'true');
      var label = document.createElement('span');
      label.textContent = 'Groups';
      toggle.appendChild(box);
      toggle.appendChild(label);
      tools.insertBefore(toggle, addButton || null);
    }
    if (table === 'clients') {
      var connectorToggle = tools.querySelector('[data-dashboard-connectors-toggle]');
      if (!connectorToggle) {
        connectorToggle = document.createElement('button');
        connectorToggle.type = 'button';
        connectorToggle.className = 'secondary btn-sm dashboard-groups-toggle';
        connectorToggle.setAttribute('data-dashboard-connectors-toggle', table);
        var connBox = document.createElement('span');
        connBox.className = 'dashboard-groups-box';
        connBox.setAttribute('aria-hidden', 'true');
        var connLabel = document.createElement('span');
        connLabel.textContent = 'Connectors';
        connectorToggle.appendChild(connBox);
        connectorToggle.appendChild(connLabel);
        tools.insertBefore(connectorToggle, toggle.nextSibling || addButton || null);
      }
    }
    var collapse = tools.querySelector('[data-dashboard-collapse-all]');
    if (!collapse) {
      collapse = document.createElement('button');
      collapse.type = 'button';
      collapse.className = 'secondary btn-sm';
      collapse.setAttribute('data-dashboard-collapse-all', table);
      tools.insertBefore(collapse, addButton || null);
    }
  }

  function setupGroupDialog() {
    if (groupDialog) {
      return;
    }
    groupDialog = document.createElement('dialog');
    groupDialog.id = 'dashboard-group-dialog';
    var form = document.createElement('form');
    var title = document.createElement('h2');
    title.textContent = 'Set group';
    groupTarget = document.createElement('p');
    groupTarget.className = 'muted';
    groupTarget.style.cssText = 'margin:4px 0 0';
    var label = document.createElement('label');
    label.htmlFor = 'dashboard-group-input';
    label.textContent = 'Group name';
    groupInput = document.createElement('input');
    groupInput.id = 'dashboard-group-input';
    groupInput.setAttribute('autocomplete', 'off');
    groupInput.setAttribute('spellcheck', 'false');
    groupInput.setAttribute('maxlength', '64');
    groupInput.setAttribute('placeholder', 'e.g. Agents');
    var hint = document.createElement('p');
    hint.className = 'muted small';
    hint.textContent = 'Type a new name or choose an existing group. Groups are labels only; they do not affect access, routing, or quota.';
    var actions = document.createElement('div');
    actions.className = 'dialog-actions';
    groupRemove = document.createElement('button');
    groupRemove.type = 'button';
    groupRemove.className = 'secondary danger';
    groupRemove.textContent = 'Remove from group';
    var cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'secondary';
    cancel.textContent = 'Cancel';
    cancel.setAttribute('data-dashboard-group-cancel', '');
    var save = document.createElement('button');
    save.type = 'submit';
    save.textContent = 'Save';
    actions.appendChild(groupRemove);
    actions.appendChild(cancel);
    actions.appendChild(save);
    form.appendChild(title);
    form.appendChild(groupTarget);
    form.appendChild(label);
    form.appendChild(groupInput);
    form.appendChild(hint);
    form.appendChild(actions);
    // Wrap the input in the combobox only once it is in the tree, so wire() can
    // insert its dropdown alongside it.
    groupCombo = window.mammothmailCombo.wire(groupInput, { options: function () { return groupNamesFor(groupEdit ? groupEdit.table : 'inboxes'); } });
    groupDialog.appendChild(form);
    document.body.appendChild(groupDialog);
    form.addEventListener('submit', function (event) {
      event.preventDefault();
      applyGroup(groupInput.value);
    });
    cancel.addEventListener('click', closeGroupDialog);
    groupRemove.addEventListener('click', function () {
      applyGroup('');
    });
    groupDialog.addEventListener('cancel', function () {
      groupEdit = null;
    });
    groupDialog.addEventListener('close', function () {
      groupEdit = null;
    });
  }

  function sortedGroupNames(table) {
    var names = Object.create(null);
    rowsFor(table).forEach(function (row) {
      var name = groupFor(table, row);
      if (name) {
        names[name] = true;
      }
    });
    return Object.keys(names).sort(function (a, b) {
      return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
    });
  }

  // groupNamesFor is the combobox option source: the groups already in use on
  // that table. It is read on every open/keystroke so newly created groups appear
  // without rebuilding the field.
  function groupNamesFor(table) {
    return sortedGroupNames(table || 'inboxes');
  }

  function updateGroupOptions(table) {
    if (groupCombo) {
      groupCombo.setOptions();
    }
  }

  function updateGroupButton(table, row) {
    var actions = row.querySelector('td.actions');
    if (!actions) {
      return;
    }
    var id = rowID(table, row);
    var button = actions.querySelector('.dashboard-row-group');
    if (!button) {
      button = document.createElement('button');
      button.type = 'button';
      button.className = 'secondary dashboard-row-group';
      button.setAttribute('data-dashboard-group-edit', table);
      button.setAttribute('data-group-id', id);
      button.innerHTML = folderIcon + '<span class="dashboard-row-group-name"></span>';
      actions.insertBefore(button, actions.firstChild);
    }
    var name = groupFor(table, row);
    button.classList.toggle('empty', !name);
    button.querySelector('.dashboard-row-group-name').textContent = name || 'Group';
    var label = table === 'inboxes' ? 'inbox' : 'client';
    button.setAttribute('aria-label', 'Set group for ' + label + ' ' + (row.cells[0] ? row.cells[0].textContent.trim() : ''));
    button.title = name ? 'Group: ' + name : 'Set group';
  }

  function setOriginalOrder(table, rows) {
    var counter = 0;
    rows.forEach(function (row) {
      if (typeof row.__mammothmailOriginalOrder !== 'number') {
        row.__mammothmailOriginalOrder = counter;
      }
      counter++;
    });
  }

  function parseCount(text) {
    var match = String(text || '').replace(/,/g, '').match(/-?\d+(?:\.\d+)?/);
    return match ? Number(match[0]) : 0;
  }

  function parseSize(text) {
    var match = String(text || '').replace(/,/g, '').match(/([\d.]+)\s*(B|KB|MB|GB|TB)?/i);
    if (!match) {
      return 0;
    }
    var powers = { B: 0, KB: 1, MB: 2, GB: 3, TB: 4 };
    return Number(match[1]) * Math.pow(1024, powers[(match[2] || 'B').toUpperCase()] || 0);
  }

  function sortValue(table, row, key) {
    var config = tables[table];
    if (table === 'inboxes' && (key === 'unread' || key === 'pending')) {
      var countCell = row.querySelector('[data-cell="' + key + '"]');
      return parseCount(countCell ? countCell.textContent : '');
    }
    var columns = config.columns;
    var index = -1;
    Object.keys(columns).some(function (cellIndex) {
      if (columns[cellIndex] === key) {
        index = Number(cellIndex);
        return true;
      }
      return false;
    });
    var cell = index >= 0 ? row.cells[index] : null;
    var text = cell ? cell.textContent.replace(/\s+/g, ' ').trim() : '';
    if (table === 'inboxes' && key === 'size') {
      return parseSize(text);
    }
    return text;
  }

  function sortedRows(table, rows) {
    var spec = state.sort[table];
    var config = tables[table];
    if (!spec) {
      return rows.slice().sort(function (a, b) {
        return a.__mammothmailOriginalOrder - b.__mammothmailOriginalOrder;
      });
    }
    return rows.slice().sort(function (a, b) {
      var av = sortValue(table, a, spec.key);
      var bv = sortValue(table, b, spec.key);
      var result;
      if (config.numeric[spec.key]) {
        result = av - bv;
      } else {
        result = String(av).localeCompare(String(bv), undefined, { numeric: true, sensitivity: 'base' });
      }
      if (!result) {
        result = a.__mammothmailOriginalOrder - b.__mammothmailOriginalOrder;
      }
      return spec.direction === 'desc' ? -result : result;
    });
  }

  function groupRows(table, rows) {
    var grouped = Object.create(null);
    rows.forEach(function (row) {
      var name = groupFor(table, row);
      if (!Object.prototype.hasOwnProperty.call(grouped, name)) {
        grouped[name] = [];
      }
      grouped[name].push(row);
    });
    var names = Object.keys(grouped).filter(function (name) { return name !== ''; });
    names.sort(function (a, b) {
      return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
    });
    if (Object.prototype.hasOwnProperty.call(grouped, '')) {
      names.push('');
    }
    return names.map(function (name) {
      return { name: name, rows: sortedRows(table, grouped[name]) };
    });
  }

  function isCollapsed(table, name) {
    return state.collapsed[table][name] === true;
  }

  function groupHead(table, name, count, expanded, columns) {
    var row = document.createElement('tr');
    row.className = 'dashboard-group-head';
    row.setAttribute('data-dashboard-group-head', '');
    var cell = document.createElement('td');
    cell.colSpan = columns;
    var button = document.createElement('button');
    button.type = 'button';
    button.className = 'dashboard-group-toggle';
    button.setAttribute('data-dashboard-group-toggle', table);
    button.setAttribute('data-group-name', name);
    button.setAttribute('aria-expanded', String(expanded));
    var chevron = document.createElement('span');
    chevron.className = 'dashboard-group-chevron';
    chevron.setAttribute('aria-hidden', 'true');
    var label = document.createElement('span');
    label.className = 'dashboard-group-name';
    if (name) {
      label.textContent = name;
    } else {
      label.className += ' muted';
      label.textContent = 'Ungrouped';
    }
    var total = document.createElement('span');
    total.className = 'muted dashboard-group-count';
    total.textContent = '(' + count + ')';
    button.appendChild(chevron);
    button.appendChild(label);
    button.appendChild(total);
    cell.appendChild(button);
    row.appendChild(cell);
    return row;
  }

  function updateCardControls(table, groups) {
    var config = tables[table];
    if (!config || !config.card) {
      return;
    }
    var toggle = config.card.querySelector('[data-dashboard-groups-toggle]');
    var collapse = config.card.querySelector('[data-dashboard-collapse-all]');
    var hasNamed = groups.some(function (group) { return group.name !== ''; });
    var shown = state.showGroups[table] !== false;
    if (toggle) {
      toggle.setAttribute('aria-pressed', shown ? 'true' : 'false');
    }
    var connectorToggle = config.card.querySelector('[data-dashboard-connectors-toggle]');
    if (connectorToggle) {
      connectorToggle.setAttribute('aria-pressed', state.showConnectors[table] === false ? 'false' : 'true');
    }
    if (collapse) {
      collapse.hidden = !shown || !hasNamed;
      var allCollapsed = hasNamed && groups.length > 0 && groups.every(function (group) { return isCollapsed(table, group.name); });
      collapse.textContent = allCollapsed ? 'Expand all' : 'Collapse all';
    }
  }

  function isConnectorRow(row) {
    return row.hasAttribute('data-connector-row');
  }

  function renderGroupedTable(table, rows) {
    var config = tables[table];
    var parked = [];
    if (table === 'clients' && state.showConnectors[table] === false) {
      // Parked rows stay appended (hidden) so rowsFor, which re-reads the
      // tbody, can find them again when the preference is turned back on.
      rows = rows.filter(function (row) {
        if (isConnectorRow(row)) {
          parked.push(row);
          return false;
        }
        return true;
      });
    }
    var groups = groupRows(table, rows);
    var hasNamed = groups.some(function (group) { return group.name !== ''; });
    var showHeaders = state.showGroups[table] !== false && hasNamed;
    var columns = config.tbody.closest('table').querySelector('thead').rows[0].cells.length;
    config.tbody.textContent = '';
    groups.forEach(function (group) {
      var collapsed = isCollapsed(table, group.name);
      if (showHeaders) {
        config.tbody.appendChild(groupHead(table, group.name, group.rows.length, !collapsed, columns));
      }
      group.rows.forEach(function (row) {
        row.hidden = showHeaders && collapsed;
        updateGroupButton(table, row);
        config.tbody.appendChild(row);
      });
    });
    parked.forEach(function (row) {
      row.hidden = true;
      config.tbody.appendChild(row);
    });
    updateCardControls(table, groups);
  }

  function renderDomainTable(rows) {
    var config = tables.domains;
    sortedRows('domains', rows).forEach(function (row) {
      config.tbody.appendChild(row);
    });
  }

  function refresh() {
    ['inboxes', 'clients'].forEach(function (table) {
      var config = tables[table];
      if (!config || !config.tbody) {
        return;
      }
      var rows = rowsFor(table);
      setOriginalOrder(table, rows);
      setupCardTools(table);
      setSortHeaders(table);
      setupGroupDialog();
      renderGroupedTable(table, rows);
    });
    if (tables.domains && tables.domains.tbody) {
      var domainRows = rowsFor('domains');
      setOriginalOrder('domains', domainRows);
      setSortHeaders('domains');
      renderDomainTable(domainRows);
    }
  }

  function openGroupDialog(table, row) {
    setupGroupDialog();
    var id = rowID(table, row);
    groupEdit = { table: table, id: id };
    groupTarget.textContent = row.cells[0] ? row.cells[0].textContent.trim() : '';
    groupInput.value = groupFor(table, row);
    groupRemove.disabled = !groupInput.value;
    updateGroupOptions(table);
    if (typeof groupDialog.showModal === 'function') {
      groupDialog.showModal();
    } else {
      groupDialog.setAttribute('open', 'open');
    }
    window.setTimeout(function () { groupInput.focus(); }, 0);
  }

  function closeGroupDialog() {
    if (groupDialog && groupDialog.open) {
      groupDialog.close();
    } else if (groupDialog) {
      groupDialog.removeAttribute('open');
    }
    groupEdit = null;
  }

  function applyGroup(value) {
    if (!groupEdit || !groupEdit.id) {
      return;
    }
    var name = cleanGroup(value);
    if (name) {
      state.groups[groupEdit.table][groupEdit.id] = name;
      if (!Object.prototype.hasOwnProperty.call(state.collapsed[groupEdit.table], name)) {
        state.collapsed[groupEdit.table][name] = false;
      }
    } else {
      delete state.groups[groupEdit.table][groupEdit.id];
    }
    closeGroupDialog();
    saveState();
    refresh();
  }

  function sortByHeader(header) {
    var table = header.getAttribute('data-dashboard-sort-table');
    var key = header.getAttribute('data-dashboard-sort-key');
    var current = state.sort[table];
    var direction = current && current.key === key && current.direction === 'asc' ? 'desc' : 'asc';
    state.sort[table] = { key: key, direction: direction };
    saveState();
    refresh();
    var next = tables[table].tbody.closest('table').querySelector('[data-dashboard-sort-key="' + key + '"]');
    if (next) {
      next.focus();
    }
  }

  function collapseAll(table) {
    var groups = groupRows(table, rowsFor(table));
    var named = groups.some(function (group) { return group.name !== ''; });
    if (!named) {
      return;
    }
    var collapse = groups.some(function (group) { return !isCollapsed(table, group.name); });
    groups.forEach(function (group) {
      state.collapsed[table][group.name] = collapse;
    });
    saveState();
    refresh();
  }

  document.addEventListener('click', function (event) {
    if (!preferencesReady) {
      return;
    }
    var header = event.target.closest('[data-dashboard-sort-key]');
    if (header) {
      sortByHeader(header);
      return;
    }
    var groupToggle = event.target.closest('[data-dashboard-group-toggle]');
    if (groupToggle) {
      var table = groupToggle.getAttribute('data-dashboard-group-toggle');
      var name = groupToggle.getAttribute('data-group-name') || '';
      state.collapsed[table][name] = !isCollapsed(table, name);
      saveState();
      refresh();
      return;
    }
    var collapse = event.target.closest('[data-dashboard-collapse-all]');
    if (collapse && !collapse.hidden) {
      collapseAll(collapse.getAttribute('data-dashboard-collapse-all'));
      return;
    }
    var groupsToggle = event.target.closest('[data-dashboard-groups-toggle]');
    if (groupsToggle) {
      var showTable = groupsToggle.getAttribute('data-dashboard-groups-toggle');
      state.showGroups[showTable] = state.showGroups[showTable] === false;
      saveState();
      refresh();
      return;
    }
    var connectorsToggle = event.target.closest('[data-dashboard-connectors-toggle]');
    if (connectorsToggle) {
      var connectorTable = connectorsToggle.getAttribute('data-dashboard-connectors-toggle');
      state.showConnectors[connectorTable] = state.showConnectors[connectorTable] === false;
      saveState();
      refresh();
      return;
    }
    var editButton = event.target.closest('[data-dashboard-group-edit]');
    if (editButton) {
      openGroupDialog(editButton.getAttribute('data-dashboard-group-edit'), editButton.closest('tr'));
    }
  });

  document.addEventListener('keydown', function (event) {
    if (preferencesReady && (event.key === 'Enter' || event.key === ' ') && event.target.matches('[data-dashboard-sort-key]')) {
      event.preventDefault();
      sortByHeader(event.target);
    }
  });

  window.mammothmailDashboardGroups = { refresh: function () {
    if (preferencesReady) {
      refresh();
    }
  } };
  fetch('/ui/dashboard/preferences', { credentials: 'same-origin', cache: 'no-store' }).then(function (response) {
    if (!response.ok || response.redirected) {
      throw new Error('Could not load dashboard preferences');
    }
    return response.json();
  }).then(function (saved) {
    state = loadState(saved);
    preferencesReady = true;
    refresh();
  }).catch(function () {
    // Do not overwrite database preferences with defaults after a failed read.
    window.alert('Could not load dashboard preferences. Refresh the page to try again.');
  });
})();

(function () {
  document.querySelectorAll('tr.row-link[data-href]').forEach(function (row) {
    row.addEventListener('click', function (e) {
      if (e.target.closest('a,button,input,select,textarea,label,form')) {
        return;
      }
      window.location.href = row.getAttribute('data-href');
    });
  });
})();

// initSenderEditor wires an allow-list editor shared by the add and edit
// inbox dialogs: a list of allowed addresses, an approver address that is
// always allowed, and a restrict toggle that reveals the list.
var initSenderEditor = (function () {
  return function (opts) {
    var list = opts.list;
    var input = opts.input;
    var note = opts.note;
    var approverEmail = opts.approverEmail;
    var restrict = opts.restrict;
    var requireAuth = opts.requireAuth;
    var section = opts.section;
    var mxSection = opts.mxSection;

    function approverValue() {
      return (approverEmail && approverEmail.value || '').trim().toLowerCase();
    }

    function isRestricted() {
      return !!(restrict && restrict.checked);
    }

    function refreshRestrictVisibility() {
      if (section) {
        section.hidden = !isRestricted();
      }
    }

    function refreshSenderNote() {
      if (!note || !list) {
        return;
      }
      var count = list.querySelectorAll('input[name=allowed]').length;
      if (count === 0 && !approverValue()) {
        note.textContent = 'This inbox currently blocks all senders.';
      } else if (count === 0) {
        note.textContent = 'Only the approver can email this inbox.';
      } else {
        note.textContent = 'Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.';
      }
    }

    function refreshSenderEmpty() {
      if (!list) {
        return;
      }
      var empty = list.querySelector('.empty');
      var hasSenders = list.querySelectorAll('input[name=allowed]').length > 0 || !!approverValue() || !!list.querySelector('.locked');
      if (!hasSenders) {
        if (!empty) {
          var li = document.createElement('li');
          li.className = 'empty';
          li.textContent = 'Add an address below to allow it to email this inbox.';
          list.appendChild(li);
        }
      } else if (empty) {
        empty.remove();
      }
    }

    function addLockedApprover(email) {
      if (!list) {
        return;
      }
      email = (email || '').trim().toLowerCase();
      if (!email) {
        return;
      }
      var li = document.createElement('li');
      li.className = 'locked';
      var label = document.createElement('span');
      label.className = 'addr';
      label.textContent = email + ' (approver)';
      var locked = document.createElement('span');
      locked.className = 'muted small';
      locked.textContent = 'always allowed';
      li.appendChild(label);
      li.appendChild(locked);
      list.appendChild(li);
    }

    function addSender(value) {
      if (!list) {
        return;
      }
      value = (value || '').trim().toLowerCase();
      if (!value) {
        return;
      }
      var li = document.createElement('li');
      var hidden = document.createElement('input');
      hidden.type = 'hidden';
      hidden.name = 'allowed';
      hidden.value = value;
      var label = document.createElement('span');
      label.className = 'addr';
      label.textContent = value;
      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'secondary icon-btn';
      remove.title = 'Remove';
      remove.setAttribute('aria-label', 'Remove');
      remove.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 4l8 8M12 4l-8 8"/></svg>';
      remove.addEventListener('click', function () {
        li.remove();
        refreshSenderNote();
        refreshSenderEmpty();
      });
      li.appendChild(hidden);
      li.appendChild(label);
      li.appendChild(remove);
      list.appendChild(li);
      refreshSenderNote();
      refreshSenderEmpty();
    }

    function setSenders(raw, approver) {
      if (!list) {
        return;
      }
      list.innerHTML = '';
      addLockedApprover(approver);
      (raw || '').split(',').forEach(function (entry) {
        if ((entry || '').trim().toLowerCase() === (approver || '').trim().toLowerCase()) {
          return;
        }
        addSender(entry);
      });
      refreshSenderNote();
      refreshSenderEmpty();
    }

    function setApprover(value) {
      if (approverEmail) {
        approverEmail.value = value || '';
      }
    }

    function setRestricted(value) {
      if (restrict) {
        restrict.checked = !!value;
      }
      refreshRestrictVisibility();
    }

    function setRequireAuth(value) {
      if (requireAuth) {
        requireAuth.checked = !!value;
      }
    }

    // setMX reveals the authenticated-sender control only for domains that
    // receive mail by direct SMTP (MX); the flag has no effect on webhook
    // providers. Disabling it also unchecks the box so a hidden control cannot
    // submit a stale value.
    function setMX(value) {
      if (mxSection) {
        mxSection.hidden = !value;
      }
      if (!value && requireAuth) {
        requireAuth.checked = false;
      }
    }

    function clearInput() {
      if (input) {
        input.value = '';
      }
    }

    function approverEdited() {
      var current = Array.prototype.map.call(list.querySelectorAll('input[name=allowed]'), function (i) {
        return i.value;
      }).join(',');
      setSenders(current, approverValue());
    }

    function reset() {
      if (list) {
        list.innerHTML = '';
      }
      setApprover('');
      clearInput();
      setRestricted(false);
      setRequireAuth(false);
      setMX(false);
      refreshSenderNote();
      refreshSenderEmpty();
    }

    if (opts.addBtn && input) {
      opts.addBtn.addEventListener('click', function () {
        addSender(input.value);
        input.value = '';
        input.focus();
      });
    }
    if (input) {
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') {
          e.preventDefault();
          addSender(input.value);
          input.value = '';
        }
      });
    }
    if (restrict) {
      restrict.addEventListener('change', refreshRestrictVisibility);
    }
    if (approverEmail) {
      approverEmail.addEventListener('input', approverEdited);
    }

    reset();

    return {
      setSenders: setSenders,
      setApprover: setApprover,
      setRestricted: setRestricted,
      setRequireAuth: setRequireAuth,
      setMX: setMX,
      clearInput: clearInput,
      reset: reset
    };
  };
})();

// initAliasEditor renders an inbox dialog's inline alias editor: a list of
// aliases (a sender name with the email address beneath it, plus edit and
// remove buttons) and an add/edit form inside the same panel. Each alias is
// submitted as a repeated `alias` field (the full address) plus a parallel
// repeated `alias_name` field, zipped by index server-side.
var initAliasEditor = (function () {
  return function (opts) {
    var list = opts.list;
    var form = opts.form || {};
    var editing = null;

    function rows() {
      if (!list) {
        return [];
      }
      return Array.prototype.slice.call(list.querySelectorAll('li.alias-row'));
    }

    function setError(msg) {
      if (!form.error) {
        return;
      }
      form.error.textContent = msg || '';
      form.error.hidden = !msg;
    }

    function refreshEmpty() {
      if (!list) {
        return;
      }
      var empty = list.querySelector('.empty');
      var hasAliases = rows().length > 0;
      if (!hasAliases) {
        if (!empty) {
          var li = document.createElement('li');
          li.className = 'empty';
          li.textContent = 'No aliases.';
          list.appendChild(li);
        }
      } else if (empty) {
        empty.remove();
      }
      if (opts.onChange) {
        opts.onChange();
      }
    }

    function iconButton(cls, label, svg) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'secondary icon-btn' + (cls ? ' ' + cls : '');
      b.title = label;
      b.setAttribute('aria-label', label);
      b.innerHTML = svg;
      return b;
    }

    var pencilSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg>';
    var crossSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 4l8 8M12 4l-8 8"/></svg>';

    function makeRow(name, address) {
      var li = document.createElement('li');
      li.className = 'alias-row';
      li.dataset.address = address;
      li.dataset.name = name || '';

      var text = document.createElement('div');
      text.className = 'alias-text';
      var nameEl = document.createElement('span');
      nameEl.className = 'alias-row-name';
      nameEl.textContent = name || '(no name)';
      if (!name) {
        nameEl.classList.add('muted');
      }
      var addrEl = document.createElement('span');
      addrEl.className = 'alias-row-addr';
      addrEl.textContent = address;
      text.appendChild(nameEl);
      text.appendChild(addrEl);

      var hiddenName = document.createElement('input');
      hiddenName.type = 'hidden';
      hiddenName.name = 'alias_name';
      hiddenName.value = name || '';
      var hiddenAddr = document.createElement('input');
      hiddenAddr.type = 'hidden';
      hiddenAddr.name = 'alias';
      hiddenAddr.value = address;

      var edit = iconButton('', 'Edit', pencilSVG);
      edit.addEventListener('click', function () {
        var at = address.lastIndexOf('@');
        openEditor(li, name, at > 0 ? address.slice(0, at) : address, at > 0 ? address.slice(at + 1) : '');
      });
      var remove = iconButton('danger', 'Remove', crossSVG);
      remove.addEventListener('click', function () {
        li.remove();
        refreshEmpty();
      });

      li.appendChild(text);
      li.appendChild(hiddenName);
      li.appendChild(hiddenAddr);
      li.appendChild(edit);
      li.appendChild(remove);
      return li;
    }

    function openEditor(row, name, local, domain) {
      editing = row || null;
      setError('');
      if (form.name) {
        form.name.value = name || '';
      }
      if (form.local) {
        form.local.value = local || '';
      }
      if (form.domain && domain) {
        form.domain.value = domain;
      }
      if (form.save) {
        form.save.textContent = editing ? 'Save' : 'Add';
      }
      if (opts.editor) {
        opts.editor.hidden = false;
      }
      if (form.name) {
        form.name.focus();
      }
    }

    function closeEditor() {
      editing = null;
      setError('');
      if (opts.editor) {
        opts.editor.hidden = true;
      }
    }

    function commit() {
      var name = (form.name ? form.name.value : '').trim();
      var local = (form.local ? form.local.value : '').trim().toLowerCase();
      var domain = (form.domain ? form.domain.value : '').trim().toLowerCase();
      if (!name) {
        setError('Name is required.');
        return;
      }
      if (name.length > 128) {
        setError('Name must be 128 characters or fewer.');
        return;
      }
      if (name.indexOf(',') !== -1 || /[\r\n]/.test(name) || /[\x00-\x1f\x7f]/.test(name)) {
        setError('Name may not contain commas, newlines or control characters.');
        return;
      }
      if (!local || local.indexOf('@') !== -1 || local.indexOf(' ') !== -1) {
        setError('Enter a valid email local part.');
        return;
      }
      if (!domain) {
        setError('Choose a domain.');
        return;
      }
      var address = local + '@' + domain;
      var duplicate = rows().some(function (r) {
        return r !== editing && r.dataset.address === address;
      });
      if (duplicate) {
        setError('That alias already exists.');
        return;
      }
      if (editing) {
        editing.dataset.address = address;
        editing.dataset.name = name;
        var nameEl = editing.querySelector('.alias-row-name');
        nameEl.textContent = name;
        nameEl.classList.remove('muted');
        editing.querySelector('.alias-row-addr').textContent = address;
        editing.querySelector('input[name=alias_name]').value = name;
        editing.querySelector('input[name=alias]').value = address;
      } else if (list) {
        list.appendChild(makeRow(name, address));
      }
      closeEditor();
      refreshEmpty();
    }

    // setAliases takes the comma-joined address list and an optional parallel
    // comma-joined name list (both emitted in the same order by the server).
    function setAliases(raw, rawNames) {
      if (!list) {
        return;
      }
      list.innerHTML = '';
      var addresses = (raw || '').split(',');
      var names = (rawNames || '').split(',');
      addresses.forEach(function (entry, i) {
        var address = (entry || '').trim().toLowerCase();
        if (!address) {
          return;
        }
        list.appendChild(makeRow((names[i] || '').trim(), address));
      });
      refreshEmpty();
    }

    function reset() {
      closeEditor();
      if (list) {
        list.innerHTML = '';
      }
      if (form.name) {
        form.name.value = '';
      }
      if (form.local) {
        form.local.value = '';
      }
      refreshEmpty();
    }

    if (opts.addBtn) {
      opts.addBtn.addEventListener('click', function () {
        var domainEl = form.domain;
        var def = domainEl && domainEl.options.length ? domainEl.options[0].value : '';
        openEditor(null, '', '', def);
      });
    }
    if (form.save) {
      form.save.addEventListener('click', commit);
    }
    if (form.cancel) {
      form.cancel.addEventListener('click', closeEditor);
    }
    [form.name, form.local].forEach(function (el) {
      if (el) {
        el.addEventListener('keydown', function (e) {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit();
          }
        });
      }
    });

    refreshEmpty();

    return {
      setAliases: setAliases,
      reset: reset
    };
  };
})();

// externalAliasBase builds the dedicated page path for one external alias.
function externalAliasBase(inboxID, aliasID) {
  return '/ui/inboxes/' + encodeURIComponent(inboxID) + '/external-aliases/' + encodeURIComponent(aliasID);
}

// renderExternalAliases fills an inbox dialog's external-alias list from the
// secret-free JSON on the edit button. Each row shows the sender name and
// address, an edit button for the sender name, a domain-style connector button
// (amber until configured) that opens the alias's connector popup, and an
// Activity link.
function renderExternalAliases(list, inboxID, raw) {
  if (!list) {
    return;
  }
  var data = [];
  try {
    data = JSON.parse(raw || '[]') || [];
  } catch (e) {
    data = [];
  }
  list.innerHTML = '';
  if (!data.length) {
    var empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = 'No external sending aliases.';
    list.appendChild(empty);
    return;
  }
  data.forEach(function (a) {
    var li = document.createElement('li');
    li.className = 'alias-row external';

    var text = document.createElement('div');
    text.className = 'alias-text';
    var nameEl = document.createElement('span');
    nameEl.className = 'alias-row-name';
    nameEl.textContent = a.display_name || '(no name)';
    if (!a.display_name) {
      nameEl.classList.add('muted');
    }
    var addrEl = document.createElement('span');
    addrEl.className = 'alias-row-addr';
    addrEl.textContent = a.address + ' · External · sending only';
    text.appendChild(nameEl);
    text.appendChild(addrEl);

    // Domain-style connector button: amber "Add" until configured, then a
    // secondary button labelled with the provider.
    var edit = document.createElement('button');
    edit.type = 'button';
    edit.className = 'secondary icon-btn external-alias-edit';
    edit.dataset.alias = a.id;
    edit.dataset.name = a.display_name || '';
    edit.dataset.address = a.address;
    edit.title = 'Edit sender name';
    edit.setAttribute('aria-label', 'Edit sender name');
    edit.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg>';

    var configure = document.createElement('button');
    configure.type = 'button';
    configure.className = (a.configured ? 'secondary' : 'amber') + ' btn-sm cell-edit external-alias-configure';
    configure.dataset.alias = a.id;
    configure.textContent = a.configured ? (a.provider || 'Configured') : 'Add';
    configure.title = a.configured ? 'Edit sending connector' : 'Configure sending connector';
    configure.setAttribute('aria-label', configure.title);

    var activity = document.createElement('a');
    activity.className = 'btn secondary icon-btn';
    activity.href = externalAliasBase(inboxID, a.id);
    activity.title = 'Activity';
    activity.setAttribute('aria-label', 'Activity');
    activity.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="2.5" width="9" height="11" rx="1.5"/><path d="M5.5 5.5h5M5.5 8h5M5.5 10.5h3"/></svg>';

    var remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'secondary icon-btn danger external-alias-remove';
    remove.dataset.alias = a.id;
    remove.dataset.address = a.address;
    remove.title = 'Delete';
    remove.setAttribute('aria-label', 'Delete');
    remove.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg>';

    li.appendChild(text);
    li.appendChild(edit);
    li.appendChild(configure);
    li.appendChild(activity);
    li.appendChild(remove);
    list.appendChild(li);
  });
}

// deleteExternalAlias POSTs the alias's delete endpoint from a detached form.
// The row lives inside the inbox edit form (which may not nest a form), so a
// one-off form is submitted instead.
function deleteExternalAlias(inboxID, aliasID, address) {
  if (!window.confirm('Delete external alias ' + (address || '') + ' and its connector? This cannot be undone.')) {
    return;
  }
  var csrf = document.querySelector('#inbox-edit-form [name=_csrf]');
  var form = document.createElement('form');
  form.method = 'post';
  form.action = '/ui/inboxes/' + encodeURIComponent(inboxID) + '/external-aliases/' + encodeURIComponent(aliasID) + '/delete';
  var tok = document.createElement('input');
  tok.type = 'hidden';
  tok.name = '_csrf';
  tok.value = csrf ? csrf.value : '';
  form.appendChild(tok);
  document.body.appendChild(form);
  form.submit();
}

// openExternalAliasDialog opens the external-alias sending editor as a sub-view
// of the inbox settings dialog. The editors are rendered server-side on the
// dashboard, keyed by alias id.
function openExternalAliasDialog(dlg, aliasID) {
  if (!dlg || !aliasID) {
    return;
  }
  var found = false;
  dlg.querySelectorAll('.extalias-sending-editor').forEach(function (ed) {
    var on = ed.getAttribute('data-alias-id') === aliasID;
    ed.hidden = !on;
    found = found || on;
  });
  if (found) {
    showInboxSubview(dlg, 'ext-alias-sending');
  }
}

// senderLabel renders an option as "Name (email)". Without a name it falls
// back to the bare address.
function senderLabel(name, address) {
  if (name) {
    return name + ' (' + address + ')';
  }
  return address;
}

// buildDefaultSenderSelect populates the Primary/Default Address <select> from
// the current alias list. The primary (empty value) is labelled with the inbox
// display name; each alias with its sender name. The desired value is kept
// selected when it still exists, otherwise the primary is selected.
function buildDefaultSenderSelect(select, primaryName, primary, aliasNames, aliases, desired) {
  if (!select) {
    return;
  }
  select.innerHTML = '';
  var primaryOpt = document.createElement('option');
  primaryOpt.value = '';
  if (primary) {
    primaryOpt.textContent = senderLabel(primaryName, primary);
  } else {
    primaryOpt.textContent = 'Primary address';
  }
  select.appendChild(primaryOpt);
  (aliases || []).forEach(function (addr) {
    if (!addr) {
      return;
    }
    var opt = document.createElement('option');
    opt.value = addr;
    opt.textContent = senderLabel((aliasNames || {})[addr], addr);
    select.appendChild(opt);
  });
  var want = (desired || '').toLowerCase();
  var matched = false;
  Array.prototype.forEach.call(select.options, function (o) {
    if (o.value && o.value.toLowerCase() === want) {
      matched = true;
    }
  });
  select.value = matched ? desired : '';
}

// currentAliasValues reads the hidden `alias` inputs from an alias editor list.
function currentAliasValues(list) {
  if (!list) {
    return [];
  }
  return Array.prototype.map.call(list.querySelectorAll('input[name=alias]'), function (i) {
    return i.value;
  });
}

// aliasNameByAddress reads a `li.alias-row` list into an address-to-name
// lookup, for labelling the default-sender select.
function aliasNameByAddress(list) {
  var out = {};
  if (!list) {
    return out;
  }
  list.querySelectorAll('li.alias-row').forEach(function (row) {
    var addr = row.dataset.address;
    if (addr) {
      out[addr] = row.dataset.name || '';
    }
  });
  return out;
}

(function () {
  var dlg = document.getElementById('inbox-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('inbox-add-form');
  var standaloneForm = document.getElementById('inbox-add-standalone-form');
  var addQuotaValue = document.getElementById('inbox-add-quota-value');
  var addQuotaUnit = document.getElementById('inbox-add-quota-unit');
  var addQuotaUnlimited = document.getElementById('inbox-add-quota-unlimited');
  if (addQuotaUnlimited) {
    addQuotaUnlimited.addEventListener('change', function () {
      if (addQuotaValue) {
        addQuotaValue.disabled = addQuotaUnlimited.checked;
      }
      if (addQuotaUnit) {
        addQuotaUnit.disabled = addQuotaUnlimited.checked;
      }
    });
  }
  var editor = initSenderEditor({
    list: document.getElementById('inbox-add-sender-list'),
    input: document.getElementById('inbox-add-sender-input'),
    note: document.getElementById('inbox-add-sender-note'),
    approverEmail: document.getElementById('inbox-add-approver-email'),
    restrict: document.getElementById('inbox-add-sender-restricted'),
    requireAuth: document.getElementById('inbox-add-require-auth'),
    section: document.getElementById('inbox-add-sender-section'),
    mxSection: document.getElementById('inbox-add-require-auth-section'),
    addBtn: document.getElementById('inbox-add-sender-add')
  });
  var addDomainEl = document.querySelector('#inbox-dialog [name=domain]');
  function refreshAddRequireAuth() {
    var opt = addDomainEl && addDomainEl.options[addDomainEl.selectedIndex];
    editor.setMX(!!(opt && opt.getAttribute('data-mx') === '1'));
  }
  if (addDomainEl) {
    addDomainEl.addEventListener('change', refreshAddRequireAuth);
  }
  var addAliasList = document.getElementById('inbox-add-alias-list');
  var addDefault = document.getElementById('inbox-add-default-sender');
  function addPrimary() {
    var localEl = document.querySelector('#inbox-dialog [name=local]');
    var domainEl = document.querySelector('#inbox-dialog [name=domain]');
    var displayEl = document.querySelector('#inbox-dialog [name=display]');
    var local = localEl ? localEl.value.trim().toLowerCase() : '';
    var domainName = domainEl ? domainEl.value.trim().toLowerCase() : '';
    return {
      address: local && domainName ? local + '@' + domainName : '',
      name: displayEl ? displayEl.value.trim() : ''
    };
  }
  function refreshAddSender() {
    var primary = addPrimary();
    buildDefaultSenderSelect(addDefault, primary.name, primary.address, aliasNameByAddress(addAliasList), currentAliasValues(addAliasList), addDefault ? addDefault.value : '');
  }
  var addDisplay = document.querySelector('#inbox-dialog [name=display]');
  if (addDisplay) {
    addDisplay.addEventListener('input', refreshAddSender);
  }
  var aliasEditor = initAliasEditor({
    list: addAliasList,
    addBtn: document.getElementById('inbox-add-alias-add'),
    editor: document.getElementById('inbox-add-alias-editor'),
    form: {
      name: document.getElementById('inbox-add-alias-name'),
      local: document.getElementById('inbox-add-alias-local'),
      domain: document.getElementById('inbox-add-alias-domain'),
      error: document.getElementById('inbox-add-alias-error'),
      save: document.getElementById('inbox-add-alias-save'),
      cancel: document.getElementById('inbox-add-alias-cancel')
    },
    onChange: refreshAddSender
  });
  var add = document.getElementById('add-inbox');
  if (add) {
    add.addEventListener('click', function () {
      if (form) {
        form.reset();
      }
      if (standaloneForm) {
        standaloneForm.reset();
      }
      editor.reset();
      refreshAddRequireAuth();
      aliasEditor.reset();
      refreshAddSender();
      if (dlg._resetTabs) {
        dlg._resetTabs();
      }
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('inbox-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
  bindInboxSettingsShell(dlg);
})();

(function () {
  var dlg = document.getElementById('add-domain-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var input = document.getElementById('add-domain-name');
  var panel = document.getElementById('add-domain-inherit');
  var parentEl = document.getElementById('add-domain-parent');
  var controls = document.getElementById('add-domain-inherit-controls');
  var names = (form.getAttribute('data-domain-names') || '')
    .split(',')
    .map(function (s) { return s.trim().toLowerCase(); })
    .filter(Boolean);
  // detect reveals the reuse-parent checkboxes when the typed name is a
  // subdomain of an existing domain; server-side detection is authoritative, so
  // with JS off the domain is still created as a subdomain with inheritance on.
  function detect() {
    if (!input || !panel) {
      return;
    }
    var v = (input.value || '').trim().toLowerCase().replace(/\.$/, '');
    var parent = '';
    names.forEach(function (d) {
      if (v.length > d.length + 1 && v.slice(-(d.length + 1)) === '.' + d) {
        if (d.length > parent.length) {
          parent = d;
        }
      }
    });
    if (parent) {
      if (parentEl) {
        parentEl.textContent = parent;
      }
      document.querySelectorAll('.add-domain-parent').forEach(function (el) {
        el.textContent = parent;
      });
      panel.hidden = false;
      if (controls) {
        controls.value = '1';
      }
    } else {
      panel.hidden = true;
      if (controls) {
        controls.value = '';
      }
    }
  }
  if (input) {
    input.addEventListener('input', detect);
  }
  var add = document.getElementById('add-domain');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      detect();
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('add-domain-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
})();

(function () {
  var dlg = document.getElementById('domain-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('domain-delete-form');
  var input = document.getElementById('domain-delete-input');
  var submit = document.getElementById('domain-delete-submit');
  var nameEl = document.getElementById('domain-delete-name');
  var inboxSummary = document.getElementById('domain-delete-inbox-summary');
  var inboxList = document.getElementById('domain-delete-inbox-list');
  var expected = '';
  function matches() {
    return input.value.trim().toLowerCase() === expected.toLowerCase();
  }
  function sync() {
    submit.disabled = !matches();
  }
  function renderInboxes(btn) {
    var boxes = [];
    try {
      boxes = JSON.parse(btn.getAttribute('data-inboxes') || '[]') || [];
    } catch (err) {
      boxes = [];
    }
    inboxList.textContent = '';
    if (!boxes.length) {
      if (inboxSummary) {
        inboxSummary.textContent = 'This domain has no mailboxes.';
      }
      inboxList.hidden = true;
      return;
    }
    if (inboxSummary) {
      inboxSummary.textContent = 'This deletes ' + boxes.length + (boxes.length === 1 ? ' mailbox:' : ' mailboxes:');
    }
    inboxList.hidden = false;
    inboxList.textContent = boxes.map(function (b) {
      return b.name ? b.name + ' — ' + b.address : b.address;
    }).join('\n');
  }
  document.querySelectorAll('.open-delete-domain').forEach(function (btn) {
    btn.addEventListener('click', function () {
      expected = btn.getAttribute('data-name') || '';
      form.setAttribute('action', '/ui/domains/' + btn.getAttribute('data-domain') + '/delete');
      nameEl.textContent = expected;
      dlg.querySelectorAll('[data-copy]').forEach(function (el) { el.setAttribute('data-copy', expected); });
      renderInboxes(btn);
      input.value = '';
      sync();
      dlg.showModal();
      input.focus();
    });
  });
  input.addEventListener('input', sync);
  form.addEventListener('submit', function (e) {
    if (!matches()) {
      e.preventDefault();
      sync();
    }
  });
})();

(function () {
  var dlg = document.getElementById('inbox-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('inbox-delete-form');
  var input = document.getElementById('inbox-delete-input');
  var submit = document.getElementById('inbox-delete-submit');
  var addressEl = document.getElementById('inbox-delete-address');
  var expected = '';
  function matches() {
    return input.value.trim().toLowerCase() === expected.toLowerCase();
  }
  function sync() {
    submit.disabled = !matches();
  }
  document.querySelectorAll('.open-delete-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      expected = btn.getAttribute('data-address') || '';
      form.setAttribute('action', '/ui/inboxes/' + btn.getAttribute('data-id') + '/delete');
      addressEl.textContent = expected;
      dlg.querySelectorAll('[data-copy]').forEach(function (el) { el.setAttribute('data-copy', expected); });
      input.value = '';
      sync();
      dlg.showModal();
      input.focus();
    });
  });
  input.addEventListener('input', sync);
  form.addEventListener('submit', function (e) {
    if (!matches()) {
      e.preventDefault();
      sync();
    }
  });
})();

// Client delete: one confirmation dialog shared by the dashboard Clients card
// and the inbox Connectors sub-view (whose rows are rendered dynamically), so
// the trigger is delegated on the document.
(function () {
  var dlg = document.getElementById('client-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('client-delete-form');
  var input = document.getElementById('client-delete-input');
  var submit = document.getElementById('client-delete-submit');
  var nameEl = document.getElementById('client-delete-name');
  var typeEl = document.getElementById('client-delete-type');
  var inboxField = form.querySelector('input[name=inbox]');
  var expected = '';
  function matches() {
    return input.value.trim().toLowerCase() === expected.toLowerCase();
  }
  function sync() {
    submit.disabled = !matches();
  }
  function open(btn) {
    expected = btn.getAttribute('data-name') || '';
    var type = btn.getAttribute('data-type') || '';
    form.setAttribute('action', '/ui/' + btn.getAttribute('data-kind') + '/' + btn.getAttribute('data-id') + '/delete');
    nameEl.textContent = expected;
    if (typeEl) {
      typeEl.textContent = type ? '(' + type + ')' : '';
    }
    if (inboxField) {
      inboxField.value = btn.getAttribute('data-inbox') || '';
    }
    dlg.querySelectorAll('[data-copy]').forEach(function (el) { el.setAttribute('data-copy', expected); });
    input.value = '';
    sync();
    dlg.showModal();
    input.focus();
  }
  document.addEventListener('click', function (e) {
    var btn = e.target && e.target.closest ? e.target.closest('.open-delete-client') : null;
    if (btn) {
      e.preventDefault();
      e.stopPropagation();
      open(btn);
    }
  });
  input.addEventListener('input', sync);
  form.addEventListener('submit', function (e) {
    if (!matches()) {
      e.preventDefault();
      sync();
    }
  });
})();

// Select-all with a Gmail-style "select all N" escape hatch. The header box
// selects the rows on the current page; when the folder holds more than a page
// (data-total on the banner exceeds the visible rows) the banner offers to widen
// the selection to every matching item. Widening sets scope=all on the bulk
// form, so the server re-derives the set itself rather than trusting a long id
// list; "Clear selection" resets it to the page scope and unchecks everything.
(function () {
  var form = document.getElementById('bulk-form') || document.getElementById('drafts-bulk-form');
  if (!form) {
    return;
  }
  var formId = form.id;
  var all = document.getElementById('select-all');
  var boxes = document.querySelectorAll('input[name=ids][form=' + formId + ']');
  var banner = document.querySelector('[data-select-banner]');
  var scopeInput = form.querySelector('input[name=scope]');
  var pageSpan = banner ? banner.querySelector('[data-select-page]') : null;
  var allSpan = banner ? banner.querySelector('[data-select-all-span]') : null;
  var pageCount = banner ? banner.querySelector('[data-page-count]') : null;
  var total = banner ? parseInt(banner.getAttribute('data-total'), 10) || 0 : 0;
  var totalSpans = banner ? banner.querySelectorAll('[data-total-count]') : [];
  totalSpans.forEach(function (el) {
    el.textContent = String(total);
  });

  function checkedCount() {
    var n = 0;
    boxes.forEach(function (box) {
      if (box.checked) {
        n++;
      }
    });
    return n;
  }

  function setScope(scope) {
    if (scopeInput) {
      scopeInput.value = scope;
    }
  }

  function refreshAll() {
    var n = checkedCount();
    if (all) {
      all.checked = n > 0 && n === boxes.length;
      all.indeterminate = n > 0 && n < boxes.length;
    }
    if (!banner) {
      return;
    }
    var allScope = scopeInput && scopeInput.value === 'all';
    var fullPage = n > 0 && n === boxes.length;
    if (allScope) {
      banner.hidden = false;
      if (pageSpan) pageSpan.hidden = true;
      if (allSpan) allSpan.hidden = false;
      return;
    }
    // Offer "select all N" only when the header box is checked on a full page
    // and the folder actually holds more than the page shows.
    var offerAll = fullPage && total > boxes.length;
    banner.hidden = !offerAll;
    if (pageSpan) pageSpan.hidden = !offerAll;
    if (allSpan) allSpan.hidden = true;
    if (pageCount) pageCount.textContent = String(boxes.length);
  }

  if (all) {
    all.addEventListener('change', function () {
      if (all.checked) {
        setScope('page');
      }
      boxes.forEach(function (box) {
        box.checked = all.checked;
      });
      refreshAll();
    });
  }
  boxes.forEach(function (box) {
    box.addEventListener('change', function () {
      if (!box.checked) {
        setScope('page');
      }
      refreshAll();
    });
  });

  if (banner) {
    var allLink = banner.querySelector('[data-select-all-link]');
    if (allLink) {
      allLink.addEventListener('click', function () {
        boxes.forEach(function (box) {
          box.checked = true;
        });
        setScope('all');
        refreshAll();
      });
    }
    var clearLink = banner.querySelector('[data-clear-selection]');
    if (clearLink) {
      clearLink.addEventListener('click', function () {
        boxes.forEach(function (box) {
          box.checked = false;
        });
        if (all) {
          all.checked = false;
          all.indeterminate = false;
        }
        setScope('page');
        refreshAll();
      });
    }
  }
})();

// A bulk action widened to the whole folder ("select all N") names the full
// count in its confirmation, so deleting every matching message can never be
// fired off as a single anonymous click. Page-scoped bulk actions are left to
// their own data-confirm (if any).
(function () {
  document.querySelectorAll('form.bulkbar button[data-confirm-all]').forEach(function (btn) {
    btn.addEventListener('click', function (e) {
      var form = btn.form;
      var scope = form ? form.querySelector('input[name=scope]') : null;
      if (!scope || scope.value !== 'all') {
        return;
      }
      var banner = document.querySelector('[data-select-banner]');
      var n = banner ? banner.getAttribute('data-total') : '';
      var msg = btn.getAttribute('data-confirm-all');
      if (n) {
        msg = msg.replace('?', ' all ' + n + '?');
      }
      if (!window.confirm(msg)) {
        e.preventDefault();
      }
    });
  });
})();

(function () {
  document.querySelectorAll('button[data-confirm]').forEach(function (btn) {
    btn.addEventListener('click', function (e) {
      if (!window.confirm(btn.getAttribute('data-confirm'))) {
        e.preventDefault();
      }
    });
  });
})();

// clearUrlParams strips dialog-opening query params from the address bar so a
// refresh after cancelling a URL-driven dialog does not re-open it. Replace
// is safe to call while the page is navigating (the browser ignores it), so
// submit/redirect flows are unaffected.
function clearUrlParams(names) {
  if (!window.history || !window.history.replaceState || !window.URLSearchParams) {
    return;
  }
  var url = new URL(window.location.href);
  var changed = false;
  names.forEach(function (name) {
    if (url.searchParams.has(name)) {
      url.searchParams.delete(name);
      changed = true;
    }
  });
  if (changed) {
    window.history.replaceState(null, '', url.pathname + url.search + url.hash);
  }
}

(function () {
  var dlg = document.getElementById('inbox-edit-dialog');
  if (!dlg) {
    return;
  }
  // splitBytes picks the largest binary unit that divides a byte count evenly,
  // so a cap entered as "50 MB" round-trips as "50" + MB rather than 51200 KB.
  function splitBytes(n) {
    var units = [['tb', 1099511627776], ['gb', 1073741824], ['mb', 1048576], ['kb', 1024], ['b', 1]];
    for (var i = 0; i < units.length; i++) {
      if (n % units[i][1] === 0) {
        return { value: n / units[i][1], unit: units[i][0] };
      }
    }
    return { value: n, unit: 'b' };
  }
  var form = document.getElementById('inbox-edit-form');
  var deleteBtn = document.getElementById('inbox-edit-delete');
  var address = document.getElementById('inbox-edit-address');
  var display = form.querySelector('[name=display]');
  // Approvals tab: the authoring mode selector, saved through the authoring
  // endpoint when the dialog is saved.
  var authoringMode = document.getElementById('inbox-authoring-mode');
  var authDefaultMode = 'local_approval';
  // The current inbox is standalone (remote) or not. A domain inbox is preset to
  // Mammoth Mail approval, so the mode selector is hidden rather than offered.
  var authStandalone = false;
  function syncAuthoringControls() {
    if (!authStandalone) {
      // Domain inbox: preset to Mammoth Mail approval. Only the approver field
      // remains; the selector is hidden.
      var approverInputD = document.getElementById('inbox-edit-approver-email');
      var approverNoteD = document.getElementById('inbox-approver-note');
      if (approverInputD) {
        approverInputD.disabled = false;
      }
      if (approverNoteD) {
        approverNoteD.hidden = false;
      }
      var descD = document.getElementById('inbox-authoring-desc');
      if (descD) {
        descD.textContent = '';
      }
      var controls = document.getElementById('inbox-authoring-controls');
      if (controls) {
        controls.hidden = true;
      }
      return;
    }
    // Effective mode: the explicit selection, else the kind default.
    var effective = (authoringMode && authoringMode.value) ? authoringMode.value : authDefaultMode;
    var approval = effective === 'local_approval';
    var desc = document.getElementById('inbox-authoring-desc');
    if (desc) {
      desc.textContent = approval
        ? 'Mammoth Mail approval keeps the draft in Mammoth Mail for an in-app decision (and, if an approver is set, a tokenized email).'
        : 'When a client with Assistant permission requests to send, the draft will be placed in the connected mailbox\'s Drafts folder.';
    }
    var approverInput = document.getElementById('inbox-edit-approver-email');
    var approverNote = document.getElementById('inbox-approver-note');
    var approverFields = document.getElementById('inbox-approver-fields');
    if (approverFields) {
      approverFields.hidden = !approval;
    }
    if (approverInput) {
      approverInput.disabled = !approval;
      // A remote-draft inbox has no approver: clear the field so a stale value is
      // never submitted (and the server clears any stored one on save).
      if (!approval) {
        approverInput.value = '';
      }
    }
    if (approverNote) {
      approverNote.hidden = !approval;
    }
  }
  function applyAuthoring(json) {
    var data = {};
    try {
      data = JSON.parse(json) || {};
    } catch (e) {
      data = {};
    }
    if (data.default_mode) {
      authDefaultMode = data.default_mode;
    }
    authStandalone = !!data.standalone;
    var desc = document.getElementById('inbox-authoring-desc');
    var controls = document.getElementById('inbox-authoring-controls');
    if (authoringMode) {
      // Display the effective mode for existing inboxes with no explicit override.
      authoringMode.value = data.mode || data.default_mode || authDefaultMode;
    }
    if (desc) {
      desc.textContent = authStandalone
        ? 'How this inbox handles a request to send. Mammoth Mail approval keeps the draft in Mammoth Mail for an in-app decision (and, if an approver is set, a tokenized email). Create draft places the draft in the connected mailbox\'s Drafts folder and never sends it.'
        : '';
    }
    if (controls) {
      // The mode selector exists only for a standalone inbox; a domain inbox is
      // preset to Mammoth Mail approval.
      controls.hidden = !authStandalone;
    }
    // The tab is "Approvals" for a standalone inbox; the approver field is
    // enabled by the EFFECTIVE mode, so switching to create-draft disables (and
    // clears) the approver and a create-draft job never shows approval controls.
    var tab = document.querySelector('[data-inbox-tab="approver"]');
    if (tab) {
      tab.textContent = 'Assistant Approver';
    }
    syncAuthoringControls();
  }
  if (authoringMode) {
    authoringMode.addEventListener('change', syncAuthoringControls);
  }
  var usage = document.getElementById('inbox-edit-usage');
  var trashOverride = document.getElementById('inbox-trash-retention-override');
  var trashSection = document.getElementById('inbox-trash-retention-section');
  var trashDays = document.getElementById('inbox-trash-retention-days');
  var quotaValue = document.getElementById('inbox-edit-quota-value');
  var quotaUnit = document.getElementById('inbox-edit-quota-unit');
  var quotaUnlimited = document.getElementById('inbox-edit-quota-unlimited');
  if (quotaUnlimited) {
    quotaUnlimited.addEventListener('change', function () {
      if (quotaValue) {
        quotaValue.disabled = quotaUnlimited.checked;
      }
      if (quotaUnit) {
        quotaUnit.disabled = quotaUnlimited.checked;
      }
    });
  }
  if (trashOverride && trashSection) {
    trashOverride.addEventListener('change', function () {
      trashSection.hidden = !trashOverride.checked;
    });
  }
  var autoMarkRead = document.getElementById('inbox-auto-mark-read');
  var autoTrash = document.getElementById('inbox-auto-trash');
  var autoTrashSection = document.getElementById('inbox-auto-trash-section');
  var autoTrashHours = document.getElementById('inbox-auto-trash-hours');
  var deliveryTrigger = document.getElementById('inbox-delivery-trigger');
  if (autoTrash && autoTrashSection) {
    autoTrash.addEventListener('change', function () {
      autoTrashSection.hidden = !autoTrash.checked;
    });
  }
  // The Connectors panel keeps its delivery-action controls in this form.
  var connectorsForm = document.getElementById('inbox-connectors-form');
  var connectorList = document.getElementById('inbox-connectors-list');
  var connectorEditor = document.getElementById('inbox-connector-editor');
  var connectorAdd = document.getElementById('inbox-connector-add');
  var editConnectors = [];
  var csrfValue = (form.querySelector('[name=_csrf]') || {}).value || '';
  var editor = initSenderEditor({
    list: document.getElementById('inbox-sender-list'),
    input: document.getElementById('inbox-sender-input'),
    note: document.getElementById('inbox-sender-note'),
    approverEmail: document.getElementById('inbox-edit-approver-email'),
    restrict: document.getElementById('inbox-sender-restricted'),
    requireAuth: document.getElementById('inbox-require-auth'),
    section: document.getElementById('inbox-sender-section'),
    mxSection: document.getElementById('inbox-edit-require-auth-section'),
    addBtn: document.getElementById('inbox-sender-add')
  });
  // The inbox settings shell owns section navigation and sub-views; see
  // bindInboxSettingsShell.
  var inboxSubmitting = false;
  var inboxSaveButton = document.getElementById('inbox-edit-save');
  // A genuine dismissal drops the URL params that reopened the dialog. A submit
  // navigates, so the browser ignores the replace and the server redirect takes
  // over the return path.
  dlg.addEventListener('close', function () {
    if (inboxSubmitting) {
      return;
    }
    clearUrlParams(['inbox', 'inbox_tab', 'alias', 'provider']);
  });
  form.addEventListener('submit', function () {
    inboxSubmitting = true;
  });
  if (connectorsForm) {
    connectorsForm.addEventListener('submit', function () {
      inboxSubmitting = true;
    });
  }
  // On the Connectors panel the footer Save posts the auto-actions form, which
  // is separate from the inbox identity form.
  // The footer Save commits every tab's staged edits in one POST. The inbox
  // identity/allow/approver/quota/aliases fields live in the main form; the
  // delivery auto-action controls (Connectors tab) live in a separate form, so
  // their current values are mirrored onto the main form at submit time. Staged
  // Clients & Access role changes are already attached to the main form.
  function syncAutoActionsToMainForm() {
    if (!form || !connectorsForm) {
      return;
    }
    form.querySelectorAll('input[data-auto-actions]').forEach(function (el) {
      el.remove();
    });
    connectorsForm.querySelectorAll('input[name], select[name]').forEach(function (el) {
      if (!el.name || el.disabled) {
        return;
      }
      var hidden = document.createElement('input');
      hidden.type = 'hidden';
      hidden.setAttribute('data-auto-actions', '1');
      hidden.name = el.name;
      if (el.type === 'checkbox') {
        if (el.checked) {
          hidden.value = el.value || '1';
        } else {
          return;
        }
      } else {
        hidden.value = el.value;
      }
      form.appendChild(hidden);
    });
  }
  if (inboxSaveButton) {
    inboxSaveButton.addEventListener('click', function (event) {
      // The connector editor is a sub-view with its own form and its own Save
      // target: leave those alone.
      var inConnectorEditor = dlg.classList.contains('inbox-settings--subview') &&
        dlg.querySelector('.inbox-subview.active[data-inbox-subview=connectors]');
      if (inConnectorEditor) {
        return;
      }
      event.preventDefault();
      inboxSubmitting = true;
      syncAutoActionsToMainForm();
      syncStagedKeyRoles();
      form.requestSubmit();
    });
  }
  var editAliasList = document.getElementById('inbox-alias-list');
  var editExternalList = document.getElementById('inbox-external-alias-list');
  var editInboxID = '';
  var editPrimary = '';
  var editPrimaryName = '';
  var editDefault = document.getElementById('inbox-default-sender');
  var editDesired = '';
  // dashboardGroupNames backs the Identity tab's Dashboard group combobox: the
  // distinct group names already used anywhere in the user's dashboard.
  var dashboardGroupNames = [];
  // editExternal holds the secret-free external aliases for the open inbox, so
  // the default-sender select can include them and preserve an external default.
  var editExternal = [];
  function editSenderOptions() {
    var names = aliasNameByAddress(editAliasList);
    var addrs = currentAliasValues(editAliasList);
    editExternal.forEach(function (a) {
      addrs.push(a.address);
      if (a.display_name) {
        names[a.address] = a.display_name;
      }
    });
    return { names: names, addresses: addrs };
  }
  function refreshEditSender() {
    var opts = editSenderOptions();
    var desired = editDesired || (editDefault ? editDefault.value : '');
    buildDefaultSenderSelect(editDefault, editPrimaryName, editPrimary, opts.names, opts.addresses, desired);
  }
  var aliasEditor = initAliasEditor({
    list: editAliasList,
    addBtn: document.getElementById('inbox-alias-add'),
    editor: document.getElementById('inbox-alias-editor'),
    form: {
      name: document.getElementById('inbox-alias-editor-name'),
      local: document.getElementById('inbox-alias-editor-local'),
      domain: document.getElementById('inbox-alias-editor-domain'),
      error: document.getElementById('inbox-alias-editor-error'),
      save: document.getElementById('inbox-alias-editor-save'),
      cancel: document.getElementById('inbox-alias-editor-cancel')
    },
    onChange: refreshEditSender
  });

  function escapeConnector(value) {
    return String(value == null ? '' : value).replace(/[&<>"']/g, function (ch) {
      return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch];
    });
  }

  // loadDashboardGroup fills the Identity tab's Dashboard group field from the
  // signed-in user's dashboard preferences (a personal display preference, not
  // inbox policy). A group is read from groups.inboxes[id]. The same payload
  // feeds the field's dropdown so it offers every existing group name.
  function loadDashboardGroup(inboxID) {
    var field = document.getElementById('inbox-dashboard-group');
    var input = document.getElementById('inbox-dashboard-group-input');
    if (!field || !input) {
      return;
    }
    field.hidden = false;
    input.value = '';
    dashboardGroupNames = [];
    if (!input._mammothmailCombo) {
      window.mammothmailCombo.wire(input, { options: function () { return dashboardGroupNames; } });
    }
    fetch('/ui/dashboard/preferences', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) { if (!r.ok) { throw new Error('preferences unavailable'); } return r.json(); })
      .then(function (prefs) {
        var groups = (prefs && prefs.groups && prefs.groups.inboxes) || {};
        input.value = groups[inboxID] || '';
        var seen = Object.create(null);
        dashboardGroupNames = Object.keys(groups).map(function (key) { return groups[key]; }).filter(function (name) {
          if (!name || seen[name]) {
            return false;
          }
          seen[name] = true;
          return true;
        });
        if (input._mammothmailCombo) {
          input._mammothmailCombo.setOptions();
        }
      })
      .catch(function () {
        // Preferences are display-only here: a failure leaves the field empty and
        // the save simply writes whatever the admin typed.
        input.value = '';
        dashboardGroupNames = [];
      });
  }

  // populateRemoteTabs fills the Identity tab's collapsible IMAP/SMTP sections
  // from the inbox row's embedded secret-free remote config, and shows or hides
  // them by inbox kind. Passwords are never shown: the fields stay blank and
  // only the placeholder reports whether one is stored.
  function populateRemoteTabs(btn) {
    var inboxID = btn.dataset.id || '';
    var standalone = (btn.dataset.kind || 'domain') === 'standalone';
    dlg.querySelectorAll('[data-standalone-only]').forEach(function (el) {
      el.hidden = !standalone;
    });
    var cfg = {};
    try {
      cfg = JSON.parse(btn.dataset.remote || '{}') || {};
    } catch (e) {
      cfg = {};
    }
    var host = document.getElementById('inbox-remote-host');
    var microsoft = standalone && cfg.provider === 'microsoft';
    var one = standalone && cfg.provider === 'one';
    var google = standalone && (cfg.provider === 'google' || microsoft || one);
    dlg.querySelectorAll('[data-standalone-only]').forEach(function (el) {
      el.hidden = !standalone || google;
      el.querySelectorAll('input, select').forEach(function (field) { field.disabled = google; });
    });
    // The sync schedule applies to every standalone inbox, including a
    // provider-connected one: a Microsoft delegated shared mailbox is poll-only,
    // because Graph has no change subscriptions for it. It sits outside the IMAP
    // connector block above so the provider hide rule leaves it alone.
    var schedule = document.getElementById('inbox-sync-schedule');
    if (schedule) {
      schedule.hidden = !standalone;
      schedule.querySelectorAll('input').forEach(function (field) { field.disabled = !standalone; });
    }
    var scheduleNote = document.getElementById('inbox-schedule-note');
    if (scheduleNote) {
      if (cfg.capabilities && cfg.capabilities.realtime) {
        if (microsoft) {
          scheduleNote.textContent = 'Microsoft supports instant push and it is active for this mailbox, so new mail arrives automatically; the poll below is only a safety net.';
        } else {
          scheduleNote.textContent = 'Instant push is active for this mailbox, so new mail arrives on its own; the poll below is only a safety net.';
        }
      } else if (one) {
        scheduleNote.textContent = 'One does not support instant push, so new mail arrives on the poll schedule below.';
      } else if (microsoft) {
        scheduleNote.textContent = 'This Microsoft mailbox has no instant push active, so new mail arrives on the poll schedule below.';
      } else if (google) {
        scheduleNote.textContent = 'Gmail does not support instant push, so new mail arrives on the poll schedule below.';
      } else {
        scheduleNote.textContent = 'New mail is pushed instantly when the server supports IDLE; these intervals govern the fallback poll and the periodic full sync.';
      }
    }
    var googleLink = dlg.querySelector('[data-google-reconnect]');
    if (!googleLink) {
      // The reconnect control lives under an "Advanced" disclosure so it does not
      // read as a routine action. When the provider reports the account
      // disconnected, a red detail line sits inside the disclosure directly above
      // that single control, and the disclosure is pre-opened so both are
      // visible. One reconnects in place (an API-key dialog); Google and
      // Microsoft follow their OAuth wizard flow.
      var prompt = document.createElement('div');
      prompt.className = 'banner danger';
      prompt.setAttribute('data-reconnect-prompt', '');
      prompt.setAttribute('role', 'alert');
      prompt.hidden = true;
      googleLink = document.createElement('a');
      googleLink.setAttribute('data-google-reconnect', '');
      googleLink.className = 'btn secondary';
      var advanced = document.createElement('details');
      advanced.className = 'remote-fields';
      advanced.setAttribute('data-advanced-reconnect', '');
      var summary = document.createElement('summary');
      summary.textContent = 'Advanced';
      var body = document.createElement('div');
      body.className = 'remote-fields-body';
      // The red detail line precedes the reconnect button inside the disclosure.
      body.append(prompt, googleLink);
      advanced.append(summary, body);
      var panel = dlg.querySelector('[data-inbox-panel=basic]');
      panel.append(advanced);
    }
    var prompt = dlg.querySelector('[data-reconnect-prompt]');
    var advanced = dlg.querySelector('[data-advanced-reconnect]');
    advanced.hidden = !google;    googleLink.hidden = !google;
    googleLink.textContent = (cfg && cfg.connection && cfg.connection.action_label) ? cfg.connection.action_label : (one ? 'Reconnect with One' : (microsoft ? 'Reconnect Microsoft / Outlook' : 'Reconnect Google / Gmail'));
    googleLink.href = one ? '#' : '/ui/inboxes/standalone/new?' + (microsoft ? 'microsoft_inbox=' : 'google_inbox=') + encodeURIComponent(inboxID);
    // The One reconnect opens the in-place API-key dialog rather than leaving
    // for the full import wizard. The OAuth providers still follow their wizard.
    if (one) {
      googleLink.setAttribute('data-one-reconnect', '');
      googleLink.setAttribute('data-inbox', inboxID);
      googleLink.setAttribute('href', '#');
    } else {
      googleLink.removeAttribute('data-one-reconnect');
      googleLink.removeAttribute('data-inbox');
    }
    if (prompt) {
      var conn = (cfg && cfg.connection) || null;
      var bad = !!(conn && conn.state === 'disconnected');
      prompt.hidden = !(google && bad);
      prompt.textContent = (conn && conn.detail) ? conn.detail : 'This account is disconnected. Reconnect to resume syncing.';
      // A disconnected provider pre-opens the disclosure so the red line and the
      // single reconnect control are visible without a click.
      if (bad && advanced) { advanced.open = true; }
    }
    var port = document.getElementById('inbox-remote-port');
    var username = document.getElementById('inbox-remote-username');
    var security = document.getElementById('inbox-remote-security');
    var namespace = document.getElementById('inbox-remote-namespace');
    var imapPw = document.getElementById('inbox-remote-imap-pw');
    var imapPwNote = document.getElementById('inbox-remote-imap-pw-note');
    var smtpHost = document.getElementById('inbox-remote-smtp-host');
    var smtpPort = document.getElementById('inbox-remote-smtp-port');
    var smtpUsername = document.getElementById('inbox-remote-smtp-username');
    var smtpSecurity = document.getElementById('inbox-remote-smtp-security');
    var smtpPw = document.getElementById('inbox-remote-smtp-pw');
    var smtpPwNote = document.getElementById('inbox-remote-smtp-pw-note');
    [imapPwNote, smtpPwNote].forEach(function (note) {
      if (note) note.textContent = '(leave blank only if host, port, username and security are unchanged)';
    });
    var status = document.getElementById('inbox-remote-configured');
    var settingsLink = document.getElementById('inbox-remote-settings-link');
    if (host) host.value = cfg.host || '';
    if (port) port.value = cfg.port || '';
    if (username) username.value = cfg.username || '';
    if (security) security.value = cfg.security || 'tls';
    if (namespace) namespace.value = cfg.namespace || '';
    if (imapPw) {
      imapPw.value = '';
      imapPw.placeholder = cfg.imap_password_set ? 'unchanged' : 'app password';
    }
    if (imapPwNote) {
      imapPwNote.hidden = !cfg.imap_password_set;
    }
    if (smtpHost) smtpHost.value = cfg.smtp_host || '';
    if (smtpPort) smtpPort.value = cfg.smtp_port || '';
    if (smtpUsername) smtpUsername.value = cfg.smtp_username || '';
    if (smtpSecurity) smtpSecurity.value = cfg.smtp_security || 'tls';
    if (smtpPw) {
      smtpPw.value = '';
      smtpPw.placeholder = cfg.smtp_password_set ? 'unchanged' : 'app password';
    }
    if (smtpPwNote) {
      smtpPwNote.hidden = !cfg.smtp_password_set;
    }
    if (status) {
      status.textContent = cfg.configured ? 'Connected — ' + (cfg.host || '') : 'No connector configured.';
    }
    if (settingsLink) {
      settingsLink.setAttribute('href', '/ui/inboxes/' + encodeURIComponent(inboxID) + '/remote');
    }
    var pollMinutes = document.getElementById('inbox-remote-poll-minutes');
    var fullSync = document.getElementById('inbox-remote-full-sync-minutes');
    // The form states the poll in whole minutes; the API reports it in seconds.
    // A stored 0 means "no quick poll" and must render as 0, not blank (blank
    // would silently revert to the inherited default on the next save).
    if (pollMinutes) {
      if (typeof cfg.poll_seconds === 'number' && cfg.poll_seconds === 0) {
        pollMinutes.value = '0';
      } else {
        pollMinutes.value = cfg.poll_seconds ? Math.round(cfg.poll_seconds / 60) : '';
      }
      pollMinutes.placeholder = '1';
    }
    if (fullSync) {
      fullSync.value = cfg.full_sync_minutes || '';
      fullSync.placeholder = '15';
    }
  }

  function connectorLabel(c) {
    if (c.Kind === 'hermes') {
      return 'Hermes Relay';
    }
    if (c.Kind === 'openclaw') {
      return 'OpenClaw';
    }
    return 'Webhook';
  }

  function setConnectorEditorMode(editing) {
    // The connector editor is an in-modal sub-view: the whole panels area
    // switches to it. While it is open the footer Save targets its own edit
    // form; the click handler leaves that case alone. Returning to the tab
    // restores the main form as the Save target.
    if (inboxSaveButton && connectorEditor) {
      inboxSaveButton.setAttribute('form', editing ? 'inbox-connector-edit-form' : 'inbox-edit-form');
    }
    if (editing) {
      showInboxSubview(dlg, 'connectors');
    }
    if (connectorEditor && !editing) {
      connectorEditor.textContent = '';
    }
  }

  function renderConnectorEditor(c) {
    if (!connectorEditor || !c) {
      return;
    }
    var id = encodeURIComponent(c.ID || '');
    var inbox = escapeConnector(editInboxID);
    var common = '<input type="hidden" name="_csrf" value="' + escapeConnector(csrfValue) + '"><input type="hidden" name="inbox" value="' + inbox + '">';
    var fields = '<label>Name</label><input name="name" maxlength="128" required value="' + escapeConnector(c.Name) + '">';
    var actions = '';
    if (c.Kind === 'hermes' || c.Kind === 'openclaw') {
      var isOpenClaw = c.Kind === 'openclaw';
      var label = isOpenClaw ? 'OpenClaw' : 'Hermes Relay';
      fields += '<p class="muted small">Lost the setup credentials? Delete this connector and add a new one. Connection history does not indicate whether the gateway is online now.</p>';
      fields += '<label>Outbound authority</label><select name="role"><option value="owner"' + (c.Role === 'owner' ? ' selected' : '') + '>Owner — ' + (isOpenClaw ? 'agent' : 'relay') + ' sends directly</option><option value="assistant"' + (c.Role === 'assistant' ? ' selected' : '') + '>Assistant — ' + (isOpenClaw ? 'agent' : 'relay') + ' drafts and requests approval</option></select>';
      connectorEditor.innerHTML = '<div class="card-head"><h3>' + escapeConnector(c.Name || label) + '</h3></div><form id="inbox-connector-edit-form" method="post" action="/ui/' + (isOpenClaw ? 'openclaw' : 'hermes') + '/' + id + '/edit">' + common + fields + '</form>';
    } else {
      fields += '<label>Destination URL</label><input name="url" type="url" required value="' + escapeConnector(c.URL) + '"><label>Payload</label><select name="mode"><option value="notify"' + (c.Mode === 'notify' ? ' selected' : '') + '>Notify — small JSON with the message id</option><option value="forward"' + (c.Mode === 'forward' ? ' selected' : '') + '>Forward — full raw MIME</option></select><label>Authentication</label><select name="auth" data-inline-webhook-auth><option value="signature"' + (c.AuthMode === 'signature' ? ' selected' : '') + '>Signature — signed HMAC-SHA256 header</option><option value="bearer"' + (c.AuthMode === 'bearer' ? ' selected' : '') + '>Bearer — static token</option></select><div data-inline-bearer' + (c.AuthMode === 'bearer' ? '' : ' hidden') + '><label for="inline-webhook-bearer">Bearer secret <span class="muted small">(leave blank to keep current)</span></label><div class="row"><input id="inline-webhook-bearer" name="bearer_secret" type="password" autocomplete="new-password"><button type="button" class="secondary btn-narrow" data-inline-generate-bearer>Generate</button></div><p class="muted small">Copy a generated replacement before saving. The secret changes only when you save.</p></div>';
      actions = c.AuthMode === 'signature' ? '<button type="button" class="secondary" data-rotate-webhook="' + escapeConnector(c.ID) + '">Rotate signing secret</button>' : '';
      var toggleText = c.Enabled ? 'Pause webhook' : 'Enable webhook';
      var nextEnabled = c.Enabled ? '0' : '1';
      connectorEditor.innerHTML = '<div class="card-head"><h3>' + escapeConnector(c.Name || 'Webhook') + '</h3></div><form id="inbox-connector-edit-form" method="post" action="/ui/webhooks/' + id + '/edit">' + common + fields + '<div class="dialog-actions">' + actions + '</div></form><div class="row"><form method="post" action="/ui/webhooks/' + id + '/toggle">' + common + '<input type="hidden" name="enabled" value="' + nextEnabled + '"><button class="secondary">' + toggleText + '</button></form></div>';
    }
    setConnectorEditorMode(true);
    var connectorEditForm = connectorEditor.querySelector('#inbox-connector-edit-form');
    if (connectorEditForm) {
      connectorEditForm.addEventListener('submit', function () {
        inboxSubmitting = true;
      });
    }
    var auth = connectorEditor.querySelector('[data-inline-webhook-auth]');
    if (auth) {
      function syncInlineBearer() {
        var bearer = connectorEditor.querySelector('[data-inline-bearer]');
        if (bearer) {
          bearer.hidden = auth.value !== 'bearer';
          var input = bearer.querySelector('input');
          if (input) {
            input.disabled = auth.value !== 'bearer';
          }
        }
      }
      auth.addEventListener('change', syncInlineBearer);
      syncInlineBearer();
      var generate = connectorEditor.querySelector('[data-inline-generate-bearer]');
      generate.addEventListener('click', function () {
        var input = connectorEditor.querySelector('[name=bearer_secret]');
        try {
          input.value = generateBearerSecret();
          input.type = 'text';
          input.focus();
        } catch (err) {
          window.alert('Could not generate a secret. Paste a secret from your other app.');
        }
      });
    }
    var rotate = connectorEditor.querySelector('[data-rotate-webhook]');
    if (rotate) {
      rotate.addEventListener('click', function () {
        if (!window.confirm('Rotate this webhook signing secret? The current secret stops working immediately.')) {
          return;
        }
        var body = new URLSearchParams();
        body.append('_csrf', csrfValue);
        fetch('/ui/webhooks/' + encodeURIComponent(rotate.getAttribute('data-rotate-webhook')) + '/rotate', {
          method: 'POST',
          headers: {Accept: 'application/json','Content-Type':'application/x-www-form-urlencoded;charset=UTF-8'},
          body: body.toString(),
          credentials: 'same-origin'
        }).then(function (res) {
          if (!res.ok) {
            return res.text().then(function (t) { throw new Error(t || 'Could not rotate secret'); });
          }
          return res.json();
        }).then(function (data) {
          window.alert((data.label || 'New signing secret') + '\n\n' + (data.secret || ''));
        }).catch(function (err) {
          window.alert(err.message || 'Could not rotate secret');
        });
      });
    }
  }

  function renderConnectors(connectors) {
    editConnectors = Array.isArray(connectors) ? connectors : [];
    if (!connectorList || !connectorEditor) {
      return;
    }
    connectorList.textContent = '';
    setConnectorEditorMode(false);

    var settingsSVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06-.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09A1.65 1.65 0 0 0 19.4 15z"/></svg>';
    var hermesSVG = '<img class="connector-brand-image" alt="" aria-hidden="true" src="/assets/hermes-connector.png">';
    var openclawSVG = '<img class="connector-brand-image connector-brand-openclaw" alt="" aria-hidden="true" src="/assets/openclaw-connector.png">';
    var webhookSVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 16.98h-5.99c-1.1 0-1.95.94-2.48 1.9A4 4 0 0 1 2 17c.01-.7.2-1.4.57-2"/><path d="m6 17 3.13-5.78c.53-.97.1-2.18-.5-3.1a4 4 0 1 1 6.89-4.06"/><path d="m12 6 3.13 5.73C15.66 12.7 16.9 13 18 13a4 4 0 0 1 0 8"/></svg>';
    var logSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M4 2.5h8v11H4z"/><path d="M6 5h4M6 8h4M6 11h3"/></svg>';
    var deleteSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg>';

    function iconButton(label, svg, extraClass) {
      var button = document.createElement('button');
      button.type = 'button';
      button.className = 'secondary icon-btn' + (extraClass ? ' ' + extraClass : '');
      button.title = label;
      button.setAttribute('aria-label', label);
      button.innerHTML = svg;
      return button;
    }

    if (!editConnectors.length) {
      var empty = document.createElement('p');
      empty.className = 'muted';
      empty.textContent = 'No connectors configured for this inbox.';
      connectorList.appendChild(empty);
    }

    editConnectors.forEach(function (c) {
      var row = document.createElement('div');
      row.className = 'connector-row';

      var kindIcon = document.createElement('span');
      kindIcon.className = 'connector-type-icon';
      kindIcon.title = connectorLabel(c);
      kindIcon.setAttribute('aria-hidden', 'true');
      kindIcon.innerHTML = c.Kind === 'hermes' ? hermesSVG : (c.Kind === 'openclaw' ? openclawSVG : webhookSVG);
      row.appendChild(kindIcon);

      var text = document.createElement('div');
      text.className = 'connector-row-main';
      var status = c.Kind === 'webhook' ? (c.Enabled ? 'Active' : 'Paused') : (c.Scope || 'Configured');
      if (c.Kind === 'hermes' || c.Kind === 'openclaw') {
        status += ' · ' + (c.LastConnectedAt ? 'Last connected: ' + new Date(c.LastConnectedAt).toLocaleString() : 'Never connected');
      }
      text.innerHTML = '<div class="connector-row-name">' + escapeConnector(c.Name || connectorLabel(c)) + '</div><div class="connector-row-meta">' + connectorLabel(c) + ' · ' + escapeConnector(status) + '</div>';
      row.appendChild(text);

      var settings = iconButton('Settings', settingsSVG, '');
      settings.addEventListener('click', function () {
        renderConnectorEditor(c);
      });
      row.appendChild(settings);

      var log = document.createElement('a');
      log.className = 'btn secondary icon-btn';
      log.href = '/ui/clients/' + encodeURIComponent(c.ID || '') + '/log';
      log.title = 'Delivery log';
      log.setAttribute('aria-label', 'Delivery log');
      log.innerHTML = logSVG;
      row.appendChild(log);

      var remove = iconButton('Delete', deleteSVG, 'danger open-delete-client');
      remove.setAttribute('data-kind', c.Kind === 'hermes' ? 'hermes' : (c.Kind === 'openclaw' ? 'openclaw' : 'webhooks'));
      remove.setAttribute('data-id', c.ID || '');
      remove.setAttribute('data-name', c.Name || '');
      remove.setAttribute('data-type', connectorLabel(c));
      remove.setAttribute('data-inbox', editInboxID);
      row.appendChild(remove);

      connectorList.appendChild(row);
    });

    var pendingTemplate = document.getElementById('pending-relay-setups');
    if (pendingTemplate) {
      var pending = document.createElement('section');
      pending.className = 'connector-list';
      pendingTemplate.content.querySelectorAll('[data-inbox]').forEach(function (source) {
        if (source.dataset.inbox !== editInboxID || Date.parse(source.dataset.expires) <= Date.now()) {
          return;
        }
        var row = source.cloneNode(true);
        row.querySelector('button').addEventListener('click', function (e) {
          if (!window.confirm('Cancel this setup? Its one-time code will stop working.')) {
            e.preventDefault();
          }
        });
        pending.appendChild(row);
      });
      if (pending.children.length) {
        var heading = document.createElement('h3');
        heading.className = 'section-head';
        heading.textContent = 'Pending setup codes';
        pending.prepend(heading);
        connectorList.appendChild(pending);
      }
    }
  }

  // ---- Clients & Access tab ----------------------------------------------
  // Per-inbox management of API keys, mailbox users and pending invitations.
  // Role changes, adds and removes post immediately (fetch) and then reload the
  // dashboard onto this dialog/tab so the lists reflect the durable state.
  var accessKeysList = document.getElementById('inbox-access-keys');
  var accessUsersList = document.getElementById('inbox-access-users');
  var accessInvitesList = document.getElementById('inbox-access-invites');
  var accessAddKey = document.getElementById('access-add-key');
  var accessAddUser = document.getElementById('access-add-user');
  var accessAddTitle = document.getElementById('access-add-title');
  var accessAddBody = document.getElementById('access-add-body');
  var editAccess = { keys: [], admin_keys: [], users: [], invites: [] };

  function accessEsc(s) {
    return String(s == null ? '' : s);
  }

  // accessAttr HTML-escapes a value for safe interpolation into innerHTML
  // (e.g. the option list built as a string).
  function accessAttr(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // accessReload returns to the server render of this inbox on the access tab.
  function accessReload(notice) {
    var dest = '/?inbox=' + encodeURIComponent(editInboxID) + '&inbox_tab=access';
    if (notice) {
      dest += '&notice=' + encodeURIComponent(notice);
    }
    window.location.assign(dest);
  }

  // accessPost submits a tiny form to a per-inbox access endpoint and reloads on
  // success, surfacing an error inline otherwise. The triggering control is
  // disabled while the request is in flight so a change reads as committed.
  function accessPost(path, fields, confirmText, control) {
    if (confirmText && !window.confirm(confirmText)) {
      return;
    }
    if (typeof window.fetch !== 'function') {
      return;
    }
    if (control) {
      control.disabled = true;
    }
    var body = new URLSearchParams();
    body.append('_csrf', csrfValue);
    (fields || []).forEach(function (f) {
      body.append(f[0], f[1]);
    });
    fetch(path, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) {
        return res.text().then(function (text) {
          throw new Error(text || 'Could not update access');
        });
      }
      accessReload('');
    }).catch(function (err) {
      if (control) {
        control.disabled = false;
      }
      var message = err.message || 'Could not update access';
      if (dlg && dlg.classList.contains('inbox-settings--subview') && accessAddBody) {
        accessAddError(message);
      } else {
        window.alert(message);
      }
    });
  }

  // accessRoleSeg renders the same None/Read/Assistant/Owner segmented control
  // used by the Clients card. Unlike the Clients card it stages the role change
  // in the DOM and commits it with the inbox settings Save (via a hidden
  // access_role_<keyID> input), so a role change does not reload the page.
  var stagedKeyRoles = {};
  function accessRoleSeg(keyID, role) {
    var wrap = document.createElement('div');
    wrap.className = 'seg access-role';
    [['', 'None'], ['read', 'Read'], ['assistant', 'Assistant'], ['owner', 'Owner']].forEach(function (pair) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = pair[1];
      var on = (pair[0] || '') === (role || '');
      btn.className = on ? 'active' : '';
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
      btn.addEventListener('click', function () {
        if (btn.disabled || btn.classList.contains('active')) {
          return;
        }
        wrap.querySelectorAll('button').forEach(function (b) {
          var active = b === btn;
          b.className = active ? 'active' : '';
          b.setAttribute('aria-pressed', active ? 'true' : 'false');
        });
        stagedKeyRoles[keyID] = pair[0] || '';
        syncStagedKeyRoles();
      });
      wrap.appendChild(btn);
    });
    return wrap;
  }

  // setRoleLocked pins a role ladder to Owner, or releases it. While the
  // domain-wide sending grant is on, Owner is the only role that may be held, so
  // the other rungs are disabled rather than silently ignored.
  function setRoleLocked(seg, locked) {
    if (!seg) {
      return;
    }
    seg.querySelectorAll('button').forEach(function (b) {
      var isOwner = b.textContent === 'Owner';
      var active = locked ? isOwner : b.classList.contains('active');
      b.className = active ? 'active' : '';
      b.setAttribute('aria-pressed', active ? 'true' : 'false');
      b.disabled = locked && !isOwner;
      b.title = locked ? 'Domain-wide sending requires Owner.' : '';
    });
  }

  // lockRoleSeg pins a ladder to Owner (or releases it), keeping the staged role
  // in step so the Save posts owner while the grant is on.
  function lockRoleSeg(seg, keyID, locked) {
    setRoleLocked(seg, locked);
    if (locked) {
      stagedKeyRoles[keyID] = 'owner';
      syncStagedKeyRoles();
    }
  }

  // syncStagedKeyRoles mirrors the staged role changes onto hidden inputs inside
  // the inbox edit form, so the footer Save submits them with everything else.
  function syncStagedKeyRoles() {
    if (!form) {
      return;
    }
    form.querySelectorAll('input[data-access-role]').forEach(function (el) {
      el.remove();
    });
    Object.keys(stagedKeyRoles).forEach(function (keyID) {
      var input = document.createElement('input');
      input.type = 'hidden';
      input.setAttribute('data-access-role', '1');
      input.name = 'access_role_' + keyID;
      input.value = stagedKeyRoles[keyID];
      form.appendChild(input);
    });
    form.querySelectorAll('input[data-access-domain-send]').forEach(function (el) {
      el.remove();
    });
    Object.keys(stagedKeyDomainSend).forEach(function (keyID) {
      var input = document.createElement('input');
      input.type = 'hidden';
      input.setAttribute('data-access-domain-send', '1');
      input.name = 'access_domain_send_' + keyID;
      input.value = stagedKeyDomainSend[keyID] ? '1' : '0';
      form.appendChild(input);
    });
  }

  // stagedKeyDomainSend holds the staged "Domain-wide sending" grant per key,
  // keyed by client id. It is only offered on a domain's catch-all inbox.
  var stagedKeyDomainSend = {};

  // accessDomainSendToggle renders the "Domain-wide sending" control as a single
  // segmented button matching the role ladder beside it. It is tied to Owner: on
  // it pins the ladder to Owner, and off it frees the ladder again, leaving the
  // role at whatever it was (the system never demotes on its own).
  function accessDomainSendToggle(k, ladder) {
    var wrap = document.createElement('div');
    wrap.className = 'seg access-domain-send';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = 'Domain send';
    btn.title = 'Send as any address on the domain. Requires an enabled catch-all inbox on the domain. This locks the role to Owner.';
    var on = !!k.domain_send;
    btn.className = on ? 'active' : '';
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    stagedKeyDomainSend[k.id] = on;
    btn.addEventListener('click', function () {
      var next = !btn.classList.contains('active');
      btn.className = next ? 'active' : '';
      btn.setAttribute('aria-pressed', next ? 'true' : 'false');
      stagedKeyDomainSend[k.id] = next;
      // Domain-wide sending and Owner are tied: turning the grant on pins the
      // ladder to Owner, and releasing it frees the ladder again. The role is
      // left as owner on release (the system never demotes on its own).
      lockRoleSeg(ladder, k.id, next);
    });
    wrap.appendChild(btn);
    return wrap;
  }

  function accessKeyRow(k, catchAll) {
    var row = document.createElement('div');
    row.className = 'access-row';
    var ladder;
    if (!k.admin) {
      // Build the ladder first so the Domain send control beside it can drive it.
      ladder = accessRoleSeg(k.id, k.role);
    }
    var main = document.createElement('div');
    main.className = 'access-main';
    var name = document.createElement('div');
    name.className = 'access-name';
    name.textContent = accessEsc(k.name);
    var meta = document.createElement('div');
    meta.className = 'access-meta';
    meta.textContent = k.admin ? 'Account Admin key · implicit Owner on every inbox' : 'API key';
    main.appendChild(name);
    main.appendChild(meta);
    row.appendChild(main);
    if (k.admin) {
      var pill = document.createElement('span');
      pill.className = 'pill';
      pill.textContent = 'Admin';
      row.appendChild(pill);
      return row;
    }
    // The Domain send control sits immediately left of the role ladder, as a
    // permission rather than a label for the key. A row that cannot hold the
    // grant keeps a spacer of equal width so every ladder stays aligned.
    if (catchAll) {
      row.appendChild(accessDomainSendToggle(k, ladder));
    } else {
      var spacer = document.createElement('div');
      spacer.className = 'access-domain-send access-domain-send-spacer';
      row.appendChild(spacer);
    }
    row.appendChild(ladder);
    // A live grant pins the ladder to Owner from the first render.
    if (catchAll && k.domain_send) {
      lockRoleSeg(ladder, k.id, true);
    }
    var remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'secondary btn-sm danger access-remove';
    remove.textContent = 'Remove';
    remove.title = 'Remove this inbox from the client. The key keeps its other mailbox access.';
    remove.addEventListener('click', function () {
      accessPost('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/keys/' + encodeURIComponent(k.id) + '/remove', [],
        'Remove this inbox from ' + k.name + '? The key keeps its other mailbox access.', remove);
    });
    row.appendChild(remove);
    return row;
  }

  function accessUserRow(u) {
    var row = document.createElement('div');
    row.className = 'access-row';
    var main = document.createElement('div');
    main.className = 'access-main';
    var name = document.createElement('div');
    name.className = 'access-name';
    name.textContent = accessEsc(u.email);
    var meta = document.createElement('div');
    meta.className = 'access-meta';
    meta.textContent = 'Mailbox user';
    main.appendChild(name);
    main.appendChild(meta);
    row.appendChild(main);
    var pill = document.createElement('span');
    pill.className = 'pill';
    pill.textContent = 'Owner';
    row.appendChild(pill);
    var remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'secondary btn-sm danger access-remove';
    remove.textContent = 'Remove';
    remove.addEventListener('click', function () {
      accessPost('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/users/' + encodeURIComponent(u.id) + '/remove', [],
        'Remove ' + u.email + ' from this inbox?', remove);
    });
    row.appendChild(remove);
    return row;
  }

  function accessInviteRow(inv) {
    var row = document.createElement('div');
    row.className = 'access-row';
    var main = document.createElement('div');
    main.className = 'access-main';
    var name = document.createElement('div');
    name.className = 'access-name';
    name.textContent = accessEsc(inv.email);
    var meta = document.createElement('div');
    meta.className = 'access-meta';
    meta.textContent = 'Invitation pending';
    main.appendChild(name);
    main.appendChild(meta);
    row.appendChild(main);
    var revoke = document.createElement('button');
    revoke.type = 'button';
    revoke.className = 'secondary btn-sm danger access-remove';
    revoke.textContent = 'Revoke';
    revoke.addEventListener('click', function () {
      accessPost('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/invites/' + encodeURIComponent(inv.id) + '/revoke', [],
        'Revoke the invitation for ' + inv.email + '?');
    });
    row.appendChild(revoke);
    return row;
  }

  function accessEmpty(text) {
    var p = document.createElement('p');
    p.className = 'access-empty muted';
    p.textContent = text;
    return p;
  }

  function renderAccess(grant) {
    editAccess = grant || { keys: [], admin_keys: [], users: [], invites: [] };
    var keys = (editAccess.keys || []).concat(editAccess.admin_keys || []);
    if (accessKeysList) {
      accessKeysList.textContent = '';
      if (!keys.length) {
        accessKeysList.appendChild(accessEmpty('No clients have access to this inbox.'));
      } else {
        keys.forEach(function (k) {
          accessKeysList.appendChild(accessKeyRow(k, !!editAccess.catch_all));
        });
      }
    }
    if (accessUsersList) {
      accessUsersList.textContent = '';
      var users = editAccess.users || [];
      if (!users.length) {
        accessUsersList.appendChild(accessEmpty('No mailbox users have access to this inbox.'));
      } else {
        users.forEach(function (u) {
          accessUsersList.appendChild(accessUserRow(u));
        });
      }
    }
    if (accessInvitesList) {
      accessInvitesList.textContent = '';
      var invites = editAccess.invites || [];
      if (!invites.length) {
        accessInvitesList.appendChild(accessEmpty('No pending invitations.'));
      } else {
        invites.forEach(function (inv) {
          accessInvitesList.appendChild(accessInviteRow(inv));
        });
      }
    }
  }

  // available account users = members without a role on this inbox already.
  function accessAvailableUsers() {
    return (window.__accessMembers || []).filter(function (m) {
      return !(editAccess.users || []).some(function (u) {
        return u.id === m.id;
      });
    });
  }

  function openAccessAdd(mode) {
    if (!accessAddBody) {
      return;
    }
    accessAddBody.textContent = '';
    if (mode === 'key') {
      if (accessAddTitle) {
        accessAddTitle.textContent = 'Add client';
      }
      var keyForm = document.createElement('div');
      keyForm.innerHTML = '<label>Name</label><input id="access-key-name" placeholder="API key" maxlength="80"><label>Role on this inbox</label>' +
        '<select id="access-key-role"><option value="read">Read</option><option value="assistant">Assistant</option><option value="owner" selected>Owner</option></select>' +
        '<p class="muted small">The key is created with access to this inbox only. Copy its secret when it is shown.</p>' +
        '<div class="dialog-actions"><button type="button" class="secondary" id="access-key-cancel">Cancel</button><button type="button" id="access-key-save">Create client</button></div>';
      accessAddBody.appendChild(keyForm);
      accessAddBody.querySelector('#access-key-cancel').addEventListener('click', function () {
        hideInboxSubview(dlg);
      });
      accessAddBody.querySelector('#access-key-save').addEventListener('click', function () {
        var name = (accessAddBody.querySelector('#access-key-name').value || '').trim();
        var role = accessAddBody.querySelector('#access-key-role').value;
        accessCreateKey(name, role);
      });
    } else {
      if (accessAddTitle) {
        accessAddTitle.textContent = 'Add mailbox user';
      }
      if (!window.__accessMembers || !window.__accessMembers.length) {
        accessAddBody.appendChild(accessEmpty('Everyone in this account already has access, or there is nobody else to add. Invite a new person by email below.'));
      }
      var wrap = document.createElement('div');
      if (window.__accessMembers && window.__accessMembers.length) {
        var opts = accessAvailableUsers().map(function (m) {
          return '<option value="' + accessAttr(m.id) + '">' + accessAttr(m.email) + '</option>';
        }).join('');
        wrap.innerHTML = '<label>Existing account member</label><div class="row"><select id="access-user-select">' + opts + '</select><button type="button" class="secondary btn-narrow" id="access-user-save">Add</button></div>' +
          '<p class="muted small">They become Owner of this inbox.</p>';
      }
      wrap.innerHTML += '<h3 class="section-head">Invite a new person</h3><label>Email</label><input type="email" id="access-invite-email" placeholder="person@example.com">' +
        '<p class="muted small">They set their own password from a single-use link and become Owner of this inbox.</p>' +
        '<div class="dialog-actions"><button type="button" class="secondary" id="access-user-cancel">Cancel</button><button type="button" id="access-invite-save">Send invitation</button></div>';
      accessAddBody.appendChild(wrap);
      accessAddBody.querySelector('#access-user-cancel').addEventListener('click', function () {
        hideInboxSubview(dlg);
      });
      var userSave = accessAddBody.querySelector('#access-user-save');
      if (userSave) {
        userSave.addEventListener('click', function () {
          var sel = accessAddBody.querySelector('#access-user-select');
          if (sel && sel.value) {
            accessPost('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/users', [['user', sel.value]]);
          }
        });
      }
      accessAddBody.querySelector('#access-invite-save').addEventListener('click', function () {
        var email = (accessAddBody.querySelector('#access-invite-email').value || '').trim();
        if (!email) {
          accessAddError('Enter an email address to invite.');
          return;
        }
        accessPost('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/invites', [['email', email]]);
      });
    }
    showInboxSubview(dlg, 'access-add');
  }

  // accessCreateKey posts the new-key form; on success the returned secret is
  // shown in the shared key dialog's result view (matching the Clients card),
  // then Done returns to this inbox's Clients & Access tab.
  function accessCreateKey(name, role) {
    if (typeof window.fetch !== 'function') {
      return;
    }
    if (!name) {
      name = 'API key';
    }
    var body = new URLSearchParams();
    body.append('_csrf', csrfValue);
    body.append('name', name);
    body.append('role', role);
    fetch('/ui/inboxes/' + encodeURIComponent(editInboxID) + '/access/keys', {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) {
        return res.text().then(function (text) {
          throw new Error(text || 'Could not create client');
        });
      }
      return res.json();
    }).then(function (data) {
      if (typeof window.mammothmailShowKeyResult === 'function') {
        window.mammothmailShowKeyResult(data, function () {
          window.location.href = '/?inbox=' + encodeURIComponent(editInboxID) + '&inbox_tab=access';
        });
      } else {
        window.location.href = '/?inbox=' + encodeURIComponent(editInboxID) + '&inbox_tab=access';
      }
    }).catch(function (err) {
      accessAddError(err.message || 'Could not create client');
    });
  }

  // accessAddError shows an inline error in the add sub-view rather than a
  // browser alert.
  function accessAddError(message) {
    if (!accessAddBody) {
      return;
    }
    var box = accessAddBody.querySelector('.access-add-error');
    if (!box) {
      box = document.createElement('div');
      box.className = 'error access-add-error';
      accessAddBody.insertBefore(box, accessAddBody.firstChild);
    }
    box.textContent = message;
    box.hidden = false;
  }

  if (accessAddKey) {
    accessAddKey.addEventListener('click', function () {
      openAccessAdd('key');
    });
  }
  if (accessAddUser) {
    accessAddUser.addEventListener('click', function () {
      openAccessAdd('user');
    });
  }

  document.querySelectorAll('.edit-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = encodeURIComponent(btn.dataset.id || '');
      editInboxID = btn.dataset.id || '';
      form.action = '/ui/inboxes/' + id + '/edit';
      if (connectorsForm) {
        connectorsForm.action = '/ui/inboxes/' + id + '/auto-actions';
      }
      if (deleteBtn) {
        deleteBtn.dataset.id = btn.dataset.id || '';
        deleteBtn.dataset.address = btn.dataset.address || '';
      }
      display.value = btn.dataset.name || '';
      address.value = btn.dataset.address || '';
      dlg._inboxKind = btn.dataset.kind || 'domain';
      populateRemoteTabs(btn);
      loadDashboardGroup(btn.dataset.id || '');
      editor.setApprover(btn.dataset.approverEmail || '');
      editor.setRestricted(btn.dataset.restricted === '1');
      editor.setRequireAuth(btn.dataset.requireAuth === '1');
      editor.setMX(btn.dataset.mx === '1');
      editor.setSenders(btn.dataset.allowed || '', btn.dataset.approverEmail || '');
      editor.clearInput();
      editPrimary = btn.dataset.address || '';
      editPrimaryName = btn.dataset.name || '';
      editDesired = btn.dataset.defaultSender || '';
      aliasEditor.setAliases(btn.dataset.aliases || '', btn.dataset.aliasNames || '');
      try {
        editExternal = JSON.parse(btn.dataset.externalAliases || '[]') || [];
      } catch (e) {
        editExternal = [];
      }
      renderExternalAliases(editExternalList, btn.dataset.id || '', btn.dataset.externalAliases || '[]');
      try {
        editConnectors = JSON.parse(btn.dataset.connectors || '[]') || [];
      } catch (e) {
        editConnectors = [];
      }
      try {
        renderAccess(JSON.parse(btn.dataset.access || '{}') || {});
      } catch (e) {
        renderAccess({ keys: [], admin_keys: [], users: [], invites: [] });
      }
      // A freshly opened inbox starts with no staged access changes.
      stagedKeyRoles = {};
      syncStagedKeyRoles();
      applyAuthoring(btn.dataset.authoring || '{}');
      if (connectorAdd) {
        connectorAdd.setAttribute('data-inbox', editInboxID);
      }
      renderConnectors(editConnectors);
      var opts = editSenderOptions();
      buildDefaultSenderSelect(editDefault, editPrimaryName, editPrimary, opts.names, opts.addresses, editDesired);
      editDesired = '';
      var extForm = document.getElementById('external-alias-form');
      if (extForm) {
        extForm.action = '/ui/inboxes/' + id + '/external-aliases';
      }
      if (usage) {
        usage.textContent = btn.dataset.usage || '—';
      }
      if (trashOverride) {
        var trashVal = btn.dataset.trashRetention || '';
        trashOverride.checked = trashVal !== '';
        if (trashSection) {
          trashSection.hidden = !trashOverride.checked;
        }
        if (trashDays) {
          trashDays.value = trashVal !== '' ? trashVal : '0';
        }
      }
      if (quotaValue) {
        var quotaBytes = parseInt(btn.dataset.storageQuota || '', 10);
        var hasQuota = btn.dataset.storageQuota !== '' && !isNaN(quotaBytes) && quotaBytes > 0;
        if (quotaUnlimited) {
          quotaUnlimited.checked = !hasQuota;
          quotaValue.disabled = !hasQuota;
          if (quotaUnit) {
            quotaUnit.disabled = !hasQuota;
          }
        }
        if (hasQuota) {
          var q = splitBytes(quotaBytes);
          quotaValue.value = q.value;
          if (quotaUnit) {
            quotaUnit.value = q.unit;
          }
        } else {
          quotaValue.value = '';
          if (quotaUnit) {
            quotaUnit.value = 'mb';
          }
        }
      }
      if (autoMarkRead) {
        autoMarkRead.checked = btn.dataset.autoMarkRead === '1';
      }
      if (autoTrash) {
        var autoHours = btn.dataset.autoTrashHours || '';
        autoTrash.checked = autoHours !== '';
        if (autoTrashSection) {
          autoTrashSection.hidden = !autoTrash.checked;
        }
        if (autoTrashHours) {
          autoTrashHours.value = autoHours !== '' ? autoHours : '24';
        }
      }
      if (deliveryTrigger) {
        // Anything but a stored "any" (including an absent value) opens on the
        // default trigger.
        deliveryTrigger.value = btn.dataset.deliveryTrigger === 'any' ? 'any' : 'all';
      }
      if (dlg._resetTabs) {
        dlg._resetTabs();
      }
      dlg.showModal();
    });
  });

  document.querySelectorAll('.open-inbox-connector').forEach(function (connectorBtn) {
    connectorBtn.addEventListener('click', function () {
      var inboxID = connectorBtn.getAttribute('data-inbox') || '';
      var target = null;
      document.querySelectorAll('.edit-inbox').forEach(function (btn) {
        if (btn.dataset.id === inboxID) {
          target = btn;
        }
      });
      if (target) {
        target.click();
        var tab = dlg.querySelector('[data-inbox-tab=connectors]');
        if (tab) {
          tab.click();
        }
      }
    });
  });

  var extEditor = document.getElementById('inbox-external-alias-editor');
  var extAdd = document.getElementById('inbox-external-alias-add');
  var extSave = document.getElementById('external-alias-save');
  function resetExternalAliasEditor() {
    var err = document.getElementById('external-alias-error');
    if (err) {
      err.hidden = true;
      err.textContent = '';
    }
    var nameEl = document.getElementById('external-alias-name');
    var addrEl = document.getElementById('external-alias-address');
    if (nameEl) {
      nameEl.value = '';
    }
    if (addrEl) {
      addrEl.value = '';
      addrEl.disabled = false;
    }
    if (extSave) {
      extSave.textContent = 'Add external alias';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm && editInboxID) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases';
    }
  }
  // openExternalAliasEditor reuses the add editor for renaming: the address is
  // immutable, so it is shown disabled and only the sender name is submitted.
  function openExternalAliasEditor(aliasID, name, address) {
    if (!extEditor) {
      return;
    }
    resetExternalAliasEditor();
    var nameEl = document.getElementById('external-alias-name');
    var addrEl = document.getElementById('external-alias-address');
    if (nameEl) {
      nameEl.value = name || '';
    }
    if (addrEl) {
      addrEl.value = address || '';
      addrEl.disabled = true;
    }
    if (extSave) {
      extSave.textContent = 'Save';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases/' + encodeURIComponent(aliasID) + '/edit';
    }
    extEditor.hidden = false;
    if (nameEl) {
      nameEl.focus();
    }
  }
  if (extEditor && extAdd) {
    extAdd.addEventListener('click', function () {
      resetExternalAliasEditor();
      extEditor.hidden = false;
      var nameEl = document.getElementById('external-alias-name');
      if (nameEl) {
        nameEl.focus();
      }
    });
    var extCancel = document.getElementById('external-alias-cancel');
    if (extCancel) {
      extCancel.addEventListener('click', function () {
        extEditor.hidden = true;
      });
    }
    var extFormEl = document.getElementById('external-alias-form');
    if (extFormEl) {
      extFormEl.addEventListener('submit', function () {
        inboxSubmitting = true;
      });
    }
  }
  if (editExternalList) {
    editExternalList.addEventListener('click', function (e) {
      var edit = e.target.closest('.external-alias-edit');
      if (edit) {
        e.preventDefault();
        openExternalAliasEditor(edit.dataset.alias, edit.dataset.name, edit.dataset.address);
        return;
      }
      var configure = e.target.closest('.external-alias-configure');
      if (configure) {
        e.preventDefault();
        openExternalAliasDialog(dlg, configure.dataset.alias);
        return;
      }
      var remove = e.target.closest('.external-alias-remove');
      if (remove) {
        e.preventDefault();
        deleteExternalAlias(editInboxID, remove.dataset.alias, remove.dataset.address);
      }
    });
  }

  if (display) {
    display.addEventListener('input', function () {
      editPrimaryName = display.value.trim();
      refreshEditSender();
    });
  }

  var cancel = document.getElementById('inbox-edit-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      if (dlg.querySelector('.inbox-subview.active[data-inbox-subview=connectors]')) {
        hideInboxSubview(dlg);
        return;
      }
      dlg.close();
    });
  }

  // Reopen the inbox edit dialog on the section named in the URL when the
  // dashboard was loaded with an inbox to open (e.g. returning from a connector
  // or alias save). An external-alias connector flash opens its sending
  // sub-view. The shell's tab listeners are attached by a later script block, so
  // defer past script execution.
  var openCard = document.querySelector('[data-open-inbox]');
  var openInbox = openCard ? openCard.getAttribute('data-open-inbox') : '';
  var openInboxTab = openCard ? (openCard.getAttribute('data-open-inbox-tab') || 'aliases') : 'aliases';
  var openAlias = openCard ? (openCard.getAttribute('data-open-alias') || '') : '';
  window.__accessMembers = [];
  if (openCard) {
    try {
      window.__accessMembers = JSON.parse(openCard.getAttribute('data-access-members') || '[]') || [];
    } catch (e) {
      window.__accessMembers = [];
    }
  }
  if (openInbox) {
    setTimeout(function () {
      var target = null;
      document.querySelectorAll('.edit-inbox').forEach(function (btn) {
        if (btn.dataset.id === openInbox) {
          target = btn;
        }
      });
      if (target) {
        target.click();
        var tab = dlg.querySelector('[data-inbox-tab=' + openInboxTab + ']');
        if (tab) {
          tab.click();
        }
        var pending = document.querySelector('.domain-dialog[data-open="1"]');
        if (pending && !pending.open && typeof pending.showModal === 'function') {
          pending.showModal();
        }
        if (openAlias) {
          openExternalAliasDialog(dlg, openAlias);
        }
      }
    }, 0);
  }
})();

// bindInboxSettingsShell wires the shared full-height inbox settings shell used
// by both the Add and Edit dialogs: vertical section navigation, a single
// scrollable panel area, in-modal sub-views (connector editor, external-alias
// sending) and a single footer save bar. It returns nothing and is idempotent
// per dialog.
function bindInboxSettingsShell(dlg) {
  if (!dlg || dlg._inboxShellBound) {
    return;
  }
  dlg._inboxShellBound = true;
  var bar = dlg.querySelector('[data-inbox-tabs]');
  if (!bar) {
    return;
  }
  var tabs = bar.querySelectorAll('[data-inbox-tab]');
  var chooser = dlg.querySelector('[data-inbox-choose]');
  var backBtn = dlg.querySelector('#inbox-back');
  var addSubmit = dlg.querySelector('#inbox-add-submit');

  // The Add dialog carries a type chooser and two flows (domain, standalone)
  // sharing one shell; the Edit dialog gains collapsible standalone-only
  // sections inside its Identity panel. Each tab/panel is tagged with its flow
  // where a flow applies; untagged tabs behave exactly as before.
  function modeFor(name) {
    if (name === 'choose') {
      return 'choose';
    }
    if (name === 'sa-provider') {
      return 'provider';
    }
    if (name === 'sa-google-advanced') {
      return 'google-advanced';
    }
    if (name.indexOf('sa-') === 0) {
      return 'standalone';
    }
    return 'domain';
  }

  function activate(name) {
    // Leaving a panel always drops back out of any sub-view.
    dlg.classList.remove('inbox-settings--subview');
    dlg.querySelectorAll('.inbox-subview.active').forEach(function (v) {
      v.classList.remove('active');
    });
    var mode = modeFor(name);
    // The chooser steps carry no tabs and no footer submit; the provider step and
    // the Google advanced-setup warning are both second halves of the standalone
    // flow, so they share the chooser layout.
    var chooserStep = mode === 'choose' || mode === 'provider' || mode === 'google-advanced';
    // The chooser steps carry no section navigation, so collapse the empty
    // left-hand column instead of leaving it blank beside the panel.
    dlg.classList.toggle('inbox-settings--chooser', chooserStep);
    tabs.forEach(function (t) {
      var tm = t.getAttribute('data-inbox-mode');
      // Untagged tabs belong to every kind. Tagged tabs show only in their
      // flow's mode. This never hides the tab that is about to be activated.
      var visible = !chooserStep && (!tm || tm === mode);
      t.hidden = !visible;
      var on = visible && t.getAttribute('data-inbox-tab') === name;
      t.classList.toggle('active', on);
      t.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    dlg.querySelectorAll('[data-inbox-panel]').forEach(function (p) {
      p.hidden = p.getAttribute('data-inbox-panel') !== name;
    });
    if (chooser) {
      var heading = dlg.querySelector('.inbox-settings-head h2');
      if (heading) {
        heading.textContent = mode === 'provider' || mode === 'standalone' || mode === 'google-advanced' ? 'Add standalone inbox' : 'Add inbox';
      }
      if (backBtn) {
        backBtn.hidden = mode === 'choose';
      }
      if (addSubmit) {
        addSubmit.hidden = chooserStep;
        if (mode === 'standalone') {
          addSubmit.textContent = 'Add standalone inbox';
          addSubmit.setAttribute('form', 'inbox-add-standalone-form');
        } else if (mode === 'domain') {
          addSubmit.textContent = 'Add Inbox';
          addSubmit.setAttribute('form', 'inbox-add-form');
        }
      }
    }
    var inboxSave = dlg.querySelector('#inbox-edit-save');
    if (inboxSave) {
      // Save is always shown; the click handler (bound once) submits the single
      // main inbox-edit form, mirroring the delivery auto-actions and any staged
      // Clients & Access role changes onto it. The connectors tab keeps the
      // main-form target so a Save there commits auto-actions too.
      inboxSave.hidden = false;
      inboxSave.textContent = 'Save';
      inboxSave.setAttribute('form', 'inbox-edit-form');
    }
  }

  tabs.forEach(function (t) {
    t.addEventListener('click', function () {
      activate(t.getAttribute('data-inbox-tab'));
    });
  });

  if (chooser) {
    dlg.querySelectorAll('[data-inbox-choose] [data-choose]').forEach(function (tile) {
      tile.addEventListener('click', function () {
        // Standalone inboxes pick a provider first: the two hosted providers
        // open their own setup page, IMAP stays in this dialog.
        if (tile.getAttribute('data-choose') === 'standalone') {
          activate('sa-provider');
          return;
        }
        activate('basic');
      });
    });
    dlg.querySelectorAll('[data-standalone-provider]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var provider = btn.getAttribute('data-standalone-provider');
        if (provider === 'imap') {
          activate('sa-basic');
          return;
        }
        if (provider === 'google') {
          // Google needs your own Google Cloud OAuth app: show an advanced-setup
          // warning with easier alternatives before opening the wizard.
          activate('sa-google-advanced');
          return;
        }
        if (provider === 'one') {
          window.location.assign('/ui/connections/one');
          return;
        }
        window.location.assign('/ui/inboxes/standalone/new?provider=' + encodeURIComponent(provider));
      });
    });
    // The Google advanced-setup warning step.
    dlg.querySelectorAll('[data-google-advanced-continue]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        window.location.assign('/ui/inboxes/standalone/new?provider=google');
      });
    });
    dlg.querySelectorAll('[data-google-advanced-one]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        window.location.assign('/ui/connections/one');
      });
    });
    dlg.querySelectorAll('[data-google-advanced-imap]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        activate('sa-basic');
      });
    });
    if (backBtn) {
      backBtn.addEventListener('click', function () {
        // The IMAP form and the Google advanced-setup warning step back to the
        // provider list; anything else returns to the inbox-type chooser.
        var panel = dlg.querySelector('[data-inbox-panel]:not([hidden])');
        var current = panel ? panel.getAttribute('data-inbox-panel') : '';
        activate(current === 'sa-basic' || current === 'sa-google-advanced' ? 'sa-provider' : 'choose');
      });
    }
  }

  dlg._resetTabs = function () {
    if (chooser) {
      activate('choose');
      return;
    }
    if (tabs.length) {
      activate(tabs[0].getAttribute('data-inbox-tab'));
    }
  };

  // A required field in a hidden panel (e.g. a provider form posted from the
  // footer) surfaces its panel rather than failing silently.
  dlg.querySelectorAll('form').forEach(function (form) {
    form.addEventListener('invalid', function (e) {
      var panel = e.target.closest ? e.target.closest('[data-inbox-panel]') : null;
      if (panel) {
        activate(panel.getAttribute('data-inbox-panel'));
      }
    }, true);
  });

  dlg.querySelectorAll('.inbox-close').forEach(function (btn) {
    btn.addEventListener('click', function () {
      dlg.close();
    });
  });
  dlg.querySelectorAll('.inbox-subview-back').forEach(function (btn) {
    btn.addEventListener('click', function () {
      hideInboxSubview(dlg);
    });
  });
}

(function () {
  var root = document.querySelector('[data-standalone-wizard]');
  if (!root) { return; }
  var status = root.querySelector('[data-google-status]');
  function show(step) {
    root.querySelectorAll('[data-wizard-step]').forEach(function (el) {
      el.hidden = el.dataset.wizardStep !== step;
    });
  }
  var params = new URLSearchParams(window.location.search);
  var reconnect = params.get('google_inbox');
	var microsoftReconnect = params.get('microsoft_inbox');
	if (microsoftReconnect) { root.querySelector('[data-microsoft-inbox]').value = microsoftReconnect; show('microsoft'); }
  if (reconnect) {
    root.querySelector('[data-google-inbox]').value = reconnect;
    show('google');
  }
  // The Add-inbox dialog picks the provider and deep-links straight to its
  // step, so this page's own provider chooser is skipped. Direct visits still
  // land on the chooser below.
  var deepProvider = params.get('provider');
  var deepLinked = !reconnect && !microsoftReconnect && (deepProvider === 'google' || deepProvider === 'microsoft' || deepProvider === 'imap');
  if (deepLinked) {
    show(deepProvider);
  }
  root.querySelector('[data-wizard-next]').addEventListener('click', function () {
	  var provider = root.querySelector('[name=standalone_provider]:checked').value;
	  if (provider === 'one') {
	    window.location.assign('/ui/connections/one');
	    return;
	  }
	  show(provider);
  });
  root.querySelectorAll('[data-wizard-back]').forEach(function (b) {
    b.addEventListener('click', function () {
      // A reconnect deep-link belongs to one inbox, so Back returns to that
      // inbox rather than to this page's provider chooser, which is the
      // new-inbox screen.
      if (reconnect || microsoftReconnect) {
        window.location.assign('/ui/inboxes/' + encodeURIComponent(reconnect || microsoftReconnect));
        return;
      }
      // Arrived from the Add-inbox dialog: step back to the dashboard rather
      // than to this page's own provider chooser.
      if (deepLinked) {
        window.location.assign('/');
        return;
      }
      show('provider');
    });
  });
  fetch('/ui/oauth/google/info', { credentials: 'same-origin' }).then(function (r) {
    if (!r.ok) { throw new Error('Could not load callback configuration'); }
    return r.json();
  }).then(function (info) {
    root.querySelector('[data-google-callback]').value = info.callback_url;
    if (!info.valid) {
      status.textContent = 'Set BASE_URL to an HTTPS hostname or localhost URL before connecting Google.';
      root.querySelector('[data-google-begin] button[type=submit], [data-google-begin] button:last-child').disabled = true;
    }
  }).catch(function (e) { status.textContent = e.message; });
  window.mammothmailCopy.wire(root.querySelector('[data-google-copy]'), {
    text: function () { return root.querySelector('[data-google-callback]').value; },
    select: function () { return root.querySelector('[data-google-callback]'); }
  });
  root.querySelectorAll('[data-google-begin], [data-google-finish]').forEach(function (form) {
    form.addEventListener('submit', function (event) {
      event.preventDefault();
      var submit = form.querySelector('button:last-child');
      submit.disabled = true;
      var popup = form.hasAttribute('data-google-begin') ? window.open('about:blank', '_blank') : null;
      status.textContent = 'Connecting…';
      fetch(form.action, { method: 'POST', credentials: 'same-origin', body: new URLSearchParams(new FormData(form)) }).then(function (r) {
        if (!r.ok) { return r.text().then(function (text) { throw new Error(text); }); }
        return r.json();
      }).then(function (result) {
        if (result.authorization_url) {
          form.querySelector('[name=client_secret]').value = '';
          if (popup) { popup.location = result.authorization_url; }
          else {
            var link = document.createElement('a');
            link.href = result.authorization_url; link.target = '_blank'; link.rel = 'noopener noreferrer';
            link.textContent = 'Open Google authorization'; status.replaceChildren(link);
          }
          if (popup) { status.textContent = 'Approve access in the Google tab. If the return page fails, paste its complete URL below.'; }
        } else if (result.inbox_id) {
          form.querySelector('[name=callback_url]').value = '';
          window.location.assign('/ui/inboxes/' + encodeURIComponent(result.inbox_id));
        }
      }).catch(function (e) {
        if (popup) { popup.close(); }
        status.textContent = e.message;
      }).finally(function () { submit.disabled = false; });
    });
  });
	// openOAuthPopup opens the provider sign-in in a centred partial-screen
	// window so the wizard stays visible behind it. It stays narrow but tall so
	// the browser keeps its address bar: the Microsoft flow finishes on a
	// localhost URL that cannot load, and copying that URL out of the window is
	// the only way to complete the connection. The fixed name reuses one window
	// when the operator starts a sign-in again.
	function openOAuthPopup() {
		var w = 520, h = 720;
		var left = Math.max(0, window.screenX + Math.round((window.outerWidth - w) / 2));
		var top = Math.max(0, window.screenY + Math.round((window.outerHeight - h) / 2));
		return window.open('about:blank', 'mm-oauth-signin', 'popup=yes,width=' + w + ',height=' + h + ',left=' + left + ',top=' + top + ',resizable=yes,scrollbars=yes,location=yes');
	}

	// flashPasteHint reveals the paste-the-URL prompt at the top of the finish
	// box and pulses it. Removing the class and forcing a reflow restarts the
	// animation, so a second sign-in flashes it again rather than leaving it
	// silently already-flashed.
	function flashPasteHint() {
		var hint = root.querySelector('[data-microsoft-paste-hint]');
		if (!hint) { return; }
		hint.hidden = false;
		hint.classList.remove('flash');
		void hint.offsetWidth;
		hint.classList.add('flash');
		hint.scrollIntoView({ block: 'nearest' });
	}

	root.querySelectorAll('[data-microsoft-begin], [data-microsoft-finish]').forEach(function (form) {
		// The begin step carries two submit buttons; keep the hidden default in
		// sync on click so the chosen type survives browsers that omit the
		// submitter from FormData.
		form.querySelectorAll('button[name=mailbox_type]').forEach(function (b) {
			b.addEventListener('click', function () {
				var field = form.querySelector('input[name=mailbox_type]');
				if (field) { field.value = b.value; }
			});
		});
		form.addEventListener('submit', function (event) {
			event.preventDefault();
			var status = root.querySelector('[data-microsoft-status]');
			form.querySelectorAll('button').forEach(function (b) { b.disabled = true; });
			var popup = form.hasAttribute('data-microsoft-begin') ? openOAuthPopup() : null;
			status.textContent = 'Connecting…';
			function submit(body) {
				fetch(form.action, { method: 'POST', credentials: 'same-origin', body: body }).then(function (r) {
					if (!r.ok) { return r.text().then(function (text) { throw new Error(text); }); }
					return r.json();
				}).then(function (result) {
					if (result.authorization_url) {
						flashPasteHint();
						if (popup) { popup.opener = null; popup.location = result.authorization_url; status.textContent = 'Approve access, then paste the complete localhost URL below.'; }
						else { var link = document.createElement('a'); link.href = result.authorization_url; link.target = '_blank'; link.rel = 'noopener noreferrer'; link.textContent = 'Open Microsoft sign-in'; status.replaceChildren(link); }
					} else if (result.mailboxes) {
						// The shared-mailbox picker replaces both halves of the connect
						// flow, leaving only the picker and Back.
						var begin = root.querySelector('[data-microsoft-begin]');
						if (begin) { begin.hidden = true; }
						form.hidden = true;
						status.textContent = '';
						showMicrosoftChoice(form, result.mailboxes, result.selection_state);
					} else if (result.inbox_id) { form.querySelector('[name=callback_url]').value = ''; window.location.assign('/ui/inboxes/' + encodeURIComponent(result.inbox_id)); }
				}).catch(function (e) { if (popup) { popup.close(); } status.textContent = e.message; }).finally(function () { form.querySelectorAll('button').forEach(function (b) { b.disabled = false; }); });
			}
			var body = new URLSearchParams(new FormData(form));
			submit(body);
		});
	});

	// showMicrosoftChoice renders the shared-mailbox picker after sign-in. The
	// directory names are untrusted tenant data, so options are built as DOM
	// nodes rather than markup.
	function showMicrosoftChoice(form, mailboxes, state) {
		var status = root.querySelector('[data-microsoft-status]');
		var box = root.querySelector('[data-microsoft-choice]');
		box.replaceChildren();
		var intro = document.createElement('p');
		intro.textContent = 'Signed in. Choose the shared mailbox to connect.';
		box.appendChild(intro);
		var select = null;
		if (mailboxes.length) {
			select = document.createElement('select');
			select.appendChild(new Option('Select a mailbox', ''));
			mailboxes.forEach(function (m) {
				select.appendChild(new Option(m.name ? m.name + ' <' + m.address + '>' : m.address, m.address));
			});
			var selectLabel = document.createElement('label');
			selectLabel.textContent = 'Accessible shared mailboxes';
			box.appendChild(selectLabel);
			box.appendChild(select);
		}
		var manual = document.createElement('input');
		manual.type = 'email';
		manual.placeholder = 'shared@example.com';
		var manualLabel = document.createElement('label');
		manualLabel.textContent = mailboxes.length ? 'Or enter the shared mailbox address' : 'Shared mailbox address';
		var connect = document.createElement('button');
		connect.type = 'button';
		connect.textContent = 'Connect shared mailbox';
		var back = document.createElement('button');
		back.type = 'button';
		back.className = 'secondary';
		back.textContent = 'Back';
		box.appendChild(manualLabel);
		box.appendChild(manual);
		box.appendChild(connect);
		box.appendChild(back);
		box.hidden = false;
		back.addEventListener('click', function () {
			// The sign-in attempt was spent completing the callback, so the
			// only honest reset is to reload the wizard and start over.
			window.location.reload();
		});
		connect.addEventListener('click', function () {
			var address = manual.value.trim();
			if (!address && select && select.value) { address = select.value; }
			if (!address) { status.textContent = 'Choose or enter the shared mailbox address.'; return; }
			connect.disabled = true;
			status.textContent = 'Connecting…';
			var csrf = form.querySelector('[name=_csrf]');
			var body = new URLSearchParams({ _csrf: csrf ? csrf.value : '', state: state, mailbox: address });
			fetch(form.action, { method: 'POST', credentials: 'same-origin', body: body }).then(function (r) {
				if (!r.ok) { return r.text().then(function (text) { throw new Error(text); }); }
				return r.json();
			}).then(function (result) {
				if (result.inbox_id) { window.location.assign('/ui/inboxes/' + encodeURIComponent(result.inbox_id)); }
			}).catch(function (e) { status.textContent = e.message; }).finally(function () { connect.disabled = false; });
		});
	}
})();

// The One setup page has no wizard step of its own, so its left-aligned Back
// follows the browser history to the screen the user came from (the Add-inbox
// dialog, the standalone wizard, or a direct link), falling back to the
// dashboard when there is no history to return to.
(function () {
  var back = document.querySelector('[data-one-page] [data-one-back]');
  if (!back) { return; }
  back.addEventListener('click', function () {
    if (window.history.length > 1 && document.referrer) {
      window.history.back();
      return;
    }
    window.location.assign('/');
  });
})();

// showInboxSubview switches the whole panels area to a sub-view (the connector
// editor or the external-alias sending editor) and marks its footer submit so
// only one Save is ever shown.
function showInboxSubview(dlg, name) {
  if (!dlg) {
    return;
  }
  dlg.querySelectorAll('.inbox-subview').forEach(function (v) {
    v.classList.toggle('active', v.getAttribute('data-inbox-subview') === name);
  });
  dlg.classList.add('inbox-settings--subview');
}

// hideInboxSubview returns from a sub-view to the current section panel.
function hideInboxSubview(dlg) {
  if (!dlg) {
    return;
  }
  dlg.classList.remove('inbox-settings--subview');
  var save = dlg.querySelector('#inbox-edit-save');
  if (save) {
    save.setAttribute('form', 'inbox-edit-form');
  }
  dlg.querySelectorAll('.inbox-subview.active').forEach(function (v) {
    v.classList.remove('active');
  });
  // Clear any transient connector editor markup the sub-view held.
  var editor = dlg.querySelector('#inbox-connector-editor');
  if (editor) {
    editor.textContent = '';
  }
}

function generateBearerSecret() {
  var bytes = new Uint8Array(32);
  window.crypto.getRandomValues(bytes);
  var raw = '';
  bytes.forEach(function (b) { raw += String.fromCharCode(b); });
  return window.btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

(function () {
  document.querySelectorAll('dialog.inbox-settings').forEach(bindInboxSettingsShell);
})();

(function () {
  var notices = document.querySelectorAll('.notice');
  if (!notices.length) {
    return;
  }

  function dismiss(el) {
    if (el.dataset.dismissed) {
      return;
    }
    el.dataset.dismissed = '1';
    el.classList.add('dismissing');
    window.setTimeout(function () {
      if (el.parentNode) {
        el.parentNode.removeChild(el);
      }
    }, 300);
  }

  notices.forEach(function (el) {
    var timer = window.setTimeout(function () {
      dismiss(el);
    }, 5000);
    el.addEventListener('click', function () {
      window.clearTimeout(timer);
      dismiss(el);
    });
  });

  // Drop the notice from the URL so a refresh does not show it again.
  if (window.history && window.history.replaceState && window.URLSearchParams) {
    var url = new URL(window.location.href);
    if (url.searchParams.has('notice')) {
      url.searchParams.delete('notice');
      window.history.replaceState(null, '', url.pathname + url.search + url.hash);
    }
  }
})();

(function () {
  var frame = document.querySelector('[data-mailframe]');
  var show = document.querySelector('[data-remote-img-show]');
  var banner = document.querySelector('#remote-img-banner');
  if (!frame || !show) {
    return;
  }
  // The banner is rendered server-side only when the message has remote images.
  // Clicking it reloads the frame with ?remote=1, relaxing the CSP to fetch them.
  show.addEventListener('click', function () {
    try {
      var url = new URL(frame.getAttribute('src'), window.location.origin);
      url.searchParams.set('remote', '1');
      frame.dataset.remoteOptIn = '1';
      frame.src = url.toString();
      if (banner) {
        banner.hidden = true;
      }
    } catch (e) {
      // ignore
    }
  });
})();

(function () {
  function bindFrame(frame) {
    if (frame.dataset.messageAutoHeight) { return; }
    frame.dataset.messageAutoHeight = '1';

    function fit() {
      try {
        var doc = frame.contentDocument;
        if (!doc || !doc.documentElement) { return; }
        var body = doc.body;
        var height = Math.max(
          360,
          doc.documentElement.scrollHeight,
          body ? body.scrollHeight : 0
        );
        frame.style.height = (height + 2) + 'px';
      } catch (e) {
        // Keep the fallback height if the frame document is unavailable.
      }
    }

    frame.addEventListener('load', fit);
    window.addEventListener('resize', fit);
    if (frame.contentDocument && frame.contentDocument.readyState === 'complete') {
      fit();
    }
  }

  document.querySelectorAll('[data-mailframe]').forEach(bindFrame);
  var addedFrames = new MutationObserver(function (records) {
    records.forEach(function (record) {
      record.addedNodes.forEach(function (node) {
        if (node.nodeType !== 1) { return; }
        if (node.matches('[data-mailframe]')) { bindFrame(node); }
        node.querySelectorAll('[data-mailframe]').forEach(bindFrame);
      });
    });
  });
  addedFrames.observe(document.body, { childList: true, subtree: true });
})();

(function () {
  var host = document.querySelector('[data-remote-body]');
  if (!host) { return; }
  var active = false;
  function markRead() {
    if (host.dataset.unread !== '1') { return; }
    var form = new URLSearchParams({ _csrf: host.dataset.csrf, read: '1' });
    fetch(host.dataset.readUrl, { method: 'POST', credentials: 'same-origin', body: form }).catch(function () {});
    host.dataset.unread = '';
  }
  // load() renders the message body; the reconnect banner it may need is built
  // by the shared mammothmailReconnectBanner helper.
  function load() {
    if (active) { return; }
    active = true;
    window.mammothmailStatus.busy('body', true);
    host.replaceChildren();
    var status = document.createElement('div');
    status.className = 'load-status pending';
    status.setAttribute('role', 'status');
    status.setAttribute('aria-busy', 'true');
    status.textContent = 'Loading message body…';
    host.appendChild(status);
    var controller = new AbortController();
    var timeout = window.setTimeout(function () { controller.abort(); }, 60000);
    fetch(host.dataset.bodyUrl, { credentials: 'same-origin', signal: controller.signal }).then(function (response) {
      // A failure payload can carry the inbox's durable connection verdict, so
      // read it on a non-2xx response instead of discarding it as "unavailable".
      return response.json().catch(function () { return {}; }).then(function (data) {
        if (!response.ok) { throw data || {}; }
        return data;
      });
    }).then(function (body) {
      host.replaceChildren();
      if (body.html) {
        var frame = document.createElement('iframe');
        frame.className = 'mailframe';
        frame.setAttribute('sandbox', 'allow-same-origin allow-popups allow-popups-to-escape-sandbox');
        frame.setAttribute('referrerpolicy', 'no-referrer');
        frame.setAttribute('title', 'Message body');
        frame.setAttribute('data-mailframe', '');
        frame.hidden = true;
        frame.onload = function () { frame.hidden = false; markRead(); };
        frame.srcdoc = '<meta http-equiv="Content-Security-Policy" content="default-src &#39;none&#39;; style-src &#39;unsafe-inline&#39;; img-src data:; base-uri &#39;none&#39;; form-action &#39;none&#39;">' + body.html;
        if (body.remote_images) {
          var banner = document.createElement('div');
          banner.className = 'banner warn';
          banner.textContent = 'Remote images are hidden to prevent read-tracking. ';
          var show = document.createElement('button');
          show.type = 'button';
          show.className = 'secondary btn-sm';
          show.textContent = 'Show images';
          show.onclick = function () {
            frame.removeAttribute('srcdoc');
            frame.src = host.dataset.htmlUrl + '?remote=1';
            banner.remove();
          };
          banner.appendChild(show);
          host.appendChild(banner);
        }
        host.appendChild(frame);
        if (body.text) {
          var fallback = document.createElement('details');
          var summary = document.createElement('summary');
          summary.textContent = 'Plain text';
          var pt = document.createElement('div');
          pt.className = 'msgbody';
          pt.textContent = body.text;
          fallback.appendChild(summary);
          fallback.appendChild(pt);
          host.appendChild(fallback);
        }
      } else {
        var text = document.createElement('div');
        text.className = 'msgbody';
        text.textContent = body.text || '(Empty message body)';
        host.appendChild(text);
        markRead();
      }
      if (body.attachments && body.attachments.length) {
        var heading = document.createElement('h3');
        heading.textContent = 'Attachments';
        var list = document.createElement('ul');
        list.className = 'attachments';
        body.attachments.forEach(function (a) {
          var item = document.createElement('li');
          var link = document.createElement('a');
          link.href = host.dataset.attachUrl.replace('{part}', encodeURIComponent(a.part_path)) + '?filename=' + encodeURIComponent(a.filename || 'attachment') + '&content_type=' + encodeURIComponent(a.content_type || '');
          link.textContent = a.filename || 'attachment';
          item.appendChild(link);
          if (a.size) {
            var size = document.createElement('span');
            size.className = 'muted';
            size.textContent = ' · ' + a.size + ' B';
            item.appendChild(size);
          }
          list.appendChild(item);
        });
        host.appendChild(heading);
        host.appendChild(list);
      }
    }).catch(function (failure) {
      // A revoked connection discovered here is a reconnect prompt, not a
      // transient load failure: show the reconnect control in place and drop the
      // Retry that could only fail again.
      var conn = failure && failure.connection;
      if (conn && conn.state === 'disconnected') {
        host.replaceChildren(window.mammothmailReconnectBanner(conn));
        return;
      }
      host.replaceChildren(status);
      status.classList.remove('pending');
      status.setAttribute('aria-busy', 'false');
      status.textContent = 'Could not load the message body. ';
      var retry = document.createElement('button');
      retry.type = 'button';
      retry.className = 'secondary';
      retry.textContent = 'Retry';
      retry.onclick = load;
      status.appendChild(retry);
    }).finally(function () {
      window.clearTimeout(timeout);
      window.mammothmailStatus.busy('body', false);
      active = false;
    });
  }
  load();
})();

(function () {
  // Copy-code button on the one-time Cloudflare Worker setup page.
  var btn = document.getElementById('cf-copy');
  if (!btn) {
    return;
  }
  var code = document.getElementById('cf-code');
  window.mammothmailCopy.wire(btn, {
    text: function () { return code ? code.textContent : ''; },
    select: code
  });
})();

(function () {
  // Copy button for the one-time DKIM private-key pop-up.
  var btn = document.getElementById('dkim-key-copy');
  if (!btn) {
    return;
  }
  var val = document.getElementById('dkim-private-key');
  window.mammothmailCopy.wire(btn, {
    text: function () { return val ? val.textContent : ''; },
    select: val
  });
})();

(function () {
  var btn = document.getElementById('domain-credential-copy');
  if (!btn) {
    return;
  }
  var url = document.getElementById('domain-credential-url');
  window.mammothmailCopy.wire(btn, {
    text: function () { return url ? url.textContent : ''; },
    select: url,
    onSuccess: function () {
      btn.classList.add('copied');
      window.mammothmailCopy.announce('Webhook URL copied.');
      window.setTimeout(function () { btn.classList.remove('copied'); }, 1500);
    }
  });
})();

(function () {
  // Copy-to-clipboard for a value the server rendered into an attribute, used by
  // the DKIM TXT record's Copy button on the sending dialog.
  document.querySelectorAll('[data-copy-value]').forEach(function (btn) {
    window.mammothmailCopy.wire(btn, {
      text: function () { return btn.getAttribute('data-copy-value') || ''; },
      select: function () { return btn.previousElementSibling; }
    });
  });
})();

(function () {
  // Live DKIM published-record light on the sending dialog, mirroring the Tusk
  // status poll: while the dialog is open, refresh the record dot so an operator
  // watching DNS propagate sees it turn green without closing and reopening. The
  // server reads the watcher's shared cached verdict, so a poll is cheap and
  // never runs a resolver lookup on the request. The interval matches the
  // dashboard lights tick's intent (seconds-scale, coalesced server-side).
  var POLL_MS = 15000;
  document.querySelectorAll('form[data-sending-domain]').forEach(function (form) {
    var dlg = form.closest('dialog');
    if (!dlg) {
      return;
    }
    var url = '/ui/domains/' + encodeURIComponent(form.getAttribute('data-sending-domain')) + '/sending/dkim/status';
    var timer = null;
    function statusText(light) {
      if (light === 'ok') { return 'DNS valid'; }
      if (light === 'danger') { return 'DNS invalid'; }
      if (light === 'grey') { return 'DNS not present'; }
      return '';
    }
    function paint(light, title) {
      form.querySelectorAll('[data-dkim-record-light]').forEach(function (dot) {
        dot.className = 'dns-light ' + (light || 'grey');
        dot.setAttribute('title', title || '');
        dot.setAttribute('aria-label', title || '');
      });
      form.querySelectorAll('[data-dkim-record-text]').forEach(function (el) {
        el.textContent = statusText(light);
      });
    }
    function refresh() {
      if (!dlg.open) {
        return;
      }
      fetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) { if (d) { paint(d.light, d.title); } })
        .catch(function () {});
    }
    function schedule() {
      if (timer) {
        window.clearTimeout(timer);
      }
      timer = window.setTimeout(function () {
        refresh();
        schedule();
      }, POLL_MS);
    }
    dlg.addEventListener('toggle', function () {
      if (dlg.open) {
        refresh();
        schedule();
      } else if (timer) {
        window.clearTimeout(timer);
        timer = null;
      }
    });
  });
})();

(function () {
  document.querySelectorAll('.setup-copy').forEach(function (btn) {
    var dlg = btn.closest('dialog') || document;
    // Each provider group owns its own webhook URL. Resolve it from the button's
    // group so a copy never picks up another provider's URL.
    var scope = btn.closest('.provider-fields') || dlg;
    var url = scope.querySelector('.setup-webhook-url');
    window.mammothmailCopy.wire(btn, {
      text: function () { return url ? url.textContent : ''; },
      select: url,
      onSuccess: function () {
        btn.classList.add('copied');
        window.mammothmailCopy.announce('Webhook URL copied.');
        window.setTimeout(function () { btn.classList.remove('copied'); }, 1500);
      }
    });
  });
})();

(function () {
  document.querySelectorAll('.open-domain-dialog').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = document.getElementById('domain-' + btn.getAttribute('data-kind') + '-dialog-' + btn.getAttribute('data-domain'));
      if (dlg && !dlg.open) {
        dlg.showModal();
      }
    });
  });
  // Pending dialogs auto-open, unless inbox settings are also reopening: the
  // inbox-edit flow then suspends first and shows the pending dialog itself,
  // so cancelling it resumes settings instead of dropping to the dashboard.
  // The inbox script block runs before this one and defers via setTimeout, so
  // check for a pending inbox reopen synchronously here.
  var inboxReopening = false;
  var openCardEl = document.querySelector('[data-open-inbox]');
  if (openCardEl && openCardEl.getAttribute('data-open-inbox')) {
    inboxReopening = true;
  }
  var pendingDialogs = document.querySelectorAll('.domain-dialog[data-open="1"]');
  var keyDialog = document.getElementById('dkim-key-dialog');
  pendingDialogs.forEach(function (dlg) {
    if (inboxReopening) {
      return;
    }
    // The one-time DKIM key pop-up is opened last (below) so it sits above the
    // sending dialog it was launched from instead of behind it.
    if (dlg === keyDialog) {
      return;
    }
    if (!dlg.open) {
      dlg.showModal();
    }
  });
  if (!inboxReopening && keyDialog && keyDialog.getAttribute('data-open') === '1' && !keyDialog.open) {
    keyDialog.showModal();
  }
  document.querySelectorAll('.domain-dialog [data-close-dialog]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = btn.closest('dialog');
      if (dlg) {
        dlg.close();
      }
    });
  });
  // A genuine dismissal of a URL-opened domain or Cloudflare dialog drops the
  // params that reopened it, so a refresh does not bring it back. The
  // external-alias sending dialogs are excluded here: their close handler lives
  // in the inbox settings block, which also restores the suspended inbox.
  document.querySelectorAll('.domain-dialog[id^="domain-"]').forEach(function (dlg) {
    dlg.addEventListener('close', function () {
      clearUrlParams(['domain', 'kind', 'provider']);
    });
  });
})();

(function () {
  // syncDialMXService materialises or removes the editable Receiver URLs field
  // for a Dial MX provider group. The field is only valid for the custom
  // service, so for Tusk MX it is kept out of the DOM; switching the service
  // select to custom injects the field from its inert <template>. The server
  // independently rejects a supplied receiver_urls for Tusk.
  function syncDialMXService(group) {
    if (!group || group.getAttribute('data-provider') !== 'dialmx') {
      return;
    }
    var service = group.querySelector('select[name="cfg_dialmx_service"]');
    if (!service) {
      return;
    }
    var tpl = group.querySelector('template.dialmx-receiver-urls-field');
    var injected = group.querySelector('.dialmx-receiver-urls');
    if (service.value === 'custom') {
      if (!injected && tpl) {
        group.insertBefore(tpl.content.cloneNode(true), tpl);
      }
    } else if (injected) {
      injected.parentNode.removeChild(injected);
    }
  }
  // syncDependentFields shows a panel only while the field it depends on holds a
  // listed value. The DKIM record panel appears the moment signing is switched
  // on, without a save, so the operator sees the record they must publish while
  // they are still in the editor. A hidden panel's inputs are disabled so a
  // switched-off block is never submitted.
  function syncDependentFields(scope) {
    scope.querySelectorAll('[data-show-field]').forEach(function (el) {
      var dep = el.getAttribute('data-show-field');
      var control = scope.querySelector('[name="' + dep + '"]');
      var wanted = (el.getAttribute('data-show-values') || '').split(',').map(function (v) {
        return v.trim().toLowerCase();
      });
      var shown = !control || wanted.indexOf((control.value || '').toLowerCase()) !== -1;
      el.hidden = !shown;
      el.querySelectorAll('input,select,textarea,button').forEach(function (input) {
        input.disabled = !shown;
      });
    });
  }
  // syncSaveLabel picks the submit label. A provider whose editor has a
  // server-generated secret offers "Next", because saving first reveals the
  // generated value the operator must copy. Every other provider reads "Save".
  function syncSaveLabel(dlg) {
    var sel = dlg.querySelector('.provider-select');
    var save = dlg.querySelector('[data-save-provider]');
    if (!sel || !save) {
      return;
    }
    var opt = sel.options[sel.selectedIndex];
    var next = !!(opt && opt.getAttribute('data-next'));
    save.textContent = next ? 'Next' : 'Save';
  }
  function sync(dlg) {
    var sel = dlg.querySelector('.provider-select');
    if (!sel) {
      return;
    }
    var value = sel.value;
    dlg.classList.toggle('domain-dialog--wide', value === 'mx' || value === 'remotemx');
    var mxPanels = dlg.querySelectorAll('[data-mx-panel]');
    mxPanels.forEach(function (panel) {
      panel.hidden = value !== 'mx';
      panel.querySelectorAll('input,select,textarea').forEach(function (input) {
        input.disabled = value !== 'mx';
      });
    });
    var remoteMXPanel = dlg.querySelector('[data-remote-mx-panel]');
    if (remoteMXPanel) {
      remoteMXPanel.hidden = value !== 'remotemx';
      remoteMXPanel.querySelectorAll('input,select,textarea').forEach(function (input) {
        input.disabled = value !== 'remotemx';
      });
    }
    dlg.querySelectorAll('.provider-fields').forEach(function (group) {
      var active = group.getAttribute('data-provider') === value;
      group.hidden = !active;
      group.querySelectorAll('input,select,textarea').forEach(function (el) {
        el.disabled = !active;
      });
      if (active) {
        syncDialMXService(group);
        syncDependentFields(group);
      }
    });
    var save = dlg.querySelector('[data-save-provider]');
    if (save) {
      save.disabled = !value;
      syncSaveLabel(dlg);
    }
    var hint = dlg.querySelector('.provider-hint');
    if (hint) {
      hint.hidden = !!value;
    }
  }
  document.querySelectorAll('.domain-dialog').forEach(function (dlg) {
    var sel = dlg.querySelector('.provider-select');
    if (!sel) {
      return;
    }
    sel.addEventListener('change', function () {
      sync(dlg);
    });
    dlg.querySelectorAll('select[name="cfg_dialmx_service"]').forEach(function (service) {
      service.addEventListener('change', function () {
        syncDialMXService(service.closest('.provider-fields'));
      });
    });
    // Switching a field another field depends on re-runs the dependent-field
    // sync, so the DKIM record appears or disappears immediately.
    dlg.querySelectorAll('[data-provider] select, [data-provider] input').forEach(function (control) {
      control.addEventListener('change', function () {
        var group = control.closest('[data-provider]');
        if (!group) {
          return;
        }
        syncDependentFields(group);
        syncSaveLabel(dlg);
      });
    });
    sync(dlg);
  });
})();

(function () {
  document.querySelectorAll('form[data-antler-domain]').forEach(function (form) {
    var dlg = form.closest('dialog');
    var provider = form.querySelector('.provider-select');
    var group = form.querySelector('[data-provider="dialmx"]');
    if (!group) { return; }
    var service = group.querySelector('[name="cfg_dialmx_service"]');
    var email = group.querySelector('[name="cfg_dialmx_contact_email"]');
    var enforcement = group.querySelector('[name="cfg_dialmx_enforcement"]');
    var save = dlg.querySelector('[data-save-provider]');
    var oldSetup = dlg.querySelector('.dialmx-setup');
    var statusMode = !!oldSetup && provider.value === 'dialmx';
    var entered = statusMode;
    var custom = service.value === 'custom';
    // The saved status view draws its connector table from the names and live
    // statuses embedded on the form, so it opens complete and already matching
    // the dashboard light it was opened from; the first poll then reconciles it.
    var connectors = [];
    try { connectors = JSON.parse(form.dataset.antlerConnectors || '[]') || []; } catch (err) { connectors = []; }
    var initialStatuses = [];
    try { initialStatuses = JSON.parse(form.dataset.antlerStatus || '[]') || []; } catch (err) { initialStatuses = []; }
    // The copy-ready DNS records and the shared extra-MX verdict are seeded too,
    // so the status view's fix prompts and "additional MX servers" warning paint
    // on open rather than waiting for the first fetch to land.
    var initialInstructions = null;
    try { initialInstructions = JSON.parse(form.dataset.antlerInstructions || 'null'); } catch (err) { initialInstructions = null; }
    var initialUnexpected = [];
    try { initialUnexpected = JSON.parse(form.dataset.antlerUnexpected || '[]') || []; } catch (err) { initialUnexpected = []; }
    var initialDNS = [];
    try { initialDNS = JSON.parse(form.dataset.antlerDns || '[]') || []; } catch (err) { initialDNS = []; }
    // The recorded Tusk manifest source is seeded too, so the status view says
    // where its receiver set came from — the live GitHub manifest or the built-in
    // defaults — without waiting for the first fetch.
    var initialManifest = null;
    try { initialManifest = JSON.parse(form.dataset.antlerManifest || 'null'); } catch (err) { initialManifest = null; }
    var providerLabel = provider.previousElementSibling;
    var serviceLabel = service.previousElementSibling;
    var danger = dlg.querySelector('.dialog-danger');
    var rotate = dlg.querySelector('form[data-antler-regenerate]');
    if (statusMode && danger) {
      // Keep key rotation and removing the receiving config; drop the
      // provider-specific Worker-secret regenerate, which is not part of an
      // Tusk MX setup.
      Array.prototype.forEach.call(danger.children, function (child) {
        var button = child.querySelector('button');
        var remove = button && button.className.indexOf('danger') !== -1;
        child.hidden = child !== rotate && !remove;
      });
    }
    var close = dlg.querySelector('[data-close-dialog]');
    var endpoint = '/ui/domains/' + encodeURIComponent(form.dataset.antlerDomain) + '/receiving/setup';
    var step = 0, busy = false, ready = false, rotating = false;
    var state = null, savedEmail = null, timer = null;
    var wizard = document.createElement('section');
    wizard.className = 'antler-wizard';
    wizard.innerHTML = '<p class="antler-progress" aria-live="polite"></p>' +
      '<section data-antler-step="0"><h3>Contact email</h3></section>' +
      '<section data-antler-step="1" hidden><h3>Publish your DNS records</h3><p class="muted small">Add these records at your DNS provider. You can continue while DNS propagates.</p><div class="antler-records"></div></section>' +
      '<section data-antler-step="2" hidden><h3>Tusk receivers</h3><p class="muted small">One ready receiver is enough to continue. Other receivers can connect later.</p><div class="antler-receivers"></div></section>' +
      '<section data-antler-step="3" hidden><h3>Receiver authentication enforcement</h3><p class="muted small">Moderate marks mail as spam when DMARC fails, or both SPF and DKIM fail. Hard marks mail as spam when any of SPF, DKIM, or DMARC fails. Missing or inconclusive results alone do not count as failures.</p></section>' +
      '<section data-antler-step="4" hidden><h3>Custom receivers</h3><p class="muted small">Enter comma-separated HTTPS receiver URLs. Your receiver operator must also provide the SMTP hostname to use in your MX records.</p><label>Receiver URLs</label><input class="antler-custom-urls" placeholder="https://mx.example.com"></section>' +
      '<div class="antler-checks" hidden><p class="antler-check-note" role="status"></p><p class="antler-manifest-note muted small" hidden></p></div>' +
      '<p class="antler-error error" role="alert" hidden></p>';
    // A plain-language intro shown on the first step only, mirroring the
    // "Inbound: Tusk MX" extract in docs/EMAIL_PROVIDERS.md: what the service
    // is, the mail-flow diagram, and the outbound-only connection, without the
    // setup steps the wizard already walks through.
    var intro = document.createElement('section');
    intro.className = 'antler-intro';
    intro.innerHTML = '<p>Tusk MX is a <b>free hosted SMTP receiver for your domain</b>. It is included as a receiving option in Mammoth Mail. You do not need to run another container, open inbound SMTP ports, or expose your web interface to the internet.</p>' +
      '<figure><img src="/assets/tusk-mx-diagram.svg" alt="Public senders deliver to the hosted Tusk relay; Mammoth Mail connects to it outbound, so no inbound port forwarding is needed."></figure>' +
      '<p>Mammoth Mail makes an <b>outbound connection</b> to the hosted Tusk receivers. Tusk accepts incoming SMTP and passes messages to Mammoth Mail over that connection. Tusk is a receiving relay, not a hosted mailbox or an outgoing mail service.</p>' +
      '<p class="muted small">Tusk MX is a community service, provided on a best-efforts basis — there is no uptime guarantee.</p>';
    group.appendChild(wizard);
    wizard.insertBefore(intro, wizard.querySelector('[data-antler-step="0"]'));
    function moveField(input, index) {
      var label = input.previousElementSibling;
      var panel = wizard.querySelector('[data-antler-step="' + index + '"]');
      if (label && label.tagName === 'LABEL') { panel.appendChild(label); }
      panel.appendChild(input);
    }
    moveField(email, 0);
    moveField(enforcement, 3);
    var advancedLabel = document.createElement('label');
    var advanced = document.createElement('input');
    advanced.type = 'checkbox'; advanced.checked = custom;
    advanced.className = 'antler-advanced';
    advanced.style = 'width:auto;margin-right:8px';
    advancedLabel.appendChild(advanced);
    advancedLabel.appendChild(document.createTextNode('Advanced: use custom receivers'));
    wizard.querySelector('[data-antler-step="0"]').appendChild(advancedLabel);
    var urls = wizard.querySelector('.antler-custom-urls');
    var existingURLs = group.querySelector('[name="cfg_dialmx_receiver_urls"]');
    urls.value = existingURLs ? existingURLs.value : '';
    advanced.addEventListener('change', function () { custom = advanced.checked; update(); });
    var panels = wizard.querySelectorAll('[data-antler-step]');
    var checksRow = wizard.querySelector('.antler-checks');
    var note = wizard.querySelector('.antler-check-note');
    var error = wizard.querySelector('.antler-error');
    // The saved status view carries its editable settings (authentication
    // enforcement and contact email) below the status records, so the status
    // picture reads first. The two field groups are the same nodes the wizard
    // steps use; placeFields moves them between the settings section and their
    // wizard steps as the mode changes.
    var emailLabel = email.previousElementSibling;
    var enforcementLabel = enforcement.previousElementSibling;
    var settings = document.createElement('section');
    settings.className = 'antler-settings';
    settings.setAttribute('data-antler-settings', '');
    var settingsHeading = document.createElement('h3');
    settingsHeading.textContent = 'Settings';
    settings.appendChild(settingsHeading);
    // The inherited setup is read-only: its settings belong to the owning domain.
    // That caveat is the only note the section needs; the non-inherited view is
    // self-explanatory from the field labels alone.
    var inherited = form.dataset.antlerInherited || '';
    if (inherited) {
      settingsHeading.textContent = 'Settings (inherited)';
      var settingsNote = document.createElement('p');
      settingsNote.className = 'muted small';
      settingsNote.textContent = 'Inherited from ' + inherited + ' — edit these on that domain.';
      settings.appendChild(settingsNote);
    }
    wizard.appendChild(settings);
    var step0Panel = wizard.querySelector('[data-antler-step="0"]');
    var step3Panel = wizard.querySelector('[data-antler-step="3"]');
    function placeFields() {
      var target = statusMode ? settings : null;
      [[emailLabel, email, target || step0Panel], [enforcementLabel, enforcement, target || step3Panel]]
        .forEach(function (entry) {
          var label = entry[0], input = entry[1], dest = entry[2];
          if (label && label.parentNode !== dest) { dest.appendChild(label); }
          if (input.parentNode !== dest) { dest.appendChild(input); }
        });
    }
    // Back is part of the dialog's bottom navigation, not the upper form.
    var footer = save.closest('.dialog-actions');
    var back = document.createElement('button');
    back.type = 'button'; back.className = 'secondary antler-back'; back.textContent = 'Back'; back.hidden = true;
    if (footer) { footer.insertBefore(back, footer.firstChild); }
    function active() { return provider.value === 'dialmx'; }
    function fail(message) { error.textContent = message; error.hidden = !message; }
    function api(method, config, regenerate) {
      // Every read asks for the receiving picture, and the server asks its
      // watcher to re-check when the cached record verdict has gone stale, so a
      // render never blocks on DNS and no extra query flag is needed.
      var url = endpoint;
      var options = { method: method, cache: 'no-store', headers: { 'Accept': 'application/json' } };
      if (config || regenerate) {
        options.headers['Content-Type'] = 'application/json';
        options.headers['X-CSRF-Token'] = form.querySelector('[name="_csrf"]').value;
        options.body = JSON.stringify(regenerate ? { provider: 'dialmx', regenerate_secret: true } : { provider: 'dialmx', config: config });
      }
      return fetch(url, options).then(function (response) {
        if (!response.ok) { throw new Error('Unable to ' + (method === 'GET' ? 'check' : 'save') + ' settings. Please try again.'); }
        return response.json();
      });
    }
    function isReady(status) {
      return status.state === 'ready' && (!status.expires_at || Date.parse(status.expires_at) > Date.now());
    }
    // Local DNS can still cache a negative answer after a receiver proves routing.
    function receiverDNS(statuses, kind, hostname) {
      var relevant = (statuses || []).filter(function (status) {
        return !hostname || String(status.smtp_hostname || '').toLowerCase().replace(/\.$/, '') === String(hostname).toLowerCase().replace(/\.$/, '');
      });
      if (relevant.some(isReady)) { return { kind: kind, state: 'ok' }; }
      var reason = kind === 'mx' ? 'not_mx' : 'key_unavailable';
      if (relevant.some(function (status) { return status.state === 'rejected' && status.reason === reason; })) {
        return { kind: kind, state: 'mismatch' };
      }
      return { kind: kind, state: null };
    }
    function receivingDNS(statuses) {
      var checks = [receiverDNS(statuses, 'txt')];
      // A ready sibling must not hide another receiver's routing refusal.
      if ((statuses || []).some(function (status) { return !isReady(status) && status.state === 'rejected' && status.reason === 'not_mx'; })) {
        checks.push({ kind: 'mx', state: 'mismatch' });
      }
      return checks;
    }
    // The status view offers Save settings only once something actually differs
    // from what is stored; otherwise Close is the only action.
    function settingsDirty() {
      var cfg = (state && state.config) || {};
      var savedContact = cfg.contact_email || '';
      var savedEnforcement = cfg.enforcement || 'moderate';
      if (inherited) { return false; }
      return email.value.trim() !== savedContact || enforcement.value !== savedEnforcement;
    }
    // A receiver row reads its real state: red is a fact about the receiver (it
    // refused, it is gone, or the core cannot reach it at all), amber is only
    // ever "in progress" — an authorization or a physical attempt still running.
    function receiverLightClass(status) {
      if (isReady(status)) { return 'green'; }
      return ['connecting', 'deferred'].indexOf(status.state) !== -1 ? 'amber' : 'red';
    }
    // receiverLabel renders the specific Tusk reason next to the traffic light,
    // not a generic word: the receiver's own verdict ("not in MX", "key
    // unavailable", "authentication failed") is the most precise thing we know
    // about that connector. The raw reason stays in the row's tooltip. The HTTP
    // status the receiver URL answered with is named too: whatever answered at
    // that URL — the receiver, or a proxy in front of it — owns that status, and
    // the core cannot tell the two apart.
    function receiverLabel(status) {
      var label = receiverLabelText(status);
      if (status.http_code) { label += ' (HTTP ' + status.http_code + ')'; }
      return label;
    }
    // receiverLabelText is the receiver's verdict on its own, without the HTTP
    // status the receiver URL answered with.
    function receiverLabelText(status) {
      if (isReady(status)) { return 'Ready to receive'; }
      switch (status.reason) {
        case 'not_mx': return 'Not in MX';
        case 'key_unavailable': return 'Key unavailable';
        case 'authentication_failed': return 'Authentication failed';
        case 'dns_unavailable': return 'DNS unavailable';
        case 'domain_limit': return 'Domain limit';
        case 'domain_cooldown': return 'Waiting to retry';
        // A settled refusal: the receiver answered but will not serve this
        // session, so the row is an error rather than a retry in progress. One
        // word covers every refused code, because at this layer a 401 and a 404
        // are both a receiver or proxy that is not serving this core.
        case 'session_rejected': return 'Receiver refused the session';
        // The receiver URL answered with its own error status: the receiver is
        // up and is expected to recover, so the row stays a retrying session.
        case 'receiver_error': return 'Receiver reported a server error';
        case 'http2_required': return 'Receiver speaks HTTP/1.1, not HTTP/2';
        case 'mode_mismatch': return 'Wrong receiver mode';
        case 'protocol_mismatch': return 'Receiver protocol mismatch';
        case 'destination_not_allowed': return 'Blocked — receiver destination not allowed';
        case 'replaced': return 'Replaced by another connection';
      }
      // Ready when this page was rendered, but the authorization window has
      // since closed and a renewal has not landed: not an authorization still
      // in progress.
      if (status.state === 'ready') { return 'Authorization expired'; }
      return {
        connecting: 'Authorizing',
        disconnected: 'Reconnecting',
        rejected: 'Rejected',
        unavailable: 'Unavailable',
        unreachable: 'Receiver unreachable',
        deferred: 'Waiting to retry'
      }[status.state] || 'Waiting';
    }
    // copyField copies one exact string to the clipboard, selecting it with the
    // shared manual-copy message when the clipboard is unavailable (plain HTTP).
    function copyField(text) {
      var copy = document.createElement('button');
      copy.type = 'button'; copy.className = 'secondary'; copy.textContent = 'Copy';
      window.mammothmailCopy.wire(copy, {
        text: text,
        select: function () { return copy.previousElementSibling; }
      });
      return copy;
    }
    // remediationLine renders one failing record to publish on the same status
    // screen: the prompt, then each part of the record on its own labelled row.
    // A field is [label, text, copyable]; copyable fields carry their own copy
    // button so neither half has to be picked apart. The live Check now /
    // auto-poll flips the row once DNS propagates, so no wizard jump is needed.
    function remediationLine(prompt, fields) {
      var box = document.createElement('div');
      var p = document.createElement('p');
      p.textContent = prompt;
      box.appendChild(p);
      fields.forEach(function (field) {
        var line = document.createElement('div');
        line.className = 'dns-remediation-row';
        var label = document.createElement('span');
        label.className = 'dns-remediation-label';
        label.textContent = field[0];
        var code = document.createElement('code');
        code.className = 'dns-remediation-value';
        code.textContent = field[1];
        line.appendChild(label); line.appendChild(code);
        if (field[2]) { line.appendChild(copyField(field[1])); }
        box.appendChild(line);
      });
      return box;
    }
    // Repair only explicit receiver refusals, never an independent cached lookup.
    function remediationBlock(dns, instructions, mxName) {
      var byKind = {};
      (dns || []).forEach(function (entry) { if (entry && entry.kind) { byKind[entry.kind] = entry; } });
      var txt = byKind.txt, mx = byKind.mx;
      var block = document.createElement('div');
      var needTXT = txt && txt.state && txt.state !== 'ok';
      var needMX = mx && mx.state && mx.state !== 'ok';
      if (!needTXT && !needMX) { return block; }
      block.className = 'antler-dns-remediation';
      if (needTXT && instructions.txt_value) {
        var txtPrompt = 'Domain not authorized — a receiver could not verify this domain\x27s TXT key. Check this TXT record:';
        block.appendChild(remediationLine(txtPrompt, [
          ['Record name', instructions.txt_name, true],
          ['Record type', 'TXT', false],
          ['Record value', instructions.txt_value, true]
        ]));
      }
      if (needMX && (instructions.mx || []).length) {
        var mxPrompt = 'A Tusk receiver reports that its hostname is not in this domain\x27s MX records. Check these MX records:';
        var mxRecords = instructions.mx || [];
        if (mxRecords.length) { block.appendChild(remediationLine(mxPrompt, [])); }
        mxRecords.forEach(function (record, index) {
          var fields = [['Record name', mxName, true], ['Record type', 'MX', false]];
          if (record.priority) { fields.push(['Priority', String(record.priority), true]); }
          fields.push(['Record value', record.hostname, true]);
          block.appendChild(remediationLine('MX record ' + (index + 1) + ':', fields));
        });
      }
      return block;
    }
    // unexpectedMXBlock warns that the domain's MX records name servers which are
    // not Tusk receivers. Mail may still be delivered via a ready Tusk
    // receiver, but routing is ambiguous, so the dashboard light is amber and this
    // block names the offending servers and suggests removing them. It is empty
    // when no extra records are published.
    function unexpectedMXBlock(data, domainName) {
      var block = document.createElement('div');
      var extra = data.unexpected_mx || [];
      if (!extra.length) { return block; }
      block.className = 'antler-dns-remediation antler-extra-mx';
      var p = document.createElement('p');
      p.textContent = 'Additional MX servers exist for ' + domainName + '. Mail may not be delivered via Tusk. Remove these records unless you intend them:';
      block.appendChild(p);
      extra.forEach(function (host) {
        var line = document.createElement('div');
        line.className = 'dns-remediation-row';
        var label = document.createElement('span');
        label.className = 'dns-remediation-label';
        label.textContent = 'Extra MX';
        var code = document.createElement('code');
        code.className = 'dns-remediation-value';
        code.textContent = host;
        line.appendChild(label); line.appendChild(code);
        block.appendChild(line);
      });
      return block;
    }
    // pendingCell is one status-table cell for a connector with no live status
    // yet: an amber dot and a "Pending" label, shaped like a live status cell so
    // the two are indistinguishable to a reader. While a check is in flight the
    // amber dots animate (see .antler-checking).
    function pendingCell(parent) {
      var td = document.createElement('td');
      var dot = document.createElement('span');
      dot.className = 'antler-light amber';
      var label = document.createElement('span');
      label.textContent = 'Pending';
      td.appendChild(dot);
      td.appendChild(label);
      parent.appendChild(td);
      return td;
    }
    // statusRow appends one connector's row: its name, then a live status cell
    // when a status is supplied or an amber "Pending" cell when none is. A live
    // cell reads plain text, matching the table's textContent shape.
    function statusRow(tbody, name, status) {
      var tr = document.createElement('tr');
      var connector = document.createElement('td');
      connector.textContent = name;
      tr.appendChild(connector);
      if (status) {
        var td = document.createElement('td');
        var dot = document.createElement('span');
        dot.className = 'antler-light ' + receiverLightClass(status);
        var label = document.createElement('span');
        label.textContent = receiverLabel(status);
        if (status.reason) { label.title = status.reason; }
        td.appendChild(dot);
        td.appendChild(label);
        tr.appendChild(td);
      } else {
        pendingCell(tr);
      }
      tbody.appendChild(tr);
    }
    // statusTable builds the saved-status connector table. Each row is named by a
    // connector hostname and its status light is filled from the live status set
    // when one matches: seeded on open from the embedded data-antler-status, then
    // refreshed on every poll. A connector with no live status yet reads amber
    // "Pending". The connector names are the only thing not known from live
    // status, so they come from data-antler-connectors; a status with no matching
    // connector (a custom-service receiver_url, which has no embedded MX list) is
    // appended so it is never dropped.
    function statusTable(connectors, statuses) {
      var byHost = {};
      var byURL = {};
      (statuses || []).forEach(function (status) {
        if (status.smtp_hostname) { byHost[status.smtp_hostname] = status; }
        if (status.receiver_url) { byURL[status.receiver_url] = status; }
      });
      var used = [];
      var table = document.createElement('table');
      table.className = 'antler-dns-table antler-status-table';
      var head = document.createElement('tr');
      ['Connector', 'Status'].forEach(function (title) {
        var th = document.createElement('th');
        th.textContent = title;
        head.appendChild(th);
      });
      var thead = document.createElement('thead'); thead.appendChild(head); table.appendChild(thead);
      var tbody = document.createElement('tbody');
      (connectors || []).forEach(function (mx) {
        var status = byHost[mx.hostname] || byURL[mx.session_url];
        if (status) { used.push(status); }
        statusRow(tbody, mx.hostname, status);
      });
      (statuses || []).forEach(function (status) {
        if (used.indexOf(status) !== -1) { return; }
        statusRow(tbody, status.smtp_hostname || status.receiver_url, status);
      });
      if (!tbody.children.length) {
        var waiting = document.createElement('tr');
        var cell = document.createElement('td');
        cell.colSpan = 2;
        cell.textContent = 'No connectors configured';
        waiting.appendChild(cell);
        tbody.appendChild(waiting);
      }
      table.appendChild(tbody);
      return table;
    }
    // renderStatusRecords paints the saved status view's record area from the
    // status set, the copy-ready instructions, the DNS verdict and the extra-MX
    // verdict. Both the on-open seed (renderSkeleton, from the embedded attributes)
    // and the live render (from a fetch or an SSE push) go through it, so the
    // dialog opens complete — rows, fix prompts and the extra-MX warning — and then
    // reconciles on the same code path rather than flashing rows without their
    // remediation.
    function renderStatusRecords(statuses, instructions, dns, unexpected, domainName, mxName) {
      var records = wizard.querySelector('.antler-records');
      records.replaceChildren();
      records.appendChild(statusTable(connectors, statuses));
      records.appendChild(unexpectedMXBlock({ unexpected_mx: unexpected || [] }, domainName));
      records.appendChild(remediationBlock(receivingDNS(statuses), instructions || {}, mxName));
    }
    // mxRecordName is the name column for the Tusk MX rows. A two-label name
    // is a zone apex and reads best as the provider's "@". A name with more
    // labels is a subdomain of some zone, so it must never be "@": that would
    // publish the MX on the parent zone and send mail to the wrong domain. Spell
    // out the fully-qualified name instead, which every DNS provider accepts and
    // which always lands on this exact domain. Deriving this from the name's own
    // shape (not the account's parent-domain link) is deliberate: a subdomain
    // added standalone has no parent link, and keying off that link is what made
    // a subdomain render "@".
    function mxRecordName(domainName) {
      var labels = String(domainName || '').split('.').filter(Boolean);
      return labels.length === 2 ? '@' : (domainName || '@');
    }
    // renderManifest states where the domain's receiver set came from, so an
    // operator can tell a live GitHub fetch from the built-in defaults. The record
    // is fixed when the set is snapshotted: a re-save never re-fetches, so it
    // describes the receivers the published MX records already point at, not the
    // manifest's state today.
    function renderManifest(manifest) {
      var manifestNote = wizard.querySelector('.antler-manifest-note');
      if (!manifest) { manifestNote.hidden = true; manifestNote.textContent = ''; return; }
      if (manifest.source === 'embedded') {
        manifestNote.textContent = 'Could not fetch Tusk receivers from GitHub — using built-in defaults';
      } else {
        var when = '';
        var at = manifest.at ? new Date(manifest.at) : null;
        // An unparsable timestamp still names the source; only the time is dropped.
        if (at && !isNaN(at.getTime())) { when = ' at ' + at.toLocaleTimeString(); }
        manifestNote.textContent = 'Tusk receivers updated from GitHub' + when;
      }
      manifestNote.hidden = false;
    }
    function renderSkeleton(connectors) {
      var instructions = initialInstructions || {};
      var domainName = form.dataset.antlerDomainName || '';
      renderStatusRecords(initialStatuses, instructions, initialDNS, initialUnexpected, domainName, mxRecordName(domainName));
      renderManifest(initialManifest);
    }
    // renderReceivers fills the wizard's own receivers step (used while setting
    // up, not the saved status view) from the live status set.
    function renderReceivers(statuses) {
      var receivers = wizard.querySelector('.antler-receivers'); receivers.replaceChildren();
      (statuses || []).forEach(function (status) {
        var row = document.createElement('p');
        row.textContent = (status.smtp_hostname || status.receiver_url) + ' — ' + receiverLabel(status) + (status.reason ? ' · ' + status.reason : '');
        var light = document.createElement('span');
        light.className = 'antler-light ' + receiverLightClass(status);
        row.prepend(light);
        receivers.appendChild(row);
      });
      if (!(statuses || []).length) { receivers.textContent = 'Waiting for receiver status…'; }
    }
    function render(data) {
      state = data;
      note.dataset.checkedAt = new Date().toLocaleTimeString();
      countdown = null;
      // The poll carries the record on every read; the seeded copy covers a
      // payload that predates it (or no save yet), so the line never flashes off.
      renderManifest(data.receiver_manifest || initialManifest);
      var instructions = data.instructions || {};
      ready = (data.status || []).some(isReady);
      var records = wizard.querySelector('.antler-records');
      records.replaceChildren();
      var domainName = form.dataset.antlerDomainName || '';
      // A subdomain must publish its MX at its own fully-qualified name, never
      // the parent zone's "@" (see mxRecordName).
      var mxName = mxRecordName(domainName);
      // The status view has one light per receiver: the receiver proves both
      // domain authority (TXT) and routing (its own hostname in the domain's
      // MX), so its single state carries the whole verdict. The table is built
      // by statusTable from the connector set embedded on the form, so it opens
      // complete and matches the dashboard light, then reconciles on each poll.
      // A connector the poll did not report keeps its "Pending" row.
      if (statusMode) {
        // The status view shares one paint path with the on-open seed, so rows,
        // fix prompts and the extra-MX warning are always consistent. The pushed
        // verdict is never merged in from a client cache: the fetch's value is the
        // server's, and a push simply triggers another fetch.
        renderStatusRecords(data.status, instructions, data.dns || [], data.unexpected_mx || [], domainName, mxName);
        renderReceivers(data.status);
        update();
        return;
      }
      // Record lights follow receiver proof; no report is unknown, not missing.
      function light(state, title) {
        var span = document.createElement('span');
        span.className = 'antler-light ' + (state === 'ok' ? 'green' : state === null ? 'amber' : 'red');
        span.title = title || '';
        var text = document.createElement('span');
        text.textContent = state === 'ok' ? 'Matching'
          : state === 'mismatch' ? 'Mismatch'
          : state === 'not_found' ? 'Not found'
          : state === 'dns_error' ? 'DNS error'
          : state === null ? 'Waiting'
          : 'Not found';
        text.title = title || '';
        return { span: span, text: text };
      }
      function lightFor(entry) {
        return light(!entry ? null : entry.state, (entry && entry.reason) || '');
      }
      var table = document.createElement('table');
      table.className = 'antler-dns-table antler-record-table';
      var head = document.createElement('tr');
      ['Name', 'Type', 'Priority', 'Value', 'Status', ''].forEach(function (title) {
        var th = document.createElement('th');
        th.textContent = title;
        head.appendChild(th);
      });
      var thead = document.createElement('thead'); thead.appendChild(head); table.appendChild(thead);
      var tbody = document.createElement('tbody');
      function statusCell(parent, light) {
        var td = document.createElement('td');
        td.appendChild(light.span);
        td.appendChild(light.text);
        parent.appendChild(td);
        return td;
      }
      function recordRow(name, type, priority, value, rowLight) {
        var tr = document.createElement('tr');
        tr.dataset.dnsKind = type.toLowerCase();
        [name, type, priority, value].forEach(function (text, index) {
          var td = document.createElement('td');
          td.textContent = text;
          if (index === 0) { td.className = 'antler-name'; }
          if (index === 3) { td.className = 'antler-value'; }
          tr.appendChild(td);
        });
        statusCell(tr, rowLight);
        var action = document.createElement('td');
        var copy = document.createElement('button');
        copy.type = 'button'; copy.className = 'secondary'; copy.textContent = 'Copy value';
        window.mammothmailCopy.wire(copy, {
          text: value,
          select: function () { return tr.querySelector('.antler-value'); }
        });
        action.appendChild(copy); tr.appendChild(action); tbody.appendChild(tr);
      }
      (instructions.mx || []).forEach(function (mx) { recordRow(mxName, 'MX', String(mx.priority), mx.hostname, lightFor(receiverDNS(data.status, 'mx', mx.hostname))); });
      if (instructions.txt_value) { recordRow(instructions.txt_name, 'TXT', '—', instructions.txt_value, lightFor(receiverDNS(data.status, 'txt'))); }
      table.appendChild(tbody);
      records.appendChild(table);
      renderReceivers(data.status);
      // The receivers step of the new-setup wizard carries the same inline
      // remediation, so an operator completing setup can publish the MX record
      // they need without leaving the step.
      wizard.querySelector('.antler-receivers').appendChild(remediationBlock(receivingDNS(data.status), instructions, mxName));
      update();
    }
    function update() {
      var enabled = active();
      dlg.classList.toggle('antler-dialog', enabled);
      if (rotate) { rotate.querySelector('button').disabled = busy || rotating; }
      provider.disabled = busy;
      service.disabled = busy || provider.value !== 'dialmx';
      provider.hidden = enabled && entered;
      if (providerLabel) { providerLabel.hidden = provider.hidden; }
      service.hidden = true;
      if (serviceLabel) { serviceLabel.hidden = true; }
      wizard.hidden = provider.value !== 'dialmx';
      // Keep the generic custom-service controls available outside the wizard.
      Array.prototype.forEach.call(group.children, function (child) {
        if (child !== wizard && child.tagName !== 'TEMPLATE') { child.hidden = enabled; }
      });
      email.type = enabled ? 'email' : 'text';
      email.required = enabled && !custom;
      email.disabled = !enabled || (custom && step !== 0);
      advanced.disabled = !enabled || busy;
      urls.disabled = !enabled;
      if (oldSetup) { oldSetup.hidden = enabled || provider.value !== 'dialmx'; }
      if (!enabled) {
        // The moved fields still belong to the custom form.
        wizard.hidden = provider.value !== 'dialmx';
        panels.forEach(function (panel, index) { panel.hidden = index !== 0 && index !== 3; });
        wizard.querySelector('.antler-progress').hidden = true;
        back.hidden = true;
        wizard.querySelector('.antler-checks').hidden = true;
        wizard.querySelectorAll('h3, [data-antler-step] > p').forEach(function (el) { el.hidden = true; });
        if (provider.value === 'dialmx') { save.textContent = 'Save'; save.disabled = false; }
        return;
      }
      wizard.querySelectorAll('h3, [data-antler-step] > p').forEach(function (el) { el.hidden = false; });
      wizard.querySelector('.antler-progress').hidden = statusMode;
      wizard.querySelector('.antler-progress').textContent = rotating ? 'New receiving key · DNS → Receivers' : custom ? 'Custom Tusk MX · Contact → Receiver URLs → DNS → Receivers → Enforcement' : 'Tusk MX · Contact → DNS → Receivers → Enforcement';
      wizard.querySelector('[data-antler-step="1"] h3').textContent = rotating ? 'Replace your authorization TXT record' : statusMode ? 'Tusk MX status' : 'Publish your DNS records';
      wizard.querySelector('[data-antler-step="1"] > p').textContent = rotating ? 'The previous key is invalid. Replace the existing authorization TXT record with the new value below. Your MX records stay the same. Receiving can resume once DNS propagates and a receiver reconnects.' : 'Add these records at your DNS provider. You can continue while DNS propagates.';
      // The saved-status view replaces the separate receiver panel with a
      // per-receiver status table and an inline remediation block.
      panels.forEach(function (panel, index) {
        panel.hidden = statusMode ? (index === 2 || index === 3 || index === 4 || (index === 0 && custom)) : index !== step;
      });
      advancedLabel.hidden = statusMode;
      // The intro belongs to the hosted Tusk service and reads once, so it
      // shows on the first step only — not in the saved status view, and not
      // when the operator has switched to custom receivers.
      intro.hidden = statusMode || custom || step !== 0;
      if (statusMode) {
        wizard.querySelector('[data-antler-step="0"] h3').hidden = true;
        var contactDescription = wizard.querySelector('[data-antler-step="0"] p');
        if (contactDescription) { contactDescription.hidden = true; }
        wizard.querySelector('[data-antler-step="1"] h3').textContent = 'Tusk MX status';
        wizard.querySelector('[data-antler-step="2"] p').hidden = true;
      }
      if (statusMode) { wizard.querySelector('[data-antler-step="1"] > p').hidden = true; }
      // The footer's close control reads "Close" once the saved status view is
      // showing and "Cancel" while the setup wizard is still running. It is
      // relabelled here on every update, not once at init, because the wizard
      // flips statusMode to true when it finishes: a one-time label left the
      // freshly-finished status view with a stray "Cancel".
      if (close) { close.textContent = statusMode ? 'Close' : 'Cancel'; }
      // Rotation's new-TXT step cannot go back to the pre-rotation state, but
      // its receiver step can.
      back.hidden = statusMode || (step === 0 && !entered) || (rotating && step === 1);
      back.disabled = busy;
      wizard.querySelector('.antler-checks').hidden = statusMode ? false : (step !== 1 && step !== 2);
      placeFields();
      settings.hidden = !statusMode;
      save.hidden = statusMode ? (custom || inherited || !settingsDirty()) : false;
      save.textContent = busy ? 'Please wait…' : statusMode ? 'Save settings' : (rotating && step === 2) || step === 3 ? 'Finish' : 'Next';
      // A receiving config this wizard stores must be removable from the wizard.
      // The server draws the footer's danger block hidden for a domain that had no
      // receiving config when the page was rendered, so the first save reveals the
      // same Remove receiving the saved status view shows — otherwise an operator
      // who starts a setup from nothing has no way back but closing the dialog and
      // reopening it. It is only ever revealed: a config already stored keeps its
      // control, and one this wizard has not stored yet leaves it hidden.
      if (danger && danger.hidden && state && state.config && state.config.service) {
        danger.hidden = false;
      }
      if (state) { ready = (state.status || []).some(isReady); }
      // Only the receivers step requires a ready receiver to advance. Key
      // rotation and the enforcement-step Finish are never readiness-gated: the
      // receiver re-authorizes on its own and the status view shows it as in
      // progress.
      save.disabled = busy || (!statusMode && !rotating && step === 2 && !ready);
      // While a check is in flight, pending amber dots animate as spinners so a
      // not-yet-resolved connector or DNS record reads as "in progress" rather
      // than a settled amber state.
      wizard.querySelector('.antler-records').classList.toggle('antler-checking', busy);
      wizard.querySelector('.antler-receivers').classList.toggle('antler-checking', busy);
      // The countdown is a wizard affordance only: the saved status view is
      // buttonless and counter-free (it reconciles on SSE and a slow poll).
      var showChecks = !statusMode && (step === 1 || step === 2);
      checksRow.hidden = !showChecks;
      if (showChecks) { renderCountdown(); }
    }
    // refresh re-reads the server's picture for this dialog. It coalesces: a
    // request that arrives while one is in flight is latched and run on
    // completion, so a slow poll cannot stack fetches, and a hidden dialog
    // defers until it is visible again rather than fetching in a background tab.
    var pendingRefresh = false, staleRetry = null;
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden && pendingRefresh) { refresh(); }
    });
    function onRefreshable() {
      if (!active() || !dlg.open) { return false; }
      if (!statusMode && (step === 0 || step === 3 || step === 4)) { return false; }
      return true;
    }
    function refresh() {
      if (!onRefreshable()) { return; }
      if (document.hidden) { pendingRefresh = true; return; }
      if (busy) { pendingRefresh = true; return; }
      if (staleRetry !== null) { window.clearTimeout(staleRetry); staleRetry = null; }
      pendingRefresh = false;
      busy = true;
      note.textContent = 'Checking…'; note.setAttribute('aria-busy', 'true'); update();
      api('GET').then(function (data) {
        render(data); fail('');
        // Allow the asynchronous watcher time to finish; never spin on a stale GET.
        if (data.dns_stale && dlg.open) {
          staleRetry = window.setTimeout(function () { staleRetry = null; refresh(); }, 2000);
        }
      }).catch(function (err) { fail(err.message); }).finally(function () {
        busy = false;
        note.removeAttribute('aria-busy');
        update();
        if (pendingRefresh) { pendingRefresh = false; refresh(); }
      });
    }
    // The wizard shows a live countdown beside the spinner while a data step is
    // open, so an operator watching a record propagate can see the check is
    // still running rather than assuming it stalled. The saved status view is
    // deliberately counter-free.
    var countdown = null, countdownDue = 0;
    var WIZARD_POLL_S = 10;
    function renderCountdown() {
      if (!note.parentNode) { return; }
      if (note.getAttribute('aria-busy') === 'true') {
        note.textContent = 'Checking…';
        countdown = null;
        return;
      }
      if (countdown === null) { countdown = WIZARD_POLL_S; }
      note.textContent = checkedLabel() + ' · checking again in ' + countdown + 's';
    }
    // checkedLabel reports the time of the last completed check, so the countdown
    // reads as "we last looked at HH:MM:SS" rather than an unanchored timer.
    function checkedLabel() {
      if (!note.dataset.checkedAt) { return 'Checking'; }
      return 'Checked ' + note.dataset.checkedAt;
    }
    // The countdown ticks once a second while a wizard data step is open and the
    // dialog is visible; at zero it asks for a refresh. It is paused (not reset)
    // while a fetch is in flight, so it resumes from the last value.
    function tickCountdown() {
      if (!active() || !dlg.open || document.hidden) { countdown = null; return; }
      if (statusMode || (step !== 1 && step !== 2) || note.getAttribute('aria-busy') === 'true') { return; }
      if (countdown === null) { countdown = WIZARD_POLL_S; }
      countdown -= 1;
      if (countdown <= 0) { countdown = null; refresh(); }
      else { renderCountdown(); }
    }
    email.addEventListener('input', update);
    if (rotate) {
      rotate.addEventListener('submit', function (event) {
        if (!active()) { return; }
        var cancelled = event.defaultPrevented;
        event.preventDefault();
        if (cancelled) { return; }
        if (busy || rotating) { return; }
        // The shared data-confirm handler runs first. Cancellation must never
        // reach the rotation endpoint.
        busy = true; fail(''); update();
        api('PUT', null, true).then(function (data) {
          rotating = true; statusMode = false; entered = true; step = 1;
          state = null; ready = false;
          render(data);
        }).catch(function () {
          fail('Unable to confirm key rotation. Check the current DNS instructions before trying again; the key may already have changed.');
        }).finally(function () { busy = false; update(); });
      });
    }
    back.addEventListener('click', function () {
      if (step === 0) { entered = false; }
      else if (step === 4) { step = 0; }
      else if (step === 1 && custom) { step = 4; }
      else { step = Math.max(0, step - 1); }
      fail(''); update();
    });
    form.addEventListener('submit', function (event) {
      if (!active()) { return; }
      event.preventDefault();
      if (busy || (!statusMode && !rotating && step === 2 && !ready)) { return; }
      // Rotation finishes regardless of readiness; the receiver re-authorizes on
      // its own and the status view reports it as in progress.
      if (rotating && step === 2) {
        busy = true; update();
        api('GET').then(function (data) {
          rotating = false; statusMode = true; render(data); fail('');
        }).catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
        return;
      }
      if (statusMode) {
        if (custom || inherited || !settingsDirty() || !email.reportValidity()) { return; }
        busy = true; update();
        api('POST', { contact_email: email.value, enforcement: enforcement.value }).then(function (data) { render(data); fail(''); })
          .catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
        return;
      }
      entered = true;
      if (step === 0 && custom) { step = 4; update(); return; }
      if (step === 1 || step === 2) { step++; update(); if (step === 2) { refresh(); } return; }
      if (!custom && !email.reportValidity()) { return; }
      if (step === 4 && !urls.value.trim()) { fail('Enter at least one receiver URL.'); return; }
      if (step === 0 && savedEmail === email.value && state) { step = 1; update(); return; }
      busy = true; fail(''); update();
      var finishing = step === 3;
      var beforeSave = finishing ? api('GET').then(function (data) {
        // A receiver reconnecting must not trap Finish or discard the chosen policy.
        render(data);
      }) : api('GET').then(function (data) {
        // Reopening an existing setup must retain its enforcement until the
        // final step, and must not unnecessarily resolve a new receiver set.
        if (data.provider === 'dialmx' && data.config.service === (custom ? 'custom' : 'hosted')) { state = data; }
      });
      beforeSave.then(function () {
        if (!finishing && !custom && state && state.config.contact_email === email.value) { return state; }
        var config = { service: custom ? 'custom' : 'hosted', enforcement: finishing ? enforcement.value : (state && state.config.enforcement || 'moderate') };
        if (custom) { config.receiver_urls = urls.value; } else { config.contact_email = email.value; }
        return api('PUT', config);
      }).then(function (data) {
        if (!finishing) { enforcement.value = data.config.enforcement || 'moderate'; }
        savedEmail = email.value; render(data);
        if (finishing) { statusMode = true; render(data); fail(''); dlg.close(); }
        else { step = 1; }
      }).catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
    });
    provider.addEventListener('change', update);
    service.addEventListener('change', update);
    enforcement.addEventListener('change', update);
    // Live updates: the dialog follows the single shared SSE connection the
    // dashboard owns (never a second stream), attaching its own listeners for
    // this domain's receiver-health and DNS pushes and detaching them when the
    // stream is replaced or the page unloads. Both pushes simply ask for a
    // refresh: the server snapshot is the one truth, so there is no second,
    // client-side copy of the verdict to drift or to miss a change while busy.
    var attachedTo = null;
    function domainMatches(event) {
      var changed = '';
      try { changed = ((JSON.parse(event.data || '{}') || {}).payload || {}).domain_id || ''; } catch (err) { changed = ''; }
      return !changed || !form.dataset.antlerDomain || changed === form.dataset.antlerDomain;
    }
    function onPush(event) {
      if (!active() || !dlg.open) { return; }
      if (!domainMatches(event)) { return; }
      refresh();
    }
    function attachListeners() {
      var shared = window.mammothmailEventsSource || null;
      if (!shared || shared === attachedTo) { return; }
      if (attachedTo) {
        attachedTo.removeEventListener('mx.health_changed', onPush);
        attachedTo.removeEventListener('mx.dns_changed', onPush);
      }
      attachedTo = shared;
      shared.addEventListener('mx.health_changed', onPush);
      shared.addEventListener('mx.dns_changed', onPush);
    }
    window.addEventListener('pagehide', function () {
      if (attachedTo) {
        attachedTo.removeEventListener('mx.health_changed', onPush);
        attachedTo.removeEventListener('mx.dns_changed', onPush);
        attachedTo = null;
      }
      if (timer) { window.clearInterval(timer); }
      if (staleRetry !== null) { window.clearTimeout(staleRetry); }
    });
    // The open dialog re-checks on two clocks, both stopped once it closes:
    //   * a slow fallback (the saved status view) that catches a change no push
    //     announced — a rogue MX record emits no receiver event.
    //   * a per-second tick that drives the wizard's visible 10s countdown.
    var STATUS_POLL_MS = 60000;
    function scheduleStatusPoll() {
      if (timer) { window.clearTimeout(timer); }
      timer = window.setTimeout(function () {
        if (active() && dlg.open && statusMode) { refresh(); }
        attachListeners();
        scheduleStatusPoll();
      }, STATUS_POLL_MS);
    }
    var tickTimer = window.setInterval(tickCountdown, 1000);
    window.addEventListener('pagehide', function () { window.clearInterval(tickTimer); });
    scheduleStatusPoll();
    // The dialog re-attaches to the shared stream whenever it is opened, in case
    // the dashboard reconnected while it was closed.
    window.addEventListener('mammothmail:source', attachListeners);
    attachListeners();
    // A status dialog the server marked to open draws its complete connector
    // table synchronously here, before the browser paints the open dialog, so it
    // never flashes thin and then expands when the first poll lands. The auto-
    // open runs in an earlier script block, so this must render eagerly rather
    // than wait for the (queued) open event.
    if (statusMode && !state) { renderSkeleton(connectors); }
    update();
    if (statusMode && dlg.open) { refresh(); }
    // Reopening a status or wizard dialog later re-checks immediately.
    dlg.addEventListener('open', function () {
      attachListeners();
      if (active() && !busy && (statusMode || step === 1 || step === 2)) { refresh(); }
    });
  });
})();

(function () {
  var input = document.querySelector('input[type=file][name=attachments]');
  if (!input || typeof DataTransfer === 'undefined') {
    return;
  }
  var dropZone = document.getElementById('attach-drop');
  var overlay = document.getElementById('attach-overlay');
  var list = document.getElementById('attach-list');
  var dragDepth = 0;

  function files() {
    return Array.prototype.slice.call(input.files || []);
  }

  function render() {
    if (!list) {
      return;
    }
    list.textContent = '';
    files().forEach(function (file, index) {
      var li = document.createElement('li');

      var name = document.createElement('span');
      name.className = 'attach-name';
      name.textContent = file.name;

      var size = document.createElement('span');
      size.className = 'attach-size';
      size.textContent = humanSize(file.size);

      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'attach-remove';
      remove.setAttribute('aria-label', 'Remove ' + file.name);
      remove.textContent = '\u00d7';
      remove.addEventListener('click', function () {
        var next = files();
        next.splice(index, 1);
        setFiles(next);
      });

      li.appendChild(name);
      li.appendChild(size);
      li.appendChild(remove);
      list.appendChild(li);
    });
  }

  function setFiles(next) {
    var dt = new DataTransfer();
    next.forEach(function (file) {
      dt.items.add(file);
    });
    try {
      input.files = dt.files;
    } catch (err) {
      return;
    }
    render();
  }

  function addFiles(incoming) {
    var merged = files();
    Array.prototype.slice.call(incoming).forEach(function (file) {
      merged.push(file);
    });
    setFiles(merged);
  }

  function humanSize(bytes) {
    if (bytes < 1024) {
      return bytes + ' B';
    }
    var units = ['KB', 'MB', 'GB'];
    var value = bytes / 1024;
    var i = 0;
    while (value >= 1024 && i < units.length - 1) {
      value /= 1024;
      i += 1;
    }
    return (value >= 10 ? Math.round(value) : value.toFixed(1)) + ' ' + units[i];
  }

  function hasFiles(e) {
    var dt = e.dataTransfer;
    if (!dt) {
      return false;
    }
    if (dt.types) {
      return Array.prototype.indexOf.call(dt.types, 'Files') !== -1;
    }
    return !!(dt.files && dt.files.length);
  }

  function showDrop(show) {
    if (overlay) {
      overlay.hidden = !show;
      overlay.classList.toggle('active', show);
    }
    if (dropZone) {
      dropZone.classList.toggle('dragover', show);
    }
  }

  input.addEventListener('change', render);

  window.addEventListener('dragenter', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    dragDepth += 1;
    showDrop(true);
  });

  window.addEventListener('dragover', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    if (e.dataTransfer) {
      e.dataTransfer.dropEffect = 'copy';
    }
  });

  window.addEventListener('dragleave', function () {
    if (dragDepth <= 0) {
      return;
    }
    dragDepth -= 1;
    if (dragDepth === 0) {
      showDrop(false);
    }
  });

  window.addEventListener('drop', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    dragDepth = 0;
    showDrop(false);
    if (e.dataTransfer) {
      addFiles(e.dataTransfer.files);
    }
  });

  render();
})();

// Operator invite dialog (account page): one dialog serves both creating an
// invitation and editing an existing operator's mailbox access.
(function () {
  var dlg = document.getElementById('operator-invite-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var email = dlg.querySelector('[name=email]');
  var submit = document.getElementById('operator-invite-submit');
  var title = document.getElementById('operator-invite-title');
  var boxes = dlg.querySelectorAll('input[name=inboxes]');
  var add = document.getElementById('add-operator');
  if (add) {
    add.addEventListener('click', function () {
      form.setAttribute('action', '/ui/account/operators/invites');
      email.value = '';
      email.readOnly = false;
      boxes.forEach(function (b) { b.checked = false; });
      if (title) { title.textContent = 'Create invitation'; }
      if (submit) { submit.textContent = 'Create invitation'; }
      dlg.showModal();
    });
  }
  document.querySelectorAll('.edit-operator').forEach(function (btn) {
    btn.addEventListener('click', function () {
      form.setAttribute('action', '/ui/account/operators/' + btn.getAttribute('data-id') + '/roles');
      email.value = btn.getAttribute('data-email') || '';
      email.readOnly = true;
      var owned = (btn.getAttribute('data-inboxes') || '').split(',');
      boxes.forEach(function (b) { b.checked = owned.indexOf(b.value) >= 0; });
      if (title) { title.textContent = 'Edit operator'; }
      if (submit) { submit.textContent = 'Save access'; }
      dlg.showModal();
    });
  });
  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

// New-account invite dialog (system admin plane).
(function () {
  var dlg = document.getElementById('account-invite-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-account');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      dlg.showModal();
    });
  }
  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

// Per-account storage quota dialog (system admin plane).
(function () {
  var dlg = document.getElementById('quota-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var valueInput = document.getElementById('quota-value');
  var unitSelect = document.getElementById('quota-unit');
  var usedNote = document.getElementById('quota-used');
  var title = document.getElementById('quota-title');
  var units = ['b', 'kb', 'mb', 'gb', 'tb'];
  var mult = { b: 1, kb: 1024, mb: 1048576, gb: 1073741824, tb: 1099511627776 };
  var used = 0;

  function preferredUnit(bytes) {
    var div = 1;
    var idx = 0;
    while (idx < units.length - 1 && bytes >= div * 1024) {
      div *= 1024;
      idx += 1;
    }
    return { unit: units[idx], value: bytes / div };
  }

  document.querySelectorAll('.edit-quota').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = btn.getAttribute('data-id');
      var quota = parseInt(btn.getAttribute('data-quota'), 10) || 0;
      used = parseInt(btn.getAttribute('data-used'), 10) || 0;
      form.setAttribute('action', '/ui/admin/accounts/' + encodeURIComponent(id) + '/quota');
      if (quota === 0) {
        valueInput.value = '0';
        unitSelect.value = 'mb';
      } else {
        var pick = preferredUnit(quota);
        valueInput.value = String(Math.round(pick.value * 1000) / 1000);
        unitSelect.value = pick.unit;
      }
      if (title) {
        title.textContent = 'Edit storage quota' + (btn.getAttribute('data-name') ? ' — ' + btn.getAttribute('data-name') : '');
      }
      if (usedNote) {
        usedNote.textContent = used > 0 ? 'Currently using ' + used.toLocaleString() + ' bytes.' : '';
      }
      dlg.showModal();
    });
  });

  form.addEventListener('submit', function (e) {
    var v = parseFloat(valueInput.value);
    var bytes = isFinite(v) && v > 0 ? Math.round(v * (mult[unitSelect.value] || 1)) : 0;
    if (bytes > 0 && used > 0 && bytes < used) {
      if (!window.confirm('This quota (' + bytes.toLocaleString() + ' bytes) is below the account\u2019s current usage (' + used.toLocaleString() + ' bytes). New mail will be rejected until usage drops. Continue?')) {
        e.preventDefault();
      }
    }
  });

  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

/* Passkey (WebAuthn) ceremonies. Two buttons share this module: sign-in on the
   login page and add-passkey on the account page. Each carries data-begin and
   data-finish endpoint paths; the server owns all policy and only the browser
   glue lives here. */
(function () {
  function csrfToken() {
    var el = document.querySelector('[name=_csrf]');
    return el ? el.value : '';
  }

  function b64urlToBuf(value) {
    var pad = value.length % 4 === 0 ? '' : '='.repeat(4 - (value.length % 4));
    var base64 = (value + pad).replace(/-/g, '+').replace(/_/g, '/');
    var raw = atob(base64);
    var buf = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) {
      buf[i] = raw.charCodeAt(i);
    }
    return buf.buffer;
  }

  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf);
    var str = '';
    for (var i = 0; i < bytes.length; i++) {
      str += String.fromCharCode(bytes[i]);
    }
    return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function prepareCreationOptions(options) {
    options.challenge = b64urlToBuf(options.challenge);
    options.user.id = b64urlToBuf(options.user.id);
    if (options.excludeCredentials) {
      options.excludeCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
    }
    return options;
  }

  function prepareRequestOptions(options) {
    options.challenge = b64urlToBuf(options.challenge);
    if (options.allowCredentials) {
      options.allowCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
    }
    return options;
  }

  function credentialToJSON(cred) {
    var response = cred.response;
    var out = { id: cred.id, rawId: bufToB64url(cred.rawId), type: cred.type, response: {}, clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {} };
    var r = response;
    out.response.clientDataJSON = bufToB64url(r.clientDataJSON);
    if (r.attestationObject !== undefined) {
      out.response.attestationObject = bufToB64url(r.attestationObject);
    }
    if (r.authenticatorData !== undefined) {
      out.response.authenticatorData = bufToB64url(r.authenticatorData);
    }
    if (r.signature !== undefined) {
      out.response.signature = bufToB64url(r.signature);
    }
    if (r.userHandle !== undefined && r.userHandle !== null) {
      out.response.userHandle = bufToB64url(r.userHandle);
    }
    return out;
  }

  function postJSON(url, body, token) {
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken(),
        'X-WebAuthn-Challenge': token || ''
      },
      body: JSON.stringify(body)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          throw new Error(data.error || 'request failed');
        }
        return data;
      });
    });
  }

  function setStatus(el, msg, isError) {
    if (!el) { return; }
    el.textContent = msg || '';
    el.classList.toggle('error', !!isError);
  }

  function wire(button, statusEl, isRegister) {
    button.addEventListener('click', function () {
      setStatus(statusEl, 'Waiting for your device…', false);
      button.disabled = true;
      var rpID = '';
      var name = isRegister ? (window.prompt('Name this passkey', 'Passkey') || 'Passkey') : '';
      // Offer to make this the only sign-in method only when a password is
      // currently enabled; a passkey-only account has nothing to disable.
      var makeOnly = isRegister && button.getAttribute('data-password-enabled') === '1' &&
        window.confirm('Use this passkey as your only sign-in method? Your password will be disabled.');
      // The register endpoints are CSRF-protected; the sign-in endpoints are
      // pre-authentication and rely on the ceremony's own origin check.
      var beginHeaders = { 'Content-Type': 'application/json' };
      if (isRegister) {
        beginHeaders['X-CSRF-Token'] = csrfToken();
      }
      fetch(button.getAttribute('data-begin'), {
        method: 'POST',
        credentials: 'same-origin',
        headers: beginHeaders
      }).then(function (res) {
        return res.json().then(function (data) {
          if (!res.ok) { throw new Error(data.error || 'could not start'); }
          return data;
        });
      }).then(function (data) {
        var publicKey;
        if (isRegister) {
          publicKey = prepareCreationOptions(data.options.publicKey || data.options);
          rpID = publicKey.rp && publicKey.rp.id;
          return navigator.credentials.create({ publicKey: publicKey }).then(function (cred) {
            if (!cred) { throw new Error('no credential returned'); }
            var url = button.getAttribute('data-finish') + '?name=' + encodeURIComponent(name) + (makeOnly ? '&only=1' : '');
            return postJSON(url, credentialToJSON(cred), data.challenge_token);
          });
        }
        publicKey = prepareRequestOptions(data.options.publicKey || data.options);
        rpID = publicKey.rpId;
        return navigator.credentials.get({ publicKey: publicKey }).then(function (cred) {
          if (!cred) { throw new Error('no credential returned'); }
          var nextField = document.querySelector('input[name="next"]');
          var finishURL = button.getAttribute('data-finish');
          if (nextField && nextField.value) {
            finishURL += '?next=' + encodeURIComponent(nextField.value);
          }
          return postJSON(finishURL, credentialToJSON(cred), data.challenge_token);
        });
      }).then(function (result) {
        if (isRegister) {
          var msg = 'Passkey added. Reloading…';
          if (result && result.password_only) {
            msg = 'Passkey added; password sign-in disabled. Reloading…';
          } else if (result && result.warning) {
            setStatus(statusEl, result.warning, true);
            button.disabled = false;
            return;
          }
          setStatus(statusEl, msg, false);
          window.location.reload();
        } else {
          window.location.href = (result && result.redirect) || '/';
        }
      }).catch(function (err) {
        var message = err.message || 'Passkey failed';
        // Only translate the browser's RP-domain rejection; other security
        // errors (and cancellation/device errors) retain their own explanation.
        if (err.name === 'SecurityError' && rpID &&
            /relying party|rp id|rpid|registrable domain/i.test(message)) {
          message = 'Passkeys are configured for "' + rpID + '", but you are visiting "' + window.location.hostname +
            '". Open Mammoth Mail at its configured BASE_URL, or ask your administrator to correct BASE_URL to the UI URL and restart Mammoth Mail. ' +
            'Use DEDICATED_RECEIVER_URL for a separate inbound receiver URL.';
        }
        setStatus(statusEl, message, true);
        button.disabled = false;
      });
    });
  }

  // canUsePasskeys reports whether this origin can run a WebAuthn ceremony at
  // all. The API and the secure-context rule are both browser-enforced, so a
  // control offered where they do not hold can only fail.
  function canUsePasskeys() {
    return !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext);
  }

  // hidePasskeyControl removes a control the browser cannot use. Removing it
  // rather than disabling it keeps the page free of a dead button and of an
  // explanation for a requirement the page simply does not meet.
  function hidePasskeyControl(button) {
    if (!button) { return; }
    // The login page wraps its button and status line in one block; the
    // account page puts the add button beside the password-recovery form, so
    // only the button goes there.
    var node = button;
    if (button.closest) {
      var block = button.closest('.auth-action');
      if (block) { node = block; }
    }
    var parent = node.parentNode;
    if (!parent) { return; }
    parent.removeChild(node);
    // A row of actions left with nothing in it is dead space.
    if (node === button && parent.children && parent.children.length === 0 && parent.parentNode) {
      parent.parentNode.removeChild(parent);
    }
  }

  var signin = document.getElementById('passkey-signin');
  var add = document.getElementById('passkey-add');
  var status = document.getElementById('passkey-status');
  if (!canUsePasskeys()) {
    hidePasskeyControl(signin);
    hidePasskeyControl(add);
    // Nothing will report a failure, so the status line goes too.
    if (status && status.parentNode) { status.parentNode.removeChild(status); }
  } else {
    if (signin) { wire(signin, status, false); }
    if (add) { wire(add, status, true); }
  }

  /* Each passkey row is a name plus a gear button that opens that passkey's
     settings dialog, where the details, rename and remove live. One dialog per
     credential is rendered server-side, so the button only has to find it. */
  document.querySelectorAll('.open-passkey-settings').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = document.getElementById('passkey-dialog-' + btn.getAttribute('data-passkey'));
      if (dlg && typeof dlg.showModal === 'function') {
        dlg.showModal();
      }
    });
  });
  document.querySelectorAll('.passkey-dialog [data-close-dialog]').forEach(function (b) {
    var dlg = b.closest('dialog');
    if (!dlg) { return; }
    b.addEventListener('click', function () { dlg.close(); });
  });

  /* The Feedback button lives in the signed-in header and opens the shared
     feedback dialog, which points users at the project mailbox. It is a plain
     mailto link, so there is no request to send. */
  var feedbackOpen = document.getElementById('feedback-open');
  var feedbackDialog = document.getElementById('feedback-dialog');
  if (feedbackOpen && feedbackDialog && typeof feedbackDialog.showModal === 'function') {
    feedbackOpen.addEventListener('click', function () { feedbackDialog.showModal(); });
    feedbackDialog.querySelectorAll('[data-close-dialog]').forEach(function (b) {
      b.addEventListener('click', function () { feedbackDialog.close(); });
    });
  }

  /* The Settings modal is rendered on /account and /admin and auto-opens so
     those routes read as one dialog. Its tab rail links to real URLs, so a tab
     click is an ordinary navigation; the only client behaviour is opening on
     load and returning to the dashboard when the dialog is dismissed. */
  var settingsDialog = document.getElementById('settings-dialog');
  if (settingsDialog && typeof settingsDialog.showModal === 'function') {
    if (!settingsDialog.open) {
      settingsDialog.showModal();
    }
    var leaveSettings = function () { window.location.assign('/'); };
    settingsDialog.querySelectorAll('.settings-close').forEach(function (b) {
      b.addEventListener('click', leaveSettings);
    });
    settingsDialog.addEventListener('cancel', function (e) {
      e.preventDefault();
      leaveSettings();
    });
    settingsDialog.addEventListener('click', function (e) {
      if (e.target === settingsDialog) {
        leaveSettings();
      }
    });
  }

})();

(function () {
  // Live updates: keep dashboard counts, inbox badge counts and the inbound
  // traffic lights current without a manual refresh.
  //
  // Resource budget: one EventSource per visible tab, one coalesced snapshot
  // fetch per burst of events (trailing debounce, single-flight, one queued
  // re-run), and no work at all while the tab is hidden beyond a cheap idle
  // connection. On hidden the stream is closed after a short grace period and
  // reopened on focus, so a forgotten background tab costs no server
  // connection.
  var page = document.body ? document.body.getAttribute('data-page') : null;
  if (page !== 'dashboard' && page !== 'inbox') {
    return;
  }
  var HIDDEN_CLOSE_MS = 60000;
  var DEBOUNCE_MS = 300;
  var MAX_RETRY_MS = 30000;

  var snapshotURL = '/ui/state?page=' + encodeURIComponent(page);
  if (page === 'inbox') {
    var inboxID = document.body.getAttribute('data-inbox');
    if (!inboxID) {
      return;
    }
    snapshotURL += '&inbox=' + encodeURIComponent(inboxID);
  }
  // The dashboard traffic light needs a server-side published-MX lookup, so it
  // is requested only when it may have changed: on load, on reconnect, and on a
  // receiver-health notification — never on an ordinary mail event. A slow
  // fallback tick also asks for lights while the dashboard is visible, so a
  // rogue MX record (which emits no receiver event) is still surfaced within
  // 15 minutes. Nothing runs once the tab is hidden or closed.
  var wantLights = page === 'dashboard';
  var LIGHTS_POLL_MS = 15 * 60 * 1000;
  var lightsTimer = null;
  function scheduleLightsTick() {
    if (lightsTimer) {
      window.clearTimeout(lightsTimer);
    }
    lightsTimer = window.setTimeout(function () {
      if (!document.hidden) {
        wantLights = true;
        refresh();
      }
      scheduleLightsTick();
    }, LIGHTS_POLL_MS);
  }
  if (page === 'dashboard') {
    scheduleLightsTick();
  }

  var source = null;
  var debounceTimer = null;
  var hiddenTimer = null;
  var retryTimer = null;
  var retryDelay = 1000;
  var inFlight = null;
  var pendingRefresh = false;
  var closedForHidden = false;

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) {
      node.className = className;
    }
    if (text !== undefined) {
      node.textContent = text;
    }
    return node;
  }

  // setSidebarCount updates a folder's count badge, creating or removing the
  // element so a badge appears and disappears exactly with its number.
  function setSidebarCount(folder, value, unread) {
    var nav = document.querySelector('[data-sidebar]');
    if (!nav) {
      return;
    }
    var link = nav.querySelector('a.folder[data-folder="' + folder + '"]');
    if (!link) {
      return;
    }
    var badge = link.querySelector('[data-count]');
    if (!value) {
      if (badge) {
        badge.remove();
      }
      return;
    }
    if (!badge) {
      badge = el('span', 'count', String(value));
      badge.setAttribute('data-count', folder);
      link.appendChild(document.createTextNode(' '));
      link.appendChild(badge);
    } else {
      badge.textContent = String(value);
    }
    badge.classList.toggle('unread', !!unread);
  }

  // setCountCell swaps a dashboard inbox-table cell between a value pill and the
  // em dash placeholder, preserving the pending cell's link.
  function setCountCell(row, cell, value, asPill) {
    var td = row.querySelector('td[data-cell="' + cell + '"]');
    if (!td) {
      return;
    }
    if (!value) {
      td.textContent = '';
      td.appendChild(el('span', 'muted', '\u2014'));
      return;
    }
    if (asPill) {
      var a = el('a', 'pill pending-pill', String(value));
      a.href = '/ui/inboxes/' + row.getAttribute('data-inbox-id') + '/drafts';
      a.title = 'Drafts awaiting approval to send';
      td.textContent = '';
      td.appendChild(a);
      return;
    }
    td.textContent = '';
    td.appendChild(el('span', 'pill unread-pill', String(value)));
  }

  // setReceivingLabel writes the server's provider label into a dashboard Domain
  // row's Receiving cell. The row is server-rendered, so a receiving dialog that
  // stores a provider while the row is on screen — the Tusk MX wizard saves the
  // provider on its first step, so cancelling a setup part-way leaves the domain
  // configured — would otherwise keep drawing an amber "Add" beside a live light
  // until a manual reload. An empty label leaves the cell alone: a domain with no
  // receiving provider, or one that inherits "(inherited)", is already rendered
  // correctly. The light dot, when the cell has one, stays the button's first
  // child, matching the server-rendered order.
  function setReceivingLabel(row, info) {
    var label = info && info.label;
    if (!row || !label) {
      return;
    }
    var btn = row.querySelector('[data-receiving-button]');
    if (!btn) {
      return;
    }
    var dot = btn.querySelector('[data-receiving-light]');
    btn.replaceChildren();
    if (dot) {
      btn.appendChild(dot);
    }
    btn.appendChild(document.createTextNode(label));
    btn.classList.remove('amber');
    btn.classList.add('secondary');
  }

  // setTrafficLight patches a dashboard Domain row's light dot in place. It is
  // parameterised by the dot's data attribute and its owning button's data
  // attribute so the receiving light (data-receiving-light) and the sending DKIM
  // light (data-sending-light) share one implementation. A falsy light removes
  // the dot, so a light that no longer applies (signing turned off) disappears.
  function setTrafficLight(row, light, title, attr, buttonAttr) {
    if (!row) {
      return;
    }
    var dot = row.querySelector('[' + attr + ']');
    if (!light) {
      if (dot) {
        dot.remove();
      }
      return;
    }
    if (!dot) {
      var btn = row.querySelector('[' + buttonAttr + ']');
      if (!btn) {
        return;
      }
      dot = el('span', 'dns-light');
      dot.setAttribute(attr, '');
      btn.insertBefore(dot, btn.firstChild);
    }
    dot.className = 'dns-light ' + light;
    dot.setAttribute('title', title || '');
    dot.setAttribute('aria-label', title || '');
  }

  // applyExtraMX is gone: the dashboard light is never patched client-side. A
  // pushed verdict may clear as well as set an extra-MX warning, and the row alone
  // cannot tell whether a receiver is still ready, so the aggregate light is always
  // recomputed by the server (a cheap, DNS-free lights snapshot) rather than
  // guessed from one row's current class.

  // labelMatch tolerates the case-insensitive label keys the store returns.
  function labelMatch(labels, name) {
    var lower = name.toLowerCase();
    for (var key in labels) {
      if (key.toLowerCase() === lower) {
        return labels[key];
      }
    }
    return 0;
  }

  function apply(data) {
    if (!data || typeof data !== 'object') {
      return;
    }
    if (data.page === 'dashboard') {
      document.querySelectorAll('tbody[data-inbox-rows] tr[data-inbox-id]').forEach(function (row) {
        var id = row.getAttribute('data-inbox-id');
        var counts = (data.inboxes && data.inboxes[id]) || {};
        setCountCell(row, 'unread', counts.unread || 0, false);
        setCountCell(row, 'pending', counts.pending || 0, true);
      });
      if (data.domains) {
        document.querySelectorAll('tbody[data-domain-rows] tr[data-domain-id]').forEach(function (row) {
          var info = data.domains[row.getAttribute('data-domain-id')];
          setReceivingLabel(row, info);
          setTrafficLight(row, info ? info.light : '', info ? info.title : '', 'data-receiving-light', 'data-receiving-button');
        });
      }
      if (data.sending) {
        document.querySelectorAll('tbody[data-domain-rows] tr[data-domain-id]').forEach(function (row) {
          var info = data.sending[row.getAttribute('data-domain-id')];
          setTrafficLight(row, info ? info.light : '', info ? info.title : '', 'data-sending-light', 'data-sending-button');
        });
      }
      if (window.mammothmailDashboardGroups) {
        window.mammothmailDashboardGroups.refresh();
      }
      return;
    }
    if (data.page === 'inbox') {
      // A standalone inbox's sync state is reported by the header status flag
      // rather than an in-list strip; the cached mail stays usable throughout.
      if (data.remote) {
        var syncing = !!data.remote_syncing;
        window.mammothmailStatus.busy('sync', syncing);
        window.mammothmailStatus.error(!syncing && data.remote_status === 'error' ? 'Mail sync failed. Retry by refreshing the remote connector.' : '');
      } else {
        window.mammothmailStatus.busy('sync', false);
        window.mammothmailStatus.error('');
      }
      if (data.remote_syncing) {
        window.setTimeout(schedule, 1500);
      }
      setSidebarCount('inbox', data.unread || 0, true);
      setSidebarCount('drafts', data.drafts || 0, false);
      setSidebarCount('outbox', data.outbox || 0, false);
      setSidebarCount('spam', data.spam || 0, false);
      setSidebarCount('trash', data.trash || 0, false);
      // A standalone inbox's counts come from the cached remote index; mark the
      // unread badge stale unless the index is complete so a live count is never
      // presented as current when it may be behind the server.
      var unreadLink = document.querySelector('[data-sidebar] a.folder[data-folder="inbox"]');
      var unreadBadge = unreadLink ? unreadLink.querySelector('[data-count="unread"]') : null;
      if (unreadBadge && data.remote) {
        var stale = data.remote_status && data.remote_status !== 'complete';
        unreadBadge.classList.toggle('stale', !!stale);
        unreadBadge.setAttribute('title', stale ? 'Counts may be behind: the remote index is not complete.' : '');
      }
      var labels = data.labels || {};
      document.querySelectorAll('[data-sidebar] a.folder[data-folder="label"]').forEach(function (link) {
        var name = link.getAttribute('data-label');
        var value = name ? (labels[name] || labelMatch(labels, name)) : 0;
        var badge = link.querySelector('[data-count]');
        if (!value) {
          if (badge) {
            badge.remove();
          }
        } else if (badge) {
          badge.textContent = String(value);
        } else {
          badge = el('span', 'count unread', String(value));
          badge.setAttribute('data-count', 'label');
          link.appendChild(document.createTextNode(' '));
          link.appendChild(badge);
        }
      });
    }
  }

  // --- live list reconcile --------------------------------------------------
  //
  // Background-refresh policy: counts and badges always update in place, but a
  // list body is only ever swapped on the terms the user would not notice —
  // they are at the top of the page, nothing is selected, no dialog is open and
  // no field has focus. Otherwise the list is left alone and simply catches up
  // on the next natural load. The list is never re-fetched while the user is
  // scrolled down or typing, so the screen never jumps under them.

  var folder = document.body.getAttribute('data-folder') || '';
  var activeLabel = document.body.getAttribute('data-label') || '';
  var listSection = document.querySelector('[data-live-list]');
  var requestsSection = document.querySelector('[data-live-requests]');

  function liveURL(part) {
    var u = '/ui/inboxes/' + encodeURIComponent(inboxID) + '/live?part=' + encodeURIComponent(part) + '&folder=' + encodeURIComponent(folder);
    if (activeLabel) {
      u += '&label=' + encodeURIComponent(activeLabel);
    }
    if (folder === 'folder') {
      u += '&folder_id=' + encodeURIComponent(new URL(window.location.href).searchParams.get('folder') || '');
    }
    var before = new URL(window.location.href).searchParams.get('before');
    if (before) { u += '&before=' + encodeURIComponent(before); }
    return u;
  }

  // atTop reports whether the user is at the very top of the scrollable area, so
  // a swap cannot change what is under their cursor.
  function atTop() {
    return (window.pageYOffset || document.documentElement.scrollTop || 0) <= 4;
  }

  // anySelected reports whether the user has checked a message for a bulk action.
  function anySelected() {
    return !!document.querySelector('.mailcheck input[type=checkbox]:checked');
  }

  // dialogOpen reports whether any modal dialog is open (settings, compose
  // overlays, confirmations), in which case the page is not a passive view.
  function dialogOpen() {
    var dlgs = document.querySelectorAll('dialog[open]');
    for (var i = 0; i < dlgs.length; i++) {
      return true;
    }
    return false;
  }

  // editing reports whether focus is in a text field, so a swap cannot steal it.
  function editing() {
    var a = document.activeElement;
    if (!a) {
      return false;
    }
    var tag = a.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || a.isContentEditable;
  }

  function canSwap() {
    return !document.hidden && atTop() && !anySelected() && !dialogOpen() && !editing();
  }

  // signature is a cheap, collision-resistant fingerprint of a fragment's
  // structure (row ids and read/status classes), so an unchanged list is left
  // in the DOM untouched rather than replaced with an identical tree.
  function signature(root) {
    if (!root) {
      return '';
    }
    var out = [];
    root.querySelectorAll('[data-row-id]').forEach(function (row) {
      out.push(row.getAttribute('data-row-id') + ':' + (row.classList.contains('unread') ? 'u' : 'r'));
    });
    // The pager link and the empty-state text are part of what can change.
    var pager = root.querySelector('a[href*="before="]');
    out.push('pager=' + (pager ? pager.getAttribute('href') : ''));
    out.push('empty=' + (root.querySelector('.muted') ? '1' : '0'));
    return out.join('|');
  }

  // listHost is the container the list and requests cards live in, used to
  // insert a requests card that did not exist on the page before.
  function listHost() {
    return document.querySelector('[data-sidebar]') ? document.querySelector('.mailcontent') : null;
  }

  var listInFlight = false;
  function reconcileLists() {
    if (listInFlight || !canSwap()) {
      return;
    }
    // The requests card lives only on the inbox folder; the list exists on every
    // folder view. Fetch only what the current page can show.
    var wantList = folder === 'inbox' || folder === 'sent' || folder === 'spam' || folder === 'trash' || folder === 'label' || folder === 'folder';
    var wantRequests = folder === 'inbox';
    if (!wantList && !wantRequests) {
      return;
    }
    listInFlight = true;
    // The refresh itself is reported by the header status flag, which delays a
    // brief spinner so a fast reconcile never flashes one.
    window.mammothmailStatus.busy('list', true);
    var jobs = [];
    if (wantList) {
      jobs.push(fetchFragment(liveURL('list'), '[data-live-list]').then(swapFragment));
    }
    if (wantRequests) {
      jobs.push(fetchFragment(liveURL('requests'), '[data-live-requests]').then(function (part) {
        // A requests card that is absent on the page but present in the incoming
        // fragment is inserted ahead of the list; an existing one is swapped.
        if (!part.incoming && !part.current) {
          return;
        }
        if (part.incoming && !part.current) {
          var host = listHost();
          var anchor = document.querySelector('[data-live-list]');
          if (host && anchor && host.contains(anchor)) {
            host.insertBefore(part.incoming, anchor);
          }
          return;
        }
        swapFragment(part);
      }));
    }
    Promise.all(jobs).catch(function () {
      // A failed background refresh is surfaced in place with a Retry, since the
      // header flag is transient state and cannot offer an action.
      var feedback = el('div', 'load-status', 'Could not update messages. ');
      feedback.setAttribute('role', 'status');
      var retry = el('button', 'secondary', 'Retry');
      retry.type = 'button';
      retry.onclick = function () { feedback.remove(); reconcileLists(); };
      feedback.appendChild(retry);
      var host = listHost();
      if (host) { host.insertBefore(feedback, host.firstChild); }
    }).then(function () {
      window.mammothmailStatus.busy('list', false);
      listInFlight = false;
    });
  }

  // swapFragment replaces the current node with the incoming one when the tree
  // actually differs, and removes the current node when the incoming fragment is
  // empty (e.g. the last pending request was approved). It is a no-op when both
  // are absent, or when the trees are identical, so an unchanged list is never
  // rewritten.
  function swapFragment(part) {
    if (!part || !part.current) {
      return;
    }
    if (!part.incoming) {
      if (part.current.parentNode) {
        part.current.parentNode.removeChild(part.current);
      }
      return;
    }
    if (signature(part.current) === signature(part.incoming) && part.current.isEqualNode(part.incoming)) {
      return;
    }
    part.current.parentNode.replaceChild(part.incoming, part.current);
  }

  // fetchFragment fetches a server-rendered fragment and returns the parsed
  // incoming element plus the element it should replace (if any). A fragment
  // body with no matching element is reported as absent so the caller can decide
  // whether to remove or insert.
  function fetchFragment(url, selector) {
    return window.fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'text/html' } }).then(function (resp) {
      if (!resp.ok) {
        throw new Error('fragment ' + resp.status);
      }
      return resp.text();
    }).then(function (html) {
      var current = document.querySelector(selector);
      var holder = document.createElement('div');
      holder.innerHTML = html;
      var incoming = holder.querySelector(selector);
      return { current: current, incoming: incoming };
    });
  }

  function refresh() {
    if (document.hidden) {
      pendingRefresh = true;
      return;
    }
    if (inFlight) {
      pendingRefresh = true;
      return;
    }
    var url = snapshotURL;
    if (wantLights) {
      url += '&lights=1';
      wantLights = false;
    }
    inFlight = window.fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } });
    inFlight.then(function (resp) {
      if (!resp.ok) {
        throw new Error('snapshot ' + resp.status);
      }
      return resp.json();
    }).then(function (data) {
      apply(data);
      reconcileLists();
    }).catch(function () {
      // A failed snapshot is not fatal: the next event or focus retries.
    }).then(function () {
      inFlight = null;
      if (pendingRefresh) {
        pendingRefresh = false;
        refresh();
      }
    });
  }

  function schedule() {
    if (debounceTimer) {
      return;
    }
    debounceTimer = window.setTimeout(function () {
      debounceTimer = null;
      refresh();
    }, DEBOUNCE_MS);
  }

  function connect() {
    if (source || document.hidden) {
      return;
    }
    var wasClosed = closedForHidden;
    closedForHidden = false;
    if (wasClosed) {
      // Reopening after a deliberate hidden-tab close is not a drop.
      window.mammothmailStatus.connecting();
    }
    try {
      source = new EventSource('/ui/events/stream');
    } catch (err) {
      source = null;
      window.mammothmailStatus.disconnected();
      return;
    }
    // Share the one connection so other UI closures (the Tusk MX status
    // dialog) can listen for named events without opening a second stream. The
    // event tells them the source object was replaced, so a dialog open across a
    // reconnect re-attaches to the live stream instead of a dead one.
    window.mammothmailEventsSource = source;
    try { window.dispatchEvent(new Event('mammothmail:source')); } catch (err) { /* older browsers: dialogs also re-attach on open */ }
    source.onopen = function () {
      retryDelay = 1000;
      window.mammothmailStatus.connected();
      // A reconnect may have missed a light change, so re-ask for lights.
      wantLights = true;
      refresh();
    };
    source.onmessage = schedule;
	  source.addEventListener('actions.changed', function () { schedule(); if (window.mammothmailRefreshActions) { window.mammothmailRefreshActions(); } });
    // Named message events (id/event/data) do not fire onmessage; listen for the
    // durable types the UI cares about, plus the transient receiver-health ping.
    ['message.received', 'message.sent', 'message.trashed', 'message.restored',
      'message.purged', 'message.spam_state_changed', 'message.labels_changed',
      'message.state_changed', 'draft.send_requested', 'draft.send_request_cancelled',
      'draft.approved', 'draft.rejected', 'draft.sent', 'draft.send_failed'].forEach(function (name) {
      source.addEventListener(name, schedule);
    });
    // A receiver-health change and a published-record change both warrant the
    // (cheap, DNS-free) lights snapshot: the server owns the aggregate verdict, so
    // the dashboard never patches a light from the event payload. A DNS change can
    // clear an extra-MX warning as well as set one, which only the server can
    // judge.
    source.addEventListener('mx.health_changed', function () {
      wantLights = true;
      schedule();
    });
    // The DNS watcher is the single owner of a domain's published-record verdict.
    // Its event is a signal, not a value: it asks for a fresh snapshot, and any
    // open dialog listening on the same stream refreshes itself.
    source.addEventListener('mx.dns_changed', function () {
      wantLights = true;
      schedule();
    });
    source.onerror = function () {
      if (source) {
        source.close();
        source = null;
      }
      if (closedForHidden || document.hidden) {
        return;
      }
      window.mammothmailStatus.disconnected();
      // Reconnect manually so we control backoff and catch up with a snapshot.
      var delay = retryDelay;
      retryDelay = Math.min(retryDelay * 2, MAX_RETRY_MS);
      retryTimer = window.setTimeout(connect, delay);
    };
  }

  function disconnect() {
    if (retryTimer) {
      window.clearTimeout(retryTimer);
      retryTimer = null;
    }
    if (source) {
      source.close();
      source = null;
      window.mammothmailEventsSource = null;
    }
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      // Keep the connection briefly for a quick tab switch, then drop it so a
      // background tab holds no server resources.
      if (hiddenTimer) {
        window.clearTimeout(hiddenTimer);
      }
      hiddenTimer = window.setTimeout(function () {
        hiddenTimer = null;
        closedForHidden = true;
        disconnect();
      }, HIDDEN_CLOSE_MS);
      return;
    }
    if (hiddenTimer) {
      window.clearTimeout(hiddenTimer);
      hiddenTimer = null;
    }
    connect();
    refresh();
  });

  window.addEventListener('pagehide', function () {
    disconnect();
    if (debounceTimer) {
      window.clearTimeout(debounceTimer);
      debounceTimer = null;
    }
    if (hiddenTimer) {
      window.clearTimeout(hiddenTimer);
      hiddenTimer = null;
    }
    if (lightsTimer) {
      window.clearTimeout(lightsTimer);
      lightsTimer = null;
    }
  });

  connect();
  refresh();
})();
(function () {
  'use strict';
  var style = document.createElement('style');
  style.textContent = '.load-status{padding:10px 14px;background:#eef4ff;color:#174ea6;border-radius:6px;margin:8px 0}.load-status[hidden]{display:none}.load-status.pending:before{content:"";display:inline-block;width:16px;height:16px;border:2px solid #b8c9e8;border-top-color:#174ea6;border-radius:50%;margin-right:10px;vertical-align:middle;animation:mail-load-spin .8s linear infinite}.navigation-load{position:fixed;top:12px;left:50%;transform:translateX(-50%);z-index:10000;box-shadow:0 2px 12px #0003}@keyframes mail-load-spin{to{transform:rotate(360deg)}}@media(prefers-reduced-motion:reduce){.load-status.pending:before{animation:none}}';
  document.head.appendChild(style);
  // Pages that carry the header status flag report navigation loading through
  // it rather than a floating banner; the flag delays a brief spinner, so a
  // fast navigation never flashes one. Other pages keep the banner, since they
  // have no flag to show.
  var hasFlag = !!document.querySelector('[data-conn-status]');
  var notice;
  var watchdog;
  function loading(text) {
    if (hasFlag) {
      window.mammothmailStatus.busy('nav', true);
      window.clearTimeout(watchdog);
      watchdog = window.setTimeout(function () {
        window.mammothmailStatus.busy('nav', false);
      }, 60000);
      return;
    }
    if (!notice) {
      notice = document.createElement('div');
      notice.className = 'load-status pending navigation-load';
      notice.setAttribute('role', 'status');
      document.body.appendChild(notice);
    }
    notice.hidden = false;
    notice.classList.add('pending');
    notice.setAttribute('aria-busy', 'true');
    notice.textContent = text;
    window.clearTimeout(watchdog);
    watchdog = window.setTimeout(function () {
      notice.classList.remove('pending');
      notice.setAttribute('aria-busy', 'false');
      notice.textContent = 'Still waiting for the server. You can retry or cancel navigation.';
    }, 60000);
  }
  document.addEventListener('click', function (event) {
    var link = event.target.closest('a[href]');
    if (!link || event.defaultPrevented || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || link.hasAttribute('download') || (link.target && link.target !== '_self')) { return; }
    var url = new URL(link.href, window.location.href);
    if (url.origin !== window.location.origin || (url.pathname === window.location.pathname && url.search === window.location.search)) { return; }
    loading(url.pathname.indexOf('/ui/messages/') === 0 ? 'Loading message…' : 'Loading…');
  });
  document.addEventListener('submit', function (event) {
    window.setTimeout(function () {
      if (!event.defaultPrevented && !event.target.target) { loading('Loading…'); }
    }, 0);
  });
  window.addEventListener('pageshow', function () {
    window.clearTimeout(watchdog);
    // A page restored from the bfcache (browser Back) never navigated away, so
    // any pending navigation loading state is stale.
    window.mammothmailStatus.busy('nav', false);
    if (notice) { notice.hidden = true; }
  });
}());

// Folder and label management (shared mailbox sidebar). "New folder" and "New
// label" open their own dialogs; each sidebar row's glyph opens the shared
// manage dialog, whose forms start with no action and are pointed at the
// clicked row's routes here, so one dialog serves every folder and label on
// the page. A folder and a label are different things, so the dialog says
// which one it is about to rename or delete.
(function () {
  var sidebar = document.querySelector('[data-sidebar]');
  if (!sidebar) { return; }
  var inboxID = sidebar.getAttribute('data-inbox') || '';
  var folderNewDlg = document.getElementById('folder-new-dialog');
  var labelNewDlg = document.getElementById('label-new-dialog');
  var manageDlg = document.getElementById('folder-manage-dialog');

  function wireClose(dlg) {
    if (!dlg) { return; }
    dlg.querySelectorAll('[data-close-dialog]').forEach(function (btn) {
      btn.addEventListener('click', function () { dlg.close(); });
    });
  }
  wireClose(folderNewDlg);
  wireClose(labelNewDlg);
  wireClose(manageDlg);

  function wireNew(btn, dlg) {
    if (!btn || !dlg || typeof dlg.showModal !== 'function') { return; }
    btn.addEventListener('click', function () {
      var input = dlg.querySelector('input[name=name], input[name=path]');
      if (input) { input.value = ''; }
      dlg.showModal();
      if (input) { input.focus(); }
    });
  }
  wireNew(sidebar.querySelector('[data-folder-new]'), folderNewDlg);
  wireNew(sidebar.querySelector('[data-label-new]'), labelNewDlg);

  if (!manageDlg || typeof manageDlg.showModal !== 'function') { return; }
  sidebar.querySelectorAll('[data-folder-manage]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var kind = btn.getAttribute('data-manage-kind') === 'folder' ? 'folder' : 'label';
      var id = kind === 'folder' ? (btn.getAttribute('data-folder-id') || '') : (btn.getAttribute('data-label-id') || '');
      var name = btn.getAttribute('data-folder-name') || '';
      var base = '/ui/inboxes/' + encodeURIComponent(inboxID) + '/' + kind + 's/' + encodeURIComponent(id);
      var renameForm = manageDlg.querySelector('[data-folder-rename-form]');
      var deleteForm = manageDlg.querySelector('[data-folder-delete-form]');
      var input = manageDlg.querySelector('[data-folder-rename-input]');
      var title = manageDlg.querySelector('#folder-manage-title');
      var del = manageDlg.querySelector('[data-manage-delete]');
      if (renameForm) { renameForm.setAttribute('action', base + '/rename'); }
      if (deleteForm) { deleteForm.setAttribute('action', base + '/delete'); }
      if (input) { input.value = name; }
      if (title) { title.textContent = kind === 'folder' ? 'Manage folder' : 'Manage label'; }
      if (del) {
        del.textContent = kind === 'folder' ? 'Delete folder' : 'Delete label';
        del.setAttribute('data-confirm', kind === 'folder' ? 'Delete this folder? It must be empty.' : 'Delete this label? This will remove the label from its messages.');
      }
      manageDlg.showModal();
      if (input) { input.focus(); input.select(); }
    });
  });
}());

// One reconnect dialog. The inbox Identity tab's reconnect control for a
// One-backed inbox opens this in place — an API key field with "Save and check" —
// rather than sending the operator through the full import wizard. The key is
// validated and stored by the reconnect endpoint; the dialog stays open on a
// failure and reports the provider's message, and closes on success after
// re-checking the connection state.
(function () {
  'use strict';
  var dlg = document.getElementById('one-reconnect-dialog');
  if (!dlg) { return; }
  var form = dlg.querySelector('#one-reconnect-form');
  var inboxField = dlg.querySelector('#one-reconnect-inbox');
  var keyField = dlg.querySelector('#one-reconnect-key');
  var status = dlg.querySelector('#one-reconnect-status');
  var submit = dlg.querySelector('#one-reconnect-submit');
  var editDlg = document.getElementById('inbox-edit-dialog');
  var busy = false;
  var restoreEditDialog = false;

  function setStatus(text, isError) {
    if (!status) { return; }
    status.textContent = text || '';
    status.className = isError ? 'error small' : 'muted small';
  }

  function open(inboxID) {
    if (!inboxID || typeof dlg.showModal !== 'function') { return; }
    // The inbox settings dialog is itself modal. Close it before opening this
    // dialog, then restore it if the operator cancels or presses Escape.
    restoreEditDialog = !!(editDlg && editDlg.open);
    if (restoreEditDialog) { editDlg.close(); }
    inboxField.value = inboxID;
    keyField.value = '';
    keyField.disabled = false;
    submit.disabled = false;
    busy = false;
    setStatus('', false);
    dlg.showModal();
    keyField.focus();
  }

  dlg.addEventListener('close', function () {
    if (!restoreEditDialog || !editDlg || typeof editDlg.showModal !== 'function') { return; }
    restoreEditDialog = false;
    editDlg.showModal();
  });

  // A page opened with ?reconnect=1 (the retired One settings link, the dashboard
  // flag, the bottom banner) asks for the dialog straight away, so a disconnected
  // One inbox reconnects from wherever the operator arrived without a detour
  // through the settings dialog. The inbox is the page's own mailbox sidebar.
  if (new URLSearchParams(window.location.search).get('reconnect') === '1') {
    var own = document.querySelector('[data-sidebar][data-inbox]');
    if (own) { open(own.getAttribute('data-inbox') || ''); }
  }

  // Any reconnect affordance for a One inbox (the Identity tab link/button, the
  // dashboard prompt) carries data-one-reconnect with the inbox id in the
  // closest row/button. Delegated so the dynamically built controls are covered.
  document.addEventListener('click', function (event) {
    var trigger = event.target.closest('[data-one-reconnect]');
    if (!trigger) { return; }
    event.preventDefault();
    var inboxID = trigger.getAttribute('data-inbox') || '';
    if (!inboxID) {
      var row = trigger.closest('[data-inbox-id]');
      if (row) { inboxID = row.getAttribute('data-inbox-id'); }
    }
    if (!inboxID) {
      var editDlg = document.getElementById('inbox-edit-dialog');
      if (editDlg && editDlg.open) {
        var addr = document.getElementById('inbox-edit-address');
        var btn = document.querySelector('.edit-inbox[data-address="' + (addr ? addr.value.replace(/"/g, '&quot;') : '') + '"]');
        if (btn) { inboxID = btn.getAttribute('data-id') || ''; }
      }
    }
    if (!inboxID) { return; }
    open(inboxID);
  });

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (busy) { return; }
    var key = keyField.value.trim();
    if (!key) { setStatus('Enter a One API key.', true); return; }
    busy = true;
    submit.disabled = true;
    keyField.disabled = true;
    setStatus('Checking the key against this inbox…', false);
    var csrfField = form.querySelector('[name=_csrf]');
    fetch('/ui/connections/one/reconnect', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        'Accept': 'application/json',
        'X-CSRF-Token': csrfField ? csrfField.value : ''
      },
      body: JSON.stringify({ inbox_id: inboxField.value, api_key: key })
    }).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (data) {
        return { ok: response.ok, data: data };
      });
    }).then(function (result) {
      busy = false;
      submit.disabled = false;
      keyField.disabled = false;
      if (result.ok && result.data && result.data.status === 'connected') {
        setStatus('Connection restored.', false);
        // Reflect the healthy state in place: drop the reconnect prompt and
        // refresh so the dashboard flag and Identity tab clear without a manual
        // reload.
        var prompt = document.querySelector('[data-reconnect-prompt]');
        if (prompt) { prompt.hidden = true; }
        restoreEditDialog = false;
        window.setTimeout(function () { dlg.close(); window.location.reload(); }, 600);
        return;
      }
      var message = (result.data && result.data.error) || 'The key was rejected. Check it and try again.';
      setStatus(message, true);
      keyField.focus();
      keyField.select();
    }).catch(function () {
      busy = false;
      submit.disabled = false;
      keyField.disabled = false;
      setStatus('Reconnect failed. Please try again.', true);
    });
  });

  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

// Provider reconnect controls in an inbox banner act in place instead of
// handing the operator to the setup wizard. Google reuses the saved app
// credentials for a one-step OAuth round trip in this tab. Microsoft opens the
// wizard's sign-in step in a popup, because its localhost redirect still needs
// the paste-the-URL step, and the inbox reloads once that popup closes. The
// href stays the no-JS fallback and the path to different credentials, so any
// failure just follows it.
(function () {
  document.addEventListener('click', function (event) {
    var trigger = event.target.closest('[data-google-reconnect-now], [data-microsoft-reconnect]');
    if (!trigger) { return; }
    var href = trigger.getAttribute('href') || '';
    event.preventDefault();
    if (trigger.hasAttribute('data-microsoft-reconnect')) {
      if (!href) { return; }
      var popup = window.open(href, 'mm-microsoft-reconnect', 'popup=yes,width=560,height=760,resizable=yes,scrollbars=yes');
      if (!popup) { window.location.assign(href); return; }
      var watch = window.setInterval(function () {
        if (!popup.closed) { return; }
        window.clearInterval(watch);
        window.location.reload();
      }, 1000);
      return;
    }
    var inbox = trigger.getAttribute('data-inbox') || document.body.getAttribute('data-inbox') || '';
    var csrf = document.querySelector('[name=_csrf]');
    fetch('/ui/oauth/google/begin', {
      method: 'POST',
      credentials: 'same-origin',
      body: new URLSearchParams({ _csrf: csrf ? csrf.value : '', inbox_id: inbox })
    }).then(function (response) {
      if (!response.ok) { throw new Error('reconnect'); }
      return response.json();
    }).then(function (result) {
      if (!result || !result.authorization_url) { throw new Error('reconnect'); }
      window.location.assign(result.authorization_url);
    }).catch(function () {
      if (href) { window.location.assign(href); }
    });
  });
})();

// mammothmailReconnectBanner builds the per-inbox reconnect banner the server
// renders, from the connection state the /ui/actions poll reports. It is used by
// the message reader (a failure the client only discovers after painting) and by
// the sync strip (a verdict recorded while the page is already open), so both
// surfaces show the same control. The controls keep the same hooks as the
// server-rendered markup: the delegated handlers below open the One dialog or
// start the provider's in-place reconnect.
function mammothmailReconnectBanner(conn, inboxID) {
  var banner = document.createElement('div');
  banner.className = 'banner danger';
  banner.setAttribute('role', 'alert');
  banner.setAttribute('data-inbox-reconnect', '');
  var text = document.createElement('span');
  text.textContent = conn.detail || 'This connection needs to be reconnected.';
  banner.appendChild(text);
  inboxID = inboxID || document.body.getAttribute('data-inbox') || '';
  if (conn.reconnect === 'one') {
    var button = document.createElement('button');
    button.type = 'button';
    button.className = 'secondary btn-sm';
    button.setAttribute('data-one-reconnect', '');
    button.setAttribute('data-inbox', inboxID);
    button.textContent = conn.action_label || 'Reconnect with One';
    banner.appendChild(button);
  } else if (conn.reconnect === 'imap') {
    var settings = document.createElement('a');
    settings.className = 'btn secondary btn-sm';
    settings.href = '/ui/inboxes/' + encodeURIComponent(inboxID) + '/remote';
    settings.textContent = conn.action_label || 'Reconnect IMAP';
    banner.appendChild(settings);
  } else if (conn.reconnect) {
    var link = document.createElement('a');
    link.className = 'btn secondary btn-sm';
    var field = conn.reconnect === 'microsoft' ? 'microsoft_inbox' : 'google_inbox';
    link.href = '/ui/inboxes/standalone/new?' + field + '=' + encodeURIComponent(inboxID);
    link.textContent = conn.action_label || 'Reconnect';
    link.setAttribute('data-inbox', inboxID);
    link.setAttribute(conn.reconnect === 'microsoft' ? 'data-microsoft-reconnect' : 'data-google-reconnect-now', '');
    banner.appendChild(link);
  }
  return banner;
}
window.mammothmailReconnectBanner = mammothmailReconnectBanner;

// syncReconnectBanner puts that banner at the top of the mailbox content when
// the page did not already render one, so a connection the provider rejected
// mid-visit is actionable without a reload.
function syncReconnectBanner(conn, inboxID) {
  var content = document.querySelector('.mailcontent');
  if (!content || content.querySelector('[data-inbox-reconnect]')) { return; }
  content.insertBefore(mammothmailReconnectBanner(conn, inboxID), content.firstChild);
}

// The One import screen lets the operator tick whole rows. Any click in a row
// toggles its checkbox, highlights while selected, and shows None in the
// Index-history select until the row is actually selected, so an unselected
// row can never carry a window.
(function () {
  document.querySelectorAll('table.one-import-table').forEach(function (table) {
    table.querySelectorAll('tbody tr').forEach(function (row) {
      var box = row.querySelector('input[type=checkbox]');
      var select = row.querySelector('select');
      if (!box) {
        return;
      }
      if (!box.disabled) {
        row.style.cursor = 'pointer';
      }
      function hasNoneOption() {
        return !!select && !!select.querySelector('option[value=""]');
      }
      function sync() {
        var on = box.checked;
        row.classList.toggle('selected', on);
        if (select) {
          // An unusable row keeps its select permanently disabled.
          select.disabled = !on || box.disabled;
          if (hasNoneOption()) {
            if (!on) {
              if (select.value !== '') {
                select.setAttribute('data-chosen', select.value);
              }
              select.value = '';
            } else if (!box.disabled && select.value === '') {
              select.value = select.getAttribute('data-chosen') || 'all';
            }
          }
        }
      }
      box.addEventListener('change', sync);
      if (select) {
        select.addEventListener('change', function () {
          if (select.value !== '') {
            select.setAttribute('data-chosen', select.value);
          }
        });
      }
      row.addEventListener('click', function (e) {
        if (box.disabled) {
          return;
        }
        // The checkbox itself toggles natively; anything else in the row
        // toggles via the row, except an enabled select which needs its
        // clicks to open the dropdown. A disabled select still selects.
        if (e.target.closest('input,textarea,label,form,a,button')) {
          return;
        }
        var sel = e.target.closest('select');
        if (sel && !sel.disabled) {
          return;
        }
        box.checked = !box.checked;
        sync();
      });
      sync();
    });
  });
})();
