// Minimal drag-and-drop for the Kanban board. On drop, POST the new column to
// the server; if it succeeds, move the card in the DOM. No external deps.
(function () {
  let dragId = null;

  document.addEventListener("dragstart", function (e) {
    const card = e.target.closest(".card");
    if (!card) return;
    dragId = card.dataset.id;
    e.dataTransfer.effectAllowed = "move";
  });

  document.querySelectorAll(".cards").forEach(function (list) {
    list.addEventListener("dragover", function (e) {
      e.preventDefault();
      list.classList.add("drop-target");
    });
    list.addEventListener("dragleave", function () {
      list.classList.remove("drop-target");
    });
    list.addEventListener("drop", function (e) {
      e.preventDefault();
      list.classList.remove("drop-target");
      if (!dragId) return;
      const column = list.dataset.column;
      const position = list.querySelectorAll(".card").length;
      const card = document.querySelector('.card[data-id="' + dragId + '"]');
      const body = new URLSearchParams({ column: column, position: String(position) });
      fetch("/cards/" + dragId + "/move", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: body.toString(),
      }).then(function (res) {
        if (res.ok && card) {
          list.appendChild(card);
          updateCounts();
        }
      });
      dragId = null;
    });
  });

  function updateCounts() {
    document.querySelectorAll(".column").forEach(function (col) {
      const n = col.querySelectorAll(".card").length;
      const badge = col.querySelector("h2 .count");
      if (badge) badge.textContent = n;
    });
  }
})();
