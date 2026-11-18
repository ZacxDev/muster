#!/usr/bin/env python3
"""A fake Anthropic Messages server for the Compose chief's smoke test.

muster's LLM proxy is pointed at it with MUSTER_TEST_LLM_UPSTREAM, so the chief's
Claude Code CLI runs real turns, and real muster tool calls, without any model or
key. It is scripted, and it answers only what the script names:

  * a request whose tools include mcp__muster__tasks_list, and whose last user
    turn holds no tool_result, but asks "who is on what": a tool_use of
    mcp__muster__tasks_list {}. That tool is muster's own, served over MCP from
    muster's connect port, so its result is real board data;
  * "comment on it": a tool_use of mcp__muster__task_comment, WHETHER OR NOT it
    was offered. The Compose chief is not offered it (muster serves it only the
    tools it pre-allows), so this is a model naming a tool it was never given:
    the CLI must refuse it, and it must not run;
  * "touch the file": a tool_use of Bash running `touch` (BASH_TOUCH), a tool
    the chief IS offered but does not pre-allow, so its prompt must be DENIED
    (not left hanging);
  * a tool_result for either call: "COMMENT-RESULT:" / "BASH-RESULT:", then
    "error:" when the result is an error, then its first words — the test reads
    whether the tool was refused or ran;
  * a request whose last user turn holds a tool_result: the text
    "SEEN:<every MARK-… token in that tool result>", or "SEEN:none" when it
    holds none. A reply naming a marker therefore proves the marker came back
    through the real tool call;
  * any other request (the CLI's own side requests, the kickoff): the text "ok".

It never errors on purpose: an unrecognised path is 404 and logged, so the test
can fail on it.

GET /ledger returns every request it was sent, as JSON: the model, whether it
carried tools (how many, and their names), whether it held a tool_result, the x-api-key and authorization
headers' presence and value (fake values only; the test sets them), and what it
answered. The smoke test reads it to check the pinned model and that the
owner's key, and nothing else, reached the provider.
"""
import itertools
import json
import re
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LEDGER = []
LOCK = threading.Lock()
MARK = re.compile(r"MARK-[A-Za-z0-9]+")


def last_user(messages):
    for m in reversed(messages or []):
        if m.get("role") == "user":
            return m
    return {}


def blocks(content):
    if isinstance(content, str):
        return [{"type": "text", "text": content}]
    return content or []


def text_of(block):
    if block.get("type") == "text":
        return block.get("text", "")
    if block.get("type") == "tool_result":
        return "".join(text_of(b) for b in blocks(block.get("content")))
    return ""


COUNTER = itertools.count(1)
TASK = re.compile(r"TASK-(\d+)")


def tool_input_for_comment(text):
    """A VALID task_comment call on the task the prompt names (TASK-<id>): had
    the tool run, a comment would exist, which the test checks there is not."""
    m = TASK.search(text)
    return {"id": int(m.group(1)), "body": "fake-llm comment that must never land"} if m else {}


# The file BASH_TOUCH's command would create: the test checks it does not exist.
BASH_TOUCH_PATH = "/tmp/fake-llm-touch-must-never-land"
BASH_TOUCH = {"command": "touch " + BASH_TOUCH_PATH, "description": "fake-llm: a command that needs a prompt"}


def tool_uses(msgs):
    """Every tool_use id the model was answered with in this conversation."""
    return {b.get("id", "") for m in msgs if m.get("role") == "assistant"
            for b in blocks(m.get("content")) if b.get("type") == "tool_use"}


