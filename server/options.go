package server

import (
	"github.com/gin-contrib/pprof"
	"github.com/kercylan98/minotaur/utils/log"
	"github.com/kercylan98/minotaur/utils/timer"
	"google.golang.org/grpc"
	"time"
)

const (
	// WebsocketMessageTypeText 表示文本数据消息。文本消息负载被解释为 UTF-8 编码的文本数据
	WebsocketMessageTypeText = 1
	// WebsocketMessageTypeBinary 表示二进制数据消息
	WebsocketMessageTypeBinary = 2
	// WebsocketMessageTypeClose 表示关闭控制消息。可选消息负载包含数字代码和文本。使用 FormatCloseMessage 函数来格式化关闭消息负载
	WebsocketMessageTypeClose = 8
	// WebsocketMessageTypePing 表示 ping 控制消息。可选的消息负载是 UTF-8 编码的文本
	WebsocketMessageTypePing = 9
	// WebsocketMessageTypePong 表示一个 pong 控制消息。可选的消息负载是 UTF-8 编码的文本
	WebsocketMessageTypePong = 10
)

type Option func(srv *Server)
type option struct {
	disableAnts  bool // 是否禁用协程池
	antsPoolSize int  // 协程池大小
}

type runtime struct {
	deadlockDetect            time.Duration // 是否开启死锁检测
	supportMessageTypes       map[int]bool  // websocket模式下支持的消息类型
	certFile, keyFile         string        // TLS文件
	messagePoolSize           int           // 消息池大小
	ticker                    *timer.Ticker // 定时器
	tickerAutonomy            bool          // 定时器是否独立运行
	connTickerSize            int           // 连接定时器大小
	websocketReadDeadline     time.Duration // websocket连接超时时间
	websocketCompression      int           // websocket压缩等级
	websocketWriteCompression bool          // websocket写入压缩
	limitLife                 time.Duration // 限制最大生命周期
	packetWarnSize            int           // 数据包大小警告
}

// WithPacketWarnSize 通过数据包大小警告的方式创建服务器，当数据包大小超过指定大小时，将会输出 WARN 类型的日志
//   - 默认值为 DefaultPacketWarnSize
//   - 当 size <= 0 时，表示不设置警告
func WithPacketWarnSize(size int) Option {
	return func(srv *Server) {
		if size <= 0 {
			srv.packetWarnSize = 0
			log.Info("WithPacketWarnSize", log.String("State", "Ignore"), log.String("Reason", "size <= 0"))
			return
		}
		srv.packetWarnSize = size
	}
}

// WithLimitLife 通过限制最大生命周期的方式创建服务器
//   - 通常用于测试服务器，服务器将在到达最大生命周期时自动关闭
func WithLimitLife(t time.Duration) Option {
	return func(srv *Server) {
		srv.limitLife = t
	}
}

// WithWebsocketWriteCompression 通过数据写入压缩的方式创建Websocket服务器
//   - 默认不开启数据压缩
func WithWebsocketWriteCompression() Option {
	return func(srv *Server) {
		if srv.network != NetworkWebsocket {
			return
		}
		srv.websocketWriteCompression = true
	}
}

// WithWebsocketCompression 通过数据压缩的方式创建Websocket服务器
//   - 默认不开启数据压缩
func WithWebsocketCompression(level int) Option {
	return func(srv *Server) {
		if srv.network != NetworkWebsocket {
			return
		}
		if !(-2 <= level && level <= 9) {
			panic("websocket: invalid compression level")
		}
		srv.websocketCompression = level
	}
}

// WithDeadlockDetect 通过死锁、死循环、永久阻塞检测的方式创建服务器
//   - 当检测到死锁、死循环、永久阻塞时，服务器将会生成 WARN 类型的日志，关键字为 "SuspectedDeadlock"
//   - 默认不开启死锁检测
func WithDeadlockDetect(t time.Duration) Option {
	return func(srv *Server) {
		if t > 0 {
			srv.deadlockDetect = t
			log.Info("DeadlockDetect", log.String("Time", t.String()))
		}
	}
}

