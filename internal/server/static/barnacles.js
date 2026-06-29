// "Decay made visible": every decaying card (inbox, triage rows + focus card,
// flotsam) declares data-barnacles + data-seed. For each, seed a tiny PRNG and
// scatter that many barnacle dots in a right-triangle anchored to the card's
// bottom-right corner. The seed is the link ID, so the crust is unique per link
// but identical every render — two same-age cards grow different patterns.
(function () {
  function rng(seed) {
    var s = seed >>> 0 || 1;
    return function () {
      s = (s * 1664525 + 1013904223) >>> 0;
      return s / 4294967296;
    };
  }

  var REGION_W = 130,
    REGION_H = 96; // triangular region anchored to the bottom-right corner

  document.querySelectorAll("[data-barnacles]").forEach(function (card, i) {
    var box = card.querySelector(".barnacles");
    if (!box) return;
    var n = +card.dataset.barnacles || 0;
    var rand = rng(+card.dataset.seed || (i + 1) * 97);
    for (var k = 0; k < n; k++) {
      var u = rand(),
        v = rand();
      if (u + v > 1) {
        u = 1 - u;
        v = 1 - v;
      } // uniform within the right triangle
      var x = u * REGION_W,
        y = v * REGION_H;
      var size = 9 + Math.floor(rand() * 12); // 9–20px
      var b = document.createElement("i");
      b.className = "barnacle";
      b.style.width = b.style.height = size + "px";
      b.style.right = 4 + x + "px";
      b.style.bottom = 4 + y + "px";
      b.style.opacity = (0.78 + rand() * 0.22).toFixed(2);
      box.appendChild(b);
    }
  });
})();
