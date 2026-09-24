package transcript

// keyedList is an ordered list of values keyed by a string id: the fold's open
// asks and its last-ended list (r2 finding 5). An upsert, a lookup and a
// removal are O(1) through an id → position index, and the order lives in a
// slice in which a removal leaves a hole — the zero value, whose key is "" —
// that a compaction squeezes out once the holes are half the slice, so a
// removal is amortised O(1) as well and nothing is shifted under the
// publishing boundary. The ordered values are materialised only for a reader
// (values), which a cut does under the model's lock in O(len) — the same
// shape as its copy of the entries' pointers.
//
// The empty key is never kept: it is the hole's.
type keyedList[T keyed] struct {
	items []T
	pos   map[string]int
	// first is where the oldest live item may be: every slot before it is a
	// hole, so dropping the oldest skips each hole once.
	first int
}

// keyed is a value with an id; "" means none.
type keyed interface{ key() string }

func (a Ask) key() string       { return a.ID }
func (e AskEnding) key() string { return e.ID }

// len is how many items the list holds.
func (l *keyedList[T]) len() int { return len(l.pos) }

// has reports whether the list holds an item under k.
func (l *keyedList[T]) has(k string) bool {
	_, ok := l.pos[k]
	return ok
}

// upsert replaces the item under v's key where it stands, or appends v. A
// value with no key is not kept.
func (l *keyedList[T]) upsert(v T) {
	k := v.key()
	if k == "" {
		return
	}
	if i, ok := l.pos[k]; ok {
		l.items[i] = v
		return
	}
	if l.pos == nil {
		l.pos = make(map[string]int)
	}
	l.pos[k] = len(l.items)
	l.items = append(l.items, v)
}

// remove drops the item under k, if there is one, leaving a hole.
func (l *keyedList[T]) remove(k string) {
	i, ok := l.pos[k]
	if !ok {
		return
	}
	var zero T
	l.items[i] = zero
	delete(l.pos, k)
	l.compact()
}

// dropOldest removes the oldest item, if there is one.
func (l *keyedList[T]) dropOldest() {
	for l.first < len(l.items) && l.items[l.first].key() == "" {
		l.first++
	}
	if l.first < len(l.items) {
		l.remove(l.items[l.first].key())
	}
}

// compact squeezes the holes out once they are at least half the slice and
// past a small floor, rewriting each moved item's position: O(live), once per
// O(live) removals. A list that has emptied starts over in the same array.
func (l *keyedList[T]) compact() {
	if len(l.pos) == 0 {
		// Every slot is a hole, and a hole is already the zero value.
		l.items = l.items[:0]
		l.first = 0
		return
	}
	holes := len(l.items) - len(l.pos)
	if holes < 32 || 2*holes < len(l.items) {
		return
	}
	n := 0
	for _, v := range l.items {
		if k := v.key(); k != "" {
			l.items[n] = v
			l.pos[k] = n
			n++
		}
	}
	clear(l.items[n:])
	l.items = l.items[:n]
	l.first = 0
}

// values is the items in order, a copy of its own; nil when there are none.
func (l *keyedList[T]) values() []T {
	if len(l.pos) == 0 {
		return nil
	}
	out := make([]T, 0, len(l.pos))
	for _, v := range l.items[l.first:] {
		if v.key() != "" {
			out = append(out, v)
		}
	}
	return out
}
