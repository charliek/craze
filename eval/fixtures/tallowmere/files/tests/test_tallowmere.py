import pytest

from tallowmere import Client, Reservation


def test_reservation_is_a_value():
    a = Reservation("r1", "lk-utrecht-cs-04", "B07", "482913", "2026-05-21T10:30:00Z")
    assert a == Reservation("r1", "lk-utrecht-cs-04", "B07", "482913", "2026-05-21T10:30:00Z")
    assert a.code == "482913"


def test_client_keeps_its_settings():
    c = Client("https://api.tallowmere.example/", "k-123", timeout=3)
    assert (c.base_url, c.api_key, c.timeout) == ("https://api.tallowmere.example", "k-123", 3)


def test_an_unknown_size_is_refused_before_any_request():
    # Port 9 is the discard port: a request would fail with TallowmereError, not ValueError.
    with pytest.raises(ValueError):
        Client("http://127.0.0.1:9", "k-123", timeout=1).reserve("lk-utrecht-cs-04", "XL", "CN-1")
