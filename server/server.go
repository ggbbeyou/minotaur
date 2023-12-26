package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/alphadose/haxmap"
	"github.com/gin-gonic/gin"
	"github.com/kercylan98/minotaur/server/internal/logger"
	"github.com/kercylan98/minotaur/utils/concurrent"
	"github.com/kercylan98/minotaur/utils/log"
	"github.com/kercylan98/minotaur/utils/network"
	"github.com/kercylan98/minotaur/utils/str"
	"github.com/kercylan98/minotaur/utils/super"
	"github.com/kercylan98/minotaur/utils/timer"
	"github.com/panjf2000/ants/v2"
	"github.com/panjf2000/gnet"
	"github.com/xtaci/kcp-go/v5"
	"google.golang.org/grpc"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// New 根据特定网络类型创建一个服务器
func New(network Network, options ...Option) *Server {
	network.check()
	server := &Server{
		runtime: &runtime{
			packetWarnSize:       DefaultPacketWarnSize,
			dispatcherBufferSize: DefaultDispatcherBufferSize,
			connWriteBufferSize:  DefaultConnWriteBufferSize,
		},
		option:           &option{},
		network:          network,
		online:           haxmap.New[string, *Conn](),
		closeChannel:     make(chan struct{}, 1),
		systemSignal:     make(chan os.Signal, 1),
		dispatchers:      make(map[string]*dispatcher),
		dispatcherMember: map[string]map[string]*Conn{},
		currDispatcher:   map[string]*dispatcher{},
	}
	server.ctx, server.cancel = context.WithCancel(context.Background())
	server.event = newEvent(server)

	network.preprocessing(server)
	for _, option := range options {
		option(server)
	}
	if !server.disableAnts {
		if server.antsPoolSize <= 0 {
			server.antsPoolSize = DefaultAsyncPoolSize
		}
		var err error
		server.ants, err = ants.NewPool(server.antsPoolSize, ants.WithLogger(new(logger.Ants)))
		if err != nil {
			panic(err)
		}
	}
	server.option = nil
	return server
}

// Server 网络服务器
type Server struct {
	*event                                               // 事件
	*runtime                                             // 运行时
	*option                                              // 可选项
	ginServer                *gin.Engine                 // HTTP模式下的路由器
	httpServer               *http.Server                // HTTP模式下的服务器
	grpcServer               *grpc.Server                // GRPC模式下的服务器
	gServer                  *gNet                       // TCP或UDP模式下的服务器
	multiple                 *MultipleServer             // 多服务器模式下的服务器
	ants                     *ants.Pool                  // 协程池
	messagePool              *concurrent.Pool[*Message]  // 消息池
	ctx                      context.Context             // 上下文
	cancel                   context.CancelFunc          // 停止上下文
	online                   *haxmap.Map[string, *Conn]  // 在线连接
	systemDispatcher         *dispatcher                 // 系统消息分发器
	systemSignal             chan os.Signal              // 系统信号
	closeChannel             chan struct{}               // 关闭信号
	multipleRuntimeErrorChan chan error                  // 多服务器模式下的运行时错误
	dispatchers              map[string]*dispatcher      // 消息分发器集合
	dispatcherMember         map[string]map[string]*Conn // 消息分发器包含的连接
	currDispatcher           map[string]*dispatcher      // 当前连接所处消息分发器
	dispatcherLock           sync.RWMutex                // 消息分发器锁
	messageCounter           atomic.Int64                // 消息计数器
	addr                     string                      // 侦听地址
	network                  Network                     // 网络类型
	closed                   uint32                      // 服务器是否已关闭
	services                 []func()                    // 服务
}

// preCheckAndAdaptation 预检查及适配
func (srv *Server) preCheckAndAdaptation(addr string) (startState <-chan error, err error) {
	if srv.event == nil {
		return nil, ErrConstructed
	}
	srv.addr = addr
	if srv.multiple == nil && srv.network != NetworkKcp {
		kcp.SystemTimedSched.Close()
	}

	return srv.network.adaptation(srv), nil
}

