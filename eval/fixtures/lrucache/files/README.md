# lrucache

A generic, fixed-size LRU cache.

```go
c := lrucache.New[image.Image](512, func(key string, img image.Image) {
    metrics.Evictions.Inc()
})
c.Put("thumb/42", img)
img, ok := c.Get("thumb/42")
```

`go test ./...` runs the tests.
