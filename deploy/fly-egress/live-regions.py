#!/usr/bin/env python3
"""Live region-rotation test against the team router on loopback.

Each case sends one real gpt-6-astra turn (HTTP or websocket) with a unique
session id, optionally with X-Subrouter-Test-Fault so chosen regions answer
"at capacity" without calling upstream. The router must still serve the
turn from another region. Prints one JSON line per case.
Needs SUBROUTER_CODEX_FAULT_INJECTION=1 in the worker config (loopback
only; remove it after the run) and `pip install websockets`.
usage: ROUTER_PATH=/v1/responses python3 live-regions.py [cases...]   (default: all)
"""
import asyncio, json, os, sys, time, uuid, urllib.request, urllib.error

import websockets

ROUTER = "127.0.0.1:31415"
PATH = os.environ.get("ROUTER_PATH", "/backend-api/codex/responses")
MODEL = "gpt-6-astra"


def body(session):
    return {"model": MODEL, "instructions": "Reply with the single word ok.",
            "input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "ok?"}]}],
            "tools": [], "tool_choice": "auto", "parallel_tool_calls": False,
            "reasoning": {"effort": "low", "summary": "auto"}, "store": False, "stream": True,
            "include": [], "prompt_cache_key": session}


def headers(session, fault, autonomous):
    h = {"content-type": "application/json", "accept": "text/event-stream", "originator": "codex_cli_rs",
         "OpenAI-Beta": "responses=experimental", "session_id": session,
         "X-Subrouter-Session": session, "user-agent": "codex_cli_rs/0.162.1 (subrouter-live-regions)"}
    if fault:
        h["X-Subrouter-Test-Fault"] = "capacity=" + fault
    if autonomous:
        h["X-Subrouter-Retry-Policy"] = "autonomous"
    return h


def http_case(name, fault=""):
    session = f"live-{name}-{uuid.uuid4()}"
    req = urllib.request.Request("http://" + ROUTER + PATH, data=json.dumps(body(session)).encode(),
                                 headers=headers(session, fault, False), method="POST")
    t0 = time.time()
    rec = {"case": name, "transport": "http", "fault": fault, "session": session}
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            rec["status"] = r.status
            rec["region"] = r.headers.get("X-Subrouter-Region")
            for line in r:
                line = line.decode(errors="replace").strip()
                if line.startswith("event:"):
                    ev = line[6:].strip()
                    rec.setdefault("first_event", ev)
                    rec.setdefault("ttfe_s", round(time.time() - t0, 2))
                    if ev in ("response.completed", "response.failed"):
                        rec["final"] = ev
                        break
    except urllib.error.HTTPError as e:
        rec["status"] = e.code
        rec["body"] = e.read(300).decode(errors="replace")
    except Exception as e:  # noqa: BLE001
        rec["status"], rec["exc"] = 0, repr(e)[:200]
    rec["elapsed_s"] = round(time.time() - t0, 2)
    rec["ok"] = rec.get("final") == "response.completed"
    return rec


async def ws_turn(name, fault="", autonomous=False):
    session = f"live-{name}-{uuid.uuid4()}"
    rec = {"case": name, "transport": "websocket", "fault": fault, "autonomous": autonomous, "session": session}
    h = headers(session, fault, autonomous)
    h.pop("content-type"), h.pop("accept")
    t0 = time.time()
    try:
        async with websockets.connect("ws://" + ROUTER + PATH, additional_headers=h, open_timeout=60,
                                      max_size=None) as ws:
            rec["region"] = ws.response.headers.get("X-Subrouter-Region")
            event = dict(body(session), type="response.create")
            event.pop("stream", None)
            await ws.send(json.dumps(event))
            while True:
                msg = json.loads(await asyncio.wait_for(ws.recv(), timeout=180))
                kind = msg.get("type", "")
                rec.setdefault("first_event", kind)
                rec.setdefault("ttfe_s", round(time.time() - t0, 2))
                if kind in ("response.completed", "response.failed", "error"):
                    rec["final"] = kind
                    if kind != "response.completed":
                        rec["error"] = json.dumps(msg)[:300]
                    break
    except Exception as e:  # noqa: BLE001
        rec["exc"] = repr(e)[:300]
    rec["elapsed_s"] = round(time.time() - t0, 2)
    rec["ok"] = rec.get("final") == "response.completed"
    return rec


FAR = "fly-egress-ams:3128,fly-egress-arn:3128,fly-egress-cdg:3128,fly-egress-fra:3128,fly-egress-lhr:3128,cmux-egress-fra:3128,cmux-egress-tyo:3128"
CASES = {
    "http-baseline": lambda: http_case("http-baseline"),
    "http-local-fault": lambda: http_case("http-local-fault", "local"),
    "http-deep-fault": lambda: http_case("http-deep-fault", "local," + FAR),
    "ws-baseline": lambda: asyncio.run(ws_turn("ws-baseline")),
    "ws-local-fault": lambda: asyncio.run(ws_turn("ws-local-fault", "local")),
    "ws-deep-fault": lambda: asyncio.run(ws_turn("ws-deep-fault", "local," + FAR)),
    "ws-autonomous-local-fault": lambda: asyncio.run(ws_turn("ws-autonomous-local-fault", "local", True)),
}

if __name__ == "__main__":
    for name in (sys.argv[1:] or list(CASES)):
        print(json.dumps(CASES[name]()), flush=True)
