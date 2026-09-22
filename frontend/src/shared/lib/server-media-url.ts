// Normalize media owned by this deployment without rebasing external URLs.
export function serverMediaURL(value: string, apiBaseUrl: string, browserOrigin: string, kind: "images" | "videos" | "any" = "any"): string {
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
    if (kind === "any" ? !/^\/v1\/media\/(?:images|videos)\//.test(path) : !path.startsWith(`/v1/media/${kind}/`)) return "";
    return `${base}${path}${resolved.search}${resolved.hash}`;
  } catch {
    return "";
  }
}

export function serverImageURL(value: string, apiBaseUrl: string, browserOrigin: string): string {
  return serverMediaURL(value, apiBaseUrl, browserOrigin, "images");
}
