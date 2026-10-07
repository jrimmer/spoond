// The notifications panel's per-viewer dismissal (#qov): the panel is
// spoond's own system messages, and each message row carries a dismiss
// control (a × at its right edge). A dismissal is per browser, kept in
// localStorage under the message id, and never sent to the server — the
// dashboard stays read-only.
//
// A message hidden this way stays hidden while its trigger stays active,
// and comes back if the trigger clears and fires again.
//
// The panel rows come from the character grid: the page wrapper tags the
// panel rows (borders and title) with data-notifications and each message
// row with data-notice-id="<id>". This script:
//
//  1. Reads the message rows ([data-notice-id]) and the panel rows
//     ([data-notifications]) from the DOM after every render.
//  2. Forgets a dismissal whose id is no longer active, so a trigger that
//     clears and fires again shows again.
//  3. Hides every active row that is dismissed and the whole panel when
//     no message stays visible.
//  4. Adds a × control to every message row and records a click on it.
//
// The logic is best effort: with localStorage unavailable (private mode,
// a sandboxed page) it degrades to per-page dismissal. It is plain
// functions plus DOM scans, not an ESM import, so the panel works even if
// the module script fails to load. The reconciliation mirrors the Go
// function reconcileDismissed in render.go, which the tests cover; keep
// the two in step.

(function () {
  "use strict";

  // STORE_KEY is the localStorage key holding the dismissed ids. It is
  // versioned so a future format change never trips on an old value.
  var STORE_KEY = "spoond.dismissed.v1";
  var DISMISS_LABEL = "Dismiss this notification";
  var NOTICE_SEL = "[data-notice-id]";
  var PANEL_SEL = "[data-notifications]";

  // readDismissed parses the stored dismissal list, or an empty array
  // when localStorage is missing, empty, or holds junk.
  function readDismissed() {
    try {
      var raw = window.localStorage.getItem(STORE_KEY);
      if (!raw) return [];
      var parsed = JSON.parse(raw);
      if (Object.prototype.toString.call(parsed) !== "[object Array]") return [];
      var out = [];
      for (var i = 0; i < parsed.length; i++) {
        if (typeof parsed[i] === "string") out.push(parsed[i]);
      }
      return out;
    } catch (e) {
      return [];
    }
  }

  // writeDismissed persists the list, ignoring a storage failure.
  function writeDismissed(ids) {
    try {
      window.localStorage.setItem(STORE_KEY, JSON.stringify(ids));
    } catch (e) {
      /* no storage: a dismissal lives for this page only */
    }
  }

  // elements returns every node matching sel as an array.
  function elements(sel) {
    var nodes = document.querySelectorAll(sel);
    var out = [];
    for (var i = 0; i < nodes.length; i++) out.push(nodes[i]);
    return out;
  }

  // noticeID is a message row's id (its data-notice-id).
  function noticeID(el) {
    return el.getAttribute("data-notice-id") || "";
  }

  // activeIds is the ids the server currently sends.
  function activeIds() {
    var rows = elements(NOTICE_SEL);
    var ids = [];
    for (var i = 0; i < rows.length; i++) ids.push(noticeID(rows[i]));
    return ids;
  }

  // setDisplay sets an element's inline display only when it changes, so
  // a redundant write never churns the DOM.
  function setDisplay(el, shown) {
    var want = shown ? "" : "none";
    if (el.style.display !== want) el.style.display = want;
  }

  // applySync reconciles the stored dismissals with the active messages:
  // it forgets a dismissal whose id is no longer active, persists the
  // trimmed list, hides the dismissed rows, and hides the panel (borders
  // and title included) when none stay visible. It returns the active
  // ids.
  function applySync() {
    var active = activeIds();
    var activeSet = {};
    for (var i = 0; i < active.length; i++) activeSet[active[i]] = true;

    var stored = readDismissed();
    var kept = [];
    var dismissed = {};
    for (var j = 0; j < stored.length; j++) {
      // Forget a dismissal whose trigger has cleared: a later firing of
      // the same id must show again.
      if (activeSet[stored[j]]) {
        kept.push(stored[j]);
        dismissed[stored[j]] = true;
      }
    }
    if (kept.length !== stored.length) writeDismissed(kept);

    var rows = elements(NOTICE_SEL);
    var anyVisible = false;
    for (var k = 0; k < rows.length; k++) {
      var hidden = dismissed[noticeID(rows[k])] === true;
      setDisplay(rows[k], !hidden);
      if (!hidden) anyVisible = true;
    }
    var panel = elements(PANEL_SEL);
    for (var m = 0; m < panel.length; m++) setDisplay(panel[m], anyVisible);
    return active;
  }

  // dismiss records one id and applies the change at once.
  function dismiss(id) {
    if (!id) return;
    var stored = readDismissed();
    for (var i = 0; i < stored.length; i++) {
      if (stored[i] === id) return;
    }
    stored.push(id);
    writeDismissed(stored);
    applySync();
  }

  // injectControls adds a dismiss control (a × at the row's right edge) to
  // every message row that does not have one. The row is a grid cell run
  // (a span); the control is a span carrying the row's id in data-dismiss
  // so one delegated listener covers every row, including rows Datastar
  // patches in later.
  function injectControls() {
    var rows = elements(NOTICE_SEL);
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      if (row.querySelector("[data-dismiss]")) continue;
      var control = document.createElement("span");
      control.setAttribute("data-dismiss", noticeID(row));
      control.setAttribute("role", "button");
      control.setAttribute("tabindex", "0");
      control.setAttribute("aria-label", DISMISS_LABEL);
      control.setAttribute("title", DISMISS_LABEL);
      control.textContent = " ×";
      row.appendChild(control);
    }
  }

  // refresh injects the controls (idempotent), then reconciles the stored
  // dismissals with what the server now sends, and returns the active
  // ids.
  function refresh() {
    injectControls();
    return applySync();
  }

  // controlAt walks up from an event target to a dismiss control.
  function controlAt(el) {
    while (el && el !== document) {
      if (el.hasAttribute && el.hasAttribute("data-dismiss")) return el;
      el = el.parentNode;
    }
    return null;
  }

  function onClick(ev) {
    var control = controlAt(ev.target);
    if (control) {
      dismiss(control.getAttribute("data-dismiss"));
      ev.preventDefault();
    }
  }

  function onKey(ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var control = controlAt(ev.target);
    if (control) {
      dismiss(control.getAttribute("data-dismiss"));
      ev.preventDefault();
    }
  }

  // watch runs the panel's dismissal on every rendered row: once now,
  // then on every click/keypress and DOM mutation, so a Datastar row
  // patch (which replaces spans without a reload) is picked up too. The
  // observer watches childList only, so the display changes this script
  // makes do not re-trigger it.
  function watch() {
    refresh();
    document.addEventListener("click", onClick, true);
    document.addEventListener("keydown", onKey, true);
    if (window.MutationObserver) {
      new MutationObserver(function () { refresh(); })
        .observe(document.documentElement, {childList: true, subtree: true});
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", watch);
  } else {
    watch();
  }

  // Expose the pieces for a browser console or a future test without
  // leaking anything else.
  window.spoondNotices = {
    storeKey: STORE_KEY,
    readDismissed: readDismissed,
    writeDismissed: writeDismissed,
    activeIds: activeIds,
    applySync: applySync,
    dismiss: dismiss,
    injectControls: injectControls,
    refresh: refresh
  };
})();
