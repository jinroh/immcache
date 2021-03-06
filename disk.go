package immcache

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultEvictionPeriodMin      = 30 // in seconds
	defaultEvictionEmergencyRatio = 1.5
)

var (
	errCorruptedCache = errors.New("immcache: corrupted")
	errSizeNotMatch   = errors.New("immcache: size does not match")
)

const (
	inited = 1
	closed = 2
)

// DiskCache implement an immutable cache using the local filesystem as its
// persistence layer.
type DiskCache struct {
	state uint32               // 0 = non-initialized, 1 = initialized, 2 = closed
	index Index                // owned by mu
	size  int64                // owned by mu
	calls map[string]*loadCall // owned by mu
	mu    sync.Mutex           // not a RWMutex: indexes may have write ops on read

	// "constants" after initialization
	basePath string
	secret   []byte
	sizeMax  int64

	evict     chan int64
	evictLast time.Time // owned by the eviction routine under the evict channel

	opts *DiskCacheOptions
}

// DiskCacheOptions are the options to create a disk cache.
type DiskCacheOptions struct {
	BasePath       string
	BasePathPrefix string
	Secret         []byte
	DiskSizeMax    int64

	EvictionPeriodMin      time.Duration
	EvictionEmergencyRatio float64
}

type diskEntry struct {
	sum  []byte
	size int64
}

type loadCall struct {
	sync.WaitGroup
	er error
}

// NewDiskCache returns a Immutable allowing to store files in the local
// filesystem. The cached files are stored in the given base directory, or the
// default OS temporary folder if empty, and stored using the given prefix.
func NewDiskCache(index Index, opts DiskCacheOptions) (c *DiskCache) {
	return &DiskCache{
		index: index,
		calls: make(map[string]*loadCall),
		opts:  &opts,
	}
}

func (c *DiskCache) init() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := atomic.LoadUint32(&c.state)
	if state > 0 {
		return state == inited
	}

	if l := len(c.opts.Secret); l > 0 {
		c.secret = make([]byte, l)
		copy(c.secret, c.opts.Secret)
	}

	var err error
	if c.opts.BasePath == "" || c.opts.BasePathPrefix != "" {
		c.basePath, err = ioutil.TempDir(c.opts.BasePath, c.opts.BasePathPrefix)
	} else {
		c.basePath, err = c.opts.BasePath, os.MkdirAll(c.opts.BasePath, 0700)
	}
	if err != nil {
		atomic.StoreUint32(&c.state, closed)
		return false
	}

	c.sizeMax = c.opts.DiskSizeMax

	if c.sizeMax > 0 {
		c.evict = make(chan int64, 1)
		c.evictLast = time.Now()
		go c.evictRoutine()
	}

	atomic.StoreUint32(&c.state, inited)
	return true
}

func (c *DiskCache) PurgeAndClose() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := atomic.LoadUint32(&c.state)
	if state == closed {
		return nil
	}
	if state == inited {
		c.index = nil
		if c.basePath != "" {
			os.RemoveAll(c.basePath)
			c.basePath = ""
		}
		if c.evict != nil {
			close(c.evict)
			c.evict = nil
		}
	}
	atomic.StoreUint32(&c.state, closed)
	return nil
}

func (c *DiskCache) BasePath() string {
	if atomic.LoadUint32(&c.state) == inited {
		return c.basePath
	}
	return ""
}

func (c *DiskCache) GetOrLoad(key string, loader Loader) (rc io.ReadCloser, err error) {
	if atomic.LoadUint32(&c.state) == inited || c.init() {
		return c.getOrLoad(key, loader)
	}
	_, rc, err = loader.Load(key)
	return
}

func (c *DiskCache) getOrLoad(key string, loader Loader) (src io.ReadCloser, err error) {
	var entry diskEntry
	var call *loadCall
	var cacheHit, callHit bool

	defer func() {
		didLoad := !callHit && !cacheHit
		if didLoad && err != nil {
			c.mu.Lock()
			delete(c.calls, key)
			c.mu.Unlock()
			call.er = err
			call.Done()
		}
	}()

	{
		c.mu.Lock()
		entry, cacheHit = c.get(key)
		if !cacheHit {
			if call, callHit = c.calls[key]; !callHit {
				call = new(loadCall)
				call.Add(1)
				c.calls[key] = call
			}
		}
		c.mu.Unlock()
	}

	// another call on the given key is in-flight: waitint for it to finish to
	// avoid multiple calls on the same key while populating the cache. if the
	// in-flight has failed, we call the loader and bail without populating the
	// cache.
	if callHit {
		call.Wait()
		if call.er == nil {
			c.mu.Lock()
			entry, cacheHit = c.get(key)
			c.mu.Unlock()
		}
		if call.er != nil || !cacheHit {
			_, src, err = loader.Load(key)
			return
		}
		call = nil
	}

	// a cache hit was achieved, either directly from the cache, or after waiting
	// for an in-flight loader to finish: open the file with the specified
	// checksum.
	if cacheHit {
		src, err = c.openFile(entry.sum)
		if err == nil {
			return
		}
		// if we hitted another error than "file does not exist" — meaning there
		// is an issue fetching files from the local disk — we bail early and
		// return the loader value.
		if !os.IsNotExist(err) {
			_, src, err = loader.Load(key)
			return
		}
	}

	// at this point, we are launching a new load request.

	var size int64
	size, src, err = loader.Load(key)
	if err != nil || size < 0 {
		return
	}
	// if file size takes more than the tenth of the total available size of the
	// cache, do not put this file into the cache.
	if c.sizeMax > 0 && size/10 > c.sizeMax {
		return
	}

	// create the temporary file in which we stream the content of the source.
	// the temporary file is created in the basePath to make sure we can safely
	// rename the file to its destination without having a copy (ie. from the
	// same device/partition).
	tmp, errt := ioutil.TempFile(c.basePath, "")
	if errt != nil {
		return
	}

	return &diskTee{
		src:  src,
		tmp:  tmp,
		key:  key,
		call: call,
		size: size,
		c:    c,
		h:    c.hash(),
	}, nil
}

