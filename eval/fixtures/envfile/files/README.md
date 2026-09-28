# envfile

A small reader for `.env` files, used by the deploy scripts.

```python
import envfile

settings = envfile.load(".env")
```

See the module docstring for the syntax. Run the tests with `pytest`.
