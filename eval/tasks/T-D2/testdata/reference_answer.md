I started `python app.py` in the background (it listens on 127.0.0.1:8000), requested both routes with curl, and then stopped the server.

- `GET /health` returns `{"status":"ok","checks":{"db":"up","queue":"up"}}`
- `GET /version` returns `{"version":"2.7.3","build":"a41c9e0"}`

The server is no longer running, and I changed no files.
