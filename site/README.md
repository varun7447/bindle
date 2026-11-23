# Product website

A static site: three pages of plain HTML, one stylesheet and a small script. No framework, no build step.

| File | What it is |
|---|---|
| `index.html` | Home: hero, integrations, console tour, features, how it works, security, FAQ |
| `features.html` | Every feature, with a category filter |
| `docs.html` | Get started: quickstart, local stack, instrumenting an agent, configuration, SDK API, CLI and MCP, make targets, deploying to AWS, repository layout |
| `styles.css` | All styles, built on the brand tokens (see below) |
| `main.js` | Tabs (console tour, code examples, docs language), the features filter, the FAQ accordion and the copy button |
| `images/` | Console screenshots |
| `favicon.svg` | The mark, from `brand/logo/favicon.svg` |

## Preview locally

```bash
python3 -m http.server -d site 8000
```

Then open http://localhost:8000. The fonts (Geist, Geist Mono) load from Google Fonts, so the preview needs internet access to show them.

## Editing

- **Colours** are CSS variables. The first block of `styles.css` repeats the dark-theme values from [`brand/tokens.css`](../brand/tokens.css); keep the two in sync when the brand changes. `--site-*` variables are site-only shades.
- **Screenshots** in `images/` are copies of `docs/images/console-*.png`, so the site can be deployed from this folder alone. When you replace the screenshots in `docs/images/`, copy them here too.
- **Copy** describes only what ships today, following the voice rules in [`brand/BRAND.md`](../brand/BRAND.md).
- **Accessibility:** tabs use `role="tablist"`/`role="tab"`/`aria-selected` and support the arrow keys, Home and End; FAQ buttons update `aria-expanded`; the copy button announces "Copied" through a live region; focus outlines stay visible.

## Deploy (GitHub Pages)

[`.github/workflows/pages.yml`](../.github/workflows/pages.yml) publishes this folder to GitHub Pages whenever a push to `main` changes `site/`, or when you run the **Website** workflow by hand from the Actions tab. Nothing is built: the folder is uploaded as-is.

- **Address:** `https://varun7447.github.io/bindle/` (links are relative, so the site also works on a custom domain).
- **One-time setup:** in the repository's **Settings → Pages**, the source must be **GitHub Actions**.
- **Custom domain (optional):** add it under **Settings → Pages → Custom domain**, create the DNS record GitHub shows, and turn on **Enforce HTTPS**.
- **Terms:** GitHub Pages is meant for project sites like this one, not for hosting a commercial service or anything that takes payments.
