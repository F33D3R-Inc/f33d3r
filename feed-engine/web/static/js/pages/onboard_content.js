const prefs = ['default','safe','adult'];
function selectContent(val) {
  const keyMap = {default:'default', safe_mode:'safe', adult_enabled:'adult'};
  const id = keyMap[val] || val;
  prefs.forEach(p => {
    document.getElementById('pref-'+p)?.classList.toggle('selected', p===id);
    const chk = document.getElementById('check-'+(p==='adult'?'adult-pref':p));
    if (chk) chk.classList.toggle('hidden', p!==id);
  });
  // Update radio
  const radio = document.querySelector(`input[value="${val}"]`);
  if (radio) radio.checked = true;
}
