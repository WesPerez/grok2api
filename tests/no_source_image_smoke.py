#!/usr/bin/env python3
"""GitHub-hosted-only, offline smoke of an already published image.

The service has a fresh container-local database and only loopback networking.
nsenter runs the stdlib HTTP client in that network namespace (not the image's
filesystem). No host ports, account credentials or production mounts are used.
This proves startup/management without upstream availability, not inference.
"""

import argparse
import base64
import json
import os
from pathlib import Path
import re
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(*args, timeout=120):
    return subprocess.run(args, check=True, text=True, capture_output=True,
                          timeout=timeout).stdout.strip()


def probe(password):
    # No proxy inherited from the runner can make an offline check go online.
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(path, *, token=None, payload=None):
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        req = urllib.request.Request("http://127.0.0.1:8000" + path,
                                     data=json.dumps(payload).encode() if payload is not None else None,
                                     headers=headers)
        try:
            response = client.open(req, timeout=3)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            raw = response.read(2 * 1024 * 1024)
            return response.status, raw

    def api(path, expected=200, **kwargs):
        status, raw = request(path, **kwargs)
        require(status == expected, f"{path}: HTTP {status}, expected {expected}")
        return json.loads(raw)

    require(sorted(name for _, name in socket.if_nameindex()) == ["lo"],
            "Probe must have loopback-only networking")
    deadline = time.monotonic() + 75
    while True:
        try:
            require(api("/healthz").get("ok") is True, "healthz is not healthy")
            break
        except (OSError, RuntimeError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)

    api("/api/admin/v1/accounts", expected=401)
    login = api("/api/admin/v1/auth/login", payload={"username": "offline-admin", "password": password})
    token = login["data"]["tokens"]["accessToken"]
    require(api("/api/admin/v1/me", token=token)["data"]["username"] == "offline-admin",
            "Management authentication is not functional")
    data = api("/api/admin/v1/accounts", token=token)["data"]
    require(data["total"] == 0 and data["items"] == [], "Expected a fresh account-free store")
    # The shipped provider catalog is seeded locally at startup. Catalog rows
    # are not configured upstream accounts: verify their availability instead
    # of incorrectly requiring the management catalog itself to be empty.
    catalog_ids = set()
    catalog_total = None
    for page in range(1, 31):
        data = api(f"/api/admin/v1/models?page={page}&pageSize=100", token=token)["data"]
        if catalog_total is None:
            catalog_total = data["total"]
        require(data["total"] == catalog_total, "Offline catalog changed during pagination")
        for model in data["items"]:
            require(model["available"] is False and model["totalAccounts"] == 0 and
                    model["supportedAccounts"] == 0 and model["accountIds"] == [],
                    "Catalog must not claim a configured/available account")
            require(model["id"] not in catalog_ids, "Catalog pagination repeated a row")
            catalog_ids.add(model["id"])
        if len(catalog_ids) == catalog_total:
            break
        require(bool(data["items"]), "Catalog pagination ended before the reported total")
    require(len(catalog_ids) == catalog_total, "Offline catalog exceeds smoke pagination budget")

    status, raw = request("/")
    require(status == 200 and b"<html" in raw.lower(), "Management UI is not served")
    assets = re.findall(r'<script[^>]+src="(/assets/[^"?]+\.js)"', raw.decode())
    require(bool(assets), "Management UI has no bundled entrypoint")
    for path in assets:
        status, asset = request(path)
        require(status == 200 and len(asset) > 100, "Management UI entrypoint is missing")

    # A local client key is not an upstream account; it proves the authenticated
    # client catalog reports an honest empty result without an external source.
    key = api("/api/admin/v1/client-keys", expected=201, token=token,
              payload={"name": "offline-smoke", "enabled": True})["data"]["secret"]
    while True:
        ready = api("/readyz", expected=503)
        if ready.get("state") == "not_ready":
            break
        require(time.monotonic() < deadline, "Startup reconciliation did not finish")
        time.sleep(1)
    require(ready.get("ready") is False, "Empty service must not advertise inference readiness")
    components = ready.get("components", {})
    require(components.get("runtime_store", {}).get("state") == "ready", "Local runtime failed")
    require(components.get("billing_ledger", {}).get("state") == "ready", "Local billing ledger failed")
    require(components.get("model_routes", {}).get("state") in ("ready", "unavailable"), "Model configuration not reported")
    require(all(components.get(name, {}).get("state") not in ("ready", "degraded", "warming")
                for name in ("grok_build", "grok_web", "grok_console")), "Empty providers must not claim availability")
    require(api("/v1/models", token=key)["data"] == [], "Client catalog should be empty")
    require(api("/healthz").get("ok") is True, "Empty readiness broke liveness")
    api("/api/admin/v1/me", token=token)
    return {"result": "passed", "accounts": 0, "availableModels": 0,
            "managementCatalogModels": catalog_total, "network": "loopback-only",
            "healthStatus": 200, "adminLoginStatus": 200, "adminMeStatus": 200,
            "unauthenticatedAccountsStatus": 401, "uiAndEntrypointStatus": 200,
            "clientModelsStatus": 200, "readinessStatus": 503, "readiness": ready,
            "scope": "published-image startup and management; no inference or production mutation"}


