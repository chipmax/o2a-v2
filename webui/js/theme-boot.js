// Apply the stored theme before first paint; theme.js owns every change.
try {
  var opencodeTheme = localStorage.getItem("opencode2api.ui.theme") || "light";
  document.documentElement.setAttribute("data-theme", opencodeTheme === "dark" ? "dark" : "light");
} catch (_) {}
