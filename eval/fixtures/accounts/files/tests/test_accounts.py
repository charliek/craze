import pytest

from accounts import Directory


@pytest.fixture
def d():
    return Directory()


def test_sign_up_normalises_the_email(d):
    u = d.sign_up("  Ada@Example.COM ", " Ada ")
    assert u.email == "ada@example.com"
    assert u.name == "Ada"


@pytest.mark.parametrize("bad", ["", "ada", "ada@", "@example.com", "ada@example", "a b@example.com"])
def test_sign_up_rejects_invalid_addresses(d, bad):
    with pytest.raises(ValueError, match="invalid email address"):
        d.sign_up(bad, "x")


def test_sign_up_rejects_blocked_domains(d):
    with pytest.raises(ValueError, match="email domain not allowed"):
        d.sign_up("x@Mailinator.com", "x")


def test_sign_up_rejects_duplicates(d):
    d.sign_up("ada@example.com", "Ada")
    with pytest.raises(ValueError, match="already registered"):
        d.sign_up("ADA@example.com", "Ada again")


def test_change_email(d):
    u = d.sign_up("ada@example.com", "Ada")
    assert d.change_email(u.id, " Ada@Lovelace.org").email == "ada@lovelace.org"


def test_change_email_validates(d):
    u = d.sign_up("ada@example.com", "Ada")
    with pytest.raises(ValueError, match="invalid email address"):
        d.change_email(u.id, "not-an-email")
    with pytest.raises(ValueError, match="email domain not allowed"):
        d.change_email(u.id, "ada@tempmail.dev")
    assert d.users[u.id].email == "ada@example.com"


def test_change_email_to_own_address_is_fine(d):
    u = d.sign_up("ada@example.com", "Ada")
    assert d.change_email(u.id, "ADA@example.com").email == "ada@example.com"


def test_change_email_to_someone_elses(d):
    d.sign_up("bob@example.com", "Bob")
    u = d.sign_up("ada@example.com", "Ada")
    with pytest.raises(ValueError, match="already registered"):
        d.change_email(u.id, "bob@example.com")


def test_invite(d):
    u = d.sign_up("ada@example.com", "Ada")
    assert d.invite(u.id, " Bob@Example.com") == "bob@example.com"
    assert d.invites == {"bob@example.com": "ada@example.com"}


def test_invite_validates(d):
    u = d.sign_up("ada@example.com", "Ada")
    with pytest.raises(ValueError, match="invalid email address"):
        d.invite(u.id, "bob@")
    with pytest.raises(ValueError, match="email domain not allowed"):
        d.invite(u.id, "bob@example.invalid")
    assert d.invites == {}


def test_sign_up_consumes_the_invite(d):
    u = d.sign_up("ada@example.com", "Ada")
    d.invite(u.id, "bob@example.com")
    d.sign_up("bob@example.com", "Bob")
    assert d.invites == {}
