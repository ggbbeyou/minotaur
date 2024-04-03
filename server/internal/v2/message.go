package server

import (
	"github.com/kercylan98/minotaur/server/internal/v2/queue"
	"github.com/kercylan98/minotaur/server/internal/v2/reactor"
	"github.com/kercylan98/minotaur/utils/log/v2"
	"github.com/kercylan98/minotaur/utils/super"
	"runtime/debug"
)

type Message interface {
	Execute()
}

func SyncMessage(srv *server, handler func(srv *server)) Message {
	return &syncMessage{srv: srv, handler: handler}
}

type syncMessage struct {
	srv     *server
	handler func(srv *server)
}

func (s *syncMessage) Execute() {
	s.handler(s.srv)
}

func AsyncMessage(srv *server, ident string, handler func(srv *server) error, callback func(srv *server, err error)) Message {
	return &asyncMessage{
		ident:    ident,
		srv:      srv,
		handler:  handler,
		callback: callback,
	}
}

type asyncMessage struct {
	ident    string
	srv      *server
	handler  func(srv *server) error
	callback func(srv *server, err error)
}

func (s *asyncMessage) Execute() {
	var q *queue.Queue[int, string, Message]
	var dispatch = func(ident string, message Message, beforeHandler ...func(queue *queue.Queue[int, string, Message], msg Message)) {
		if ident == "" {
			_ = s.srv.reactor.DispatchWithSystem(message, beforeHandler...)
		} else {
			_ = s.srv.reactor.Dispatch(ident, message, beforeHandler...)
		}
	}

	dispatch(
		s.ident,
		SyncMessage(s.srv, func(srv *server) {
			_ = srv.ants.Submit(func() {
				defer func(srv *server, msg *asyncMessage) {
					if err := super.RecoverTransform(recover()); err != nil {
						if errHandler := srv.GetMessageErrorHandler(); errHandler != nil {
							errHandler(srv, msg, err)
						} else {
							srv.GetLogger().Error("Message", log.Err(err))
							debug.PrintStack()
						}
					}
				}(s.srv, s)

				err := s.handler(srv)
				var msg Message
				msg = SyncMessage(srv, func(srv *server) {
					defer func() {
						q.WaitAdd(s.ident, -1)
						if err := super.RecoverTransform(recover()); err != nil {
							if errHandler := srv.GetMessageErrorHandler(); errHandler != nil {
								errHandler(srv, msg, err)
							} else {
								srv.GetLogger().Error("Message", log.Err(err))
								debug.PrintStack()
							}
						}
					}()
					if s.callback != nil {
						s.callback(srv, err)
					}
				})
				dispatch(s.ident, msg)

			})
		}),
		func(queue *queue.Queue[int, string, Message], msg Message) {
			queue.WaitAdd(reactor.SysIdent, 1)
			q = queue
		},
	)
}
