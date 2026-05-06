const ages = ['adult','minor16','under16'];
function selectAge(a) {
  ages.forEach(id => {
    document.getElementById('age-'+id)?.classList.toggle('selected', id===a);
    const check = document.getElementById('check-'+id);
    if (check) check.classList.toggle('hidden', id!==a);
  });
}
selectAge('adult');
document.getElementById('age-adult').classList.add('selected');
