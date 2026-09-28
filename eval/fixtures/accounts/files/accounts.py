"""User accounts: sign-up, email changes and team invitations."""

from __future__ import annotations

import re
from dataclasses import dataclass, field

EMAIL_RE = re.compile(r"[a-z0-9._%+-]+@[a-z0-9-]+(\.[a-z0-9-]+)+")
BLOCKED_DOMAINS = {"mailinator.com", "example.invalid", "tempmail.dev"}


@dataclass
class User:
    id: int
    email: str
    name: str


@dataclass
class Directory:
    users: dict[int, User] = field(default_factory=dict)
    invites: dict[str, str] = field(default_factory=dict)  # email -> inviter's email
    _next_id: int = 1

    def by_email(self, email: str) -> User | None:
        return next((u for u in self.users.values() if u.email == email), None)

    def sign_up(self, email: str, name: str) -> User:
        email = email.strip().lower()
        if not EMAIL_RE.fullmatch(email):
            raise ValueError(f"invalid email address: {email!r}")
        if email.rsplit("@", 1)[1] in BLOCKED_DOMAINS:
            raise ValueError(f"email domain not allowed: {email!r}")
        if self.by_email(email) is not None:
            raise ValueError(f"{email} is already registered")
        user = User(self._next_id, email, name.strip())
        self.users[user.id] = user
        self._next_id += 1
        self.invites.pop(email, None)
        return user

    def change_email(self, user_id: int, new_email: str) -> User:
        user = self.users[user_id]
        new_email = new_email.strip().lower()
        if not EMAIL_RE.fullmatch(new_email):
            raise ValueError(f"invalid email address: {new_email!r}")
        if new_email.rsplit("@", 1)[1] in BLOCKED_DOMAINS:
            raise ValueError(f"email domain not allowed: {new_email!r}")
        other = self.by_email(new_email)
        if other is not None and other.id != user_id:
            raise ValueError(f"{new_email} is already registered")
        user.email = new_email
        return user

    def invite(self, inviter_id: int, email: str) -> str:
        inviter = self.users[inviter_id]
        email = email.strip().lower()
        if not EMAIL_RE.fullmatch(email):
            raise ValueError(f"invalid email address: {email!r}")
        if email.rsplit("@", 1)[1] in BLOCKED_DOMAINS:
            raise ValueError(f"email domain not allowed: {email!r}")
        if self.by_email(email) is not None:
            raise ValueError(f"{email} is already registered")
        self.invites[email] = inviter.email
        return email
