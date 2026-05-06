        document.querySelectorAll('.social-link-input').forEach(function(inp) {
          var prefix = inp.dataset.prefix;
          // Show just the username part in the field
          if (inp.value && inp.value.startsWith(prefix)) {
            inp.value = inp.value.slice(prefix.length);
          }
          // On focus: no change — user sees just username
          inp.addEventListener('blur', function() {
            // Strip prefix if user accidentally typed the full URL
            if (this.value.startsWith('http')) {
              try {
                var url = new URL(this.value);
                var base = new URL(prefix);
                if (url.hostname === base.hostname) {
                  this.value = url.pathname.replace(/^\/(@?)/, '');
                }
              } catch(_) {}
            }
          });
          // Before form submit: prepend prefix if value is non-empty
          inp.form && inp.form.addEventListener('submit', function() {
            if (inp.value && !inp.value.startsWith('http')) {
              inp.value = prefix + inp.value.replace(/^\//, '');
            }
          }, {once:false});
        });
