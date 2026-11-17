# Console design mockups

Design source for the Bindle console (sample data only). These are the
artboards from the Claude design canvas "Agent Observability Console":
https://claude.ai/artifact/CqY7S9NwRvgXeZEdFJQKyx

| File | Screen |
|---|---|
| `Main.dc.html` | Overview: KPIs, runs-per-hour chart, active alerts, recent runs |
| `Trace.dc.html` | Trace explorer: step waterfall + step detail (input, output, reasoning, redaction notice) |
| `Connect.dc.html` | Connect an agent: collection methods, framework picker, setup snippet, ingest key |
| `canvas.json` | Canvas layout (artboard positions and sizes) |

The `.dc.html` files use the canvas's component format (`<x-dc>` markup plus a
`DCLogic` class), so they render inside the design canvas, not as standalone
pages. Use them as the visual spec when building `web/` (Next.js):

- Background `#0E1116`, panels `#161B22`, borders `#232A33`, text `#E6E8EB`, muted `#9AA3AE`, accent `#3DD6B0`
- Step colors: llm `#6CA8FF`, tool `#F2A65A`, decision `#B79CFF`, retrieval `#7FD4E8`, eval `#F5C26B`, handoff `#E58AD0`
- Fonts: Inter (UI), JetBrains Mono (IDs, numbers, code)
