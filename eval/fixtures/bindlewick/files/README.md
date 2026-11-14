# bindlewick

A small Python client for the Bindlewick Circulation API, used by the self-service kiosks
at our branches to lend, list and renew items. Standard library only: `urllib` for HTTP,
`json` for bodies.

```python
from bindlewick import Client, RenewalRefused

client = Client("https://api.bindlewick.example", api_key="...", branch="ELM")
item = client.checkout("p-1042", "31205000418823")
print(item.title, "is due back on", item.due)

for item in client.checkouts("p-1042"):
    try:
        client.renew(item.id)
    except RenewalRefused:
        print(item.title, "cannot be renewed again")
```

`branch` is the code of the branch the kiosk stands in. The API's documentation is at
https://docs.bindlewick.example.

Run the tests with `python -m pytest`.