// Run 使用特定地址运行服务器
//   - server.NetworkTcp (addr:":8888")
//   - server.NetworkTcp4 (addr:":8888")
//   - server.NetworkTcp6 (addr:":8888")
//   - server.NetworkUdp (addr:":8888")
//   - server.NetworkUdp4 (addr:":8888")
//   - server.NetworkUdp6 (addr:":8888")
//   - server.NetworkUnix (addr:"socketPath")
//   - server.NetworkHttp (addr:":8888")
//   - server.NetworkWebsocket (addr:":8888/ws")
//   - server.NetworkKcp (addr:":8888")
//   - server.NetworkNone (addr:"")
func (srv *Server) Run(addr string) (err error) {
	var startState <-chan error
	if startState, err = srv.preCheckAndAdaptation(addr); err != nil {
		return err
	}
	onServicesInit(srv)
	onMessageSystemInit(srv)
	if srv.multiple == nil {
		showServersInfo(serverMark, srv)
	}
	if err = <-startState; err != nil {
		return err
	}
	srv.OnStartFinishEvent()

	if srv.multiple == nil {
		signal.Notify(srv.systemSignal, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-srv.systemSignal:
			srv.shutdown(nil)
		}

		select {
		case <-srv.closeChannel:
			close(srv.closeChannel)
		}
	}

	return nil
}

// IsSocket 是否是 Socket 模式
func (srv *Server) IsSocket() bool {
	return srv.network == NetworkTcp || srv.network == NetworkTcp4 || srv.network == NetworkTcp6 ||
		srv.network == NetworkUdp || srv.network == NetworkUdp4 || srv.network == NetworkUdp6 ||
		srv.network == NetworkUnix || srv.network == NetworkKcp || srv.network == NetworkWebsocket
}

// RunNone 是 Run("") 的简写，仅适用于运行 NetworkNone 服务器
func (srv *Server) RunNone() error {
	return srv.Run(str.None)
}

// Context 获取服务器上下文
func (srv *Server) Context() context.Context {
	return srv.ctx
}

// TimeoutContext 获取服务器超时上下文，context.WithTimeout 的简写
func (srv *Server) TimeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(srv.ctx, timeout)
}

// GetOnlineCount 获取在线人数
func (srv *Server) GetOnlineCount() int {
	return int(srv.online.Len())
}

// GetOnlineBotCount 获取在线机器人数量
func (srv *Server) GetOnlineBotCount() int {
	var count int
	srv.online.ForEach(func(id string, conn *Conn) bool {
		if conn.IsBot() {
			count++
		}
		return true
	})
	return count
}

// GetOnline 获取在线连接
func (srv *Server) GetOnline(id string) *Conn {
	c, _ := srv.online.Get(id)
	return c
}

// GetOnlineAll 获取所有在线连接
func (srv *Server) GetOnlineAll() map[string]*Conn {
	var m = map[string]*Conn{}
	srv.online.ForEach(func(id string, conn *Conn) bool {
		m[id] = conn
		return true
	})
	return m
}

// IsOnline 是否在线
func (srv *Server) IsOnline(id string) bool {
	_, exist := srv.online.Get(id)
	return exist
}

// CloseConn 关闭连接
func (srv *Server) CloseConn(id string) {
	if conn, exist := srv.online.Get(id); exist {
		conn.Close()
	}
}

// Ticker 获取服务器定时器
func (srv *Server) Ticker() *timer.Ticker {
	if srv.ticker == nil {
		panic(ErrNoSupportTicker)
	}
	return srv.ticker
}

// Shutdown 主动停止运行服务器
func (srv *Server) Shutdown() {
	srv.systemSignal <- syscall.SIGQUIT
}

