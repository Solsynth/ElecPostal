package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/server"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	"src.solsynth.dev/sosys/elecpostal/internal/solar"
)

// App is the application runtime.
type App struct {
	cfg     *config.Config
	db      *database.DB
	emailSvc *service.EmailService
	solarMgr *solar.Manager
	httpSrv *http.Server
	grpcSrv *grpc.Server
	grpcLn  net.Listener
}

// New creates a new App from configuration.
func New(cfg *config.Config) (*App, error) {
	db, err := database.Open(cfg)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(); err != nil {
		return nil, err
	}

	var solarClient *solar.Client
	if cfg.SolarNetwork.BaseURL != "" {
		solarClient = solar.NewClient(cfg.SolarNetwork.BaseURL, cfg.SolarNetwork.AccessToken)
	}

	emailSvc := service.NewEmailService(db, solarClient)
	router := server.NewRouter(cfg, emailSvc)

	httpSrv := &http.Server{
		Addr:         ":" + cfg.HTTP.Port,
		Handler:      router,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	grpcOpts := []grpc.ServerOption{}
	if cfg.GRPC.UseTLS {
		if cfg.GRPC.CertFile == "" || cfg.GRPC.KeyFile == "" {
			return nil, fmt.Errorf("grpc tls requires cert and key files")
		}
		creds, err := credentials.NewServerTLSFromFile(cfg.GRPC.CertFile, cfg.GRPC.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load grpc tls credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, healthServer)
	reflection.Register(grpcSrv)

	var solarMgr *solar.Manager
	if cfg.SolarNetwork.BaseURL != "" && cfg.SolarNetwork.AccessToken != "" {
		solarMgr = solar.NewManager(cfg.SolarNetwork.BaseURL, cfg.SolarNetwork.AccountName, cfg.SolarNetwork.AccessToken)
	}

	return &App{
		cfg:      cfg,
		db:       db,
		emailSvc: emailSvc,
		solarMgr: solarMgr,
		httpSrv:  httpSrv,
		grpcSrv:  grpcSrv,
	}, nil
}

// Start runs background services and servers.
func (a *App) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", ":"+a.cfg.GRPC.Port)
	if err != nil {
		return err
	}
	a.grpcLn = ln

	if a.solarMgr != nil {
		if err := a.solarMgr.Start(ctx); err != nil {
			return err
		}
	}

	go func() {
		if err := a.grpcSrv.Serve(ln); err != nil {
			logging.Log.Error().Err(err).Msg("grpc server stopped")
		}
	}()
	go func() {
		if err := a.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Log.Error().Err(err).Msg("http server stopped")
		}
	}()

	logging.Log.Info().
		Str("http", a.cfg.HTTP.Port).
		Str("grpc", a.cfg.GRPC.Port).
		Msg("elecpostal started")
	return nil
}

// Stop gracefully shuts down the application.
func (a *App) Stop(ctx context.Context) error {
	if a.httpSrv != nil {
		_ = a.httpSrv.Shutdown(ctx)
	}
	if a.grpcSrv != nil {
		a.grpcSrv.GracefulStop()
	}
	if a.grpcLn != nil {
		_ = a.grpcLn.Close()
	}
	if a.solarMgr != nil {
		_ = a.solarMgr.Stop(ctx)
	}
	return nil
}
