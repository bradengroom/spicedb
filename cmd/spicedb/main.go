package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"

	"github.com/jzelinskie/cobrautil"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	api "github.com/authzed/spicedb/internal/REDACTEDapi/api"
	health "github.com/authzed/spicedb/internal/REDACTEDapi/healthcheck"
	"github.com/authzed/spicedb/internal/datastore"
	"github.com/authzed/spicedb/internal/datastore/memdb"
	"github.com/authzed/spicedb/internal/services"
)

func main() {
	var rootCmd = &cobra.Command{
		Use:               "spicedb",
		Short:             "A tuple store for ACLs.",
		PersistentPreRunE: cobrautil.SyncViperPreRunE("CALADAN"),
		Run:               rootRun,
	}

	rootCmd.Flags().String("grpc-addr", ":50051", "address to listen on for serving gRPC services")
	rootCmd.Flags().String("grpc-cert-path", "", "local path to the TLS certificate used to serve gRPC services")
	rootCmd.Flags().String("grpc-key-path", "", "local path to the TLS key used to serve gRPC services")
	rootCmd.Flags().Bool("grpc-no-tls", false, "serve unencrypted gRPC services")
	rootCmd.Flags().String("metrics-addr", ":9090", "address to listen on for serving metrics and profiles")
	rootCmd.Flags().Bool("log-debug", false, "enable logging debug events")

	rootCmd.Execute()
}

func rootRun(cmd *cobra.Command, args []string) {
	logger, _ := zap.NewProduction()
	if cobrautil.MustGetBool(cmd, "log-debug") {
		logger, _ = zap.NewDevelopment()
	}
	defer logger.Sync()

	var grpcServer *grpc.Server
	if cobrautil.MustGetBool(cmd, "grpc-no-tls") {
		grpcServer = grpc.NewServer()
	} else {
		var err error
		grpcServer, err = NewTlsGrpcServer(
			cobrautil.MustGetStringExpanded(cmd, "grpc-cert-path"),
			cobrautil.MustGetStringExpanded(cmd, "grpc-key-path"),
		)
		if err != nil {
			logger.Fatal("failed to create TLS gRPC server", zap.Error(err))
		}
	}

	nsDatastore, err := memdb.NewMemdbNamespaceDatastore()
	if err != nil {
		logger.Fatal("failed to init in-memory namespace datastore", zap.Error(err))
	}

	tDatastore, err := memdb.NewMemdbTupleDatastore()
	if err != nil {
		logger.Fatal("failed to init in-memory tuple datastore", zap.Error(err))
	}

	RegisterGrpcServices(grpcServer, nsDatastore, tDatastore)

	go func() {
		addr := cobrautil.MustGetString(cmd, "grpc-addr")
		l, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Fatal("failed to listen on addr for gRPC server", zap.Error(err), zap.String("addr", addr))
		}

		logger.Info("gRPC server started listening", zap.String("addr", addr))
		grpcServer.Serve(l)
	}()

	metricsrv := NewMetricsServer(cobrautil.MustGetString(cmd, "metrics-addr"))
	go func() {
		if err := metricsrv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal("failed while serving metrics", zap.Error(err))
		}
	}()

	signalctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	for {
		select {
		case <-signalctx.Done():
			logger.Info("received interrupt")
			grpcServer.GracefulStop()

			if err := metricsrv.Close(); err != nil {
				logger.Fatal("failed while shutting down metrics server", zap.Error(err))
			}
			return
		}
	}
}

func NewMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return &http.Server{
		Addr:    addr,
		Handler: mux,
	}
}

func RegisterGrpcServices(srv *grpc.Server, nsds datastore.NamespaceDatastore, tds datastore.TupleDatastore) {
	api.RegisterACLServiceServer(srv, services.NewACLServer(tds))
	api.RegisterNamespaceServiceServer(srv, services.NewNamespaceServer(nsds))
	api.RegisterWatchServiceServer(srv, services.NewWatchServer())
	health.RegisterHealthServer(srv, services.NewHealthServer())
	reflection.Register(srv)
}

func NewTlsGrpcServer(certPath, keyPath string) (*grpc.Server, error) {
	if certPath != "" && keyPath != "" {
		return nil, errors.New("missing one of required values: cert path, key path")
	}

	creds, err := credentials.NewServerTLSFromFile(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	return grpc.NewServer(grpc.Creds(creds)), nil
}
