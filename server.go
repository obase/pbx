package apix

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/obase/httpx/cache"
	"github.com/obase/httpx/ginx"
	"github.com/obase/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"net"
	"net/http"
	"os"
)

/*
扩展逻辑服务器:
1. 支持proto静态注册
2. 支持api + conf.yml动态注册
3. 支持httpPlugin, httpCache机制
*/
type XServer struct {
	*ginx.Server                 // 扩展gin.Server
	init         map[string]bool // file初始化标志
	serverOption []grpc.ServerOption
	middleFilter []gin.HandlerFunc
	services     []*Service
	routesFunc   func(server *ginx.Server)
	registFunc   func(server *grpc.Server)
}

func NewServer() *XServer {
	return &XServer{
		Server: ginx.New(),
		init:   make(map[string]bool), // fixbug
	}
}

func (s *XServer) dispose() {
	s.Server = nil
	s.init = nil
	s.serverOption = nil
	s.middleFilter = nil
	s.services = nil
	s.routesFunc = nil
	s.registFunc = nil
}

/*用于apigen工具的方法*/
func (s *XServer) Init(f func(server *XServer)) {
	k := fmt.Sprintf("%p", f)
	if _, ok := s.init[k]; ok {
		return
	}
	f(s)
	s.init[k] = true
}

func (s *XServer) ServerOption(so grpc.ServerOption) {
	s.serverOption = append(s.serverOption, so)
}

func (s *XServer) MiddleFilter(mf gin.HandlerFunc) {
	s.middleFilter = append(s.middleFilter, mf)
}

func (s *XServer) Service(desc *grpc.ServiceDesc, impl interface{}) *Service {
	gs := &Service{
		serviceDesc: desc,
		serviceImpl: impl,
	}
	s.services = append(s.services, gs)
	return gs
}

/* 补充gin的IRouter路由信息*/
func (server *XServer) Routes(rf func(server *ginx.Server)) {
	server.routesFunc = rf
}

func (server *XServer) Regist(rf func(server *grpc.Server)) {
	server.registFunc = rf
}

func (server *XServer) Serve() error {
	return server.ServeWith(LoadConfig())
}

func (server *XServer) ServeWith(config *Config) error {

	config = mergeConfig(config)

	// 没有配置任何启动,直接退出. 注意: 没有默认80之类的设置
	if config.GrpcPort == 0 && config.HttpPort == 0 {
		return nil
	}

	var (
		grpcServer   *grpc.Server
		grpcListener net.Listener
		httpServer   *http.Server
		httpListener net.Listener
		httpCache    cache.Cache
		err          error
		grpcfunc     func() // 用于延迟启动
		httpfunc     func() // 用于延迟启动
	)

	defer func() {
		log.Flush()
		// 反注册consul服务,另外还设定了超时反注册,双重保障
		if config.Name != "" {
			deregisterService(config)
		}
		// 退出需要明确关闭
		if grpcListener != nil {
			grpcListener.Close()
		}
		if httpListener != nil {
			httpListener.Close()
		}
		if httpCache != nil {
			httpCache.Close()
		}
	}()

	// 创建grpc服务器
	if config.GrpcPort > 0 {
		// 设置keepalive超时
		if config.GrpcKeepAlive != 0 {
			server.serverOption = append(server.serverOption, grpc.KeepaliveParams(keepalive.ServerParameters{
				Time: config.GrpcKeepAlive,
			}))
		}
		grpcServer = grpc.NewServer(server.serverOption...)
		// 安装grpc相关配置
		for _, smeta := range server.services {
			grpcServer.RegisterService(smeta.serviceDesc, smeta.serviceImpl)
		}
		if server.registFunc != nil {
			server.registFunc(grpcServer) // 附加额外的Grpc设置,预防额外逻辑
		}
		// 注册grpc服务
		if config.Name != "" {
			registerServiceGrpc(grpcServer, config)
		}
		// 创建监听端口
		grpcListener, err = graceListenGrpc(config.GrpcHost, config.GrpcPort)
		if err != nil {
			log.Error(nil, "grpc server listen error: %v", err)
			log.Flush()
			return err
		}
		// 启动grpc服务
		grpcfunc = func() {
			if err = grpcServer.Serve(grpcListener); err != nil {
				log.Error(nil, "grpc server serve error: %v", err)
				log.Flush()
				os.Exit(1)
			}
		}
	}

	// 创建http服务器
	if config.HttpPort > 0 {
		server.Server.Use(server.middleFilter...)
		// 安装http相关配置
		var upgrader *websocket.Upgrader
		var httpRouter ginx.IRouter = server.Server // 设置为顶层
		for _, smeta := range server.services {
			if smeta.groupPath != "" {
				httpRouter = httpRouter.Group(smeta.groupPath, smeta.groupFilter...)
			}
			for _, mmeta := range smeta.methods {
				// POST handle
				if mmeta.handlePath != "" {
					handlers := append(mmeta.handleFilter, createHandleFunc(mmeta.adapter, mmeta.tag))
					httpRouter.POST(mmeta.handlePath, handlers...)
				}
				// GET socket
				if mmeta.socketPath != "" {
					if upgrader == nil {
						upgrader = createSocketUpgrader(config)
					}
					handlers := append(mmeta.socketFilter, createSocketFunc(upgrader, mmeta.adapter, mmeta.tag))
					httpRouter.GET(mmeta.socketPath, handlers...)
				}
			}
		}
		if server.routesFunc != nil {
			// 附加额外的API设置,预防额外逻辑
			server.routesFunc(server.Server)
		}
		// 注册http检查
		if config.Name != "" {
			registerServiceHttp(server.Server, config)
		}
		httpCache = cache.New(config.HttpCache)
		mux, err := server.Server.Compile(config.HttpEntry, config.HttpPlugin, httpCache)
		if err != nil {
			log.Error(context.Background(), "http server compile error: %v", err)
			log.Flush()
			return err
		}
		httpServer = &http.Server{
			Handler: mux,
		}
		// 创建监听端口
		httpListener, err = graceListenHttp(config.HttpHost, config.HttpPort, config.HttpKeepAlive)
		if err != nil {
			log.Error(context.Background(), "http server listen error: %v", err)
			log.Flush()
			return err
		}
		// 支持TLS,或http2.0
		if config.HttpCertFile != "" {
			httpfunc = func() {
				if err := httpServer.ServeTLS(httpListener, config.HttpCertFile, config.HttpKeyFile); err != nil {
					log.Error(nil, "http server serve error: %v", err)
					log.Flush()
					os.Exit(1)
				}
			}
		} else {
			httpfunc = func() {
				if err := httpServer.Serve(httpListener); err != nil {
					log.Error(nil, "http server serve error: %v", err)
					log.Flush()
					os.Exit(1)
				}
			}
		}
	}
	// 释放ginx.Server无用缓存
	server.dispose()

	// 延迟启动
	if grpcfunc != nil {
		go grpcfunc()
	}
	if httpfunc != nil {
		go httpfunc()
	}
	// 优雅关闭http与grpc服务
	graceShutdownOrRestart(grpcServer, grpcListener, httpServer, httpListener)

	return nil
}
