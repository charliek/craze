// Command kv is a small demo of the store: it loads a JSON snapshot, runs one
// command against it, and writes the snapshot back.
//
//	kv -file data.json -bucket users put ada '{"role":"admin"}'
//	kv -file data.json -bucket users get ada
//	kv -file data.json list-buckets
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"example.com/kvstore"
)

// snapshot is the file format: bucket name -> key -> value.
type snapshot map[string]map[string]string

func load(path string) (*kvstore.Store, error) {
	s := kvstore.New()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	for name, kv := range snap {
		b, err := s.CreateBucket(name)
		if err != nil {
			return nil, err
		}
		for k, v := range kv {
			b.Put(k, []byte(v))
		}
	}
	return s, nil
}

func save(path string, s *kvstore.Store) error {
	snap := snapshot{}
	for _, name := range s.Buckets() {
		b, _ := s.Bucket(name)
		kv := map[string]string{}
		for _, k := range b.Keys() {
			v, _ := b.Get(k)
			kv[k] = string(v)
		}
		snap[name] = kv
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func main() {
	file := flag.String("file", "kv.json", "the snapshot file")
	bucket := flag.String("bucket", "default", "the bucket to use")
	flag.Parse()
	if err := run(*file, *bucket, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "kv:", err)
		os.Exit(1)
	}
}

func run(file, bucket string, args []string) error {
	s, err := load(file)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: kv [-file f] [-bucket b] get <key> | put <key> <value> | list-buckets")
	}
	switch args[0] {
	case "list-buckets":
		for _, name := range s.Buckets() {
			fmt.Println(name)
		}
		return nil
	case "get":
		if len(args) != 2 {
			return errors.New("get takes one key")
		}
		b, err := s.Bucket(bucket)
		if err != nil {
			return err
		}
		v, ok := b.Get(args[1])
		if !ok {
			return fmt.Errorf("%s: no such key in bucket %s", args[1], bucket)
		}
		fmt.Println(string(v))
		return nil
	case "put":
		if len(args) != 3 {
			return errors.New("put takes a key and a value")
		}
		b, err := s.Bucket(bucket)
		if errors.Is(err, kvstore.ErrNoBucket) {
			b, err = s.CreateBucket(bucket)
		}
		if err != nil {
			return err
		}
		b.Put(args[1], []byte(args[2]))
		return save(file, s)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
