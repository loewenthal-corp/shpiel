import { fetchReleases, fetchStars, latestRelease } from "./github";
import { homePage, notFoundPage, releasesPage } from "./html";

const SECURITY_HEADERS: Record<string, string> = {
  "Content-Type": "text/html; charset=utf-8",
  // Browsers may cache rendered pages for 5 minutes; the GitHub data
  // behind them is edge-cached for 10 (see github.ts).
  "Cache-Control": "public, max-age=300",
  "X-Content-Type-Options": "nosniff",
  "Referrer-Policy": "strict-origin-when-cross-origin",
  // The site ships zero JavaScript; everything is rendered in the worker.
  "Content-Security-Policy":
    "default-src 'none'; style-src 'self'; font-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'",
};

function html(body: string, status = 200): Response {
  return new Response(body, { status, headers: SECURITY_HEADERS });
}

export default {
  async fetch(request): Promise<Response> {
    const url = new URL(request.url);

    if (url.hostname === "www.shpiel.dev") {
      url.hostname = "shpiel.dev";
      return Response.redirect(url.toString(), 301);
    }

    switch (url.pathname) {
      case "/": {
        const [stars, releases] = await Promise.all([fetchStars(), fetchReleases()]);
        return html(homePage(stars, latestRelease(releases)));
      }
      case "/releases": {
        const [stars, releases] = await Promise.all([fetchStars(), fetchReleases()]);
        return html(releasesPage(stars, releases));
      }
      default:
        return html(notFoundPage(await fetchStars()), 404);
    }
  },
} satisfies ExportedHandler;
