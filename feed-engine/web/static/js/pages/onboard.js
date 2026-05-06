function selectOnboardTheme(el, id, surface, accent) {
  document.querySelectorAll('.theme-option').forEach(o => o.classList.remove('selected'));
  el.classList.add('selected');
  const app = document.getElementById('f33d3r-app');
  app.dataset.theme = id;
  app.style.setProperty('--surface', surface);
  app.style.setProperty('--accent', accent);
  app.style.setProperty('--accent-muted', accent + 'AA');
}
