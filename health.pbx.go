package pbx

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/hashicorp/consul/api"
	"github.com/obase/pbx/grpc_health_v1"
	"google.golang.org/grpc"
	"net/http"
	"sync"
)

var consulClient *api.Client

var consulOnce sync.Once

func initConculClient(conf *Conf) {
	var err error
	config := api.DefaultConfig()
	if conf.Mode == gin.DebugMode && conf.CenterAddress != "" {
		config.Address = conf.CenterAddress
	}
	if consulClient, err = api.NewClient(config); err != nil { // 兼容旧的逻辑
		Errorf(context.Background(), "Create consul client failed. error: %v", err)
		panic(err)
	}
}

func RegisterHttpHealthCheck(httpServer *gin.Engine, conf *Conf) {
	httpServer.GET("/health", CheckHttpHealth)
	consulOnce.Do(func() {
		initConculClient(conf)
	})
	if consulClient != nil {

		regs := &api.AgentServiceRegistration{
			Kind:    api.ServiceKind("http"),
			ID:      conf.HttpName() + "@" + conf.HttpAddr(),
			Name:    conf.HttpName(),
			Address: conf.HttpHost,
			Port:    conf.HttpPort,
			Tags:    []string{"http", conf.Name, conf.HttpName()},
			Check: &api.AgentServiceCheck{
				HTTP:     fmt.Sprintf("http://%s/health", conf.HttpAddr()),
				Timeout:  conf.HttpCheckTimeout,
				Interval: conf.HttpCheckInterval,
			},
		}
		if err := consulClient.Agent().ServiceRegister(regs); err != nil {
			Errorf(context.Background(), "consul register service error: %v", err)
		}

		// 下述完全是兼容旧的服务注册逻辑
		regs.ID = conf.Name + "@" + conf.HttpAddr()
		regs.Name = conf.Name
		regs.Tags = []string{"http", conf.Name}
		if err := consulClient.Agent().ServiceRegister(regs); err != nil {
			Errorf(context.Background(), "consul register service error: %v", err)
		}

	} else {
		Errorf(context.Background(), "consul client connect failed")
	}
}

func RegisterGrpcHealthCheck(grpcServer *grpc.Server, conf *Conf) {
	service := &HealthService{}
	grpc_health_v1.RegisterHealthServer(grpcServer, service)
	consulOnce.Do(func() {
		initConculClient(conf)
	})
	if consulClient != nil {
		regs := &api.AgentServiceRegistration{
			Kind:    api.ServiceKind("grpc"),
			ID:      conf.GrpcName() + "@" + conf.GrpcAddr(),
			Name:    conf.GrpcName(),
			Address: conf.GrpcHost,
			Port:    conf.GrpcPort,
			Tags:    []string{"grpc", conf.Name, conf.GrpcName()},
			Check: &api.AgentServiceCheck{
				GRPC:     fmt.Sprintf("%v/%v", conf.GrpcAddr(), service),
				Timeout:  conf.GrpcCheckTimeout,
				Interval: conf.GrpcCheckInterval,
			},
		}
		if err := consulClient.Agent().ServiceRegister(regs); err != nil {
			Errorf(context.Background(), "consul register service error: %v", err)
		}
	} else {
		Errorf(context.Background(), "consul client connect failed")
	}
}

func CheckHttpHealth(ctx *gin.Context) {
	ctx.String(http.StatusOK, "OK")
}

type HealthService struct {
}

func (hs *HealthService) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (rsp *grpc_health_v1.HealthCheckResponse, err error) {
	rsp = &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_SERVING,
	}
	return
}