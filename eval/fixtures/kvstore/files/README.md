# kvstore

An in-memory key-value store. Keys live in **buckets**: each bucket is its own
keyspace, so two buckets can hold the same key with different values.

```go
s := kvstore.New()
users, _ := s.CreateBucket("users")
users.Put("ada", []byte(`{"role":"admin"}`))

b, err := s.Bucket("users")      // kvstore.ErrNoBucket if it does not exist
names := s.Buckets()              // sorted bucket names
err = s.DeleteBucket("users")     // drops the bucket and its keys
```

## The kv command

`cmd/kv` keeps a store in a JSON file (bucket → key → value):

```shell
go run ./cmd/kv -file data.json -bucket users put ada admin
go run ./cmd/kv -file data.json -bucket users get ada
go run ./cmd/kv -file data.json list-buckets
```

`-bucket` defaults to `default`; `put` creates the bucket if needed.
