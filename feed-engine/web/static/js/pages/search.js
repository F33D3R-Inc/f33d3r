function showTab(name) {
  const tabs = ['posts', 'people'];
  tabs.forEach(t => {
    const panel = document.getElementById('panel-' + t);
    const btn   = document.getElementById('tab-' + t);
    if (!panel || !btn) return;
    const active = t === name;
    panel.style.display = active ? '' : 'none';
    btn.style.borderBottomColor = active ? 'var(--accent)' : 'transparent';
    btn.style.color = active ? 'var(--text-primary)' : 'var(--text-secondary)';
  });
}
