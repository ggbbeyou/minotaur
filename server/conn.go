package server

import (
	"github.com/gorilla/websocket"
	"github.com/kercylan98/minotaur/utils/concurrent"
	"github.com/kercylan98/minotaur/utils/super"
	"github.com/panjf2000/gnet"
	"github.com/xtaci/kcp-go/v5"
	"net"
	"strings"
	"sync"
	"time"
)

// newKcpConn 创建一个处理KCP的连接
func newKcpConn(server *Server, session *kcp.UDPSession) *Conn {
	c := &Conn{
		server:     server,
		remoteAddr: session.RemoteAddr(),
		ip:         session.RemoteAddr().String(),
		kcp:        session,
		data:       map[any]any{},
	}
	if index := strings.LastIndex(c.ip, ":"); index != -1 {
		c.ip = c.ip[0:index]
	}
	var wait = new(sync.WaitGroup)
	wait.Add(1)
	go c.writeLoop(wait)
	wait.Wait()
	return c
}

// newKcpConn 创建一个处理GNet的连接
func newGNetConn(server *Server, conn gnet.Conn) *Conn {
	c := &Conn{
		server:     server,
		remoteAddr: conn.RemoteAddr(),
		ip:         conn.RemoteAddr().String(),
		gn:         conn,
		data:       map[any]any{},
	}
	if index := strings.LastIndex(c.ip, ":"); index != -1 {
		c.ip = c.ip[0:index]
	}
	var wait = new(sync.WaitGroup)
	wait.Add(1)
	go c.writeLoop(wait)
	wait.Wait()
	return c
}

// newKcpConn 创建一个处理WebSocket的连接
func newWebsocketConn(server *Server, ws *websocket.Conn, ip string) *Conn {
	c := &Conn{
		server:     server,
		remoteAddr: ws.RemoteAddr(),
		ip:         ip,
		ws:         ws,
		data:       map[any]any{},
	}
	var wait = new(sync.WaitGroup)
	wait.Add(1)
	go c.writeLoop(wait)
	wait.Wait()
	return c
}

// newGatewayConn 创建一个处理网关消息的连接
func newGatewayConn(conn *Conn, connId string) *Conn {
	c := &Conn{
		server: conn.server,
		data:   map[any]any{},
	}
	c.gw = func(packet Packet) {
		var gp = GP{
			C:  connId,
			WT: packet.WebsocketType,
			D:  packet.Data,
			T:  time.Now().UnixNano(),
		}
		pd := super.MarshalJSON(&gp)
		packet.Data = append(pd, 0xff)
		conn.Write(packet)
	}
	return c
}

// NewEmptyConn 创建一个适用于测试的空连接
func NewEmptyConn(server *Server) *Conn {
	c := &Conn{
		server:     server,
		remoteAddr: &net.TCPAddr{},
		ip:         "0.0.0.0:0",
		data:       map[any]any{},
	}
	var wait = new(sync.WaitGroup)
	wait.Add(1)
	go c.writeLoop(wait)
	wait.Wait()
	return c
}

// Conn 服务器连接
type Conn struct {
	server     *Server
	remoteAddr net.Addr
	ip         string
	ws         *websocket.Conn
	gn         gnet.Conn
	kcp        *kcp.UDPSession
	gw         func(packet Packet)
	data       map[any]any
	mutex      sync.Mutex
	packetPool *concurrent.Pool[*connPacket]
	packets    []*connPacket
}

// IsEmpty 是否是空连接
func (slf *Conn) IsEmpty() bool {
	return slf.ws == nil && slf.gn == nil && slf.kcp == nil && slf.gw == nil
}

// Reuse 重用连接
//   - 重用连接时，会将当前连接的数据复制到新连接中
//   - 通常在于连接断开后，重新连接时使用
func (slf *Conn) Reuse(conn *Conn) {
	slf.mutex.Lock()
	conn.mutex.Lock()
	defer func() {
		slf.mutex.Unlock()
		conn.mutex.Unlock()
	}()
	slf.Close()
	slf.remoteAddr = conn.remoteAddr
	slf.ip = conn.ip
	slf.ws = conn.ws
	slf.gn = conn.gn
	slf.kcp = conn.kcp
	slf.data = conn.data
	slf.packetPool = conn.packetPool
	slf.packets = conn.packets
}

