import pytest

from envfile import ParseError, parse


def test_simple_pairs():
    assert parse("A=1\nB=two\n") == {"A": "1", "B": "two"}


def test_comments_and_blank_lines():
    assert parse("# config\n\nA=1\n   # indented comment\n") == {"A": "1"}


def test_export_prefix():
    assert parse("export TOKEN=abc") == {"TOKEN": "abc"}


def test_whitespace_is_trimmed():
    assert parse("  NAME =  craze  ") == {"NAME": "craze"}


def test_quotes_are_removed():
    assert parse("A='x y'\nB=\"z\"") == {"A": "x y", "B": "z"}


def test_double_quote_escapes():
    assert parse(r'MSG="line one\nsay \"hi\""') == {"MSG": 'line one\nsay "hi"'}


def test_inline_comment_after_unquoted_value():
    assert parse("PORT=8080 # web") == {"PORT": "8080"}


def test_later_keys_win():
    assert parse("A=1\nA=2") == {"A": "2"}


def test_missing_equals_is_an_error():
    with pytest.raises(ParseError) as e:
        parse("A=1\nnonsense\n")
    assert e.value.lineno == 2


def test_bad_key_is_an_error():
    with pytest.raises(ParseError):
        parse("1A=x")
