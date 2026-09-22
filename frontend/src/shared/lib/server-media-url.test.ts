import assert from "node:assert/strict";
import test from "node:test";

import { serverImageURL, serverMediaURL } from "./server-media-url.ts";

test("server images retain the deployment path exactly once", () => {
  const origin = "https://grok.example";
  for (const source of ["/v1/media/images/a?download=1#preview", "/grok2api/v1/media/images/a?download=1#preview", `${origin}/v1/media/images/a?download=1#preview`]) {
    assert.equal(serverImageURL(source, "/grok2api/", origin), "/grok2api/v1/media/images/a?download=1#preview");
  }
  assert.equal(serverImageURL("/v1/media/images/a", "", origin), "/v1/media/images/a");
  assert.equal(serverImageURL("https://api.example/grok/v1/media/images/a", "https://api.example/grok", origin), "https://api.example/grok/v1/media/images/a");
});

test("video results retain the deployment path and remain distinct from images", () => {
  const origin = "https://grok.example";
  for (const source of ["/v1/media/videos/a?download=1", "/grok2api/v1/media/videos/a?download=1", `${origin}/v1/media/videos/a?download=1`]) {
    assert.equal(serverMediaURL(source, "/grok2api/", origin), "/grok2api/v1/media/videos/a?download=1");
    assert.equal(serverImageURL(source, "/grok2api/", origin), "");
  }
  assert.equal(serverMediaURL("/v1/media/videos/a", "", origin), "/v1/media/videos/a");
  assert.equal(serverMediaURL("https://external.example/v1/media/videos/a", "/grok2api", origin), "");
});

test("external and unrelated paths are not mistaken for deployment media", () => {
  for (const source of ["", "https://external.example/v1/media/images/a", "/grok2api-other/v1/media/images/a", "/v1/media/images/../other", "javascript:alert(1)", "//external.example/v1/media/images/a"]) {
    assert.equal(serverImageURL(source, "/grok2api", "https://grok.example"), "");
  }
});
