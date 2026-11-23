// Interactions for the product website: tabs, the features filter, the FAQ
// accordion and the copy button. No dependencies; works without a build step.
(function () {
  'use strict';

  function all(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

  // Tabs: a role="tablist" whose buttons point at panels with aria-controls.
  // onSelect lets a tablist do something other than show one panel (the
  // features filter). Arrow keys, Home and End move between tabs.
  function tablist(list, onSelect) {
    var tabs = all('[role="tab"]', list);
    function select(i, focus) {
      tabs.forEach(function (t, j) {
        var on = i === j;
        t.classList.toggle('on', on);
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.setAttribute('tabindex', on ? '0' : '-1');
        if (!onSelect) {
          var panel = document.getElementById(t.getAttribute('aria-controls'));
          if (panel) panel.hidden = !on;
        }
      });
      if (onSelect) onSelect(tabs[i]);
      if (focus) tabs[i].focus();
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(i, false); });
      t.addEventListener('keydown', function (e) {
        var n = tabs.length, k = e.key, to = null;
        if (k === 'ArrowRight' || k === 'ArrowDown') to = (i + 1) % n;
        else if (k === 'ArrowLeft' || k === 'ArrowUp') to = (i - 1 + n) % n;
        else if (k === 'Home') to = 0;
        else if (k === 'End') to = n - 1;
        if (to !== null) { e.preventDefault(); select(to, true); }
      });
    });
  }

  all('[role="tablist"]').forEach(function (list) {
    if (list.getAttribute('aria-label') === 'Filter features') {
      tablist(list, function (tab) {
        var group = tab.getAttribute('data-filter');
        all('#feature-list > article').forEach(function (a) {
          a.hidden = group !== 'All' && a.getAttribute('data-group') !== group;
        });
      });
    } else {
      tablist(list);
    }
  });

  // FAQ: one answer open at a time; clicking the open question closes it.
  var questions = all('button.faq-q');
  questions.forEach(function (q) {
    q.addEventListener('click', function () {
      var opening = q.getAttribute('aria-expanded') !== 'true';
      questions.forEach(function (other) {
        var open = opening && other === q;
        other.setAttribute('aria-expanded', open ? 'true' : 'false');
        var answer = document.getElementById(other.getAttribute('aria-controls'));
        if (answer) answer.hidden = !open;
        var sign = other.querySelector('[aria-hidden="true"]');
        if (sign) sign.textContent = open ? '−' : '+';
      });
    });
  });

  // Copy button: label changes to "Copied"; a live region tells screen readers.
  all('[data-copy]').forEach(function (b) {
    b.addEventListener('click', function () {
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(b.getAttribute('data-copy')).then(function () {
        b.textContent = 'Copied';
        var status = b.parentNode.querySelector('[data-copy-status]');
        if (status) status.textContent = 'Copied';
      }, function () {});
    });
  });
})();
