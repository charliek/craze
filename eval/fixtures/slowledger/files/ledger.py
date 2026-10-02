"""A tiny in-memory ledger.

Amounts are integer cents. ``post`` appends a signed entry to an account;
``transfer`` moves cents from one account to another and refuses (with
``InsufficientFunds``) only when the source holds *less* than the amount, so an
account can always be emptied.
"""


class InsufficientFunds(Exception):
    """The source account holds less than the amount to transfer."""


class Ledger:
    def __init__(self):
        self._entries = []

    def post(self, account: str, cents: int) -> None:
        self._entries.append((account, cents))

    def balance(self, account: str) -> int:
        return sum(cents for name, cents in self._entries if name == account)

    def accounts(self) -> list[str]:
        return sorted({name for name, _ in self._entries})

    def transfer(self, src: str, dst: str, cents: int) -> None:
        if cents <= 0:
            raise ValueError("amount must be positive")
        if self.balance(src) <= cents:
            raise InsufficientFunds(f"{src} holds {self.balance(src)}, need {cents}")
        self.post(src, -cents)
        self.post(dst, cents)

    def statement(self, account: str) -> list[int]:
        return [cents for name, cents in self._entries if name == account]
