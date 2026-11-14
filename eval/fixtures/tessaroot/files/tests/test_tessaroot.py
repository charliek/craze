from tessaroot import Client, Reading, TessarootError, error_from_body


def test_reading_from_json():
    r = Reading.from_json({"at": "2026-03-14T06:00:00Z", "value": 21.5, "unit": "degC", "sensor": "t-01"})
    assert r == Reading("2026-03-14T06:00:00Z", 21.5, "degC")
    assert not r.provisional


def test_client_keeps_its_settings():
    c = Client("https://api.tessaroot.example/", "tk-123", timeout=3)
    assert (c.base_url, c.token, c.timeout) == ("https://api.tessaroot.example", "tk-123", 3)


def test_error_from_body():
    e = error_from_body(404, b'{"error": {"code": "TSR-4040", "message": "no such house"}}')
    assert isinstance(e, TessarootError)
    assert (e.status, e.code) == (404, "TSR-4040")
    assert "no such house" in str(e)
    plain = error_from_body(502, b"<html>Bad gateway</html>")
    assert (plain.status, plain.code) == (502, None)