// shutdown 停止运行服务器
func (srv *Server) shutdown(err error) {
	if !atomic.CompareAndSwapUint32(&srv.closed, 0, 1) {
		return
	}
	if err != nil {
		log.Error("Server", log.String("state", "shutdown"), log.Err(err))
	}

	for srv.messageCounter.Load() > 0 {
		log.Info("Server", log.Any("network", srv.network), log.String("listen", srv.addr),
			log.String("action", "shutdown"), log.String("state", "waiting"), log.Int64("message", srv.messageCounter.Load()))
		time.Sleep(time.Second)
	}
	if srv.multiple == nil {
		srv.OnStopEvent()
	}
	defer func() {
		if srv.multipleRuntimeErrorChan != nil {
			srv.multipleRuntimeErrorChan <- err
		}
	}()
	srv.cancel()
	if srv.gServer != nil {
		if shutdownErr := gnet.Stop(context.Background(), fmt.Sprintf("%s://%s", srv.network, srv.addr)); err != nil {
			log.Error("Server", log.Err(shutdownErr))
		}
	}
	if srv.tickerPool != nil {
		srv.tickerPool.Release()
	}
	if srv.ticker != nil {
		srv.ticker.Release()
	}
	if srv.ants != nil {
		srv.ants.Release()
		srv.ants = nil
	}
	srv.dispatcherLock.Lock()
	for s, d := range srv.dispatchers {
		srv.OnShuntChannelClosedEvent(d.name)
		d.close()
		delete(srv.dispatchers, s)
	}
	srv.dispatcherLock.Unlock()
	if srv.grpcServer != nil {
		srv.grpcServer.GracefulStop()
	}
	if srv.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if shutdownErr := srv.httpServer.Shutdown(ctx); shutdownErr != nil {
			log.Error("Server", log.Err(shutdownErr))
		}
	}

	if err != nil {
		if srv.multiple != nil {
			srv.multiple.RegExitEvent(func() {
				log.Panic("Server", log.Any("network", srv.network), log.String("listen", srv.addr),
					log.String("action", "shutdown"), log.String("state", "exception"), log.Err(err))
			})
			for i, server := range srv.multiple.servers {
				if server.addr == srv.addr {
					srv.multiple.servers = append(srv.multiple.servers[:i], srv.multiple.servers[i+1:]...)
					break
				}
			}
		} else {
			log.Panic("Server", log.Any("network", srv.network), log.String("listen", srv.addr),
				log.String("action", "shutdown"), log.String("state", "exception"), log.Err(err))
		}
	} else {
		log.Info("Server", log.Any("network", srv.network), log.String("listen", srv.addr),
			log.String("action", "shutdown"), log.String("state", "normal"))
	}
	srv.closeChannel <- struct{}{}
}

// GRPCServer 当网络类型为 NetworkGRPC 时将被允许获取 grpc 服务器，否则将会发生 panic
func (srv *Server) GRPCServer() *grpc.Server {
	if srv.grpcServer == nil {
		panic(ErrNetworkOnlySupportGRPC)
	}
	return srv.grpcServer
}

// HttpRouter 当网络类型为 NetworkHttp 时将被允许获取路由器进行路由注册，否则将会发生 panic
//   - 通过该函数注册的路由将无法在服务器关闭时正常等待请求结束
//
// Deprecated: 从 Minotaur 0.0.29 开始，由于设计原因已弃用，该函数将直接返回 *gin.Server 对象，导致无法正常的对请求结束时进行处理
func (srv *Server) HttpRouter() gin.IRouter {
	if srv.ginServer == nil {
		panic(ErrNetworkOnlySupportHttp)
	}
	return srv.ginServer
}

// HttpServer 替代 HttpRouter 的函数，返回一个 *Http[*HttpContext] 对象
//   - 通过该函数注册的路由将在服务器关闭时正常等待请求结束
//   - 如果需要自行包装 Context 对象，可以使用 NewHttpHandleWrapper 方法
func (srv *Server) HttpServer() *Http[*HttpContext] {
	if srv.ginServer == nil {
		panic(ErrNetworkOnlySupportHttp)
	}
	return NewHttpHandleWrapper(srv, func(ctx *gin.Context) *HttpContext {
		return NewHttpContext(ctx)
	})
}

// GetMessageCount 获取当前服务器中消息的数量
func (srv *Server) GetMessageCount() int64 {
	return srv.messageCounter.Load()
}

