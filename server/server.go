package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/kercylan98/minotaur/utils/concurrent"
	"github.com/kercylan98/minotaur/utils/log"
	"github.com/kercylan98/minotaur/utils/network"
	"github.com/kercylan98/minotaur/utils/str"
	"github.com/kercylan98/minotaur/utils/super"
	"github.com/kercylan98/minotaur/utils/timer"
	"github.com/panjf2000/ants/v2"
	"github.com/panjf2000/gnet"
	"github.com/panjf2000/gnet/pkg/logging"
	"github.com/xtaci/kcp-go/v5"
	"google.golang.org/grpc"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// New 根据特定网络类型创建一个服务器
func New(network Network, options ...Option) *Server {
	server := &Server{
		runtime: &runtime{
			messagePoolSize: DefaultMessageBufferSize,
		},
		option:       &option{},
		network:      network,
		online:       concurrent.NewBalanceMap[string, *Conn](),
		closeChannel: make(chan struct{}, 1),
		systemSignal: make(chan os.Signal, 1),
		ctx:          context.Background(),
		dispatchers:  make(map[string]*dispatcher),
	}
	server.event = newEvent(server)

	switch network {
	case NetworkHttp:
		server.ginServer = gin.New()
		server.httpServer = &http.Server{
			Handler: server.ginServer,
		}
	case NetworkGRPC:
		server.grpcServer = grpc.NewServer()
	case NetworkWebsocket:
		server.websocketReadDeadline = DefaultWebsocketReadDeadline
	}

	for _, option := range options {
		option(server)
	}

	if !server.disableAnts {
		if server.antsPoolSize <= 0 {
			server.antsPoolSize = DefaultAsyncPoolSize
		}
		var err error
		server.ants, err = ants.NewPool(server.antsPoolSize, ants.WithLogger(log.GetLogger()))
		if err != nil {
			panic(err)
		}
	}

	server.option = nil
	return server
}

