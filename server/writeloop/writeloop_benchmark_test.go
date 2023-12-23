package writeloop_test

import (
	"github.com/kercylan98/minotaur/server/writeloop"
	"testing"
)

func BenchmarkWriteLoop_Put(b *testing.B) {
	wl := writeloop.NewWriteLoop(wp, func(message *Message) error {
		return nil
	}, nil)

	defer func() {
		wl.Close()
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			wl.Put(wp.Get())
		}
	})
	b.StopTimer()

}
