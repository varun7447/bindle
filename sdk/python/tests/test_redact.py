from bindle._redact import Redactor


def test_default_rules():
    r = Redactor()
    assert "[REDACTED:EMAIL]" in r.apply("mail jane.doe@acme.io")
    assert "[REDACTED:SSN]" in r.apply("ssn 123-45-6789")
    assert "[REDACTED:CARD]" in r.apply("card 4111 1111 1111 1111")
    assert "[REDACTED:SECRET]" in r.apply("key AKIAABCDEFGHIJKLMNOP")
    assert "[REDACTED:PHONE]" in r.apply("call (415) 555-0134")


def test_non_luhn_numbers_kept():
    assert "CARD" not in Redactor().apply("order 1234 5678 9012 3456")


def test_attribute_values_and_lists():
    r = Redactor()
    out = r.apply_attributes({"a": "x@y.com", "b": ["x@y.com", 3], "c": 5})
    assert out["a"] == "[REDACTED:EMAIL]"
    assert out["b"] == ["[REDACTED:EMAIL]", 3]
    assert out["c"] == 5


def test_custom_pattern():
    r = Redactor(extra_patterns={"member_id": r"\bMBR-\d{6}\b"})
    assert r.apply("member MBR-123456") == "member [REDACTED:MEMBER_ID]"
