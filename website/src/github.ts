export const REPO = "loewenthal-corp/shpiel";
export const GITHUB_URL = `https://github.com/${REPO}`;

// Fresh window: serve from cache without revalidating.
const CACHE_TTL_SECONDS = 600; // 10 minutes
// Stale window: serve old data immediately while refreshing in the background.
const STALE_TTL_SECONDS = 3600; // 1 hour

// Header written into cached responses so we can compute age without relying
// on the Cache-Control max-age that Cloudflare may strip or rewrite.
const CACHED_AT_HEADER = "X-Cached-At";

export interface Release {
  tag: string;
  name: string;
  url: string;
  publishedAt: string | null;
  body: string;
  prerelease: boolean;
}

// Fetch from the GitHub API and store the result in the Worker cache.
async function doFetch(url: string, cache: Cache): Promise<Response | null> {
  try {
    const res = await fetch(url, {
      headers: {
        "User-Agent": "shpiel.dev-website",
        Accept: "application/vnd.github+json",
      },
      cf: { cacheTtl: CACHE_TTL_SECONDS, cacheEverything: true },
    });
    if (!res.ok) return null;

    // Clone before caching; the original body is returned to the caller.
    const headers = new Headers(res.headers);
    headers.set(CACHED_AT_HEADER, Date.now().toString());
    headers.set("Cache-Control", `public, max-age=${STALE_TTL_SECONDS}`);
    await cache.put(new Request(url), new Response(res.clone().body, { headers }));

    return res;
  } catch {
    return null;
  }
}

// Stale-while-revalidate: return cached data immediately (even if stale) and
// kick off a background refresh when the fresh window has elapsed. Only the
// very first cold request blocks on the GitHub API.
async function cachedFetch(url: string, ctx: ExecutionContext): Promise<Response | null> {
  const cache = caches.default;
  const cached = await cache.match(new Request(url));

  if (cached) {
    const cachedAt = cached.headers.get(CACHED_AT_HEADER);
    const ageSeconds = cachedAt ? (Date.now() - parseInt(cachedAt, 10)) / 1000 : Infinity;

    if (ageSeconds < STALE_TTL_SECONDS) {
      if (ageSeconds >= CACHE_TTL_SECONDS) {
        // Stale but within the stale window — serve immediately, refresh behind.
        ctx.waitUntil(doFetch(url, cache));
      }
      return cached;
    }
  }

  // Cold start or beyond the stale window — block on the real fetch.
  return doFetch(url, cache);
}

export async function fetchStars(ctx: ExecutionContext): Promise<number | null> {
  const res = await cachedFetch(`https://api.github.com/repos/${REPO}`, ctx);
  if (!res) return null;
  try {
    const repo = (await res.json()) as { stargazers_count?: number };
    return typeof repo.stargazers_count === "number" ? repo.stargazers_count : null;
  } catch {
    return null;
  }
}

export async function fetchReleases(ctx: ExecutionContext): Promise<Release[] | null> {
  const res = await cachedFetch(`https://api.github.com/repos/${REPO}/releases?per_page=20`, ctx);
  if (!res) return null;
  try {
    const releases = (await res.json()) as Array<{
      tag_name: string;
      name: string | null;
      html_url: string;
      published_at: string | null;
      body: string | null;
      prerelease: boolean;
      draft: boolean;
    }>;
    return releases
      .filter((r) => !r.draft)
      .map((r) => ({
        tag: r.tag_name,
        name: r.name || r.tag_name,
        url: r.html_url,
        publishedAt: r.published_at,
        body: r.body ?? "",
        prerelease: r.prerelease,
      }));
  } catch {
    return null;
  }
}

export function latestRelease(releases: Release[] | null): Release | null {
  if (!releases || releases.length === 0) return null;
  return releases.find((r) => !r.prerelease) ?? releases[0] ?? null;
}