// UseShunt 切换连接所使用的消息分流渠道，当分流渠道 name 不存在时将会创建一个新的分流渠道，否则将会加入已存在的分流渠道
//   - 默认情况下，所有连接都使用系统通道进行消息分发，当指定消息分流渠道时，将会使用指定的消息分流渠道进行消息分发
//   - 在使用 WithDisableAutomaticReleaseShunt 创建服务器后，必须始终在连接不再使用后主动通过 ReleaseShunt 释放消息分流渠道，否则将造成内存泄漏
func (srv *Server) UseShunt(conn *Conn, name string) {
	srv.dispatcherLock.Lock()
	defer srv.dispatcherLock.Unlock()
	d, exist := srv.dispatchers[name]
	if !exist {
		d = generateDispatcher(srv.dispatcherBufferSize, name, srv.dispatchMessage)
		srv.OnShuntChannelCreatedEvent(d.name)
		go d.start()
		srv.dispatchers[name] = d
	}

	curr, exist := srv.currDispatcher[conn.GetID()]
	if exist {
		if curr.name == name {
			return
		}

		delete(srv.dispatcherMember[curr.name], conn.GetID())
		if curr.name != serverSystemDispatcher && len(srv.dispatcherMember[curr.name]) == 0 {
			delete(srv.dispatchers, curr.name)
			curr.transfer(d)
			srv.OnShuntChannelClosedEvent(d.name)
			curr.close()
		}
	}
	srv.currDispatcher[conn.GetID()] = d

	member, exist := srv.dispatcherMember[name]
	if !exist {
		member = map[string]*Conn{}
		srv.dispatcherMember[name] = member
	}

	member[conn.GetID()] = conn
}

// HasShunt 检查特定消息分流渠道是否存在
func (srv *Server) HasShunt(name string) bool {
	srv.dispatcherLock.RLock()
	defer srv.dispatcherLock.RUnlock()
	_, exist := srv.dispatchers[name]
	return exist
}

// GetConnCurrShunt 获取连接当前所使用的消息分流渠道
func (srv *Server) GetConnCurrShunt(conn *Conn) string {
	srv.dispatcherLock.RLock()
	defer srv.dispatcherLock.RUnlock()
	d, exist := srv.currDispatcher[conn.GetID()]
	if exist {
		return d.name
	}
	return serverSystemDispatcher
}

// GetShuntNum 获取消息分流渠道数量
func (srv *Server) GetShuntNum() int {
	srv.dispatcherLock.RLock()
	defer srv.dispatcherLock.RUnlock()
	return len(srv.dispatchers)
}

// getConnDispatcher 获取连接所使用的消息分发器
func (srv *Server) getConnDispatcher(conn *Conn) *dispatcher {
	if conn == nil {
		return srv.systemDispatcher
	}
	srv.dispatcherLock.RLock()
	defer srv.dispatcherLock.RUnlock()
	d, exist := srv.currDispatcher[conn.GetID()]
	if exist {
		return d
	}
	return srv.systemDispatcher
}

// ReleaseShunt 释放分流渠道中的连接，当分流渠道中不再存在连接时将会自动释放分流渠道
//   - 在未使用 WithDisableAutomaticReleaseShunt 选项时，当连接关闭时将会自动释放分流渠道中连接的资源占用
//   - 若执行过程中连接正在使用，将会切换至系统通道
func (srv *Server) ReleaseShunt(conn *Conn) {
	srv.releaseDispatcher(conn)
}

// releaseDispatcher 关闭消息分发器
func (srv *Server) releaseDispatcher(conn *Conn) {
	if conn == nil {
		return
	}
	cid := conn.GetID()
	srv.dispatcherLock.Lock()
	defer srv.dispatcherLock.Unlock()
	d, exist := srv.currDispatcher[cid]
	if exist && d.name != serverSystemDispatcher {
		delete(srv.dispatcherMember[d.name], cid)
		if len(srv.dispatcherMember[d.name]) == 0 {
			srv.OnShuntChannelClosedEvent(d.name)
			d.close()
			delete(srv.dispatchers, d.name)
		}
		delete(srv.currDispatcher, cid)
	}
}

