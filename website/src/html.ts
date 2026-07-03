import { GITHUB_URL, type Release } from "./github";
import { esc, formatCount, formatDate, renderMarkdown } from "./markdown";

interface PageProps {
  title: string;
  description: string;
  path: string;
  stars: number | null;
}

function navLink(href: string, label: string, currentPath: string): string {
  const current = href === currentPath ? ' aria-current="page"' : "";
  return `<a href="${href}"${current}>${label}</a>`;
}

function layout(props: PageProps, content: string): string {
  const stars =
    props.stars === null ? "" : `<span class="stars">★ ${esc(formatCount(props.stars))}</span>`;
  const url = `https://shpiel.dev${props.path}`;
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(props.title)}</title>
<meta name="description" content="${esc(props.description)}">
<meta name="theme-color" content="#000000">
<link rel="canonical" href="${url}">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="stylesheet" href="/style.css">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Shpiel">
<meta property="og:title" content="${esc(props.title)}">
<meta property="og:description" content="${esc(props.description)}">
<meta property="og:url" content="${url}">
<meta name="twitter:card" content="summary">
</head>
<body>
<header class="nav">
  <div class="nav-inner">
    <a class="brand" href="/">shpiel<span class="tld">.dev</span></a>
    <nav aria-label="Main">
      ${navLink("/releases", "Releases", props.path)}
      <a class="gh" href="${GITHUB_URL}">GitHub${stars}</a>
    </nav>
  </div>
</header>
<main>
${content}
</main>
<footer class="footer">
  <div class="footer-links wrap">
    <a href="${GITHUB_URL}">GitHub</a>
    <a href="/releases">Releases</a>
    <a href="${GITHUB_URL}/blob/main/spec.md">Spec</a>
    <a href="${GITHUB_URL}/blob/main/docs/spegel.md">Spegel guide</a>
    <span class="footer-meta">Apache-2.0 · A Loewenthal Corporation project</span>
  </div>
  <svg class="footer-mark" viewBox="0 0 1600 300" preserveAspectRatio="xMidYMax meet" aria-hidden="true">
    <text x="800" y="242" text-anchor="middle" textLength="1460" lengthAdjust="spacingAndGlyphs">shpiel</text>
  </svg>
</footer>
</body>
</html>`;
}

const FEATURES: Array<{ title: string; body: string }> = [
  {
    title: "Exact HF front",
    body: "Repo info, byte-range file resolution, tree listings, commits, preupload, git-LFS batch — matching the Hub's headers, error codes, and pagination quirks, because compatibility is only useful when it's exact.",
  },
  {
    title: "Xet, server-side",
    body: "<code>huggingface_hub</code> 1.x uploads through the Xet protocol with no LFS fallback. Shpiel implements the CAS API — the first open-source server that does — so stock clients push and pull chunk-level.",
  },
  {
    title: "Pull-through caching",
    body: "On a miss, fetch from huggingface.co, persist to your backend, serve. Request collapsing means a hundred nodes asking for the same model cost one upstream download.",
  },
  {
    title: "OCI backend",
    body: "Models land as OCI artifacts: one repository per model, one manifest per commit, one layer per file. The <code>tar-layers</code> format is a mountable image — Kubernetes image volumes and Spegel work out of the box.",
  },
  {
    title: "Filesystem backend",
    body: "Byte-compatible with the <code>huggingface_hub</code> cache. Mount the volume, set <code>HF_HUB_OFFLINE=1</code>, and <code>from_pretrained</code> reads it directly.",
  },
  {
    title: "Ops built in",
    body: "Fan-out replication through a disk-spooled retry queue, Prometheus metrics with a ready-made Grafana dashboard, an append-only audit stream, an authenticated admin API, health probes.",
  },
];

const COMPAT: Array<{ client: string; reads: string; writes: string }> = [
  {
    client: "<code>huggingface_hub</code> / <code>hf</code> CLI 1.x",
    reads: "✓ <span class='note'>HTTP or chunk-level Xet</span>",
    writes: "✓ <span class='note'>via Xet</span>",
  },
  {
    client: "<code>huggingface_hub</code> / <code>hf</code> CLI 0.x",
    reads: "✓",
    writes: "✓ <span class='note'>via git-LFS</span>",
  },
  {
    client: "vLLM, SGLang, TGI, <code>from_pretrained</code>",
    reads: "✓ <span class='note'>incl. Range / lazy loading</span>",
    writes: "—",
  },
  {
    client: "<code>HF_HUB_OFFLINE=1</code> on a shared volume",
    reads: "✓ <span class='note'>fs backend</span>",
    writes: "—",
  },
];

export function homePage(stars: number | null, latest: Release | null): string {
  const releasePill = latest
    ? `<a class="pill" href="/releases"><span class="dot"></span>${esc(latest.tag)} out now${
        latest.publishedAt ? `<span class="pill-date">· ${esc(formatDate(latest.publishedAt))}</span>` : ""
      }</a>`
    : "";

  const features = FEATURES.map(
    (f) => `<div class="cell"><h3>${f.title}</h3><p>${f.body}</p></div>`
  ).join("\n      ");

  const compatRows = COMPAT.map(
    (r) => `<tr><td>${r.client}</td><td>${r.reads}</td><td>${r.writes}</td></tr>`
  ).join("\n        ");

  const content = `
