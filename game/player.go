package game

import "github.com/kercylan98/minotaur/server"

// Player 玩家
type Player[ID comparable] interface {
	// GetID 获取玩家ID
	GetID() ID
	// GetConn 获取玩家连接
	GetConn() *server.Conn
	// UseConn 指定连接
	UseConn(conn *server.Conn)
	// Close 关闭玩家并且释放其资源
	Close(err ...error)
}