// pushMessage 向服务器中写入特定类型的消息，需严格遵守消息属性要求
func (srv *Server) pushMessage(message *Message) {
	if !srv.OnMessageExecBeforeEvent(message) {
		srv.messagePool.Release(message)
		return
	}
	var dispatcher *dispatcher
	switch message.t {
	case MessageTypePacket,
		MessageTypeShuntTicker, MessageTypeShuntAsync, MessageTypeShuntAsyncCallback,
		MessageTypeUniqueShuntAsync, MessageTypeUniqueShuntAsyncCallback,
		MessageTypeShunt:
		dispatcher = srv.getConnDispatcher(message.conn)
	case MessageTypeSystem, MessageTypeAsync, MessageTypeUniqueAsync, MessageTypeAsyncCallback, MessageTypeUniqueAsyncCallback, MessageTypeTicker:
		dispatcher = srv.systemDispatcher
	}
	if dispatcher == nil {
		return
	}
	if (message.t == MessageTypeUniqueShuntAsync || message.t == MessageTypeUniqueAsync) && dispatcher.unique(message.name) {
		srv.messagePool.Release(message)
		return
	}
	srv.hitMessageStatistics()
	dispatcher.put(message)
}

func (srv *Server) low(message *Message, present time.Time, expect time.Duration, messageReplace ...string) {
	cost := time.Since(present)
	if cost > expect {
		if len(messageReplace) > 0 {
			for i, s := range messageReplace {
				message.marks = append(message.marks, log.String(fmt.Sprintf("Other-%d", i+1), s))
			}
		}
		var fields = make([]log.Field, 0, len(message.marks)+5)
		if message.conn != nil {
			fields = append(fields, log.String("shunt", srv.GetConnCurrShunt(message.conn)))
		}
		fields = append(fields, log.String("type", messageNames[message.t]), log.String("cost", cost.String()), log.String("message", message.String()))
		fields = append(fields, message.marks...)
		//fields = append(fields, log.Stack("stack"))
		log.Warn("ServerLowMessage", fields...)
		srv.OnMessageLowExecEvent(message, cost)
	}
}

