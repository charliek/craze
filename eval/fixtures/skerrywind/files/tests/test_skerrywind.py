from skerrywind import Client, Reading


def test_reading_from_log_line():
    r = Reading.from_log_line("2026-04-02T10:00:00Z 8.4 1009.6 5.1\n")
    assert r == Reading("2026-04-02T10:00:00Z", 8.4, 1009.6, 5.1)


def test_client_keeps_its_settings():
    c = Client("https://ingest.skerrywind.example/", "glenmore-ridge-03", "s3cret", timeout=4)
    assert (c.base_url, c.station_id, c.signing_secret, c.timeout) == (
        "https://ingest.skerrywind.example", "glenmore-ridge-03", "s3cret", 4)
