from inventory import total


def test_total_counts_every_line():
    assert total(["apples,3", "pears,4"]) == 7


def test_total_skips_comments_and_blanks():
    assert total(["# header", "apples,3", "", "pears,4,HERON-2"]) == 7


def test_total_single_line():
    assert total(["figs,5"]) == 5
