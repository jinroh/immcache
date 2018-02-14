package immcache

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDiskCache(t *testing.T) {
	cache := NewDiskCache(LRUIndex(), DiskCacheOptions{
		BasePath:       os.TempDir(),
		BasePathPrefix: "cozy-disk-test",
	})
	defer cache.PurgeAndClose()

	{
		rc, err := cache.GetOrLoad("key", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 4, ioutil.NopCloser(bytes.NewReader([]byte("toto"))), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isTee := rc.(*diskTee)
		assert.True(t, isTee)

		var b []byte
		b, err = ioutil.ReadAll(rc)
		if !assert.NoError(t, err) {
			return
		}

		assert.NoError(t, rc.Close())
		assert.Equal(t, b, []byte("toto"))
	}

	{
		rc, err := cache.GetOrLoad("keyfailure", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			r := io.MultiReader(bytes.NewReader([]byte("toto")), failReader{})
			return 16, ioutil.NopCloser(r), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isTee := rc.(*diskTee)
		assert.True(t, isTee)

		b := make([]byte, 4)
		_, err = io.ReadFull(rc, b)
		if !assert.NoError(t, err) {
			return
		}

		_, err = ioutil.ReadAll(rc)
		if assert.Error(t, err) {
			assert.Equal(t, errTestFail, err)
		}
		if !assert.NoError(t, rc.Close()) {
			return
		}

		rc, err = cache.GetOrLoad("keyfailure", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 16, nil, errTestFail
		}))
		if assert.Error(t, err) {
			assert.Equal(t, errTestFail, err)
		}
		assert.Nil(t, rc)

		rc, err = cache.GetOrLoad("keyfailure", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 16 /*= bad length */, ioutil.NopCloser(bytes.NewReader([]byte("toto"))), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isTee = rc.(*diskTee)
		assert.True(t, isTee)
		assert.NoError(t, rc.Close())

		rc, err = cache.GetOrLoad("keyfailure", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 4, ioutil.NopCloser(bytes.NewReader([]byte("toto"))), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isTee = rc.(*diskTee)
		assert.True(t, isTee)

		b, err = ioutil.ReadAll(rc)
		if !assert.NoError(t, err) {
			return
		}

		assert.Equal(t, b, []byte("toto"))
		assert.NoError(t, rc.Close())
	}

	// should have keys "key" and "keyfailure"

	{
		rc, err := cache.GetOrLoad("key", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 4, ioutil.NopCloser(bytes.NewReader([]byte("tata"))), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isFile := rc.(*diskFile)
		assert.True(t, isFile)

		var b []byte
		b, err = ioutil.ReadAll(rc)
		if !assert.NoError(t, err) {
			return
		}

		assert.NoError(t, rc.Close())
		assert.Equal(t, b, []byte("toto"))
	}

	{
		rc, err := cache.GetOrLoad("keyfailure", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 4, ioutil.NopCloser(bytes.NewReader([]byte("tata"))), nil
		}))
		if !assert.NoError(t, err) {
			return
		}

		_, isFile := rc.(*diskFile)
		assert.True(t, isFile)

		var b []byte
		b, err = ioutil.ReadAll(rc)
		if !assert.NoError(t, err) {
			return
		}

		assert.NoError(t, rc.Close())
		assert.Equal(t, b, []byte("toto"))
	}

	{
		errload := errors.New("load error")
		rc, err := cache.GetOrLoad("key2", FuncLoader(func(_ string) (int64, io.ReadCloser, error) {
			return 0, nil, errload
		}))
		assert.Error(t, err)
		assert.Equal(t, errload, err)
		assert.Nil(t, rc)
	}
}

func TestRandomWithSuccessOnly(t *testing.T) {
	const workerOps = 1024
	const concurrency = 256
	const resourcesLen = 128

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	resources := make(map[string]*resource, resourcesLen)
	keys := make([]string, resourcesLen)

	for i := 0; i < resourcesLen; i++ {
		resource := makeResource(int64(rng.Uint64()))
		resources[resource.key] = resource
		keys[i] = resource.key
	}

	cache := NewDiskCache(LRUIndex(), DiskCacheOptions{
		BasePath:       os.TempDir(),
		BasePathPrefix: "cozy-disk-test",
	})

	loader := FuncLoader(func(key string) (int64, io.ReadCloser, error) {
		if key == "bad" {
			return 0, nil, errWantedErr
		}
		r, ok := resources[key]
		if ok {
			l, rc := r.Open(false)
			return l, rc, nil
		}
		return 0, nil, errTestFail
	})

	donech := make(chan error)
	for i := 0; i < concurrency; i++ {
		go func(r *rand.Rand) {
			for j := 0; j < workerOps; j++ {
				k := keys[r.Uint64()%resourcesLen]
				rc, err := cache.GetOrLoad(k, loader)
				if err != nil {
					donech <- err
					return
				}
				_, err = ioutil.ReadAll(rc)
				if err != nil {
					donech <- err
					return
				}
				if err = rc.Close(); err != nil {
					donech <- rc.Close()
					return
				}
			}
			donech <- nil
		}(rand.New(rand.NewSource(int64(rng.Uint64()))))
	}

	for i := 0; i < concurrency; i++ {
		assert.NoError(t, <-donech)
	}

	assert.NoError(t, cache.PurgeAndClose())
}

func TestRandomWithErrors(t *testing.T) {
	const workerOps = 1024
	const concurrency = 256
	const resourcesLen = 128

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	resources := make(map[string]*resource, resourcesLen)
	keys := make([]string, resourcesLen)

	for i := 0; i < resourcesLen; i++ {
		resource := makeResource(int64(rng.Uint64()))
		resources[resource.key] = resource
		keys[i] = resource.key
	}

	cache := NewDiskCache(LRUIndex(), DiskCacheOptions{
		BasePath:       os.TempDir(),
		BasePathPrefix: "cozy-disk-test",
	})

	loader := FuncLoader(func(key string) (int64, io.ReadCloser, error) {
		r, ok := resources[key]
		if ok {
			l, rc := r.Open(true)
			return l, rc, nil
		}
		return 0, nil, errTestFail
	})

	donech := make(chan error)

	for i := 0; i < concurrency; i++ {
		go func(r *rand.Rand) {
			for j := 0; j < workerOps; j++ {
				expectLoadErr := r.Intn(50) == 0

				var k string
				if expectLoadErr {
					k = "bad"
				} else {
					k = keys[r.Uint64()%resourcesLen]
				}

				rc, err := cache.GetOrLoad(k, loader)
				if expectLoadErr {
					if err == nil {
						donech <- errTestFail
						return
					}
					continue
				} else {
					if err != nil {
						donech <- err
						return
					}
				}

				_, err = ioutil.ReadAll(rc)
				if err != nil {
					rc.Close()
					if err != errWantedErr {
						donech <- err
						return
					}
					continue
				}

				if err = rc.Close(); err != nil {
					if err != errWantedErr {
						donech <- err
						return
					}
				}
			}
			donech <- nil
		}(rand.New(rand.NewSource(int64(rng.Uint64()))))
	}

	for i := 0; i < concurrency; i++ {
		assert.NoError(t, <-donech)
	}

	assert.NoError(t, cache.PurgeAndClose())
}

func makeResource(seed int64) *resource {
	rng := rand.New(rand.NewSource(seed))

	k := int64(rng.Uint64())
	if k < 0 {
		k = -k
	}

	key := strconv.FormatInt(k, 10)

	return &resource{
		rng: rng,
		key: key,
	}
}

type resource struct {
	rng *rand.Rand
	key string
}

func (r *resource) Open(withErrors bool) (int64, *resourceHandler) {
	const maxResourceLen = 10 * 1024
	rng := rand.New(rand.NewSource(int64(r.rng.Uint64())))
	l := r.rng.Intn(maxResourceLen-1) + 1
	if l < 0 {
		l = -l
	}
	if l == 0 {
		l = 1
	}
	l64 := int64(l)
	return l64, &resourceHandler{
		rng: rng,
		l:   l64,
		err: withErrors,
	}
}

type resourceHandler struct {
	rng *rand.Rand
	l   int64
	n   int64
	err bool
}

func (r *resourceHandler) Read(p []byte) (n int, err error) {
	if r.err && r.rng.Intn(30) == 0 {
		return 0, errWantedErr
	}
	n = len(p)
	if int64(n)+r.n > r.l {
		n = int(r.l - r.n)
	}
	for i := 0; i < n; i++ {
		p[i] = byte(r.rng.Int()) // slow as hell, but whatev'
	}
	r.n += int64(n)
	if r.n == r.l {
		err = io.EOF
	}
	return
}

func (r *resourceHandler) Close() error {
	if r.err && r.rng.Intn(30) == 0 {
		return errWantedErr
	}
	return nil
}

var errTestFail = errors.New("failure")
var errWantedErr = errors.New("wanted")

type failReader struct{}

func (f failReader) Read(p []byte) (n int, err error) { return 0, errTestFail }
func (f failReader) Close() error                     { return nil }
