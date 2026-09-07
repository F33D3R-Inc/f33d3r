package middleware

import (
	"io/fs"
	"net/http"
)

// NoDirListing wraps an http.FileSystem so that directories do not exist.
// http.FileServer renders an index of any directory that lacks index.html —
// a map of every asset, build artefact and stray file under web/static, handed
// to anyone who asks for /static/ or /static/js/. Refusing the directory at the
// filesystem level is what makes FileServer answer 404 for it, and also stops
// the "dir" → "dir/" redirect that would otherwise announce which paths are
// directories.
func NoDirListing(inner http.FileSystem) http.FileSystem {
	return noDirFS{inner}
}

type noDirFS struct{ http.FileSystem }

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.FileSystem.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}