// RemoteAddr 获取远程地址
func (slf *Conn) RemoteAddr() net.Addr {
	return slf.remoteAddr
}

// GetID 获取连接ID
//   - 为远程地址的字符串形式
func (slf *Conn) GetID() string {
	return slf.remoteAddr.String()
}

// GetIP 获取连接IP
func (slf *Conn) GetIP() string {
	return slf.ip
}

// Close 关闭连接
func (slf *Conn) Close() {
	if slf.ws != nil {
		_ = slf.ws.Close()
	} else if slf.gn != nil {
		_ = slf.gn.Close()
	} else if slf.kcp != nil {
		_ = slf.kcp.Close()
	}
	if slf.packetPool != nil {
		slf.packetPool.Close()
	}
	slf.packetPool = nil
	slf.packets = nil
}

// SetData 设置连接数据
func (slf *Conn) SetData(key, value any) *Conn {
	slf.data[key] = value
	return slf
}

// GetData 获取连接数据
func (slf *Conn) GetData(key any) any {
	return slf.data[key]
}

// ReleaseData 释放数据
func (slf *Conn) ReleaseData() *Conn {
	for k := range slf.data {
		delete(slf.data, k)
	}
	return slf
}

// IsWebsocket 是否是websocket连接
func (slf *Conn) IsWebsocket() bool {
	return slf.server.network == NetworkWebsocket
}

// Write 向连接中写入数据
//   - messageType: websocket模式中指定消息类型
func (slf *Conn) Write(packet Packet) {
	if slf.gw != nil {
		slf.gw(packet)
		return
	}
	packet = slf.server.OnConnectionWritePacketBeforeEvent(slf, packet)
	if slf.packetPool == nil {
		return
	}
	cp := slf.packetPool.Get()
	cp.websocketMessageType = packet.WebsocketType
	cp.packet = packet.Data
	slf.mutex.Lock()
	slf.packets = append(slf.packets, cp)
	slf.mutex.Unlock()
}

// WriteWithCallback 与 Write 相同，但是会在写入完成后调用 callback
//   - 当 callback 为 nil 时，与 Write 相同
func (slf *Conn) WriteWithCallback(packet Packet, callback func(err error)) {
	if slf.gw != nil {
		slf.gw(packet)
		return
	}
	packet = slf.server.OnConnectionWritePacketBeforeEvent(slf, packet)
	if slf.packetPool == nil {
		return
	}
	cp := slf.packetPool.Get()
	cp.websocketMessageType = packet.WebsocketType
	cp.packet = packet.Data
	cp.callback = callback
	slf.mutex.Lock()
	slf.packets = append(slf.packets, cp)
	slf.mutex.Unlock()
}

// writeLoop 写循环
func (slf *Conn) writeLoop(wait *sync.WaitGroup) {
	slf.packetPool = concurrent.NewPool[*connPacket](10*1024,
		func() *connPacket {
			return &connPacket{}
		}, func(data *connPacket) {
			data.packet = nil
			data.websocketMessageType = 0
			data.callback = nil
		},
	)
	defer func() {
		if err := recover(); err != nil {
			slf.Close()
		}
	}()
	wait.Done()
	for {
		slf.mutex.Lock()
		if slf.packetPool == nil {
			return
		}
		if len(slf.packets) == 0 {
			slf.mutex.Unlock()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		packets := slf.packets[0:]
		slf.packets = slf.packets[0:0]
		slf.mutex.Unlock()
		for i := 0; i < len(packets); i++ {
			data := packets[i]
			var err error
			if slf.IsWebsocket() {
				err = slf.ws.WriteMessage(data.websocketMessageType, data.packet)
			} else {
				if slf.gn != nil {
					switch slf.server.network {
					case NetworkUdp, NetworkUdp4, NetworkUdp6:
						err = slf.gn.SendTo(data.packet)
					default:
						err = slf.gn.AsyncWrite(data.packet)
					}

				} else if slf.kcp != nil {
					_, err = slf.kcp.Write(data.packet)
				}
			}
			callback := data.callback
			slf.packetPool.Release(data)
			if callback != nil {
				callback(err)
			}
			if err != nil {
				panic(err)
			}
		}
	}
}
