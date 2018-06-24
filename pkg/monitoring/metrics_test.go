package monitoring

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestNopReporter(t *testing.T) {

	nop := NopReporter{}

	assert.NotPanics(t, func() {
		nop.Counter("a", 3.)
		nop.Gauge("b", 4.)
		nop.Histogram("c", 1.0)
		nop.Inc("d")
		t := nop.Timer("e")
		t.Done()
	})
}
