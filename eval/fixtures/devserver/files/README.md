# devserver

A tiny status service for local development (Python standard library only).

```shell
python app.py          # listens on 127.0.0.1:8000 (override with PORT)
curl localhost:8000/health
curl localhost:8000/version
```

It runs in the foreground until interrupted.
