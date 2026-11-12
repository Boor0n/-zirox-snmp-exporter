// Applies the saved theme before the page paints (loaded without defer).
try {
  var t = localStorage.getItem('zirox-theme');
  if (t) document.documentElement.dataset.theme = t;
  if (localStorage.getItem('zirox-dense') === '1') document.documentElement.classList.add('dense');
} catch (e) {}
