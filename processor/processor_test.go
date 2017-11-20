package processor

import (
	"bytes"
	"github.com/aldor007/mort/config"
	"github.com/aldor007/mort/lock"
	"github.com/aldor007/mort/object"
	"github.com/aldor007/mort/throttler"
	"io/ioutil"
	"net/http"
	"testing"
)

func BenchmarkNewRequestProcessorMemoryLock(b *testing.B) {
	benchmarks := []struct {
		name       string
		url        string
		filePath   string
		configPath string
	}{
		{"Process small image, small result", "http://mort/local/small.jpg-small", "./tests/benchmark/local/small.jpg", "./tests/benchmark/small.yml"},
		{"Process large image, small result", "http://mort/local/large.jpeg-small", "./tests/benchmark/local/large.jpeg", "./tests/benchmark/small.yml"},
	}

	for _, bm := range benchmarks {
		data, err := ioutil.ReadFile(bm.filePath)
		if err != nil {
			panic(err)
		}
		req, _ := http.NewRequest("GET", bm.url, ioutil.NopCloser(bytes.NewReader(data)))

		config := config.Config{}
		err = config.Load(bm.configPath)
		if err != nil {
			panic(err)
		}

		obj, _ := object.NewFileObject(req.URL, &config)
		rp := NewRequestProcessor(3, lock.NewMemoryLock(), throttler.NewBucketThrottler(10))
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res := rp.Process(req, obj)
				if res.StatusCode != 200 {
					b.Fatalf("Invalid response sc %s test name %s", res.StatusCode, bm.name)
				}
			}
		})
	}

}

func BenchmarkNewRequestProcessorNopLock(b *testing.B) {
	benchmarks := []struct {
		name       string
		url        string
		filePath   string
		configPath string
	}{
		{"Process small image, small result", "http://mort/local/small.jpg-small", "./tests/benchmark/local/small.jpg", "./tests/benchmark/small.yml"},
		{"Process large image, small result", "http://mort/local/large.jpeg-small", "./tests/benchmark/local/large.jpeg", "./tests/benchmark/small.yml"},
	}

	for _, bm := range benchmarks {
		data, err := ioutil.ReadFile(bm.filePath)
		if err != nil {
			panic(err)
		}
		req, _ := http.NewRequest("GET", bm.url, ioutil.NopCloser(bytes.NewReader(data)))

		config := config.Config{}
		err = config.Load(bm.configPath)
		if err != nil {
			panic(err)
		}

		obj, _ := object.NewFileObject(req.URL, &config)
		rp := NewRequestProcessor(3, lock.NewNopLock(), throttler.NewBucketThrottler(10))
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res := rp.Process(req, obj)
				if res.StatusCode != 200 {
					b.Fatalf("Invalid response sc %s test name %s", res.StatusCode, bm.name)
				}
			}
		})
	}

}
