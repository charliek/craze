from orvelline import Booking, Client, OrvellineError, _api_error


def test_booking_from_json():
    b = Booking.from_json({"reference": "OB-2207", "traveller_id": "tr_51c0aa", "pass_code": "ZONE-AB-7D",
                           "starts_on": "2026-10-03", "price": {"amount_cents": 4150, "currency": "EUR"}})
    assert b == Booking("OB-2207", "tr_51c0aa", "ZONE-AB-7D", "2026-10-03", 4150)


def test_client_keeps_its_settings():
    c = Client("https://api.orvelline.example/", "ok_test_123", timeout=3)
    assert (c.base_url, c.api_key, c.timeout) == ("https://api.orvelline.example", "ok_test_123", 3)


def test_an_error_body_is_parsed():
    e = _api_error(422, b'{"error": {"code": "pass.unknown", "message": "No such pass", "details": {"x": 1}}}')
    assert (e.status, e.code, e.message, e.details) == (422, "pass.unknown", "No such pass", {"x": 1})
    assert isinstance(e, OrvellineError)


def test_an_error_body_that_is_not_json():
    e = _api_error(502, b"<html>Bad gateway</html>")
    assert (e.status, e.code, e.details) == (502, "http_error", {})
