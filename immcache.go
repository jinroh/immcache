package immcache

import "io"

// Immutable defines an interface for a simple immutable key/value cache that
// can define a load function to actually load the resource when the key
// returns a cache-miss.
//
// The PurgeAncClose method can be used to purge the underlying cache, cleaning
// all the cache resources. The cache is then closed, and can not be used
// anymore.
type Immutable interface {
	GetOrLoad(key string, loader Loader) (io.ReadCloser, error)
	PurgeAndClose() error
}

// Loader is a function called to load the fetched resource, in the case of a
// cache-miss. It should return the size of the content.
type Loader func() (int64, io.ReadCloser, error)

type Index interface {
	Get(key string) (val interface{}, ok bool)
	Set(key string, val interface{})
	RemoveUnused() (key string, value interface{}, ok bool)
}
