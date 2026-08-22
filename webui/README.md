# Admin web UI

The gateway's single-page admin interface: list, create, and revoke API
keys, and watch month-to-date spend against the budget. It talks only to
the same-origin `/api/*` endpoints and makes no other network requests —
fonts are bundled.

Built with Svelte 5, Vite, Tailwind CSS 4, and
[Foundation UI](https://github.com/SnowballSH/foundationui). Rebuild
with `npm ci && npm run build` (Node 22+); the output in `dist/` is
committed and embedded by the Go server.