// dispatchMessage 消息分发
func (srv *Server) dispatchMessage(dispatcher *dispatcher, msg *Message) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if srv.deadlockDetect > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), srv.deadlockDetect)
		go func(ctx context.Context, msg *Message) {
			select {
			case <-ctx.Done():
				if err := ctx.Err(); errors.Is(err, context.DeadlineExceeded) {
					log.Warn("Server", log.String("MessageType", messageNames[msg.t]), log.String("Info", msg.String()), log.Any("SuspectedDeadlock", msg))
					srv.OnDeadlockDetectEvent(msg)
				}
			}
		}(ctx, msg)
	}

	present := time.Now()
	if msg.t != MessageTypeAsync && msg.t != MessageTypeUniqueAsync && msg.t != MessageTypeShuntAsync && msg.t != MessageTypeUniqueShuntAsync {
		defer func(msg *Message) {
			super.Handle(cancel)
			if err := super.RecoverTransform(recover()); err != nil {
				stack := string(debug.Stack())
				log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.String("Info", msg.String()), log.Any("error", err), log.String("stack", stack))
				fmt.Println(stack)
				srv.OnMessageErrorEvent(msg, err)
			}
			if msg.t == MessageTypeUniqueAsyncCallback || msg.t == MessageTypeUniqueShuntAsyncCallback {
				dispatcher.antiUnique(msg.name)
			}

			srv.low(msg, present, time.Millisecond*100)
			srv.messageCounter.Add(-1)

			if atomic.CompareAndSwapUint32(&srv.closed, 0, 0) {
				srv.messagePool.Release(msg)
			}
		}(msg)
	} else {
		if cancel != nil {
			defer cancel()
		}
	}

	switch msg.t {
	case MessageTypePacket:
		if !srv.OnConnectionPacketPreprocessEvent(msg.conn, msg.packet, func(newPacket []byte) {
			msg.packet = newPacket
		}) {
			srv.OnConnectionReceivePacketEvent(msg.conn, msg.packet)
		}
	case MessageTypeTicker, MessageTypeShuntTicker:
		msg.ordinaryHandler()
	case MessageTypeAsync, MessageTypeShuntAsync, MessageTypeUniqueAsync, MessageTypeUniqueShuntAsync:
		if err := srv.ants.Submit(func() {
			defer func() {
				if err := super.RecoverTransform(recover()); err != nil {
					if msg.t == MessageTypeUniqueAsync || msg.t == MessageTypeUniqueShuntAsync {
						dispatcher.antiUnique(msg.name)
					}
					stack := string(debug.Stack())
					log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.Any("error", err), log.String("stack", stack))
					fmt.Println(stack)
					srv.OnMessageErrorEvent(msg, err)
				}
				super.Handle(cancel)
				srv.low(msg, present, time.Second)
				srv.messageCounter.Add(-1)

				if atomic.CompareAndSwapUint32(&srv.closed, 0, 0) {
					srv.messagePool.Release(msg)
				}
			}()
			var err error
			if msg.exceptionHandler != nil {
				err = msg.exceptionHandler()
			}
			if msg.errHandler != nil {
				if msg.conn == nil {
					if msg.t == MessageTypeUniqueAsync {
						srv.PushUniqueAsyncCallbackMessage(msg.name, err, msg.errHandler)
						return
					}
					srv.PushAsyncCallbackMessage(err, msg.errHandler)
					return
				}
				if msg.t == MessageTypeUniqueShuntAsync {
					srv.PushUniqueShuntAsyncCallbackMessage(msg.conn, msg.name, err, msg.errHandler)
					return
				}
				srv.PushShuntAsyncCallbackMessage(msg.conn, err, msg.errHandler)
				return
			}
			dispatcher.antiUnique(msg.name)
			if err != nil {
				log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.Any("error", err), log.String("stack", string(debug.Stack())))
			}
		}); err != nil {
			panic(err)
		}
	case MessageTypeAsyncCallback, MessageTypeShuntAsyncCallback, MessageTypeUniqueAsyncCallback, MessageTypeUniqueShuntAsyncCallback:
		msg.errHandler(msg.err)
	case MessageTypeSystem, MessageTypeShunt:
		msg.ordinaryHandler()
	default:
		log.Warn("Server", log.String("not support message type", msg.t.String()))
	}
}

// PushSystemMessage 向服务器中推送 MessageTypeSystem 消息
//   - 系统消息仅包含一个可执行函数，将在系统分发器中执行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushSystemMessage(handler func(), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToSystemMessage(handler, mark...))
}

// PushAsyncMessage 向服务器中推送 MessageTypeAsync 消息
//   - 异步消息将在服务器的异步消息队列中进行处理，处理完成 caller 的阻塞操作后，将会通过系统消息执行 callback 函数
//   - callback 函数将在异步消息处理完成后进行调用，无论过程是否产生 err，都将被执行，允许为 nil
//   - 需要注意的是，为了避免并发问题，caller 函数请仅处理阻塞操作，其他操作应该在 callback 函数中进行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushAsyncMessage(caller func() error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToAsyncMessage(caller, callback, mark...))
}

// PushAsyncCallbackMessage 向服务器中推送 MessageTypeAsyncCallback 消息
//   - 异步消息回调将会通过一个接收 error 的函数进行处理，该函数将在系统分发器中执行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushAsyncCallbackMessage(err error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToAsyncCallbackMessage(err, callback, mark...))
}

// PushShuntAsyncMessage 向特定分发器中推送 MessageTypeAsync 消息，消息执行与 MessageTypeAsync 一致
//   - 需要注意的是，当未指定 UseShunt 时，将会通过 PushAsyncMessage 进行转发
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushShuntAsyncMessage(conn *Conn, caller func() error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToShuntAsyncMessage(conn, caller, callback, mark...))
}

