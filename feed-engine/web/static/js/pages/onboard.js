F33D3R.page('onboard', function () {
  function selectOnboardTheme(el, id, surface, accent) {
    document.querySelectorAll('.theme-option').forEach(o => o.classList.remove('selected'));
    el.classList.add('selected');
    document.body.dataset.theme = id;
  }
  window.selectOnboardTheme = selectOnboardTheme;
});
