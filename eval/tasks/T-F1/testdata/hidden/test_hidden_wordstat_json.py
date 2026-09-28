import json

from wordstat import main


def _run(capsys, argv):
    code = main(argv)
    return code, capsys.readouterr().out


def test_json_single_file(tmp_path, capsys):
    p = tmp_path / "a.txt"
    p.write_text("hello world\n")
    code, out = _run(capsys, ["--json", str(p)])
    assert code == 0
    doc = json.loads(out)
    assert doc == {
        "files": [{"path": str(p), "lines": 1, "words": 2, "chars": 12}],
        "total": {"lines": 1, "words": 2, "chars": 12},
    }


def test_json_several_files_keep_their_order(tmp_path, capsys):
    b, a = tmp_path / "b.txt", tmp_path / "a.txt"
    b.write_text("y z\nw\n")
    a.write_text("x\n")
    code, out = _run(capsys, ["--json", str(b), str(a)])
    assert code == 0
    doc = json.loads(out)
    assert [f["path"] for f in doc["files"]] == [str(b), str(a)]
    assert doc["files"][0] == {"path": str(b), "lines": 2, "words": 3, "chars": 6}
    assert doc["total"] == {"lines": 3, "words": 4, "chars": 8}


def test_json_flag_after_the_paths(tmp_path, capsys):
    p = tmp_path / "a.txt"
    p.write_text("")
    code, out = _run(capsys, [str(p), "--json"])
    assert code == 0
    assert json.loads(out)["total"] == {"lines": 0, "words": 0, "chars": 0}


def test_table_is_still_the_default(tmp_path, capsys):
    p = tmp_path / "a.txt"
    p.write_text("hello world\n")
    code, out = _run(capsys, [str(p)])
    assert code == 0
    assert out.splitlines()[0].split() == ["lines", "words", "chars", "path"]
