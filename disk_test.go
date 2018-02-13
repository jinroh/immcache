package immcache

import (
	"bytes"
	"encoding/binary"
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

func TestRandom(t *testing.T) {
	const workerOps = 1024
	const concurrency = 128
	const resourcesLen = 128

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	resources := make(map[string][]byte, resourcesLen)
	keys := make([]string, resourcesLen)

	for i := 0; i < resourcesLen; i++ {
		key, val := genResource(rng)
		resources[key] = val
		keys[i] = key
	}

	cache := NewDiskCache(LRUIndex(), DiskCacheOptions{
		BasePath:       os.TempDir(),
		BasePathPrefix: "cozy-disk-test",
	})

	loader := FuncLoader(func(key string) (int64, io.ReadCloser, error) {
		b, ok := resources[key]
		if ok {
			return int64(len(b)), ioutil.NopCloser(bytes.NewReader(b)), nil
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
				b, err := ioutil.ReadAll(rc)
				if err != nil {
					donech <- err
					return
				}
				if !bytes.Equal(b, resources[k]) {
					donech <- errTestFail
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

func genResource(rng *rand.Rand) (key string, val []byte) {
	const maxResourceLen = 10 * 1024

	len := int((((rng.Uint32() + 8) % maxResourceLen) / 8) * 8)
	k := int64(rng.Uint64())
	if k < 0 {
		k = -k
	}
	key = strconv.FormatInt(k, 10)
	val = make([]byte, len)
	for i := 0; i < len/8; i++ {
		binary.BigEndian.PutUint64(val[i*8:], rng.Uint64())
	}
	return
}

var errTestFail = errors.New("text")

type failReader struct{}

func (f failReader) Read(p []byte) (n int, err error) { return 0, errTestFail }
func (f failReader) Close() error                     { return nil }
