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
	"src.solsynth.dev/sosys/elecpostal/internal/realtime"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
	"src.solsynth.dev/sosys/elecpostal/internal/server"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	"src.solsynth.dev/sosys/elecpostal/internal/smtp"
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
)

// App is the application runtime.
type App struct {
	cfg       *config.Config
	db        *database.DB
	emailSvc  *service.EmailService
	httpSrv   *http.Server
	grpcSrv   *grpc.Server
	grpcLn    net.Listener
	smtpSrvs  []*smtp.Server
	smtpQueue *smtp.NATSQueue
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
	if cfg.WebSocket.Target != "" {
		publisher, err := realtime.NewClient(cfg.WebSocket.Target, cfg.WebSocket.UseTLS, cfg.WebSocket.TLSSkipVerify)
		if err != nil {
			return nil, err
		}
		emailSvc.SetRealtimePublisher(publisher)
	}
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
		logging.Log.Info().Str("adapter", "direct-smtp").Str("inbound_host", cfg.Mail.Relay.InboundHost).Msg("outbound relay configured")
	case "ses":
		sesRelay, err := relay.NewSESAdapter(context.Background(), relay.SESConfig{Region: cfg.Mail.Relay.Region})
		if err != nil {
			return nil, fmt.Errorf("configure SES relay: %w", err)
		}
		emailSvc.SetRelay(relay.NewLocalMXRouter(cfg.Mail.Relay.InboundHost, emailSvc.DeliverLocal, sesRelay))
		logging.Log.Info().Str("adapter", "ses").Str("region", cfg.Mail.Relay.Region).Str("inbound_host", cfg.Mail.Relay.InboundHost).Msg("outbound relay configured")
	default:
		logging.Log.Warn().Str("adapter", cfg.Mail.Relay.Adapter).Msg("outbound relay is not configured; sent emails will not be delivered")
	}
	if cfg.FileSystem.Target != "" {
		fileClient, err := filesystem.NewClient(cfg.FileSystem.Target, cfg.FileSystem.UseTLS, cfg.FileSystem.TLSSkipVerify)
		if err != nil {
			return nil, err
		}
		emailSvc.SetAttachmentUploader(fileClient)
		logging.Log.Info().Str("target", cfg.FileSystem.Target).Msg("filesystem attachment uploader configured")
	}
	if cfg.Workspace.Target != "" {
		workspaceClient, err := workspace.NewClient(cfg.Workspace.Target, cfg.Workspace.UseTLS, cfg.Workspace.TLSSkipVerify)
		if err != nil {
			return nil, err
		}
		workspaceClient.SetSendLimitPolicy(workspace.SendLimitPolicy{
			Free:       sendLimitsFromConfig(cfg.Mail.SendLimits.Free),
			Pro:        sendLimitsFromConfig(cfg.Mail.SendLimits.Pro),
			Enterprise: sendLimitsFromConfig(cfg.Mail.SendLimits.Enterprise),
		})
		emailSvc.SetWorkspaceProvider(workspaceClient)
		logging.Log.Info().Str("target", cfg.Workspace.Target).Msg("workspace quota provider configured")
	}
	router := server.NewRouter(cfg, emailSvc)
	smtpConfigs := cfg.Mail.SMTP
	smtpSrvs := make([]*smtp.Server, 0, len(smtpConfigs))
	for _, listener := range smtpConfigs {
		smtpSrv, err := smtp.New(listener, cfg.Mail.Domain, emailSvc)
		if err != nil {
			return nil, fmt.Errorf("configure SMTP server: %w", err)
		}
		smtpSrvs = append(smtpSrvs, smtpSrv)
	}
	var smtpQueue *smtp.NATSQueue
	if cfg.NATS.Target != "" {
		smtpQueue, err = smtp.NewNATSQueue(cfg.NATS, emailSvc)
		if err != nil {
			return nil, fmt.Errorf("configure SMTP NATS queue: %w", err)
		}
		for _, smtpSrv := range smtpSrvs {
			smtpSrv.SetDeliveryQueue(smtpQueue)
		}
	}

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
		cfg:       cfg,
		db:        db,
		emailSvc:  emailSvc,
		httpSrv:   httpSrv,
		grpcSrv:   grpcSrv,
		smtpSrvs:  smtpSrvs,
		smtpQueue: smtpQueue,
	}, nil
}

func sendLimitsFromConfig(limits config.MailSendLimitConfig) workspace.SendLimits {
	return workspace.SendLimits{
		MailboxDaily: limits.MailboxDaily, MailboxMonthly: limits.MailboxMonthly,
		WorkspaceDaily: limits.WorkspaceDaily, WorkspaceMonthly: limits.WorkspaceMonthly,
	}
}

// Start runs background services and servers.
func (a *App) Start(ctx context.Context) error {
	if a.smtpQueue != nil {
		if err := a.smtpQueue.Start(); err != nil {
			return err
		}
	}
	for _, smtpSrv := range a.smtpSrvs {
		if err := smtpSrv.Start(); err != nil {
			for _, started := range a.smtpSrvs {
				_ = started.Close()
			}
			if a.smtpQueue != nil {
				_ = a.smtpQueue.Close()
			}
			return err
		}
	}
	ln, err := net.Listen("tcp", ":"+a.cfg.GRPC.Port)
	if err != nil {
		for _, smtpSrv := range a.smtpSrvs {
			_ = smtpSrv.Close()
		}
		if a.smtpQueue != nil {
			_ = a.smtpQueue.Close()
		}
		return err
	}
	a.grpcLn = ln

	go func() {
		if err := a.grpcSrv.Serve(ln); err != nil {
			logging.Log.Error().Err(err).Msg("grpc server stopped")
		}
	}()
	go a.purgeArchivedEmails(ctx)
	go a.deliverScheduledEmails(ctx)
	go func() {
		if err := a.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Log.Error().Err(err).Msg("http server stopped")
		}
	}()

	logging.Log.Info().
		Str("http", a.cfg.HTTP.Port).
		Str("grpc", a.cfg.GRPC.Port).
		Int("smtp_listener_count", len(a.cfg.Mail.SMTP)).
		Msg("elecpostal started")
	return nil
}

func (a *App) deliverScheduledEmails(ctx context.Context) {
	deliver := func() {
		if count, err := a.emailSvc.DeliverScheduledEmails(ctx); err != nil {
			logging.Log.Error().Err(err).Msg("deliver scheduled emails")
		} else if count > 0 {
			logging.Log.Info().Int64("count", count).Msg("delivered scheduled emails")
		}
	}
	deliver()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deliver()
		}
	}
}

func (a *App) purgeArchivedEmails(ctx context.Context) {
	purge := func() {
		count, err := a.emailSvc.PurgeArchivedEmails(ctx)
		if err != nil {
			logging.Log.Error().Err(err).Msg("purge archived emails")
			return
		}
		if count > 0 {
			logging.Log.Info().Int64("count", count).Msg("purged archived emails")
		}
		usageCount, err := a.emailSvc.PurgeExpiredSendUsage(ctx)
		if err != nil {
			logging.Log.Error().Err(err).Msg("purge expired email send usage")
		} else if usageCount > 0 {
			logging.Log.Info().Int64("count", usageCount).Msg("purged expired email send usage")
		}
	}
	purge()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

// Stop gracefully shuts down the application.
func (a *App) Stop(ctx context.Context) error {
	for _, smtpSrv := range a.smtpSrvs {
		_ = smtpSrv.Close()
	}
	if a.smtpQueue != nil {
		_ = a.smtpQueue.Close()
	}
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