// Server 网络服务器
type Server struct {
	*event                                                         // 事件
	*runtime                                                       // 运行时
	*option                                                        // 可选项
	network                  Network                               // 网络类型
	addr                     string                                // 侦听地址
	systemSignal             chan os.Signal                        // 系统信号
	online                   *concurrent.BalanceMap[string, *Conn] // 在线连接
	ginServer                *gin.Engine                           // HTTP模式下的路由器
	httpServer               *http.Server                          // HTTP模式下的服务器
	grpcServer               *grpc.Server                          // GRPC模式下的服务器
	gServer                  *gNet                                 // TCP或UDP模式下的服务器
	isRunning                bool                                  // 是否正在运行
	isShutdown               atomic.Bool                           // 是否已关闭
	closeChannel             chan struct{}                         // 关闭信号
	ants                     *ants.Pool                            // 协程池
	messagePool              *concurrent.Pool[*Message]            // 消息池
	messageLock              sync.RWMutex                          // 消息锁
	multiple                 *MultipleServer                       // 多服务器模式下的服务器
	multipleRuntimeErrorChan chan error                            // 多服务器模式下的运行时错误
	runMode                  RunMode                               // 运行模式
	messageCounter           atomic.Int64                          // 消息计数器
	ctx                      context.Context                       // 上下文
	dispatchers              map[string]*dispatcher                // 消息分发器
	dispatcherLock           sync.RWMutex                          // 消息分发器锁
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
func (slf *Server) Run(addr string) error {
	if slf.network == NetworkNone {
		addr = "-"
	}
	if slf.event == nil {
		return ErrConstructed
	}
	slf.event.check()
	slf.addr = addr
	var protoAddr = fmt.Sprintf("%s://%s", slf.network, slf.addr)
	var messageInitFinish = make(chan struct{}, 1)
	var connectionInitHandle = func(callback func()) {
		slf.messageLock.Lock()
		slf.messagePool = concurrent.NewPool[*Message](slf.messagePoolSize,
			func() *Message {
				return &Message{}
			},
			func(data *Message) {
				data.reset()
			},
		)
		slf.messageLock.Unlock()
		if slf.network != NetworkHttp && slf.network != NetworkWebsocket && slf.network != NetworkGRPC {
			slf.gServer = &gNet{Server: slf}
		}
		if callback != nil {
			go callback()
		}
		go func() {
			messageInitFinish <- struct{}{}
			d, _ := slf.useDispatcher(serverSystemDispatcher)
			d.start()
		}()
	}

	switch slf.network {
	case NetworkNone:
		go connectionInitHandle(func() {
			slf.isRunning = true
			slf.OnStartBeforeEvent()
		})
	case NetworkGRPC:
		listener, err := net.Listen(string(NetworkTcp), slf.addr)
		if err != nil {
			return err
		}
		go connectionInitHandle(nil)
		go func() {
			slf.isRunning = true
			slf.OnStartBeforeEvent()
			if err := slf.grpcServer.Serve(listener); err != nil {
				slf.isRunning = false
				slf.PushErrorMessage(err, MessageErrorActionShutdown)
			}
		}()
	case NetworkTcp, NetworkTcp4, NetworkTcp6, NetworkUdp, NetworkUdp4, NetworkUdp6, NetworkUnix:
		go connectionInitHandle(func() {
			slf.isRunning = true
			slf.OnStartBeforeEvent()
			if err := gnet.Serve(slf.gServer, protoAddr,
				gnet.WithLogger(log.GetLogger()),
				gnet.WithLogLevel(super.If(slf.runMode == RunModeProd, logging.ErrorLevel, logging.DebugLevel)),
				gnet.WithTicker(true),
				gnet.WithMulticore(true),
			); err != nil {
				slf.isRunning = false
				slf.PushErrorMessage(err, MessageErrorActionShutdown)
			}
		})
	case NetworkKcp:
		listener, err := kcp.ListenWithOptions(slf.addr, nil, 0, 0)
		if err != nil {
			return err
		}
		go connectionInitHandle(func() {
			slf.isRunning = true
			slf.OnStartBeforeEvent()
			for {
				session, err := listener.AcceptKCP()
				if err != nil {
					continue
				}

				conn := newKcpConn(slf, session)
				slf.OnConnectionOpenedEvent(conn)
				slf.OnConnectionOpenedAfterEvent(conn)

				go func(conn *Conn) {
					defer func() {
						if err := recover(); err != nil {
							e, ok := err.(error)
							if !ok {
								e = fmt.Errorf("%v", err)
							}
							conn.Close(e)
						}
					}()

					buf := make([]byte, 4096)
					for !conn.IsClosed() {
						n, err := conn.kcp.Read(buf)
						if err != nil {
							if conn.IsClosed() {
								break
							}
							panic(err)
						}
						slf.PushPacketMessage(conn, 0, buf[:n])
					}
				}(conn)
			}
		})
	case NetworkHttp:
		switch slf.runMode {
		case RunModeDev:
			gin.SetMode(gin.DebugMode)
		case RunModeTest:
			gin.SetMode(gin.TestMode)
		case RunModeProd:
			gin.SetMode(gin.ReleaseMode)
		}
		go func() {
			slf.isRunning = true
			slf.OnStartBeforeEvent()
			slf.httpServer.Addr = slf.addr
			go connectionInitHandle(nil)
			if len(slf.certFile)+len(slf.keyFile) > 0 {
				if err := slf.httpServer.ListenAndServeTLS(slf.certFile, slf.keyFile); err != nil {
					slf.isRunning = false
					slf.PushErrorMessage(err, MessageErrorActionShutdown)
				}
			} else {
				if err := slf.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slf.isRunning = false
					slf.PushErrorMessage(err, MessageErrorActionShutdown)
				}
			}

		}()
	case NetworkWebsocket:
		go connectionInitHandle(func() {
			var pattern string
			var index = strings.Index(addr, "/")
			if index == -1 {
				pattern = "/"
			} else {
				pattern = addr[index:]
				slf.addr = slf.addr[:index]
			}
			var upgrade = websocket.Upgrader{
				ReadBufferSize:  4096,
				WriteBufferSize: 4096,
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			}
			http.HandleFunc(pattern, func(writer http.ResponseWriter, request *http.Request) {
				ip := request.Header.Get("X-Real-IP")
				ws, err := upgrade.Upgrade(writer, request, nil)
				if err != nil {
					return
				}
				if len(ip) == 0 {
					addr := ws.RemoteAddr().String()
					if index := strings.LastIndex(addr, ":"); index != -1 {
						ip = addr[0:index]
					}
				}
				if slf.websocketCompression > 0 {
					_ = ws.SetCompressionLevel(slf.websocketCompression)
				}
				ws.EnableWriteCompression(slf.websocketWriteCompression)
				conn := newWebsocketConn(slf, ws, ip)
				conn.SetData(wsRequestKey, request)
				for k, v := range request.URL.Query() {
					if len(v) == 1 {
						conn.SetData(k, v[0])
					} else {
						conn.SetData(k, v)
					}
				}
				slf.OnConnectionOpenedEvent(conn)

				defer func() {
					if err := recover(); err != nil {
						e, ok := err.(error)
						if !ok {
							e = fmt.Errorf("%v", err)
						}
						conn.Close(e)
					}
				}()
				for !conn.IsClosed() {
					if slf.websocketReadDeadline > 0 {
						if err := ws.SetReadDeadline(time.Now().Add(slf.websocketReadDeadline)); err != nil {
							panic(err)
						}
					}
					messageType, packet, readErr := ws.ReadMessage()
					if readErr != nil {
						if conn.IsClosed() {
							break
						}
						panic(readErr)
					}
					if len(slf.supportMessageTypes) > 0 && !slf.supportMessageTypes[messageType] {
						panic(ErrWebsocketIllegalMessageType)
					}
					slf.PushPacketMessage(conn, messageType, packet)
				}
			})
			go func() {
				slf.isRunning = true
				slf.OnStartBeforeEvent()
				if len(slf.certFile)+len(slf.keyFile) > 0 {
					if err := http.ListenAndServeTLS(slf.addr, slf.certFile, slf.keyFile, nil); err != nil {
						slf.isRunning = false
						slf.PushErrorMessage(err, MessageErrorActionShutdown)
					}
				} else {
					if err := http.ListenAndServe(slf.addr, nil); err != nil {
						slf.isRunning = false
						slf.PushErrorMessage(err, MessageErrorActionShutdown)
					}
				}

			}()
		})
	default:
		return ErrCanNotSupportNetwork
	}

	<-messageInitFinish
	close(messageInitFinish)
	messageInitFinish = nil
	if slf.multiple == nil {
		ip, _ := network.IP()
		log.Info("Server", log.String(serverMark, "===================================================================="))
		log.Info("Server", log.String(serverMark, "RunningInfo"),
			log.Any("network", slf.network),
			log.String("ip", ip.String()),
			log.String("listen", slf.addr),
		)
		log.Error("Server", log.String(serverMark, "===================================================================="))
		slf.OnStartFinishEvent()
		time.Sleep(time.Second)
		if !slf.isShutdown.Load() {
			slf.OnMessageReadyEvent()
		}

		signal.Notify(slf.systemSignal, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-slf.systemSignal:
			slf.shutdown(nil)
		}

		select {
		case <-slf.closeChannel:
			close(slf.closeChannel)
		}
	} else {
		slf.OnStartFinishEvent()
		time.Sleep(time.Second)
		if !slf.isShutdown.Load() {
			slf.OnMessageReadyEvent()
		}
	}

	return nil
}

