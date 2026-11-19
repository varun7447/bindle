import asyncio
import json

import pytest

import bindle
from bindle import Cassette, RecordedToolError, UnrecordedCall


def recording(**over):
    data = {
        "format": "bindle.cassette", "version": 1, "trace_id": "0af7651916cd43dd8448eb211c80319c",
        "agent": "refunds", "agent_version": "v2", "input": "Where is order A1?", "output": "It shipped.",
        "tool_calls": [
            {"step_id": "1", "tool": "lookup_order", "arguments": '{"order_id": "A1", "verbose": false}',
             "result": '{"status": "shipped"}', "status": "success"},
            {"step_id": "2", "tool": "lookup_order", "arguments": '{"verbose": false, "order_id": "A1"}',
             "result": '{"status": "delivered"}', "status": "success"},
            {"step_id": "3", "tool": "refund", "arguments": '{"order_id": "A1"}', "result": "", "status": "failure",
             "error": "refund service timed out"},
            {"step_id": "4", "tool": "notes", "arguments": "plain text", "result": "noted", "status": "success"},
        ],
        "incomplete": [],
    }
    data.update(over)
    return data


def test_load_and_fields(tmp_path):
    p = tmp_path / "run.cassette.json"
    p.write_text(json.dumps(recording()), encoding="utf-8")
    c = Cassette.load(str(p))
    assert c.input == "Where is order A1?" and c.output == "It shipped." and c.agent == "refunds"
    assert len(c.calls) == 4
    with pytest.raises(ValueError):
        Cassette({"format": "something else"})
    with pytest.raises(ValueError):
        Cassette(recording(version=2))
    with pytest.raises(ValueError):
        Cassette(recording(), on_missing="ignore")


def test_result_matches_arguments_in_order_and_decodes_json():
    c = Cassette(recording())
    assert c.result("lookup_order", {"verbose": False, "order_id": "A1"}) == {"status": "shipped"}
    assert c.result("lookup_order", '{"order_id":"A1","verbose":false}', raw=True) == '{"status": "delivered"}'
    assert c.result("notes", "plain text") == "noted"
    with pytest.raises(RecordedToolError, match="timed out"):
        c.result("refund", {"order_id": "A1"})
    with pytest.raises(UnrecordedCall):
        c.result("send_email", {"to": "x"})
    with pytest.raises(UnrecordedCall):
        c.result("lookup_order", {"order_id": "A1"})  # both recorded calls used
    rep = c.report()
    assert rep["matched_by_arguments"] == 4 and rep["missing"] == ["send_email", "lookup_order"] and rep["unused"] == []


def test_falls_back_to_call_order_for_new_arguments():
    c = Cassette(recording())
    assert c.result("lookup_order", {"order_id": "B2"}) == {"status": "shipped"}
    assert c.report()["matched_by_order"] == 1
    strict = Cassette(recording(), match_order=False)
    with pytest.raises(UnrecordedCall):
        strict.result("lookup_order", {"order_id": "B2"})


def test_decorator_sync_async_and_call_through():
    c = Cassette(recording(), on_missing="call")
    real_calls = []

    @c.tool
    def lookup_order(order_id, verbose=False):
        real_calls.append(order_id)
        return {"status": "real"}

    @c.tool(name="refund")
    async def issue_refund(order_id):
        return "real refund"

    @c.tool
    def send_email(to):
        real_calls.append(to)
        return "sent"

    assert lookup_order("A1") == {"status": "shipped"}  # matched with the default filled in
    assert lookup_order(order_id="A1", verbose=False) == {"status": "delivered"}
    with pytest.raises(RecordedToolError):
        asyncio.run(issue_refund("A1"))
    assert send_email("ops@example.com") == "sent"  # unrecorded: real tool
    assert real_calls == ["ops@example.com"]
    assert c.unused()[0].tool == "notes"
    c.reset()
    assert len(c.unused()) == 4


def test_decorator_raises_on_unrecorded_by_default():
    c = Cassette(recording())

    @c.tool
    def send_email(to):
        return "sent"

    with pytest.raises(UnrecordedCall, match="send_email"):
        send_email("ops@example.com")


def test_matches_bindle_trace_tool_recordings():
    data = recording(tool_calls=[{"tool": "lookup", "arguments": '{"args": ["A1"], "kwargs": {}}', "result": "42", "status": "success"}])
    c = Cassette(data, match_order=False)

    @c.tool
    def lookup(order_id):
        return "real"

    assert lookup("A1") == 42


def test_exported_from_package():
    assert bindle.Cassette is Cassette and "Cassette" in bindle.__all__
