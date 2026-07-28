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
	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
	"src.solsynth.dev/sosys/elecpostal/internal/server"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

// App is the application runtime.
type App struct {
	cfg      *config.Config
	db       *database.DB
	emailSvc *service.EmailService
	httpSrv  *http.Server
	grpcSrv  *grpc.Server
	grpcLn   net.Listener
}

const healthServiceName = "elecpostal"

// New creates a new App from configuration.
func New(cfg *config.Config) (*App, error) {
	db, err := database.Open(cfg)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(); err != nil {
		return nil, err
	}

	var notifier *ring.Client
	if cfg.Ring.Target != "" {
		notifier, err = ring.NewClient(cfg.Ring.Target, cfg.Ring.UseTLS, cfg.Ring.TLSSkipVerify)
		if err != nil {
			return nil, err
		}
	}

	emailSvc := service.NewEmailService(db, notifier)
	emailSvc.SetDomain(cfg.Mail.Domain)
	switch cfg.Mail.Relay.Adapter {
	case "direct-smtp":
		directRelay, err := relay.NewDirectSMTPAdapter(relay.DirectSMTPConfig{
			Hostname:      cfg.Mail.Relay.Host,
			InboundHost:   cfg.Mail.Relay.InboundHost,
			RequireTLS:    cfg.Mail.Relay.TLSMode == "required",
			TLSSkipVerify: cfg.Mail.Relay.TLSSkipVerify,
			LocalDelivery: emailSvc.DeliverLocal,
		})
		if err != nil {
			return nil, fmt.Errorf("configure direct SMTP relay: %w", err)
		}
		emailSvc.SetRelay(directRelay)
	case "ses":
		sesRelay, err := relay.NewSESAdapter(context.Background(), relay.SESConfig{Region: cfg.Mail.Relay.Region})
		if err != nil {
			return nil, fmt.Errorf("configure SES relay: %w", err)
		}
		emailSvc.SetRelay(relay.NewLocalMXRouter(cfg.Mail.Relay.InboundHost, emailSvc.DeliverLocal, sesRelay))
	}
	if cfg.FileSystem.Target != "" {
		fileClient, err := filesystem.NewClient(cfg.FileSystem.Target, cfg.FileSystem.UseTLS, cfg.FileSystem.TLSSkipVerify)
		if err != nil {
			return nil, err
		}
		emailSvc.SetAttachmentUploader(fileClient)
	}
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
	// Publish both the standard aggregate status and the service-specific name.
	// Gateways vary between checking an empty service name and an explicit one.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus(healthServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, healthServer)
	reflection.Register(grpcSrv)

	return &App{
		cfg:      cfg,
		db:       db,
		emailSvc: emailSvc,
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
	if a.emailSvc != nil {
		_ = a.emailSvc.Close()
	}
	return nil
}
