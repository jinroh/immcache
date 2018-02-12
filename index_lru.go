package immcache

import (
	"container/list"
)

// LRU is a LRU cache.
type LRU struct {
	l *list.List
	m map[string]*list.Element
}

type lruEntry struct {
	k string
	v interface{}
}

// LRUIndex returns a new cache with the provided maximum items.
func LRUIndex() *LRU {
	return &LRU{
		l: list.New(),
		m: make(map[string]*list.Element),
	}
}

// Set adds the provided key and value to the cache.
func (c *LRU) Set(key string, value interface{}) {
	if e, ok := c.m[key]; ok {
		c.l.MoveToFront(e)
	} else {
		c.m[key] = c.l.PushFront(&lruEntry{key, value})
	}
}

// Get fetches the key's value from the cache.
// The ok result will be true if the item was found.
func (c *LRU) Get(key string) (value interface{}, ok bool) {
	if e, ok := c.m[key]; ok {
		c.l.MoveToFront(e)
		return e.Value.(*lruEntry).v, true
	}
	return
}

// RemoveUnused removes the oldest item in the cache and returns its key and
// value. If the cache is empty, the empty string and nil are returned.
func (c *LRU) RemoveUnused() (key string, value interface{}, ok bool) {
	if e := c.l.Back(); e != nil {
		c.l.Remove(e)
		ent := e.Value.(*lruEntry)
		delete(c.m, ent.k)
		return ent.k, ent.v, true
	}
	return
}

var _ Index = &LRU{}
