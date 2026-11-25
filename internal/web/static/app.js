// byname — progressive enhancement only; every page works without JavaScript.
'use strict';

// Ask before destructive actions (forms marked data-confirm).
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) {
    e.preventDefault();
  }
});

// "/" focuses the search box, like most search UIs.
document.addEventListener('keydown', function (e) {
  if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
  var t = e.target;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
  var box = document.querySelector('.search input');
  if (box) {
    e.preventDefault();
    box.focus();
    box.select();
  }
});