// RunNone 是 Run("") 的简写，仅适用于运行 NetworkNone 服务器
func (slf *Server) RunNone() error {
	return slf.Run(str.None)
}

// Context 获取服务器上下文
func (slf *Server) Context() context.Context {
	return slf.ctx
}

// TimeoutContext 获取服务器超时上下文，context.WithTimeout 的简写
func (slf *Server) TimeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(slf.ctx, timeout)
}

// GetOnlineCount 获取在线人数
func (slf *Server) GetOnlineCount() int {
	return slf.online.Size()
}

// GetOnline 获取在线连接
func (slf *Server) GetOnline(id string) *Conn {
	return slf.online.Get(id)
}

// GetOnlineAll 获取所有在线连接
func (slf *Server) GetOnlineAll() map[string]*Conn {
	return slf.online.Map()
}

// IsOnline 是否在线
func (slf *Server) IsOnline(id string) bool {
	return slf.online.Exist(id)
}

// CloseConn 关闭连接
func (slf *Server) CloseConn(id string) {
	if conn, exist := slf.online.GetExist(id); exist {
		conn.Close()
	}
}

// Ticker 获取服务器定时器
func (slf *Server) Ticker() *timer.Ticker {
	if slf.ticker == nil {
		panic(ErrNoSupportTicker)
	}
	return slf.ticker
}

