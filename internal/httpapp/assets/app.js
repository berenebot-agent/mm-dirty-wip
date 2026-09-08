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

  function sync() {
    document.querySelectorAll('.key-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-type') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
  }

  sel.addEventListener('change', sync);
  var add = document.getElementById('add-key');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      sync();
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('key-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
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