func (c *DiskCache) addFileLocked(err error, tmppath, key string, size int64, sum []byte) error {
	var totalSize int64

	c.mu.Lock()
	if err == nil {
		err = c.rename(tmppath, sum)
		if err == nil || os.IsExist(err) {
			c.index.Set(key, diskEntry{sum, size})
		}
		if err == nil {
			c.size += size
			totalSize = c.size
		}
	}
	delete(c.calls, key)
	c.mu.Unlock()

	if c.sizeMax > 0 && totalSize > c.sizeMax {
		select {
		case c.evict <- totalSize:
		default:
		}
	}

	return err
}

func (c *DiskCache) hash() hash.Hash {
	if c.secret != nil {
		return hmac.New(sha256.New, c.secret)
	}
	return sha256.New()
}

func (c *DiskCache) rename(tmppath string, sum []byte) (err error) {
	newpath := c.getFilename(sum)
	err = os.MkdirAll(filepath.Dir(newpath), 0700)
	if err != nil && !os.IsExist(err) {
		return
	}
	// make sure we cannot concurrently create the same file and messing the size
	// of the cache. since file paths are calculated via a checksum, two files
	// with different keys but with the same content will collide.
	f, err := os.OpenFile(newpath, os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	return os.Rename(tmppath, newpath)
}

func (c *DiskCache) get(key string) (entry diskEntry, ok bool) {
	var value interface{}
	if value, ok = c.index.Get(key); ok {
		entry = value.(diskEntry)
	}
	return
}

func (c *DiskCache) getFilename(sum []byte) string {
	key := hex.EncodeToString(sum)
	return filepath.Join(c.basePath, key[:2], key[2:32])
}

func (c *DiskCache) openFile(sum []byte) (*diskFile, error) {
	filename := c.getFilename(sum)
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	return &diskFile{
		f:   f,
		h:   c.hash(),
		sum: sum,
	}, nil
}

func (c *DiskCache) evictRoutine() {
	evictionPeriodMin := c.opts.EvictionPeriodMin
	evictionEmergencyRatio := c.opts.EvictionEmergencyRatio
	if evictionPeriodMin == 0 {
		evictionPeriodMin = defaultEvictionPeriodMin * time.Second
	}
	if evictionEmergencyRatio < 1.0 {
		evictionEmergencyRatio = defaultEvictionEmergencyRatio
	}
	for size := range c.evict {
		runEviction := time.Until(c.evictLast) >= evictionPeriodMin ||
			float64(size)/float64(c.sizeMax) >= evictionEmergencyRatio
		if runEviction {
			c.eviction()
			c.evictLast = time.Now()
		}
	}
}

func (c *DiskCache) eviction() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.size > c.sizeMax {
		_, value, ok := c.index.RemoveUnused()
		if !ok {
			break
		}
		entry := value.(diskEntry)
		err := os.Remove(c.getFilename(entry.sum))
		if err != nil && !os.IsNotExist(err) {
			break
		}
		c.size -= entry.size
	}
}

type diskFile struct {
	f   *os.File
	h   hash.Hash
	bfr *bufio.Reader
	sum []byte
}

func (f *diskFile) Read(p []byte) (n int, err error) {
	if f.bfr == nil {
		f.bfr = bufio.NewReader(f.f)
	}
	n, err = f.bfr.Read(p)
	if n > 0 {
		f.h.Write(p[:n])
	}
	return
}

func (f *diskFile) Close() (err error) {
	if err = f.f.Close(); err != nil {
		return
	}
	if !hmac.Equal(f.h.Sum(nil), f.sum) {
		os.Remove(f.f.Name())
		return errCorruptedCache
	}
	return
}

type diskTee struct {
	src  io.ReadCloser
	tmp  *os.File
	bfr  *bufio.Writer
	key  string
	size int64

	c *DiskCache
	h hash.Hash

	call *loadCall

	n int64
	e error
}

func (t *diskTee) Read(p []byte) (n int, err error) {
	n, err = t.src.Read(p)
	if t.bfr == nil {
		t.bfr = bufio.NewWriter(t.tmp)
	}
	if n > 0 && t.e == nil {
		nw, errw := t.bfr.Write(p[:n])
		if errw != nil {
			t.e = errw
		} else if nw != n {
			t.e = io.ErrShortWrite
		} else {
			t.h.Write(p[:n])
			t.n += int64(n)
		}
	}
	return
}

func (t *diskTee) Close() (err error) {
	errc := t.src.Close()
	var errw error
	if t.bfr != nil {
		errw = t.bfr.Flush()
	}
	if errwc := t.tmp.Close(); errw == nil {
		errw = errwc
	}
	if errw == nil {
		errw = t.e
	}
	if errw == nil && t.n != t.size {
		errw = errSizeNotMatch
	}
	errw = t.c.addFileLocked(errw, t.tmp.Name(), t.key, t.size, t.h.Sum(nil))
	if errw != nil {
		os.Remove(t.tmp.Name())
	}
	// wake the entry's waitgroup
	if t.call != nil {
		t.call.er = errw
		t.call.Done()
	}
	return errc
}

func genRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("immcache: could not generate random bytes: %s",
			err)
	}
	return b, nil
}
