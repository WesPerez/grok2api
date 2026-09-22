#!/usr/bin/env python3
"""Run two explicit Responses requests to verify Grok streaming and tool round trips.

The API key stays in memory. This writes no files and never executes model tools.
There are no automatic retries; a failed or different-model response fails the check.
"""

import argparse
import collections
import hashlib
import json
from pathlib import Path
import time
import urllib.error
import urllib.request
import uuid


def report(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def run_request(url, key, payload, trace, round_number, timeout):
    started = time.monotonic()
    request = urllib.request.Request(
        url,
        data=json.dumps(payload, ensure_ascii=False).encode(),
        headers={
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
            "User-Agent": "grok47-responses-smoke/1.0",
            "X-Request-ID": trace + "-r" + str(round_number),
        },
    )
    event_counts = collections.Counter()
    first_event = first_output = None
    previous_event = started
    largest_gap = 0.0
    reasoning_characters = 0
    output_characters = 0
    heartbeat_count = 0
    argument_deltas = {}
    output_items = []
    terminal = None
    data_lines = []

    def process_event(raw):
        nonlocal first_event, first_output, previous_event, largest_gap
        nonlocal reasoning_characters, output_characters, terminal
        if not raw or raw == "[DONE]":
            return
        event = json.loads(raw)
        kind = event.get("type", "")
        now = time.monotonic()
        event_counts[kind] += 1
        if first_event is None:
            first_event = now - started
        largest_gap = max(largest_gap, now - previous_event)
        previous_event = now
        delta = event.get("delta", "")
        if isinstance(delta, str) and delta and kind.endswith(".delta"):
            if first_output is None:
                first_output = now - started
                report({"round": round_number, "first_output_event": kind,
                        "first_output_seconds": round(first_output, 3)})
            if "reasoning" in kind:
                reasoning_characters += len(delta)
            elif kind == "response.output_text.delta":
                output_characters += len(delta)
        if kind == "response.function_call_arguments.delta":
            item_id = event.get("item_id", "")
            argument_deltas[item_id] = argument_deltas.get(item_id, "") + delta
        elif kind == "response.output_item.done":
            output_items.append(event.get("item", {}))
        elif kind in ("response.completed", "response.failed", "response.incomplete"):
            terminal = event

    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            report({"round": round_number, "trace": trace, "http_status": response.status,
                    "headers_seconds": round(time.monotonic() - started, 3),
                    "content_type": response.headers.get("Content-Type"),
                    "response_request_id": response.headers.get("X-Request-ID")})
            if "text/event-stream" not in response.headers.get("Content-Type", ""):
                raise RuntimeError("expected a server-sent event stream")
            for line in response:
                line = line.decode("utf-8").rstrip("\r\n")
                if line.startswith(":"):
                    heartbeat_count += 1
                elif line.startswith("data:"):
                    data_lines.append(line[5:].removeprefix(" "))
                elif not line and data_lines:
                    process_event("\n".join(data_lines))
                    data_lines.clear()
                    if terminal is not None:
                        break
            if data_lines and terminal is None:
                process_event("\n".join(data_lines))
    except urllib.error.HTTPError as error:
        detail = error.read(4096)
        try:
            parsed = json.loads(detail)
            code = parsed.get("error", {}).get("code")
        except (ValueError, AttributeError):
            code = None
        report({"round": round_number, "http_status": error.code, "error_code": code})
        raise RuntimeError("upstream rejected the smoke request") from None

    result = (terminal or {}).get("response", {})
    report({
        "round": round_number,
        "duration_seconds": round(time.monotonic() - started, 3),
        "first_event_seconds": None if first_event is None else round(first_event, 3),
        "first_output_seconds": None if first_output is None else round(first_output, 3),
        "largest_event_gap_seconds": round(largest_gap, 3),
        "events": dict(event_counts),
        "heartbeats": heartbeat_count,
        "terminal": (terminal or {}).get("type"),
        "actual_model": result.get("model"),
        "response_id": result.get("id"),
        "reasoning_characters": reasoning_characters,
        "output_characters": output_characters,
        "usage": result.get("usage"),
        "error_code": (result.get("error") or {}).get("code"),
    })
    if not terminal or terminal.get("type") != "response.completed":
        raise RuntimeError("response did not complete successfully")
    if result.get("model") != payload["model"]:
        raise RuntimeError("actual response model does not match the requested model")
    return result.get("output") or output_items, argument_deltas


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True, help="Responses API base, without /responses")
    parser.add_argument("--api-key-file", type=Path, required=True)
    parser.add_argument("--model", default="grok-4.7")
    parser.add_argument("--effort", default="xhigh", choices=("low", "medium", "high", "xhigh"))
    parser.add_argument("--timeout", type=float, default=360)
    args = parser.parse_args()
    key = args.api_key_file.read_text().strip()
    if not key:
        raise RuntimeError("empty API key file")
    trace = "grok47-link-audit-" + str(uuid.uuid4())
    expected = {
        "path": r"E:\IdeaProjects\小鳄鱼骑单车\crocodile-bike.svg",
        "text": '第一行\nsecond line\r\nquote "ok"; literal \\n; \\u4e2d; $value; `tick`',
        "pattern": r"^\d{2}\\x$",
    }
    original_input = [
        {"role": "developer", "content": (
            "This is a harmless JSON tool-transport test. Call echo_sample once, copying all "
            "strings exactly. After its result, reply only CHAIN_OK and the returned digest."
        )},
        {"role": "user", "content": "Call echo_sample with exactly this JSON object: "
         + json.dumps(expected, ensure_ascii=False)},
    ]
    common = {
        "model": args.model,
        "reasoning": {"effort": args.effort, "summary": "auto"},
        "stream": True,
        "store": False,
        "max_output_tokens": 4096,
        "prompt_cache_key": trace,
        "tools": [{
            "type": "function", "name": "echo_sample",
            "description": "Return validation and digest for supplied strings; no side effects.",
            "parameters": {
                "type": "object",
                "properties": {name: {"type": "string"} for name in expected},
                "required": list(expected), "additionalProperties": False,
            },
        }],
    }
    url = args.base_url.rstrip("/") + "/responses"
    output, deltas = run_request(url, key, {
        **common, "input": original_input,
        "tool_choice": {"type": "function", "name": "echo_sample"},
    }, trace, 1, args.timeout)
    calls = [item for item in output if item.get("type") == "function_call"]
    if len(calls) != 1 or calls[0].get("name") != "echo_sample":
        raise RuntimeError("expected exactly one echo_sample function call")
    call = calls[0]
    actual = json.loads(call["arguments"])
    matches = {name: actual.get(name) == value for name, value in expected.items()}
    delta_match = deltas.get(call.get("id", "")) == call["arguments"]
    report({"tool_argument_matches": matches, "delta_matches_final_arguments": delta_match})
    if actual != expected or not delta_match:
        raise RuntimeError("tool arguments were not preserved exactly")
    digest = hashlib.sha256(json.dumps(actual, ensure_ascii=False, sort_keys=True).encode()).hexdigest()[:16]
    result = {"verified": True, "digest": digest}
    second_output, _ = run_request(url, key, {
        **common,
        "input": original_input + output + [{
            "type": "function_call_output", "call_id": call["call_id"],
            "output": json.dumps(result),
        }],
        "tool_choice": "none",
    }, trace, 2, args.timeout)
    final_text = "".join(part.get("text", "") for item in second_output
                         if item.get("type") == "message" for part in item.get("content", [])
                         if part.get("type") == "output_text")
    passed = digest in final_text and "CHAIN_OK" in final_text
    report({"tool_result_consumed": passed, "trace": trace})
    if not passed:
        raise RuntimeError("the final answer did not consume the tool result")


if __name__ == "__main__":
    main()
