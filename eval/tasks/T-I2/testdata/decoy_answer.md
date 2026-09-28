The flakiness comes from timing. `Allow` refills the bucket based on elapsed time, and CI machines are slower and more loaded, so the refill calculation sometimes sees less time than expected and the bucket runs dry early. Running with -race also slows things down.

A good fix is to add a small sleep between requests in the test, or to raise the burst so the test has some headroom.
