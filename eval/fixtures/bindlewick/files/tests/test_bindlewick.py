from bindlewick import BindlewickError, Checkout, Client, RenewalRefused


def test_checkout_is_a_value():
    a = Checkout("c-1", "31205000418823", "The Lantern Orchard", "2026-06-01")
    assert a == Checkout("c-1", "31205000418823", "The Lantern Orchard", "2026-06-01")
    assert (a.id, a.barcode, a.title, a.due) == ("c-1", "31205000418823", "The Lantern Orchard", "2026-06-01")


def test_a_refused_renewal_is_an_api_error():
    assert issubclass(RenewalRefused, BindlewickError)


def test_client_keeps_its_settings():
    c = Client("https://api.bindlewick.example/", "bw-123", branch="ELM", timeout=3)
    assert (c.base_url, c.api_key, c.branch, c.timeout) == ("https://api.bindlewick.example", "bw-123", "ELM", 3)
