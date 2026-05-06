const roles = ['user','creator','official'];
function selectRole(r) {
  roles.forEach(id => {
    document.getElementById('role-'+id)?.classList.toggle('selected', id===r);
    const check = document.getElementById('check-'+id);
    if (check) check.classList.toggle('hidden', id!==r);
  });
  document.getElementById('creator-type-wrap').style.display = r==='creator' ? '' : 'none';
}
// Init
selectRole('user');
document.getElementById('role-user').classList.add('selected');
