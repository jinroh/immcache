package immcache

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/crypto"
)

const (
	defaultEvictionPeriodMin      = 30 // in seconds
	defaultEvictionEmergencyRatio = 1.5
)

var (
	errCorruptedCache = errors.New("imutcache: corrupted")
	errSizeNotMatch   = errors.New("imutcache: size does not match")
)

// DiskCache implement an immutable cache using the local filesystem as its
// persistence layer.
type DiskCache struct {
	basePath string
	index    Index
	secret   []byte
	size     int64
	sizeMax  int64

	indexmu sync.RWMutex
	initmu  sync.Mutex
	inited  bool
	closed  bool

	evict     chan int64
	evictLast time.Time

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

// NewDiskCache returns a Immutable allowing to store files in the local
// filesystem. The cached files are stored in the given base directory, or the
// default OS temporary folder if empty, and stored using the given prefix.
func NewDiskCache(index Index, opts DiskCacheOptions) (c *DiskCache) {
	return &DiskCache{
		index: index,
		opts:  &opts,
	}
}

func (c *DiskCache) init() bool {
	c.initmu.Lock()
	defer c.initmu.Unlock()
	if c.inited || c.closed {
		return !c.closed
	}

	var err error
	if c.opts.BasePath == "" || c.opts.BasePathPrefix != "" {
		c.basePath, err = ioutil.TempDir(c.opts.BasePath, c.opts.BasePathPrefix)
	} else {
		c.basePath, err = c.opts.BasePath, os.MkdirAll(c.opts.BasePath, 0700)
	}
	if err != nil {
		c.closed = true
		return false
	}

	if len(c.opts.Secret) > 0 {
		c.secret = c.opts.Secret
	} else {
		c.secret = crypto.GenerateRandomBytes(16)
	}

	c.sizeMax = c.opts.DiskSizeMax

	if c.sizeMax > 0 {
		c.evict = make(chan int64, 5)
		go c.evictRoutine()
	}

	c.inited = true
	return true
}

func (c *DiskCache) PurgeAndClose() error {
	c.initmu.Lock()
	defer c.initmu.Unlock()
	if c.closed {
		return nil
	}
	if c.inited {
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
	c.closed = true
	return nil
}

func (c *DiskCache) BasePath() string {
	c.initmu.Lock()
	defer c.initmu.Unlock()
	return c.basePath
}

func (c *DiskCache) GetOrLoad(key string, loader Loader) (rc io.ReadCloser, err error) {
	if !c.init() {
		_, rc, err = loader()
		return
	}
	return c.getOrLoad(key, loader)
}

func (c *DiskCache) getOrLoad(key string, loader Loader) (src io.ReadCloser, err error) {
	// fast case, if the file already is in our index
	c.indexmu.RLock()
	value, ok := c.index.Get(key)
	c.indexmu.RUnlock()
	if ok {
		sum := value.(diskEntry).sum
		src, err = c.openFile(sum, c.secret)
		if err == nil {
			return
		}
		// if we hitted another error than "file does not exist" — meaning there
		// is an issue fetching files from the local disk — we bail early and
		// return the loader value.
		if !os.IsNotExist(err) {
			_, src, err = loader()
			return
		}
	}

	var size int64
	size, src, err = loader()
	if err != nil || size < 0 {
		return
	}
	// if file size takes more than the tenth of the total available size of the
	// cache, do not put this file into the cache.
	if c.sizeMax > 0 && size/10 > c.sizeMax {
		return
	}

	// create the temporary file in which we stream the content of the source.
	tmp, errt := ioutil.TempFile(c.basePath, "")
	if errt != nil {
		return
	}

	return &diskTee{
		src:  src,
		tmp:  tmp,
		key:  key,
		size: size,
		c:    c,
		h:    hmac.New(sha256.New, c.secret),
	}, nil
}

func (c *DiskCache) addFileLocked(tmp *os.File, key string, size int64, sum []byte) error {
	var totalSize int64
	err := c.renameOrCopy(tmp, sum)
	c.indexmu.Lock()
	if err == nil {
		c.index.Set(key, diskEntry{sum, size})
		c.size += size
		totalSize = c.size
	}
	c.indexmu.Unlock()
	if c.sizeMax > 0 && totalSize > c.sizeMax {
		c.evict <- totalSize
	}
	return err
}

func (c *DiskCache) renameOrCopy(tmp *os.File, sum []byte) (err error) {
	newpath := c.getFilename(sum)
	err = os.MkdirAll(filepath.Dir(newpath), 0700)
	if err == nil || os.IsExist(err) {
		err = os.Rename(tmp.Name(), newpath)
	}
	return
}

func (c *DiskCache) getFilename(sum []byte) string {
	key := hex.EncodeToString(sum)
	return filepath.Join(c.basePath, key[:2], key[2:32])
}

func (c *DiskCache) openFile(sum, secret []byte) (*diskFile, error) {
	filename := c.getFilename(sum)
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, secret)
	return &diskFile{
		f:   f,
		h:   h,
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
	c.indexmu.Lock()
	defer c.indexmu.Unlock()
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
	c    *DiskCache
	h    hash.Hash

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
	if errw == nil {
		errw = t.c.addFileLocked(t.tmp, t.key, t.size, t.h.Sum(nil))
	}
	if errw != nil {
		os.Remove(t.tmp.Name())
	}
	return errc
}