// WithDisableAsyncMessage 通过禁用异步消息的方式创建服务器
func WithDisableAsyncMessage() Option {
	return func(srv *Server) {
		srv.disableAnts = true
	}
}

// WithAsyncPoolSize 通过指定异步消息池大小的方式创建服务器
//   - 当通过 WithDisableAsyncMessage 禁用异步消息时，此选项无效
//   - 默认值为 DefaultAsyncPoolSize
func WithAsyncPoolSize(size int) Option {
	return func(srv *Server) {
		srv.antsPoolSize = size
	}
}

// WithWebsocketReadDeadline 设置 Websocket 读取超时时间
//   - 默认： DefaultWebsocketReadDeadline
//   - 当 t <= 0 时，表示不设置超时时间
func WithWebsocketReadDeadline(t time.Duration) Option {
	return func(srv *Server) {
		if srv.network != NetworkWebsocket {
			return
		}
		srv.websocketReadDeadline = t
	}
}

// WithTicker 通过定时器创建服务器，为服务器添加定时器功能
//   - size：服务器定时器时间轮大小
//   - connSize：服务器连接定时器时间轮大小
//   - autonomy：定时器是否独立运行（独立运行的情况下不会作为服务器消息运行，会导致并发问题）
func WithTicker(size, connSize int, autonomy bool) Option {
	return func(srv *Server) {
		srv.connTickerSize = connSize
		srv.tickerAutonomy = autonomy
		if !autonomy {
			srv.ticker = timer.GetTicker(size)
		} else {
			srv.ticker = timer.GetTicker(size, timer.WithCaller(func(name string, caller func()) {
				srv.PushTickerMessage(name, caller)
			}))
		}
	}
}

// WithTLS 通过安全传输层协议TLS创建服务器
//   - 支持：Http、Websocket
func WithTLS(certFile, keyFile string) Option {
	return func(srv *Server) {
		switch srv.network {
		case NetworkHttp, NetworkWebsocket:
			srv.certFile = certFile
			srv.keyFile = keyFile
		}
	}
}

// WithGRPCServerOptions 通过GRPC的可选项创建GRPC服务器
func WithGRPCServerOptions(options ...grpc.ServerOption) Option {
	return func(srv *Server) {
		if srv.network != NetworkGRPC {
			return
		}
		srv.grpcServer = grpc.NewServer(options...)
	}
}

// WithWebsocketMessageType 设置仅支持特定类型的Websocket消息
func WithWebsocketMessageType(messageTypes ...int) Option {
	return func(srv *Server) {
		if srv.network != NetworkWebsocket {
			return
		}
		var supports = make(map[int]bool)
		for _, messageType := range messageTypes {
			switch messageType {
			case WebsocketMessageTypeText, WebsocketMessageTypeBinary, WebsocketMessageTypeClose, WebsocketMessageTypePing, WebsocketMessageTypePong:
				supports[messageType] = true
			}
		}
		srv.supportMessageTypes = supports
	}
}

// WithMessageBufferSize 通过特定的消息缓冲池大小运行服务器
//   - 默认大小为 DefaultMessageBufferSize
//   - 消息数量超出这个值的时候，消息处理将会造成更大的开销（频繁创建新的结构体），同时服务器将输出警告内容
func WithMessageBufferSize(size int) Option {
	return func(srv *Server) {
		if size <= 0 {
			size = 1024
		}
		srv.messagePoolSize = size
	}
}

// WithPProf 通过性能分析工具PProf创建服务器
func WithPProf(pattern ...string) Option {
	return func(srv *Server) {
		if srv.network != NetworkHttp {
			return
		}
		pprof.Register(srv.ginServer, pattern...)
	}
}
