Immcache
========

Immcache is a simple immutable key/value cache written in Go with a minimalist
API where data blobs are stored in the local filesystem. It is well designed for these use cases:

- serving small http assets that are versionned/revisioned and immutable
- having a little local cache to avoid pressuring another system


Installing
----------

```
$ go get github.com/jinroh/immcache
```


Usage
-----

The API used is simply defined by the `GetOrLoad` function in the `Immutable` inteface:

```go
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
```


Example
-------

```go
package main

import (
    "fmt"
    "github.com/jinroh/immcache"
)

func main() {
    index := immcache.LRUIndex()

    // cache can be used safely concurrently
    cache := NewDiskCache(index, DiskCacheOptions{
        BasePath:       os.TempDir(),
        BasePathPrefix: "immcache",
        DiskSizeMax:    20 << (2 * 10), // 20MB,

        EvictionPeriodMin: 30 * time.Second, // the minimum period allowed to evit the cache
        EvictionEmergencyRation: 1.5,        // the emergency ratio at which point an eviction is scheduled immediatly
    })

    url := "https://myurl.com/immutableeasset.json"

    {
        // this fetch will populate the cache
        rc, err := cachedHTTPGet(url)
        if err != nil {
            panic(err)
        }
        b, err := ioutil.ReadAll(rc)
        if err != nil {
            panic(err)
        }
        if rc.Close() != nil {
            panic(err)
        }
    }

    {  
        // this fetch will be cached
        rc, err := cachedHTTPGet(url)
        if err != nil {
            panic(err)
        }
        b, err := ioutil.ReadAll(rc)
        if err != nil {
            panic(err)
        }
        if rc.Close() != nil {
            panic(err)
        }
    }

    if errc := cache.PurgeAndClose(); errc != nil {
        fmt.Printf("Could not purge cache: %s\n", err)
    }
}

func cachedHTTPGet(url string) (io.ReadCloser, error) {
    // using url as key for immutable HTTP assets
    return cache.GetOrLoad(url, func() (size int64, rc io.ReadCloser, err error) {
        res, err := http.Get(url)
        if err != nil {
            return
        }
        if res.StatusCode != 200 {
            err = fmt.Errorf("Unexpected http response %d", res.StatusCode)
            return
        }
        return res.ContentLength, res.Body, nil
    })
}
```
