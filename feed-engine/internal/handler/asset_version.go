package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

// assetSubdirs are hashed for the cache-busting stamp. Only css and js: media and
// uploads hold user content and would make the walk unbounded.
var assetSubdirs = []string{"css", "js"}

// computeAssetVersion derives the ?v= stamp from the bytes on disk: editing any
// stylesheet or script changes every asset URL, an unchanged deploy does not.
func computeAssetVersion(staticDir string) (string, error) {
	sum := sha256.New()
	for _, sub := range assetSubdirs {
		root := filepath.Join(staticDir, sub)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			// Path as well as bytes, so a rename changes the stamp.
			if _, err := io.WriteString(sum, p); err != nil {
				return err
			}
			_, err = io.Copy(sum, f)
			return err
		})
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", root, err)
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// assetVersion returns the content hash, or a per-boot stamp if the tree cannot be
// read — cache-hostile on purpose, never silently stale.
func assetVersion(staticDir string) string {
	v, err := computeAssetVersion(staticDir)
	if err == nil {
		return v
	}
	log.Printf("[assets] cannot hash %s, falling back to a per-boot asset version: %v", staticDir, err)
	return fmt.Sprintf("boot%d", time.Now().UnixNano())
}