def decide(body):
    """What to answer, from the conversation's history (the CLI re-sends all of
    it each request), as a small state machine. It never relies on where in a
    turn a block sits: the CLI may send a new prompt in the same user turn as an
    earlier tool result, and the CLI and muster's proxy add text after results.

      1. a tool_result for our Bash call anywhere → report it (BASH-RESULT:,
         "error:" when it is one, its first words);
      2. "touch the file" in the last user turn, Bash offered and not yet
         asked for → ask for BASH_TOUCH;
      3. a tool_result for our task_comment call anywhere → report it
         (COMMENT-RESULT:, likewise);
      4. "comment on it" in the last user turn and task_comment not yet asked
         for → ask for task_comment (a valid call on the TASK-<id> it names),
         offered or not;
      5. a tool_result for our tasks_list call in the last user turn → SEEN: and
         the MARK-… tokens in it, or "none";
      6. "who is on what" in the last user turn and tasks_list not yet asked
         for → ask for tasks_list;
      7. anything else → "ok".
    """
    msgs = body.get("messages") or []
    last = blocks(last_user(msgs).get("content"))
    tools = [t.get("name") for t in body.get("tools") or []]
    used = tool_uses(msgs)
    results = [b for m in msgs if m.get("role") == "user" for b in blocks(m.get("content")) if b.get("type") == "tool_result"]
    def report(prefix, label):
        found = [b for b in results if str(b.get("tool_use_id", "")).startswith(prefix)]
        if found:
            b = found[-1]
            return ("text", label + ("error:" if b.get("is_error") else "") + text_of(b)[:200])
        return None

    texts = [b.get("text", "") for b in last if b.get("type") == "text"]
    asked = " ".join(texts).lower()
    # Newest step first: the Bash step comes after the comment step, so its
    # request wins over a comment result still in the history.
    if (r := report("toolu_bash_", "BASH-RESULT:")):
        return r
    if "Bash" in tools and "touch the file" in asked and not any(u.startswith("toolu_bash_") for u in used):
        return ("tool", "Bash", dict(BASH_TOUCH))
    if (r := report("toolu_comment_", "COMMENT-RESULT:")):
        return r
    if "comment on it" in asked and not any(u.startswith("toolu_comment_") for u in used):
        return ("tool", "mcp__muster__task_comment", tool_input_for_comment(" ".join(texts)))
    tasks = [b for b in last if b.get("type") == "tool_result" and str(b.get("tool_use_id", "")).startswith("toolu_tasks_")]
    if tasks:
        marks = sorted(set(MARK.findall(" ".join(text_of(b) for b in tasks))))
        return ("text", "SEEN:" + (",".join(marks) if marks else "none"))
    if ("mcp__muster__tasks_list" in tools and "who is on what" in asked
            and not any(u.startswith("toolu_tasks_") for u in used)):
        return ("tool", "mcp__muster__tasks_list")
    return ("text", "ok")


def sse(handler, event, data):
    handler.wfile.write(f"event: {event}\ndata: {json.dumps(data)}\n\n".encode())
    handler.wfile.flush()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("fake-llm: " + (fmt % args) + "\n")

    def do_GET(self):
        if self.path == "/ledger":
            with LOCK:
                body = json.dumps(LEDGER).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n)
        if path != "/v1/messages":
            sys.stderr.write(f"fake-llm: UNEXPECTED POST {self.path}\n")
            with LOCK:
                LEDGER.append({"path": self.path, "unexpected": True})
            self.send_response(404)
            self.end_headers()
            return
        body = json.loads(raw or b"{}")
        decided = decide(body)
        kind, value = decided[0], decided[1]
        tool_input = decided[2] if len(decided) > 2 else {}
        tool_id = ""
        if kind == "tool":
            prefix = "toolu_comment_" if value.endswith("task_comment") else "toolu_bash_" if value == "Bash" else "toolu_tasks_"
            tool_id = prefix + str(next(COUNTER))
        last = blocks(last_user(body.get("messages")).get("content"))
        with LOCK:
            LEDGER.append({
                "path": self.path,
                "model": body.get("model"),
                "tools": len(body.get("tools") or []),
                "tool_names": sorted(t.get("name", "") for t in body.get("tools") or []),
                "tool_result": any(b.get("type") == "tool_result" for b in last),
                "x_api_key": self.headers.get("x-api-key"),
                "authorization": self.headers.get("authorization"),
                "answered": f"{kind}:{value}",
            })
        model = body.get("model") or "fake"
        msg = {"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": [],
               "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 0}}
        if not body.get("stream"):
            content = ([{"type": "text", "text": value}] if kind == "text"
                       else [{"type": "tool_use", "id": tool_id, "name": value, "input": tool_input}])
            msg.update(content=content, stop_reason="end_turn" if kind == "text" else "tool_use",
                       usage={"input_tokens": 10, "output_tokens": 5})
            out = json.dumps(msg).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        sse(self, "message_start", {"type": "message_start", "message": msg})
        if kind == "text":
            sse(self, "content_block_start", {"type": "content_block_start", "index": 0,
                                              "content_block": {"type": "text", "text": ""}})
            sse(self, "content_block_delta", {"type": "content_block_delta", "index": 0,
                                              "delta": {"type": "text_delta", "text": value}})
            stop = "end_turn"
        else:
            sse(self, "content_block_start", {"type": "content_block_start", "index": 0,
                                              "content_block": {"type": "tool_use", "id": tool_id, "name": value, "input": {}}})
            sse(self, "content_block_delta", {"type": "content_block_delta", "index": 0,
                                              "delta": {"type": "input_json_delta", "partial_json": json.dumps(tool_input)}})
            stop = "tool_use"
        sse(self, "content_block_stop", {"type": "content_block_stop", "index": 0})
        sse(self, "message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                                    "usage": {"output_tokens": 5}})
        sse(self, "message_stop", {"type": "message_stop"})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
