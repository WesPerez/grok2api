import assert from "node:assert/strict";
import test from "node:test";

import { serverImageURL } from "./server-media-url.ts";

test("server images retain the deployment path exactly once", () => {
  const origin = "https://grok.example";
  for (const source of ["/v1/media/images/a?download=1#preview", "/grok2api/v1/media/images/a?download=1#preview", `${origin}/v1/media/images/a?download=1#preview`]) {
    assert.equal(serverImageURL(source, "/grok2api/", origin), "/grok2api/v1/media/images/a?download=1#preview");
  }
  assert.equal(serverImageURL("/v1/media/images/a", "", origin), "/v1/media/images/a");
  assert.equal(serverImageURL("https://api.example/grok/v1/media/images/a", "https://api.example/grok", origin), "https://api.example/grok/v1/media/images/a");
});

test("external and unrelated paths are not mistaken for deployment media", () => {
  for (const source of ["", "https://external.example/v1/media/images/a", "/grok2api-other/v1/media/images/a", "/v1/media/images/../other", "javascript:alert(1)", "//external.example/v1/media/images/a"]) {
    assert.equal(serverImageURL(source, "/grok2api", "https://grok.example"), "");
  }
});