// Shutdown 主动停止运行服务器
func (slf *Server) Shutdown() {
	slf.systemSignal <- syscall.SIGQUIT
}

// shutdown 停止运行服务器
func (slf *Server) shutdown(err error) {
	if err != nil {
		log.Error("Server", log.String("state", "shutdown"), log.Err(err))
	}
	slf.isShutdown.Store(true)
	for slf.messageCounter.Load() > 0 {
		log.Info("Server", log.Any("network", slf.network), log.String("listen", slf.addr),
			log.String("action", "shutdown"), log.String("state", "waiting"), log.Int64("message", slf.messageCounter.Load()))
		time.Sleep(time.Second)
	}
	if slf.multiple == nil {
		slf.OnStopEvent()
	}
	defer func() {
		if slf.multipleRuntimeErrorChan != nil {
			slf.multipleRuntimeErrorChan <- err
		}
	}()
	if slf.gServer != nil && slf.isRunning {
		if shutdownErr := gnet.Stop(context.Background(), fmt.Sprintf("%s://%s", slf.network, slf.addr)); err != nil {
			log.Error("Server", log.Err(shutdownErr))
		}
	}
	if slf.ticker != nil {
		slf.ticker.Release()
	}
	if slf.ants != nil {
		slf.ants.Release()
		slf.ants = nil
	}
	slf.dispatcherLock.Lock()
	for s, d := range slf.dispatchers {
		d.close()
		delete(slf.dispatchers, s)
	}
	slf.dispatcherLock.Unlock()
	if slf.grpcServer != nil && slf.isRunning {
		slf.grpcServer.GracefulStop()
	}
	if slf.httpServer != nil && slf.isRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if shutdownErr := slf.httpServer.Shutdown(ctx); shutdownErr != nil {
			log.Error("Server", log.Err(shutdownErr))
		}
	}

	if err != nil {
		if slf.multiple != nil {
			slf.multiple.RegExitEvent(func() {
				log.Panic("Server", log.Any("network", slf.network), log.String("listen", slf.addr),
					log.String("action", "shutdown"), log.String("state", "exception"), log.Err(err))
			})
			for i, server := range slf.multiple.servers {
				if server.addr == slf.addr {
					slf.multiple.servers = append(slf.multiple.servers[:i], slf.multiple.servers[i+1:]...)
					break
				}
			}
		} else {
			log.Panic("Server", log.Any("network", slf.network), log.String("listen", slf.addr),
				log.String("action", "shutdown"), log.String("state", "exception"), log.Err(err))
		}
	} else {
		log.Info("Server", log.Any("network", slf.network), log.String("listen", slf.addr),
			log.String("action", "shutdown"), log.String("state", "normal"))
	}
	slf.closeChannel <- struct{}{}
}

// GRPCServer 当网络类型为 NetworkGRPC 时将被允许获取 grpc 服务器，否则将会发生 panic
func (slf *Server) GRPCServer() *grpc.Server {
	if slf.grpcServer == nil {
		panic(ErrNetworkOnlySupportGRPC)
	}
	return slf.grpcServer
}

