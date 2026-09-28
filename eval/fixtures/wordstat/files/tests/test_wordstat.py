from wordstat import Counts, count_text, main


def test_count_text():
    assert count_text("one two\nthree\n") == Counts(lines=2, words=3, chars=14)


def test_empty_text():
    assert count_text("") == Counts()


def test_table_for_one_file(tmp_path, capsys):
    p = tmp_path / "a.txt"
    p.write_text("hello world\n")
    assert main([str(p)]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split() == ["lines", "words", "chars", "path"]
    assert lines[1].split() == ["1", "2", "12", str(p)]
    assert len(lines) == 2


def test_table_total_for_several_files(tmp_path, capsys):
    a, b = tmp_path / "a.txt", tmp_path / "b.txt"
    a.write_text("x\n")
    b.write_text("y z\nw\n")
    assert main([str(a), str(b)]) == 0
    last = capsys.readouterr().out.splitlines()[-1]
    assert last.split() == ["3", "4", "8", "total"]


def test_missing_file(tmp_path, capsys):
    assert main([str(tmp_path / "nope.txt")]) == 1
    assert "nope.txt" in capsys.readouterr().err
