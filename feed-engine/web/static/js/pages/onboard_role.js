F33D3R.page('onboard_role', function () {
  const roles = ['user','creator','official'];
  function selectRole(r) {
    roles.forEach(id => {
      document.getElementById('role-'+id)?.classList.toggle('selected', id===r);
      const check = document.getElementById('check-'+id);
      if (check) check.classList.toggle('hidden', id!==r);
    });
    const wrap = document.getElementById('creator-type-wrap');
    if (wrap) wrap.style.display = r==='creator' ? '' : 'none';
  }
  window.selectRole = selectRole;
  // Init
  if (document.getElementById('role-user')) {
    selectRole('user');
    document.getElementById('role-user').classList.add('selected');
  }
});
