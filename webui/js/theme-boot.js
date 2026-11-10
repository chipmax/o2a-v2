// Apply the stored theme before first paint; theme.js owns every change.
try {
  var opencodeTheme = localStorage.getItem("opencode2api.ui.theme") || "auto";
  var opencodeDark =
    opencodeTheme === "dark" ||
    (opencodeTheme === "auto" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.setAttribute("data-theme", opencodeDark ? "dark" : "light");
} catch (_) {}
