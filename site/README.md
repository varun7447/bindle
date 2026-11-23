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

## Deploy to Cloudflare Pages

Nothing is set up yet. To publish the site:

1. In the Cloudflare dashboard, go to **Workers & Pages → Create → Pages → Connect to Git** and choose this repository.
2. Build settings: framework preset **None**, build command empty, build output directory **`site`**.
3. Save and deploy. Cloudflare serves the folder as-is and redeploys on every push to the production branch you pick; other branches get preview URLs.
4. Optional: add a custom domain under the project's **Custom domains** tab.

Alternatively, deploy from your machine with Wrangler: `npx wrangler pages deploy site --project-name <your-project>`.
