// Before paint: if the server didn't pin a theme (user follows the OS), pick
// Deep Tide in dark mode, else Foam & Shore — avoids a flash. Loaded
// synchronously in <head> so it runs before the body renders. Kept external (not
// inline) so the Content-Security-Policy can use script-src 'self'.
(function () {
  var el = document.documentElement;
  if (!el.getAttribute("data-theme")) {
    var dark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
    el.setAttribute("data-theme", dark ? "deep" : "foam");
  }
})();