// HttpRouter 当网络类型为 NetworkHttp 时将被允许获取路由器进行路由注册，否则将会发生 panic
//   - 通过该函数注册的路由将无法在服务器关闭时正常等待请求结束
//
// Deprecated: 从 Minotaur 0.0.29 开始，由于设计原因已弃用，该函数将直接返回 *gin.Server 对象，导致无法正常的对请求结束时进行处理
func (slf *Server) HttpRouter() gin.IRouter {
	if slf.ginServer == nil {
		panic(ErrNetworkOnlySupportHttp)
	}
	return slf.ginServer
}

// HttpServer 替代 HttpRouter 的函数，返回一个 *Http[*HttpContext] 对象
//   - 通过该函数注册的路由将在服务器关闭时正常等待请求结束
//   - 如果需要自行包装 Context 对象，可以使用 NewHttpHandleWrapper 方法
func (slf *Server) HttpServer() *Http[*HttpContext] {
	if slf.ginServer == nil {
		panic(ErrNetworkOnlySupportHttp)
	}
	return NewHttpHandleWrapper(slf, func(ctx *gin.Context) *HttpContext {
		return NewHttpContext(ctx)
	})
}

// GetMessageCount 获取当前服务器中消息的数量
func (slf *Server) GetMessageCount() int64 {
	return slf.messageCounter.Load()
}

// useDispatcher 添加消息分发器
//   - 该函数在分发器不重复的情况下将创建分发器，当分发器已存在将直接返回
func (slf *Server) useDispatcher(name string) (*dispatcher, bool) {
	slf.dispatcherLock.Lock()
	d, exist := slf.dispatchers[name]
	if exist {
		slf.dispatcherLock.Unlock()
		return d, false
	}
	d = generateDispatcher(slf.dispatchMessage)
	slf.dispatchers[name] = d
	slf.dispatcherLock.Unlock()
	return d, true
}

// releaseDispatcher 关闭消息分发器
func (slf *Server) releaseDispatcher(name string) {
	slf.dispatcherLock.Lock()
	d, exist := slf.dispatchers[name]
	if exist {
		delete(slf.dispatchers, name)
		d.close()
	}
	slf.dispatcherLock.Unlock()
}

// pushMessage 向服务器中写入特定类型的消息，需严格遵守消息属性要求
func (slf *Server) pushMessage(message *Message) {
	if slf.messagePool.IsClose() || !slf.OnMessageExecBeforeEvent(message) {
		slf.messagePool.Release(message)
		return
	}
	var dispatcher *dispatcher
	switch message.t {
	case MessageTypePacket:
		if slf.shuntMatcher == nil {
			dispatcher, _ = slf.useDispatcher(serverSystemDispatcher)
			break
		}
		fallthrough
	case MessageTypeShuntTicker, MessageTypeShuntAsync, MessageTypeShuntAsyncCallback:
		var created bool
		dispatcher, created = slf.useDispatcher(slf.shuntMatcher(message.conn))
		if created {
			go dispatcher.start()
		}
	case MessageTypeSystem, MessageTypeAsync, MessageTypeAsyncCallback, MessageTypeError, MessageTypeTicker:
		dispatcher, _ = slf.useDispatcher(serverSystemDispatcher)
	}
	if dispatcher == nil {
		return
	}
	slf.messageCounter.Add(1)
	dispatcher.put(message)
}

func (slf *Server) low(message *Message, present time.Time, expect time.Duration, messageReplace ...string) {
	cost := time.Since(present)
	if cost > expect {
		if len(messageReplace) > 0 {
			for i, s := range messageReplace {
				message.marks = append(message.marks, log.String(fmt.Sprintf("Other-%d", i+1), s))
			}
		}
		log.Warn("Server", log.String("type", "low-message"), log.String("cost", cost.String()), log.String("message", message.String()), log.Stack("stack"))
		slf.OnMessageLowExecEvent(message, cost)
	}
}

