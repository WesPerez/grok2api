// Normalize media owned by this deployment without rebasing external image URLs.
export function serverImageURL(value: string, apiBaseUrl: string, browserOrigin: string): string {
  const source = value.trim();
  if (!source) return "";
  try {
    const base = apiBaseUrl.replace(/\/+$/, "");
    const api = new URL(base || "/", `${browserOrigin}/`);
    const resolved = new URL(source, `${browserOrigin}/`);
    if (resolved.origin !== browserOrigin && resolved.origin !== api.origin) return "";
    const basePath = api.pathname.replace(/\/+$/, "");
    let path = resolved.pathname;
    if (basePath && path.startsWith(`${basePath}/`)) path = path.slice(basePath.length);
    if (!path.startsWith("/v1/media/images/")) return "";
    return `${base}${path}${resolved.search}${resolved.hash}`;
  } catch {
    return "";
  }
}