// PushShuntAsyncCallbackMessage 向特定分发器中推送 MessageTypeAsyncCallback 消息，消息执行与 MessageTypeAsyncCallback 一致
//   - 需要注意的是，当未指定 UseShunt 时，将会通过 PushAsyncCallbackMessage 进行转发
func (srv *Server) PushShuntAsyncCallbackMessage(conn *Conn, err error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToShuntAsyncCallbackMessage(conn, err, callback, mark...))
}

// PushPacketMessage 向服务器中推送 MessageTypePacket 消息
//   - 当存在 UseShunt 的选项时，将会根据选项中的 shuntMatcher 进行分发，否则将在系统分发器中处理消息
func (srv *Server) PushPacketMessage(conn *Conn, wst int, packet []byte, mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToPacketMessage(
		&Conn{wst: wst, connection: conn.connection},
		packet, mark...,
	))
}

// PushTickerMessage 向服务器中推送 MessageTypeTicker 消息
//   - 通过该函数推送定时消息，当消息触发时将在系统分发器中处理消息
//   - 可通过 timer.Ticker 或第三方定时器将执行函数(caller)推送到该消息中进行处理，可有效的避免线程安全问题
//   - 参数 name 仅用作标识该定时器名称
//
// 定时消息执行不会有特殊的处理，仅标记为定时任务，也就是允许将各类函数通过该消息发送处理，但是并不建议
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushTickerMessage(name string, caller func(), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToTickerMessage(name, caller, mark...))
}

// PushShuntTickerMessage 向特定分发器中推送 MessageTypeTicker 消息，消息执行与 MessageTypeTicker 一致
//   - 需要注意的是，当未指定 UseShunt 时，将会通过 PushTickerMessage 进行转发
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (srv *Server) PushShuntTickerMessage(conn *Conn, name string, caller func(), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToShuntTickerMessage(conn, name, caller, mark...))
}

// PushUniqueAsyncMessage 向服务器中推送 MessageTypeAsync 消息，消息执行与 MessageTypeAsync 一致
//   - 不同的是当上一个相同的 unique 消息未执行完成时，将会忽略该消息
func (srv *Server) PushUniqueAsyncMessage(unique string, caller func() error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToUniqueAsyncMessage(unique, caller, callback, mark...))
}

// PushUniqueAsyncCallbackMessage 向服务器中推送 MessageTypeAsyncCallback 消息，消息执行与 MessageTypeAsyncCallback 一致
func (srv *Server) PushUniqueAsyncCallbackMessage(unique string, err error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToUniqueAsyncCallbackMessage(unique, err, callback, mark...))
}

// PushUniqueShuntAsyncMessage 向特定分发器中推送 MessageTypeAsync 消息，消息执行与 MessageTypeAsync 一致
//   - 需要注意的是，当未指定 UseShunt 时，将会通过系统分流渠道进行转发
//   - 不同的是当上一个相同的 unique 消息未执行完成时，将会忽略该消息
func (srv *Server) PushUniqueShuntAsyncMessage(conn *Conn, unique string, caller func() error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToUniqueShuntAsyncMessage(conn, unique, caller, callback, mark...))
}

// PushUniqueShuntAsyncCallbackMessage 向特定分发器中推送 MessageTypeAsyncCallback 消息，消息执行与 MessageTypeAsyncCallback 一致
//   - 需要注意的是，当未指定 UseShunt 时，将会通过系统分流渠道进行转发
func (srv *Server) PushUniqueShuntAsyncCallbackMessage(conn *Conn, unique string, err error, callback func(err error), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToUniqueShuntAsyncCallbackMessage(conn, unique, err, callback, mark...))
}

// PushShuntMessage 向特定分发器中推送 MessageTypeShunt 消息，消息执行与 MessageTypeSystem 一致，不同的是将会在特定分发器中执行
func (srv *Server) PushShuntMessage(conn *Conn, caller func(), mark ...log.Field) {
	srv.pushMessage(srv.messagePool.Get().castToShuntMessage(conn, caller, mark...))
}