<section class="hero wrap">
  ${releasePill}
  <h1>An <mark>HF&#8209;compatible</mark> model&nbsp;relay.</h1>
  <p class="lead">Shpiel speaks the Hugging Face Hub API — read, write, and Xet — and stores models in the infrastructure your cluster already runs: OCI registries and filesystems today, object storage next. Every existing HF tool works unchanged.</p>
  <pre><code><span class="c"># the whole integration:</span>
export HF_ENDPOINT=https://shpiel.internal

hf download Qwen/Qwen3-0.6B   <span class="c"># LAN-speed, cached in your registry</span></code></pre>
  <div class="cta-row">
    <a class="btn" href="#install">Get started</a>
    <a class="btn btn-ghost" href="${GITHUB_URL}">View on GitHub</a>
  </div>
  <p class="clients">hf CLI · huggingface_hub · vLLM · SGLang · TGI — no new tools, no new auth universe.</p>
</section>

<section class="section wrap">
  <p class="eyebrow">Why</p>
  <h2>The bridge as <mark>one boring binary</mark>.</h2>
  <div class="prose-cols">
    <p>Researchers live on the Hugging Face API: every training script ends in <code>push_to_hub()</code>, every inference engine starts with <code>from_pretrained()</code>. Clusters want weights as versioned, content-addressed, P2P-distributable artifacts — that's where cold-start time, egress cost, and reliability are actually won. Today the bridge between those planes is shell scripts, or a heavyweight self-hosted hub with its own database and auth universe.</p>
    <p>Shpiel is the bridge as one boring binary: no database, one YAML file, identical on a laptop and in Kubernetes. On autoscaled GPU fleets it turns time-to-first-token from an internet-sized download into a LAN-speed pull — and, paired with <a href="https://spegel.dev">Spegel</a>, a peer-to-peer pull from neighboring nodes.</p>
  </div>
</section>

<section class="section wrap">
  <p class="eyebrow">The pieces</p>
  <div class="features">
      ${features}
  </div>
</section>

<section class="section wrap" id="install">
  <p class="eyebrow">Install</p>
  <div class="install-grid">
    <div>
      <h3>Run it</h3>
      <pre><code><span class="c"># container image</span>
docker run -p 8080:8080 ghcr.io/loewenthal-corp/shpiel:latest \\
  serve --local --listen-api :8080

<span class="c"># Helm chart</span>
helm install shpiel oci://ghcr.io/loewenthal-corp/charts/shpiel

<span class="c"># from source</span>
go install github.com/loewenthal-corp/shpiel/cmd/shpiel@latest

<span class="c"># binaries for linux/darwin on every release:</span>
<span class="c"># github.com/loewenthal-corp/shpiel/releases</span></code></pre>
    </div>
    <div>
      <h3>Point your tools at it</h3>
      <pre><code><span class="c"># laptop mode: fs store in ~/.shpiel, pull-through on</span>
shpiel serve --local

export HF_ENDPOINT=http://127.0.0.1:8080
hf download Qwen/Qwen3-0.6B   <span class="c"># first pull caches it</span>
hf download Qwen/Qwen3-0.6B   <span class="c"># second is served locally</span>
hf upload my-org/my-model ./model</code></pre>
    </div>
  </div>
  <p class="section-note">Beyond laptop mode, everything is one YAML file — <a href="${GITHUB_URL}/blob/main/config.example.yaml">config.example.yaml</a> documents every knob. The Helm chart's <code>config</code> value <em>is</em> that file.</p>
