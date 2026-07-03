export const REPO = "loewenthal-corp/shpiel";
export const GITHUB_URL = `https://github.com/${REPO}`;

// Edge-cache GitHub responses for 10 minutes: fresh enough for a release
// day, and it keeps us far below the unauthenticated API rate limit even
// though all visitors in a colo share the worker's egress IPs.
const CACHE_TTL_SECONDS = 600;

export interface Release {
  tag: string;
  name: string;
  url: string;
  publishedAt: string | null;
  body: string;
  prerelease: boolean;
}

async function cachedFetch(url: string): Promise<Response | null> {
  try {
    const res = await fetch(url, {
      headers: {
        "User-Agent": "shpiel.dev-website",
        Accept: "application/vnd.github+json",
      },
      cf: { cacheTtl: CACHE_TTL_SECONDS, cacheEverything: true },
    });
    return res.ok ? res : null;
  } catch {
    return null;
  }
}

export async function fetchStars(): Promise<number | null> {
  const res = await cachedFetch(`https://api.github.com/repos/${REPO}`);
  if (!res) return null;
  try {
    const repo = (await res.json()) as { stargazers_count?: number };
    return typeof repo.stargazers_count === "number" ? repo.stargazers_count : null;
  } catch {
    return null;
  }
}

export async function fetchReleases(): Promise<Release[] | null> {
  const res = await cachedFetch(`https://api.github.com/repos/${REPO}/releases?per_page=20`);
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
