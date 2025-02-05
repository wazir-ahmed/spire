package endpoints

import (
	"context"
	"errors"
	"net"

	discovery_v2 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	secret_v3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/sirupsen/logrus"
	healthv1 "github.com/spiffe/spire/pkg/agent/api/health/v1"
	"github.com/spiffe/spire/pkg/agent/endpoints/sdsv2"
	"github.com/spiffe/spire/pkg/agent/endpoints/sdsv3"
	"github.com/spiffe/spire/pkg/agent/endpoints/workload"
	"github.com/spiffe/spire/pkg/common/api/middleware"
	"github.com/spiffe/spire/pkg/common/peertracker"
	"github.com/spiffe/spire/pkg/common/telemetry"
	workload_pb "github.com/vishnusomank/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type Server interface {
	ListenAndServe(ctx context.Context) error
	RunTCPAgent(ctx context.Context) error
}

type Endpoints struct {
	addr              net.Addr
	TCPAddr           *net.TCPAddr
	log               logrus.FieldLogger
	metrics           telemetry.Metrics
	workloadAPIServer workload_pb.SpiffeWorkloadAPIServer
	sdsv2Server       discovery_v2.SecretDiscoveryServiceServer
	sdsv3Server       secret_v3.SecretDiscoveryServiceServer
	healthServer      grpc_health_v1.HealthServer

	hooks struct {
		// test hook used to indicate that is listening
		listening chan struct{}
	}
}

func New(c Config) *Endpoints {
	attestor := PeerTrackerAttestor{Attestor: c.Attestor}

	if c.newWorkloadAPIServer == nil {
		c.newWorkloadAPIServer = func(c workload.Config) workload_pb.SpiffeWorkloadAPIServer {
			return workload.New(c)
		}
	}
	if c.newSDSv2Server == nil {
		c.newSDSv2Server = func(c sdsv2.Config) discovery_v2.SecretDiscoveryServiceServer {
			return sdsv2.New(c)
		}
	}
	if c.newSDSv3Server == nil {
		c.newSDSv3Server = func(c sdsv3.Config) secret_v3.SecretDiscoveryServiceServer {
			return sdsv3.New(c)
		}
	}
	if c.newHealthServer == nil {
		c.newHealthServer = func(c healthv1.Config) grpc_health_v1.HealthServer {
			return healthv1.New(c)
		}
	}

	allowedClaims := make(map[string]struct{}, len(c.AllowedForeignJWTClaims))
	for _, claim := range c.AllowedForeignJWTClaims {
		allowedClaims[claim] = struct{}{}
	}

	workloadAPIServer := c.newWorkloadAPIServer(workload.Config{
		Manager:                       c.Manager,
		Attestor:                      attestor,
		AllowUnauthenticatedVerifiers: c.AllowUnauthenticatedVerifiers,
		AllowedForeignJWTClaims:       allowedClaims,
		TrustDomain:                   c.TrustDomain,
	})

	sdsv2Server := c.newSDSv2Server(sdsv2.Config{
		Attestor:          attestor,
		Manager:           c.Manager,
		DefaultSVIDName:   c.DefaultSVIDName,
		DefaultBundleName: c.DefaultBundleName,
	})

	sdsv3Server := c.newSDSv3Server(sdsv3.Config{
		Attestor:                    attestor,
		Manager:                     c.Manager,
		DefaultSVIDName:             c.DefaultSVIDName,
		DefaultBundleName:           c.DefaultBundleName,
		DefaultAllBundlesName:       c.DefaultAllBundlesName,
		DisableSPIFFECertValidation: c.DisableSPIFFECertValidation,
	})

	healthServer := c.newHealthServer(healthv1.Config{
		Addr: c.BindAddr,
	})

	return &Endpoints{
		addr:              c.BindAddr,
		TCPAddr:           c.AgentAddr,
		log:               c.Log,
		metrics:           c.Metrics,
		workloadAPIServer: workloadAPIServer,
		sdsv2Server:       sdsv2Server,
		sdsv3Server:       sdsv3Server,
		healthServer:      healthServer,
	}
}

func (e *Endpoints) ListenAndServe(ctx context.Context) error {
	unaryInterceptor, streamInterceptor := middleware.Interceptors(
		Middleware(e.log, e.metrics),
	)

	server := grpc.NewServer(
		grpc.Creds(peertracker.NewCredentials()),
		grpc.UnaryInterceptor(unaryInterceptor),
		grpc.StreamInterceptor(streamInterceptor),
	)

	workload_pb.RegisterSpiffeWorkloadAPIServer(server, e.workloadAPIServer)
	discovery_v2.RegisterSecretDiscoveryServiceServer(server, e.sdsv2Server)
	secret_v3.RegisterSecretDiscoveryServiceServer(server, e.sdsv3Server)
	grpc_health_v1.RegisterHealthServer(server, e.healthServer)

	l, err := e.createListener()
	if err != nil {
		return err
	}
	defer l.Close()

	// Update the listening address with the actual address.
	// If a TCP address was specified with port 0, this will
	// update the address with the actual port that is used
	// to listen.
	e.addr = l.Addr()
	e.log.WithFields(logrus.Fields{
		telemetry.Network: e.addr.Network(),
		telemetry.Address: e.addr,
	}).Info("Starting Workload and SDS APIs")
	e.triggerListeningHook()
	errChan := make(chan error)
	go func() {
		errChan <- server.Serve(l)
	}()
	select {
	case err = <-errChan:
	case <-ctx.Done():
		e.log.Info("Stopping Workload and SDS APIs")
		server.Stop()
		err = <-errChan
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
	}
	return err
}

func (e *Endpoints) triggerListeningHook() {
	if e.hooks.listening != nil {
		e.hooks.listening <- struct{}{}
	}
}

func (e *Endpoints) createTCPAgent(ctx context.Context) *grpc.Server {

	unaryInterceptor, streamInterceptor := middleware.Interceptors(
		Middleware(e.log, e.metrics),
	)
	return grpc.NewServer(
		grpc.Creds(peertracker.NewCredentials()),
		grpc.UnaryInterceptor(unaryInterceptor),
		grpc.StreamInterceptor(streamInterceptor),
	)
}

// runTCPServer will start the agent and block until it exits or we are dying.
func (e *Endpoints) RunTCPAgent(ctx context.Context) error {
	tcpServer := e.createTCPAgent(ctx)
	grpc_health_v1.RegisterHealthServer(tcpServer, e.healthServer)
	workload_pb.RegisterSpiffeWorkloadAPIServer(tcpServer, e.workloadAPIServer)
	discovery_v2.RegisterSecretDiscoveryServiceServer(tcpServer, e.sdsv2Server)
	secret_v3.RegisterSecretDiscoveryServiceServer(tcpServer, e.sdsv3Server)

	l, err := e.createTCPListener()
	if err != nil {
		return err
	}
	defer l.Close()

	e.log.WithFields(logrus.Fields{
		telemetry.Network: e.TCPAddr.Network(),
		telemetry.Address: e.TCPAddr,
	}).Info("Starting Workload and SDS APIs")
	e.triggerListeningHook()
	errChan := make(chan error)
	go func() {
		errChan <- tcpServer.Serve(l)
	}()
	select {
	case err = <-errChan:
	case <-ctx.Done():
		e.log.Info("Stopping Workload and SDS APIs")
		tcpServer.Stop()
		err = <-errChan
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
	}
	return err

}
