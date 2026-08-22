# Admin web UI

Single-page admin interface for the gateway: it lists API keys, creates and revokes them, and shows month-to-date spend against the configured budget, talking only to the same-origin `/api/*` JSON endpoints. Built with Svelte 5, Vite, Tailwind CSS 4, and the [Foundation UI](https://github.com/SnowballSH/foundationui) design system, with fonts bundled so the page makes no external network requests at runtime. Build it with `npm ci && npm run build` (Node 22+), which produces the static bundle in `dist/` for the Go server to embed and serve.
