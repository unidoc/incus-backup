// Applies the stored theme choice before the app boots, so a dark-mode user never sees a white flash.
// Kept as a separate file because the server's Content-Security-Policy forbids inline scripts.
;(function () {
  var t = 'system'
  try { t = localStorage.getItem('ib.theme') || 'system' } catch (e) { /* storage blocked */ }
  var dark = t === 'dark' || (t === 'system' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.dataset.theme = dark ? 'dark' : 'light'
})()
