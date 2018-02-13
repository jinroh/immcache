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
Loader interface {
    Load(key string) (int64, io.ReadCloser, error)
}
```


Example
-------

```go
package main

import (
    "bytes"
    "fmt"
    "io"
    "io/ioutil"
    "net/http"
    "os"
    "time"

    "github.com/jinroh/immcache"
)

func main() {
    index := immcache.LRUIndex()

    // cache can be used safely concurrently
    cache := immcache.NewDiskCache(index, immcache.DiskCacheOptions{
        BasePath:       os.TempDir(),
        BasePathPrefix: "immcache",
        DiskSizeMax:    20 << (2 * 10), // 20MB,

        EvictionPeriodMin:      30 * time.Second, // the minimum period allowed to evit the cache
        EvictionEmergencyRatio: 1.5,              // the emergency ratio at which point an eviction is scheduled immediatly
    })

    // FuncLoader transforms a load request into a Loader interface.
    loader := immcache.FuncLoader(httpGet)

    url := "https://raw.githubusercontent.com/jinroh/immcache/88c42cb2cdd32c8188b3716ac633d780500a1272/README.md"
    if args := os.Args; len(args) > 1 {
        url = args[1]
    }

    var b1, b2 []byte
    {
        // this fetch will populate the cache
        rc, err := cache.GetOrLoad(url, loader)
        exitOnErr(err)
        b1, err = ioutil.ReadAll(rc)
        exitOnErr(err)
        exitOnErr(rc.Close())
    }

    {
        // this fetch will be cached
        rc, err := cache.GetOrLoad(url, loader)
        exitOnErr(err)
        b2, err = ioutil.ReadAll(rc)
        exitOnErr(err)
        exitOnErr(rc.Close())
    }

    if !bytes.Equal(b1, b2) {
        fmt.Println("Bytes slice are not equal !!!")
    } else {
        fmt.Println(string(b1))
    }
    if errc := cache.PurgeAndClose(); errc != nil {
        fmt.Printf("Could not purge cache: %s\n", errc)
    }
}

func httpGet(url string) (size int64, rc io.ReadCloser, err error) {
    res, err := http.Get(url)
    if err != nil {
        return
    }
    if res.StatusCode != 200 {
        err = fmt.Errorf("Unexpected http response %d", res.StatusCode)
        return
    }
    return res.ContentLength, res.Body, nil
}

func exitOnErr(err error) {
    if err != nil {
        fmt.Printf("exit: %s\n", err)
        os.Exit(1)
    }
}
```