def main():
    require(os.environ.get("GITHUB_ACTIONS") == "true" and
            os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted",
            "This smoke may run only on GitHub-hosted Actions")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image")
    parser.add_argument("--revision")
    parser.add_argument("--output", default="no-source-image-evidence.json")
    parser.add_argument("--probe", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.probe:
        print(json.dumps(probe(input()), ensure_ascii=False))
        return
    require(bool(re.fullmatch(r"ghcr\.io/wesperez/grok2api@sha256:[a-f0-9]{64}", args.image or "")),
            "Use an immutable published Grok image digest")
    require(bool(re.fullmatch(r"[a-f0-9]{40}", args.revision or "")), "Expected image revision is required")
    run("docker", "pull", "--platform", "linux/amd64", args.image, timeout=240)
    metadata = json.loads(run("docker", "image", "inspect", args.image))[0]
    require(metadata["Config"]["Labels"].get("org.opencontainers.image.revision") == args.revision,
            "Published image revision differs from expected source")
    name = "grok-offline-smoke-" + secrets.token_hex(8)
    password = secrets.token_hex(24)
    # Only synthetic, short-lived bootstrap data is mounted. The application
    # creates its own empty DB inside the disposable container, never on host.
    with tempfile.TemporaryDirectory(prefix="grok-offline-") as directory:
        config = Path(directory) / "config.yaml"
        config.write_text("secrets:\n  jwtSecret: " + json.dumps(secrets.token_hex(32)) +
                          "\n  credentialEncryptionKey: " + json.dumps(base64.b64encode(secrets.token_bytes(32)).decode()) +
                          "\nbootstrapAdmin:\n  username: offline-admin\n  password: " + json.dumps(password) +
                          "\ndatabase:\n  driver: sqlite\n  sqlite:\n    path: /app/data/backend.db\n" +
                          "runtimeStore:\n  driver: memory\nfrontend:\n  staticPath: /app/frontend/dist\n")
        config.chmod(0o600)
        created = False
        try:
            run("docker", "create", "--name", name, "--network", "none", "--pull", "never",
                "--platform", "linux/amd64", "--memory", "512m", "--cpus", "2",
                "--mount", f"type=bind,src={config},dst=/run/grok2api/config.yaml,readonly", args.image)
            created = True
            run("docker", "start", name)
            info = json.loads(run("docker", "inspect", name))[0]
            require(info["HostConfig"]["NetworkMode"] == "none", "Container is not offline")
            require(info["State"]["Running"], "Container exited during startup")
            # Enter only the network namespace; stdlib Python stays on runner.
            result = subprocess.run(["sudo", "-n", "nsenter", "--target", str(info["State"]["Pid"]),
                                     "--net", "--", "env", "GITHUB_ACTIONS=true", "RUNNER_ENVIRONMENT=github-hosted",
                                     "python3", str(Path(__file__).resolve()), "--probe"],
                                    input=password + "\n", text=True, capture_output=True, timeout=100)
            require(result.returncode == 0, "Offline HTTP probe failed:\n" + result.stderr[-4000:])
            evidence = json.loads(result.stdout)
            evidence.update({"image": args.image, "imageId": metadata["Id"], "revision": args.revision,
                             "testSha": os.environ.get("GITHUB_SHA"), "runId": os.environ.get("GITHUB_RUN_ID"),
                             "runnerEnvironment": os.environ.get("RUNNER_ENVIRONMENT")})
            Path(args.output).write_text(json.dumps(evidence, indent=2, ensure_ascii=False) + "\n")
            print(json.dumps(evidence, ensure_ascii=False))
        finally:
            if created:
                # Exact randomly named container owned solely by this invocation.
                run("docker", "rm", "--force", "--volumes", name)


if __name__ == "__main__":
    main()
