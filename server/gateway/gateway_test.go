package gateway_test

import (
	"fmt"
	"github.com/kercylan98/minotaur/server"
	gateway2 "github.com/kercylan98/minotaur/server/gateway"
	"testing"
	"time"
)

func TestGateway_RunEndpointServer(t *testing.T) {
	srv := server.New(server.NetworkWebsocket, server.WithDeadlockDetect(time.Second*3))
	srv.RegConnectionReceivePacketEvent(func(srv *server.Server, conn *server.Conn, packet server.Packet) {
		fmt.Println("endpoint receive packet", string(packet.Data))
		conn.Write(packet)
	})
	if err := srv.Run(":8889"); err != nil {
		panic(err)
	}
}

func TestGateway_Run(t *testing.T) {
	srv := server.New(server.NetworkWebsocket, server.WithDeadlockDetect(time.Second*3))
	gw := gateway2.NewGateway(srv)
	srv.RegStartFinishEvent(func(srv *server.Server) {
		if err := gw.AddEndpoint(gateway2.NewEndpoint("test", "ws://127.0.0.1:8889")); err != nil {
			panic(err)
		}
	})
	if err := gw.Run(":8888"); err != nil {
		panic(err)
	}
}
