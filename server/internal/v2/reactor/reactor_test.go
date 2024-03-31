package reactor_test

import (
	"github.com/kercylan98/minotaur/server/internal/v2/reactor"
	"github.com/kercylan98/minotaur/utils/random"
	"github.com/kercylan98/minotaur/utils/times"
	"testing"
	"time"
)

func BenchmarkReactor_Dispatch(b *testing.B) {
	var r = reactor.NewReactor(1024*16, 1024, func(msg func()) {
		msg()
	}, func(msg func(), err error) {
		b.Error(err)
	}).SetDebug(false)

	go r.Run()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := r.Dispatch(random.HostName(), func() {
			}); err != nil {
				return
			}
		}
	})
}

func TestReactor_Dispatch(t *testing.T) {
	var r = reactor.NewReactor(1024*16, 1024, func(msg func()) {
		msg()
	}, func(msg func(), err error) {
		t.Error(err)
	}).SetDebug(true)

	go r.Run()

	for i := 0; i < 10000; i++ {
		go func() {
			id := random.HostName()
			for {
				time.Sleep(time.Millisecond * 20)
				if err := r.Dispatch(id, func() {

				}); err != nil {
					return
				}
			}
		}()
	}

	time.Sleep(times.Second)
	r.Close()
}
