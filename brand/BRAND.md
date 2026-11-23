# Bindle brand guide

**Direction:** Midnight + Indigo, set in Geist. Calm, precise, developer-first. One indigo accent on deep navy, with colour used for meaning, never decoration.

"Bindle" is the working name. Keep the name in as few places as possible (logo, wordmark, package names, the site's title and copy) so a rename later is an afternoon's work.

## Logo

| File | Use |
|---|---|
| `logo/mark.svg` | The mark: three staggered white bars (the steps of one run, like a trace waterfall) on an indigo `#4C56D8` tile with 5/24 rounded corners. App icon, avatar, small spaces. |
| `logo/favicon.svg` | Same as the mark. Browser tab icon. |
| `logo/lockup-on-dark.svg` | Mark + wordmark in `#E8ECF7`, for dark backgrounds (the site, the dark console). |
| `logo/lockup-on-light.svg` | Mark + wordmark in `#0A0F1E`, for light backgrounds (the light console, README, docs). |
| `logo/mark-mono-white.svg` | One-colour version for places that can't show indigo (dark photos, merch). |

- **Wordmark:** lowercase "bindle", Geist 600, tight tracking (−0.04em). Never capitalise it in the logo; in sentences write "Bindle".
- **Clear space:** at least half the mark's width on every side. Minimum size: the mark at 16 px, the lockup at 96 px wide.
- **Don't:** recolour the tile, add gradients or shadows, rotate or stretch it, put the indigo tile on a busy indigo background, or redraw the bars.
- The lockup SVGs use live Geist text. Before using them where Geist isn't loaded (emails, slides), convert the text to outlines in a design tool.

## Colour

The palette is deep navy with one indigo accent. Indigo means "Bindle" or "you can act here". Data colours are a separate set and never share a job with the brand colour.

### Brand

| Token | Hex | Role |
|---|---|---|
| Indigo | `#4C56D8` | Logo tile, primary buttons (white text) |
| Indigo light | `#8E98FF` | Links, section labels, focus and current items on dark |
| Midnight | `#0A0F1E` | Darkest background; ink on light |

### Interface themes

Full values are in `tokens.css` and `tokens.json`.

| Role | Dark (Midnight) | Light |
|---|---|---|
| Page | `#0A0F1E` | `#F7F8FB` |
| Sidebar | `#0D1326` | `#FBFBFD` |
| Card | `#111831` | `#FFFFFF` |
| Text | `#E8ECF7` | `#0A0F1E` |
| Secondary text | `#AEB6CF` | `#4A5068` |
| Muted text | `#8A94B0` | `#636A82` |
| Accent (links, focus) | `#8E98FF` | `#4C56D8` |
| Primary button | `#4C56D8` + white | `#4C56D8` + white |

The marketing site uses the dark theme only. The console offers both, following the OS by default with a manual toggle.

### Step types (data colours)

These colour trace waterfalls, time breakdowns and charts. They are the console's validated categorical palette. Every step also prints its type as a label, so colour is never the only cue.

| Step | Light | Dark |
|---|---|---|
| LLM | `#2A78D6` | `#3987E5` |
| Tool | `#EB6834` | `#D95926` |
| Retrieval | `#1BAF7A` | `#199E70` |
| Decision | `#77307C` | `#983DBC` |
| Eval | `#EDA100` | `#C98500` |
| Handoff | `#E87BA4` | `#D55181` |
| Agent / other | `#898781` | `#66708F` |

Decision is **plum**, not violet: violet sat too close to the indigo accent (colour-blind separation of 0.6 light and 4.6 dark, where 8 is the target). Plum clears the accent (14.8 light, 17.2 dark) and every other step type (8.1 or more for colour-blind readers, 15 or more for normal vision).

### Status

Good, warning, critical and info keep their own reserved colours (see `tokens.css`) and always come with an icon and a word ("✕ Failed"), never colour alone. A failure in a waterfall is shown with the critical colour as an outline or row tint, not by repainting the step's type colour.

### Contrast (WCAG)

All text pairs pass AA (4.5:1) or better:

| Pair | Ratio |
|---|---|
| Dark: text / secondary / muted on card | 14.8 / 8.7 / 5.8 |
| Dark: indigo-light link on card | 6.7 |
| White on indigo button | 5.8 |
| Light: text / secondary / muted on white | 19.1 / 8.0 / 5.4 |
| Light: muted on page | 5.1 |
| Light: indigo link on white | 5.8 |
| Site: body copy / quiet text on page | 8.8 / 6.3 |

## Typography

- **Geist** for everything, **Geist Mono** for code, IDs, durations and numbers in tables. Both are free on Google Fonts. In Next.js, use `next/font/google` (`Geist`, `Geist_Mono`).
- **Headlines:** Geist 700–800, tight tracking (−0.025 to −0.035em), line height 1.05–1.1.
- **Body:** 15–18 px, line height 1.6. Never below 13 px for anything people need to read.
- **Labels:** small uppercase eyebrows (13 px, 600, +0.08em) in indigo light above section titles.
- We tested Source Serif for headlines and chose Geist: the brand should read as a precise engineering tool.

## Shape and layout

- Corners: 6 px (chips), 10 px (buttons, inputs), 16 px (cards), 24 px (large callouts).
- Hairline borders (1 px) instead of shadows on cards. One soft indigo glow is allowed behind a hero; no other gradients.
- Content width 1200 px with 32 px side padding. Grids collapse to one column below 900 px.
- Touch targets at least 44 px tall.

## Voice

- **Answer first, evidence second.** Lead with the sentence ("refund-agent failed after 180 ms."), then the detail.
- **Plain words.** "Run", "step", "cost", "failed". Explain jargon in place, or avoid it.
- **Specific, not superlative.** Real numbers and real examples. Never invent stats, customer logos or testimonials.
- **Honest limits.** Say "unknown cost", not a guess. Say what a feature doesn't do.
- **Sentence case** for headings and buttons.
- **No roadmap in public.** The website, README and public docs describe only what ships today.

## Where the brand lives in the repo

```
brand/
  BRAND.md          this guide
  tokens.css        CSS custom properties, light and dark
  tokens.json       the same values for scripts and other stacks
  logo/             mark, favicon, lockups, one-colour mark
console/app/globals.css   the console's tokens (same names and values as tokens.css)
site/                     the product website (plain HTML and CSS on these tokens)
docs/images/              logo and console screenshots used by the README and the site
```
