# Minotaur

[![Go doc](https://img.shields.io/badge/go.dev-reference-brightgreen?logo=go&logoColor=white&style=flat)](https://pkg.go.dev/github.com/kercylan98/minotaur)
![](https://img.shields.io/badge/Email-kercylan@gmail.com-green.svg?style=flat)
![](https://komarev.com/ghpvc/?username=kercylan98)
<a target="_blank" href="https://goreportcard.com/report/github.com/kercylan98/minotaur"><img src="https://goreportcard.com/badge/github.com/kercylan98/minotaur?style=flat-square" /></a>

Minotaur 是一个基于Golang 1.20 编写的服务端开发支持库，其中采用了大量泛型设计，用于游戏服务器开发。

## 目录结构概况
```mermaid
mindmap
  root((Minotaur))
    /configuration 配置管理功能
    /game 游戏通用功能
      /builtin 游戏通用功能内置实现
    /notify 通知功能接口定义
    /planner 策划相关工具目录
      /pce 配置导表功能实现
    /server 网络服务器支持
      /cross 内置跨服功能实现
      /router 内置路由器功能实现
    /utils 工具结构函数目录
```

## Server 架构预览
![server-gdi.jpg](.github/images/server-gdi.jpg)

## 安装
注意：依赖于 **[Go](https://go.dev/) 1.20 +**

运行以下 Go 命令来安装软件包：`minotaur`
```sh
$ go get -u github.com/kercylan98/minotaur
```

## 用法
- 在`Minotaur`中大量使用了 **[泛型](https://go.dev/doc/tutorial/generics)** 、 **[观察者(事件)](https://www.runoob.com/design-pattern/observer-pattern.html)** 和 **[选项模式](https://juejin.cn/post/6844903729313873927)**，在使用前建议先进行相应了解；
- 项目文档可访问 **[pkg.go.dev](https://pkg.go.dev/github.com/kercylan98/minotaur)** 进行查阅；

### 本地文档
可使用 `godoc` 搭建本地文档服务器
#### 安装 godoc
```shell
git clone golang.org/x/tools
cd tools/cmd
go install ...
```
#### 使用 `godoc` 启动本地文档服务器
```shell
godoc -http=:9998 -play
```
#### Windows
```shell
.\local-doc.bat
```

#### Linux or MacOS
```shell
chmod 777 ./local-doc.sh
./local-doc.sh
```

#### 文档地址
- **[http://localhost:9998/pkg/github.com/kercylan98/minotaur/](http://localhost:9998/pkg/github.com/kercylan98/minotaur/)**
- **[https://pkg.go.dev/github.com/kercylan98/minotaur](https://pkg.go.dev/github.com/kercylan98/minotaur)**

### 简单示例
创建一个基于Websocket的回响服务器。
```go
package main

import (
	"github.com/kercylan98/minotaur/server"
)

func main() {
	srv := server.New(server.NetworkWebsocket)
	srv.RegConnectionReceivePacketEvent(func(srv *server.Server, conn *server.Conn, packet server.Packet) {
		conn.Write(packet)
	})
	if err := srv.Run(":9999"); err != nil {
		panic(err)
	}
}
```
访问 **[WebSocket 在线测试](http://www.websocket-test.com/)** 进行验证。
> Websocket地址: ws://127.0.0.1:9999

### 持续更新的示例项目
- **[Minotaur-Example](https://github.com/kercylan98/minotaur-example)**

### 参与贡献
请参考 **[CONTRIBUTING.md](CONTRIBUTING.md)** 贡献指南。

# JetBrains OS licenses

`Minotaur` had been being developed with `GoLand` IDE under the **free JetBrains Open Source license(s)** granted by JetBrains s.r.o., hence I would like to express my thanks here.

<a href="https://www.jetbrains.com/?from=minotaur" target="_blank"><img src="https://resources.jetbrains.com/storage/products/company/brand/logos/jb_beam.png?_gl=1*1vt713y*_ga*MTEzMjEzODQxNC4xNjc5OTY3ODUw*_ga_9J976DJZ68*MTY4ODU0MDUyMy4yMC4xLjE2ODg1NDA5NDAuMjUuMC4w&_ga=2.261225293.1519421387.1688540524-1132138414.1679967850" width="250" align="middle"/></a>