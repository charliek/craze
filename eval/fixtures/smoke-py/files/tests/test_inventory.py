from inventory import DEFAULT_WAREHOUSE, parse_line


def test_parse_line_basic():
    assert parse_line("apples, 3") == ("apples", 3, DEFAULT_WAREHOUSE)


def test_parse_line_skips_comments_and_blanks():
    assert parse_line("# header") is None
    assert parse_line("   ") is None


def test_parse_line_warehouse():
    assert parse_line("pears,4,HERON-2") == ("pears", 4, "HERON-2")
