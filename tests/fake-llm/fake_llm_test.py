#!/usr/bin/env python3
"""The fake model's script (fake_llm.decide), pinned: the Compose chief's smoke
test is only as good as the fake's refusal to make up a marker."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fake_llm  # noqa: E402

TOOLS = [{"name": "mcp__muster__tasks_list"}, {"name": "Bash"}]


def user(*content):
    return {"role": "user", "content": list(content)}


def asked(tool_id):
    return {"role": "assistant", "content": [{"type": "tool_use", "id": tool_id, "name": "x", "input": {}}]}


class Decide(unittest.TestCase):
    def test_the_question_with_the_tool_offered_is_a_tool_call(self):
        body = {"tools": TOOLS, "messages": [user({"type": "text", "text": "Who is on what right now?"})]}
        self.assertEqual(fake_llm.decide(body), ("tool", "mcp__muster__tasks_list"))

    def test_without_the_tool_it_is_plain_text(self):
        # The CLI's side requests (a title, a summary) can quote the question.
        body = {"tools": [{"name": "Bash"}], "messages": [user({"type": "text", "text": "Who is on what right now?"})]}
        self.assertEqual(fake_llm.decide(body), ("text", "ok"))

    def test_a_tool_result_is_answered_with_its_markers_only(self):
        result = {"type": "tool_result", "tool_use_id": "toolu_tasks_1", "content": [
            {"type": "text", "text": '[{"title":"MARK-abc123 is on the compose smoke"},{"title":"other"}]'}]}
        body = {"tools": TOOLS, "messages": [user({"type": "text", "text": "who is on what"}),
                                             asked("toolu_tasks_1"), user(result)]}
        self.assertEqual(fake_llm.decide(body), ("text", "SEEN:MARK-abc123"))

    def test_a_tool_result_with_no_marker_says_none(self):
        # The positive control's shape: no marker in the result, none in the reply.
        result = {"type": "tool_result", "tool_use_id": "toolu_tasks_2", "content": "[]"}
        body = {"tools": TOOLS, "messages": [asked("toolu_tasks_2"), user(result)]}
        self.assertEqual(fake_llm.decide(body), ("text", "SEEN:none"))

    def test_comment_on_it_asks_for_the_un_allowed_tool(self):
        tools = TOOLS + [{"name": "mcp__muster__task_comment"}]
        body = {"tools": tools, "messages": [user({"type": "text", "text": "Comment on it, please"})]}
        self.assertEqual(fake_llm.decide(body)[:2], ("tool", "mcp__muster__task_comment"))
        body["messages"][0]["content"][0] = {"type": "text", "text": "comment on it TASK-42"}
        self.assertEqual(fake_llm.decide(body)[2], {"id": 42, "body": "fake-llm comment that must never land"})

    def test_the_comment_result_is_reported_with_its_error_flag(self):
        result = {"type": "tool_result", "tool_use_id": "toolu_comment_3", "is_error": True,
                  "content": "Permission to use mcp__muster__task_comment has been denied."}
        # Text after the result (a reminder that even quotes the request) does
        # not turn it back into a new question: a FRESH result is answered.
        tools = TOOLS + [{"name": "mcp__muster__task_comment"}]
        body = {"tools": tools, "messages": [asked("toolu_comment_3"),
                                             user(result, {"type": "text", "text": "reminder: the user asked you to comment on it"})]}
        kind, value = fake_llm.decide(body)
        self.assertEqual((kind, value), ("text", "COMMENT-RESULT:error:Permission to use mcp__muster__task_comment has been denied."))

    def test_a_new_prompt_after_a_tool_result_in_one_turn_is_the_prompt(self):
        # The CLI can send an earlier tool_result and the new prompt in one turn,
        # and the proxy folds system text after it: the newest relevant block wins.
        tools = TOOLS + [{"name": "mcp__muster__task_comment"}]
        old = {"type": "tool_result", "tool_use_id": "toolu_tasks_1", "content": "MARK-old1"}
        body = {"tools": tools, "messages": [user(old, {"type": "text", "text": "Now comment on it, please."},
                                                  {"type": "text", "text": "<system-reminder>x</system-reminder>"})]}
        self.assertEqual(fake_llm.decide(body)[:2], ("tool", "mcp__muster__task_comment"))

    def test_the_two_shapes_seen_in_ci(self):
        tools = TOOLS + [{"name": "mcp__muster__task_comment"}]
        tasks_result = {"type": "tool_result", "tool_use_id": "toolu_tasks_1", "content": "MARK-abc1"}
        # A new prompt sent in the same user turn as the earlier tasks_list result.
        merged = {"tools": tools, "messages": [user({"type": "text", "text": "Who is on what right now?"}),
                                               asked("toolu_tasks_1"),
                                               user(tasks_result, {"type": "text", "text": "Now comment on it, please. TASK-5"})]}
        self.assertEqual(fake_llm.decide(merged), ("tool", "mcp__muster__task_comment",
                                                   {"id": 5, "body": "fake-llm comment that must never land"}))
        # The denied result, with text after it that repeats the request: reported, never asked again.
        denied = {"type": "tool_result", "tool_use_id": "toolu_comment_2", "is_error": True, "content": "denied"}
        after = {"tools": tools, "messages": merged["messages"] + [asked("toolu_comment_2"),
                 user(denied, {"type": "text", "text": "Now comment on it, please. TASK-5"})]}
        self.assertEqual(fake_llm.decide(after), ("text", "COMMENT-RESULT:error:denied"))

    def test_a_marker_in_the_question_is_not_echoed(self):
        # A marker typed in the prompt must not satisfy the check: only a tool
        # result's does.
        body = {"tools": TOOLS, "messages": [user({"type": "text", "text": "who is on what? MARK-fromprompt"})]}
        kind, value = fake_llm.decide(body)
        self.assertNotIn("MARK-fromprompt", value)


if __name__ == "__main__":
    unittest.main()
