import pytest

from envfile import ParseError, parse


def test_hash_inside_double_quotes_is_kept():
    assert parse('PASSWORD="hunter#2"') == {"PASSWORD": "hunter#2"}


def test_hash_inside_single_quotes_is_kept():
    assert parse("PASSWORD='hunter#2'") == {"PASSWORD": "hunter#2"}


def test_quoted_value_then_inline_comment():
    assert parse('PASSWORD="hunter#2"  # rotated monthly') == {"PASSWORD": "hunter#2"}


def test_unquoted_value_starting_with_hash():
    assert parse("ACCENT=#ff6600") == {"ACCENT": "#ff6600"}


def test_hash_without_whitespace_is_part_of_the_value():
    assert parse("URL=https://example.com/docs#setup") == {"URL": "https://example.com/docs#setup"}


def test_inline_comment_after_whitespace_still_dropped():
    assert parse("PORT=8080 # web\nHOST=db\t# primary") == {"PORT": "8080", "HOST": "db"}


def test_escaped_quote_and_hash_inside_double_quotes():
    assert parse(r'MSG="say \"#1\""') == {"MSG": 'say "#1"'}


def test_unterminated_quote_is_still_an_error():
    with pytest.raises(ParseError):
        parse('A="abc')


def test_several_lines_with_hashes():
    text = "\n".join(
        [
            "# deploy settings",
            'export DB_PASSWORD="p#ss word"',
            "THEME=#222 # dark",
            "NAME=craze",
        ]
    )
    assert parse(text) == {"DB_PASSWORD": "p#ss word", "THEME": "#222", "NAME": "craze"}
