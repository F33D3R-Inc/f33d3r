# F33D3R Brand Assets

Drop your files here. The filenames below are what the site expects — rename yours to match exactly.

## Required files

| File | Size | Use |
|------|------|-----|
| `logo.png` | 512×512px, transparent background | Sidebar logo, app icon |
| `logo.svg` | Any, transparent | Preferred for crisp scaling at all sizes |
| `logo-wordmark.png` | 400×100px approx, transparent | "F33D3R" text + icon side by side |
| `favicon.png` | 32×32px | Browser tab |
| `apple-touch-icon.png` | 180×180px | iOS home screen icon |
| `og-image.png` | **1200×630px** | Social sharing card (Facebook, Twitter, iMessage preview) |
| `banner-default.jpg` | 1500×500px | Default profile header for new users |

## Tips

- **Logo**: PNG with transparent background, OR SVG. Do NOT use a white/black background — the site puts it on dark and light panels.
- **og-image**: This is the image that shows when someone shares a f33d3r.com link. Include the logo + tagline on a dark background. 1200×630 is required.
- **favicon**: 32×32 or 64×64. Will also be used as 16×16 via browser scaling.
- **banner-default**: Used as the header image on profiles that haven't uploaded their own banner. 1500×500px, JPG is fine.

## After dropping files

The site will serve them at `/static/brand/<filename>`. No restart needed — static files are served directly.

Commit and push: `git add feed-engine/web/static/brand/ && git commit -m "Add brand assets"`