// dispatchMessage 消息分发
func (slf *Server) dispatchMessage(msg *Message) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if slf.deadlockDetect > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), slf.deadlockDetect)
		go func(ctx context.Context, msg *Message) {
			select {
			case <-ctx.Done():
				if err := ctx.Err(); err == context.DeadlineExceeded {
					log.Warn("Server", log.String("MessageType", messageNames[msg.t]), log.String("Info", msg.String()), log.Any("SuspectedDeadlock", msg))
				}
			}
		}(ctx, msg)
	}

	present := time.Now()
	if msg.t != MessageTypeAsync {
		defer func(msg *Message) {
			if err := recover(); err != nil {
				stack := string(debug.Stack())
				log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.String("Info", msg.String()), log.Any("error", err), log.String("stack", stack))
				fmt.Println(stack)
				if e, ok := err.(error); ok {
					slf.OnMessageErrorEvent(msg, e)
				}
			}

			super.Handle(cancel)
			slf.low(msg, present, time.Millisecond*100)
			slf.messageCounter.Add(-1)

			if !slf.isShutdown.Load() {
				slf.messagePool.Release(msg)
			}

		}(msg)
	}

	switch msg.t {
	case MessageTypePacket:
		if !slf.OnConnectionPacketPreprocessEvent(msg.conn, msg.packet, func(newPacket []byte) { msg.packet = newPacket }) {
			slf.OnConnectionReceivePacketEvent(msg.conn, msg.packet)
		}
	case MessageTypeError:
		switch msg.errAction {
		case MessageErrorActionNone:
			log.Panic("Server", log.Err(msg.err))
		case MessageErrorActionShutdown:
			slf.shutdown(msg.err)
		default:
			log.Warn("Server", log.String("not support message error action", msg.errAction.String()))
		}
	case MessageTypeTicker, MessageTypeShuntTicker:
		msg.ordinaryHandler()
	case MessageTypeAsync, MessageTypeShuntAsync:
		if err := slf.ants.Submit(func() {
			defer func() {
				if err := recover(); err != nil {
					stack := string(debug.Stack())
					log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.Any("error", err), log.String("stack", stack))
					fmt.Println(stack)
					if e, ok := err.(error); ok {
						slf.OnMessageErrorEvent(msg, e)
					}
				}
				super.Handle(cancel)
				slf.low(msg, present, time.Second)
				slf.messageCounter.Add(-1)

				if !slf.isShutdown.Load() {
					slf.messagePool.Release(msg)
				}
			}()
			var err error
			if msg.exceptionHandler != nil {
				err = msg.exceptionHandler()
			}
			if msg.errHandler != nil {
				if msg.conn == nil {
					slf.PushAsyncCallbackMessage(err, msg.errHandler)
					return
				}
				slf.PushShuntAsyncCallbackMessage(msg.conn, err, msg.errHandler)
				return
			}
			if err != nil {
				log.Error("Server", log.String("MessageType", messageNames[msg.t]), log.Any("error", err), log.String("stack", string(debug.Stack())))
			}
		}); err != nil {
			panic(err)
		}
	case MessageTypeAsyncCallback, MessageTypeShuntAsyncCallback:
		msg.errHandler(msg.err)
	case MessageTypeSystem:
		msg.ordinaryHandler()
	default:
		log.Warn("Server", log.String("not support message type", msg.t.String()))
	}
}

// PushSystemMessage 向服务器中推送 MessageTypeSystem 消息
//   - 系统消息仅包含一个可执行函数，将在系统分发器中执行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushSystemMessage(handler func(), mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToSystemMessage(handler, mark...))
}

// PushAsyncMessage 向服务器中推送 MessageTypeAsync 消息
//   - 异步消息将在服务器的异步消息队列中进行处理，处理完成 caller 的阻塞操作后，将会通过系统消息执行 callback 函数
//   - callback 函数将在异步消息处理完成后进行调用，无论过程是否产生 err，都将被执行，允许为 nil
//   - 需要注意的是，为了避免并发问题，caller 函数请仅处理阻塞操作，其他操作应该在 callback 函数中进行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushAsyncMessage(caller func() error, callback func(err error), mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToAsyncMessage(caller, callback, mark...))
}