// startMessageStatistics 开始消息统计
func (srv *Server) startMessageStatistics() {
	if !srv.HasMessageStatistics() {
		return
	}
	srv.runtime.messageStatistics = append(srv.runtime.messageStatistics, new(atomic.Int64))
	ticker := time.NewTicker(srv.runtime.messageStatisticsDuration)
	go func(ctx context.Context, ticker *time.Ticker, r *runtime) {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.messageStatisticsLock.Lock()
				r.messageStatistics = append([]*atomic.Int64{new(atomic.Int64)}, r.messageStatistics...)
				if len(r.messageStatistics) > r.messageStatisticsLimit {
					r.messageStatistics = r.messageStatistics[:r.messageStatisticsLimit]
				}
				r.messageStatisticsLock.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}(srv.ctx, ticker, srv.runtime)
}

// hitMessageStatistics 命中消息统计
func (srv *Server) hitMessageStatistics() {
	srv.messageCounter.Add(1)
	if !srv.HasMessageStatistics() {
		return
	}
	srv.runtime.messageStatisticsLock.RLock()
	srv.runtime.messageStatistics[0].Add(1)
	srv.runtime.messageStatisticsLock.RUnlock()
}

// GetDurationMessageCount 获取当前 WithMessageStatistics 设置的 duration 期间的消息量
func (srv *Server) GetDurationMessageCount() int64 {
	return srv.GetDurationMessageCountByOffset(0)
}

// GetDurationMessageCountByOffset 获取特定偏移次数的 WithMessageStatistics 设置的 duration 期间的消息量
//   - 该值小于 0 时，将与 GetDurationMessageCount 无异，否则将返回 +n 个期间的消息量，例如 duration 为 1 分钟，limit 为 10，那么 offset 为 1 的情况下，获取的则是上一分钟消息量
func (srv *Server) GetDurationMessageCountByOffset(offset int) int64 {
	if !srv.HasMessageStatistics() {
		return 0
	}
	srv.runtime.messageStatisticsLock.Lock()
	if offset >= len(srv.runtime.messageStatistics)-1 {
		srv.runtime.messageStatisticsLock.Unlock()
		return 0
	}
	v := srv.runtime.messageStatistics[offset].Load()
	srv.runtime.messageStatisticsLock.Unlock()
	return v
}

// GetAllDurationMessageCount 获取所有 WithMessageStatistics 设置的 duration 期间的消息量
func (srv *Server) GetAllDurationMessageCount() []int64 {
	if !srv.HasMessageStatistics() {
		return nil
	}
	srv.runtime.messageStatisticsLock.Lock()
	var vs = make([]int64, len(srv.runtime.messageStatistics))
	for i, statistic := range srv.runtime.messageStatistics {
		vs[i] = statistic.Load()
	}
	srv.runtime.messageStatisticsLock.Unlock()
	return vs
}

// HasMessageStatistics 是否了开启消息统计
func (srv *Server) HasMessageStatistics() bool {
	return srv.runtime.messageStatisticsLock != nil
}

// showServersInfo 显示服务器信息
func showServersInfo(mark string, servers ...*Server) {
	var serverInfos = make([]func(), 0, len(servers))
	var ip, _ = network.IP()
	for _, srv := range servers {
		srv := srv
		serverInfos = append(serverInfos, func() {
			log.Info("Server", log.String(mark, "RunningInfo"), log.Any("network", srv.network), log.String("ip", ip.String()), log.String("listen", srv.addr))
		})
	}
	log.Info("Server", log.String(mark, "===================================================================="))
	for _, info := range serverInfos {
		info()
	}
	log.Info("Server", log.String(mark, "===================================================================="))
}

// onServicesInit 服务初始化
func onServicesInit(srv *Server) {
	for _, service := range srv.services {
		service()
	}
}

// onMessageSystemInit 消息系统初始化
func onMessageSystemInit(srv *Server) {
	srv.messagePool = concurrent.NewPool[Message](
		func() *Message {
			return &Message{}
		},
		func(data *Message) {
			data.reset()
		},
	)
	srv.startMessageStatistics()
	srv.systemDispatcher = generateDispatcher(srv.dispatcherBufferSize, serverSystemDispatcher, srv.dispatchMessage)
	go srv.systemDispatcher.start()
	srv.OnMessageReadyEvent()
}
