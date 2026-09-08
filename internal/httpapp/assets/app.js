(function () {
  var dlg = document.getElementById('provider-dialog');
  if (!dlg) {
    return;
  }
  var sel = document.getElementById('provider-select');
  var form = dlg.querySelector('form');
  var nameInput = form.querySelector('[name=name]');
  var lastDefault = '';

  function sync() {
    document.querySelectorAll('.provider-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-provider') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
  }

  function providerDescription() {
    var opt = sel.options[sel.selectedIndex];
    return opt ? opt.text : '';
  }

  function applyDefaultName() {
    var desc = providerDescription();
    if (nameInput.value === '' || nameInput.value === lastDefault) {
      nameInput.value = desc;
    }
    lastDefault = desc;
  }

  function open() {
    form.reset();
    form.querySelector('[name=id]').value = '';
    lastDefault = '';
    applyDefaultName();
    sync();
    dlg.showModal();
  }

  function openEdit(btn) {
    form.reset();
    form.querySelector('[name=id]').value = btn.dataset.id;
    sel.value = btn.dataset.provider;
    nameInput.value = btn.dataset.name;
    lastDefault = providerDescription();
    var cfg = {};
    try {
      cfg = JSON.parse(btn.dataset.config || '{}');
    } catch (e) {
      cfg = {};
    }
    form.querySelectorAll('input,select').forEach(function (el) {
      if (el.name && el.name.indexOf('cfg_') === 0) {
        var key = el.name.split('_').slice(2).join('_');
        if (cfg[key] !== undefined && cfg[key] !== null) {
          el.value = String(cfg[key]);
        }
      }
    });
    sync();
    dlg.showModal();
  }

  sel.addEventListener('change', function () {
    sync();
    applyDefaultName();
  });
  var add = document.getElementById('add-provider');
  if (add) {
    add.addEventListener('click', open);
  }
  var cancel = document.getElementById('provider-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
  document.querySelectorAll('.edit-provider').forEach(function (btn) {
    btn.addEventListener('click', function () {
      openEdit(btn);
    });
  });
})();

(function () {
  var dlg = document.getElementById('key-dialog');
  if (!dlg) {
    return;
  }
  var sel = document.getElementById('key-type');
  var form = dlg.querySelector('form');
  var idInput = form.querySelector('[name=id]');
  var nameInput = form.querySelector('[name=name]');
  var adminInput = form.querySelector('[name=admin]');

  function sync() {
    document.querySelectorAll('.key-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-type') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
  }

  function openCreate() {
    form.reset();
    form.action = '/ui/keys';
    idInput.value = '';
    sel.disabled = false;
    sync();
    dlg.showModal();
  }

  function openEdit(btn) {
    form.reset();
    var kind = btn.dataset.kind === 'hermes' ? 'hermes' : 'api';
    form.action = '/ui/' + (kind === 'hermes' ? 'hermes' : 'keys') + '/' + btn.dataset.id + '/edit';
    idInput.value = btn.dataset.id;
    sel.value = kind;
    sel.disabled = true;
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
      form.querySelectorAll('select[name^=role_]').forEach(function (el) {
        el.value = roles[el.name.slice(5)] || '';
      });
    } else {
      var inbox = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
      if (inbox && btn.dataset.inbox) {
        inbox.value = btn.dataset.inbox;
      }
    }
    sync();
    if (kind === 'hermes') {
      var inbox2 = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
      if (inbox2) {
        inbox2.disabled = true;
      }
    }
    dlg.showModal();
  }

  sel.addEventListener('change', sync);
  var add = document.getElementById('add-key');
  if (add) {
    add.addEventListener('click', openCreate);
  }
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
  document.querySelectorAll('tr.row-link[data-href]').forEach(function (row) {
    row.addEventListener('click', function (e) {
      if (e.target.closest('a,button,input,select,textarea,label,form')) {
        return;
      }
      window.location.href = row.getAttribute('data-href');
    });
  });
})();

(function () {
  var dlg = document.getElementById('inbox-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-inbox');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('inbox-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
})();

(function () {
  var dlg = document.getElementById('add-domain-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-domain');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
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
  var dlg = document.getElementById('domain-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('domain-form');
  var del = document.getElementById('domain-delete-form');
  var select = document.getElementById('domain-catchall');
  var title = document.getElementById('domain-dialog-title');
  document.querySelectorAll('.edit-domain').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = btn.dataset.id;
      form.action = '/ui/domains/' + id + '/catchall';
      del.action = '/ui/domains/' + id + '/delete';
      if (title) {
        title.textContent = btn.dataset.name || 'Domain';
      }
      select.value = btn.dataset.catchall || '';
      dlg.showModal();
    });
  });
  var cancel = document.getElementById('domain-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
})();