// PushAsyncCallbackMessage 向服务器中推送 MessageTypeAsyncCallback 消息
//   - 异步消息回调将会通过一个接收 error 的函数进行处理，该函数将在系统分发器中执行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushAsyncCallbackMessage(err error, callback func(err error), mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToAsyncCallbackMessage(err, callback, mark...))
}

// PushShuntAsyncMessage 向特定分发器中推送 MessageTypeAsync 消息，消息执行与 MessageTypeAsync 一致
//   - 需要注意的是，当未指定 WithShunt 时，将会通过 PushAsyncMessage 进行转发
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushShuntAsyncMessage(conn *Conn, caller func() error, callback func(err error), mark ...log.Field) {
	if slf.shuntMatcher == nil {
		slf.PushAsyncMessage(caller, callback)
		return
	}
	slf.pushMessage(slf.messagePool.Get().castToShuntAsyncMessage(conn, caller, callback, mark...))
}

// PushShuntAsyncCallbackMessage 向特定分发器中推送 MessageTypeAsyncCallback 消息，消息执行与 MessageTypeAsyncCallback 一致
//   - 需要注意的是，当未指定 WithShunt 时，将会通过 PushAsyncCallbackMessage 进行转发
func (slf *Server) PushShuntAsyncCallbackMessage(conn *Conn, err error, callback func(err error), mark ...log.Field) {
	if slf.shuntMatcher == nil {
		slf.PushAsyncCallbackMessage(err, callback)
		return
	}
	slf.pushMessage(slf.messagePool.Get().castToShuntAsyncCallbackMessage(conn, err, callback, mark...))
}

// PushPacketMessage 向服务器中推送 MessageTypePacket 消息
//   - 当存在 WithShunt 的选项时，将会根据选项中的 shuntMatcher 进行分发，否则将在系统分发器中处理消息
func (slf *Server) PushPacketMessage(conn *Conn, wst int, packet []byte, mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToPacketMessage(
		&Conn{ctx: context.WithValue(conn.ctx, contextKeyWST, wst), connection: conn.connection},
		packet,
	))
}

// PushTickerMessage 向服务器中推送 MessageTypeTicker 消息
//   - 通过该函数推送定时消息，当消息触发时将在系统分发器中处理消息
//   - 可通过 timer.Ticker 或第三方定时器将执行函数(caller)推送到该消息中进行处理，可有效的避免线程安全问题
//   - 参数 name 仅用作标识该定时器名称
//
// 定时消息执行不会有特殊的处理，仅标记为定时任务，也就是允许将各类函数通过该消息发送处理，但是并不建议
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushTickerMessage(name string, caller func(), mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToTickerMessage(name, caller, mark...))
}

// PushShuntTickerMessage 向特定分发器中推送 MessageTypeTicker 消息，消息执行与 MessageTypeTicker 一致
//   - 需要注意的是，当未指定 WithShunt 时，将会通过 PushTickerMessage 进行转发
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushShuntTickerMessage(conn *Conn, name string, caller func(), mark ...log.Field) {
	if slf.shuntMatcher == nil {
		slf.PushTickerMessage(name, caller)
		return
	}
	slf.pushMessage(slf.messagePool.Get().castToShuntTickerMessage(conn, name, caller, mark...))
}

// PushErrorMessage 向服务器中推送 MessageTypeError 消息
//   - 通过该函数推送错误消息，当消息触发时将在系统分发器中处理消息
//   - 参数 errAction 用于指定错误消息的处理方式，可选值为 MessageErrorActionNone 和 MessageErrorActionShutdown
//   - 参数 errAction 为 MessageErrorActionShutdown 时，将会停止服务器的运行
//   - mark 为可选的日志标记，当发生异常时，将会在日志中进行体现
func (slf *Server) PushErrorMessage(err error, errAction MessageErrorAction, mark ...log.Field) {
	slf.pushMessage(slf.messagePool.Get().castToErrorMessage(err, errAction, mark...))
}
