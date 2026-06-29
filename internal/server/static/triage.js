// Keyboard triage for the list view. j/k move a highlight between rows; s/v/w/d
// fire that row's Schedule/Review/Wallabag/Drop action by submitting the matching
// button (so it flows through htmx). Keys are ignored while typing in a field.
(function () {
  let idx = -1;

  function rows() {
    return Array.from(document.querySelectorAll(".triage-row"));
  }

  function focusRow(i) {
    const list = rows();
    if (!list.length) {
      idx = -1;
      return;
    }
    idx = Math.max(0, Math.min(i, list.length - 1));
    list.forEach(function (r, n) {
      r.classList.toggle("focused", n === idx);
    });
    list[idx].focus();
  }

  function isTyping(el) {
    if (!el) return false;
    const tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
  }

  document.addEventListener("keydown", function (e) {
    if (isTyping(document.activeElement)) return;
    const list = rows();
    if (!list.length) return;

    switch (e.key) {
      case "j":
        e.preventDefault();
        focusRow(idx + 1);
        break;
      case "k":
        e.preventDefault();
        focusRow(idx <= 0 ? 0 : idx - 1);
        break;
      case "s":
      case "v":
      case "w":
      case "d": {
        if (idx < 0) focusRow(0);
        const row = rows()[idx];
        if (!row) return;
        const btn = row.querySelector('button[data-key="' + e.key + '"]');
        if (btn) {
          e.preventDefault();
          // click() (not form.requestSubmit) so htmx captures the submit button
          // as the submitter and includes its name/value (e.g. next_step=review).
          btn.click();
        }
        break;
      }
    }
  });
})();