</section>

<section class="section wrap">
  <p class="eyebrow">Compatibility</p>
  <table>
    <thead><tr><th>Client</th><th>Reads</th><th>Writes</th></tr></thead>
    <tbody>
        ${compatRows}
    </tbody>
  </table>
  <p class="section-note">Enforced by an executable conformance suite that runs against every serving configuration, and end-to-end tests that drive a real Python <code>huggingface_hub</code>/<code>hf_xet</code> client against the real binary. If it regresses, CI fails.</p>
</section>

<section class="section wrap cta-final">
  <h2>Stop pulling weights over the&nbsp;internet.</h2>
  <div class="cta-row">
    <a class="btn" href="${GITHUB_URL}">Get started on GitHub</a>
    <a class="btn btn-ghost" href="${GITHUB_URL}/blob/main/spec.md">Read the spec</a>
  </div>
</section>`;

  return layout(
    {
      title: "Shpiel — an HF-compatible model relay",
      description:
        "Shpiel speaks the Hugging Face Hub API — read, write, and Xet — and stores models in OCI registries and filesystems your cluster already runs. Every HF tool works unchanged.",
      path: "/",
      stars,
    },
    content
  );
}

export function releasesPage(stars: number | null, releases: Release[] | null): string {
  let body: string;
  if (releases === null) {
    body = `<p class="error">Couldn't load releases from GitHub right now — see them <a href="${GITHUB_URL}/releases">on GitHub</a> instead.</p>`;
  } else if (releases.length === 0) {
    body = `<p class="error">No releases yet — watch <a href="${GITHUB_URL}/releases">the repo</a>.</p>`;
  } else {
    const latestTag = releases.find((r) => !r.prerelease)?.tag;
    body = releases
      .map((r) => {
        const badge =
          r.tag === latestTag
            ? '<span class="badge">Latest</span>'
            : r.prerelease
              ? '<span class="badge badge-ghost">Pre-release</span>'
              : "";
        const date = r.publishedAt ? `<time datetime="${esc(r.publishedAt)}">${esc(formatDate(r.publishedAt))}</time>` : "";
        // release-please opens the body with the same "1.2.3 (date)"
        // heading our article header already shows — drop it. GitHub
        // API bodies arrive with CRLF line endings.
        const notes = r.body.replace(/\r\n/g, "\n").replace(/^\s*#{1,3}\s+\[?v?\d[^\n]*\n?/, "");
        return `<article class="release">
  <header>
    <h2 id="${esc(r.tag)}"><a href="${esc(r.url)}">${esc(r.name)}</a></h2>
    <p class="release-meta">${badge}${date}</p>
  </header>
  <div class="md">${renderMarkdown(notes)}</div>
  <p class="release-assets"><a href="${esc(r.url)}">Downloads &amp; checksums on GitHub →</a></p>
</article>`;
      })
      .join("\n");
  }

  const content = `
<section class="page wrap-narrow">
  <p class="eyebrow">Releases</p>
  <h1>Every release, <mark>one boring binary</mark>.</h1>
  <p class="lead">Cut by release-please from conventional commits; binaries for linux and darwin, amd64 and arm64, plus images and the Helm chart on every tag. The same notes ship as <a href="${GITHUB_URL}/blob/main/CHANGELOG.md">CHANGELOG.md</a> in the repo.</p>
  ${body}
</section>`;

  return layout(
    {
      title: "Releases — Shpiel",
      description: "Shpiel release history, rendered from GitHub releases.",
      path: "/releases",
      stars,
    },
    content
  );
}

export function notFoundPage(stars: number | null): string {
  const content = `
<section class="page wrap-narrow">
  <p class="eyebrow">404</p>
  <h1>Nothing at <mark>this route</mark>.</h1>
  <p class="lead">The relay only knows two: <a href="/">home</a> and <a href="/releases">releases</a>.</p>
</section>`;

  return layout(
    {
      title: "404 — Shpiel",
      description: "Page not found.",
      path: "/404",
      stars,
    },
    content
  );
}
