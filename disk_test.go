package immcache

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"testing"

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

var errTestFail = errors.New("text")

type failReader struct{}

func (f failReader) Read(p []byte) (n int, err error) { return 0, errTestFail }
func (f failReader) Close() error                     { return nil }
