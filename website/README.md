# shpiel.dev

The info site for Shpiel, served from Cloudflare Workers at
[shpiel.dev](https://shpiel.dev).

One worker ([src/index.ts](src/index.ts)) server-renders two routes —
`/` and `/releases` — with live data from GitHub (star count, releases),
edge-cached for 10 minutes. Files in
[public/](public/) (stylesheet, self-hosted Geist fonts, favicon) are
served as static assets. The site ships zero client-side JavaScript, so
a GitHub API hiccup degrades to hidden stars / a "see it on GitHub"
link, never a broken page.

## Develop

```bash
task website:dev      # wrangler dev on http://127.0.0.1:8787
task website:check    # tsc --noEmit + wrangler deploy --dry-run
```

Node and pnpm come from hermit like every other tool in this repo.

## Deploy

Deploys run on **Cloudflare Workers Builds** — the repo is connected in
the Cloudflare dashboard (Worker `shpiel-website`, path `/website`, build
watch path `website/**`). Pushes to `main` run `npx wrangler deploy`;
other branches run `npx wrangler versions upload`, which posts a preview
URL and build status on the PR. No build command: dependencies install
via pnpm (lockfile-detected) and wrangler bundles the TypeScript itself.

[website.yaml](../.github/workflows/website.yaml) is typecheck-only CI;
no Cloudflare secrets live in GitHub. For a manual deploy from a laptop:
`pnpm exec wrangler login` once, then `task website:deploy`.

[wrangler.jsonc](wrangler.jsonc) declares `shpiel.dev` and
`www.shpiel.dev` as custom domains — the zone must live in the same
Cloudflare account (the Workers Builds API token needs zone permissions
for it), and `wrangler deploy` attaches both automatically (www 301s to
the apex in the worker).

## Design

The Loewenthal black/Geist look — highlight blocks, hairline borders,
the giant footer wordmark — structured like [spegel.dev](https://spegel.dev):
hero, feature grid, install, compatibility. Fonts are committed woff2s
from [Fontsource](https://fontsource.org/fonts/geist-sans) (OFL).
