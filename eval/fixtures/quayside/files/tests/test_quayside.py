from quayside import Client, Slot


def test_slot_from_json():
    s = Slot.from_json({"berth": "B7", "vessel": "Nordic Star", "start": "2026-03-14T06:00:00Z",
                        "end": "2026-03-14T14:00:00Z", "pilot": "required"})
    assert s == Slot("B7", "Nordic Star", "2026-03-14T06:00:00Z", "2026-03-14T14:00:00Z")


def test_client_keeps_its_settings():
    c = Client("https://api.quayside.example/", "k-123", timeout=3)
    assert (c.base_url, c.api_key, c.timeout) == ("https://api.quayside.example", "k-123", 3)
