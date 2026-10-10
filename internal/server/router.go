package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	sharedauth "src.solsynth.dev/sosys/go/pkg/auth"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/handler"
	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/jmap"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

// NewRouter builds the HTTP router.
func NewRouter(cfg *config.Config, emailSvc *service.EmailService) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	authenticator, sudoValidator := buildAuthClient(cfg)
	r.Use(authMiddleware(authenticator, cfg))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "service": "elecpostal"})
	})

	// JMAP uses fixed discovery/API paths rather than the REST /api namespace.
	jmapHandler := jmap.New(emailSvc)
	r.GET("/.well-known/jmap", jmap.WellKnown)
	r.GET("/jmap/session", jmapHandler.Session)
	r.POST("/jmap/api", jmapHandler.API)
	r.POST("/jmap/upload/:accountId", jmapHandler.Upload)
	r.GET("/jmap/download/:accountId/:blobId/:name", jmapHandler.Download)

	api := r.Group("/api")
	handler.RegisterRoutes(api, emailSvc, sudoValidator)

	api.GET("/mail/host", func(c *gin.Context) {
		if _, ok := identity.RequireAccountID(c); !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"host": emailSvc.MailHost()})
	})

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	return r
}

// buildAuthClient constructs the shared gRPC authenticator once so the same
// connection backs both request authentication and the elevation check
// (GrpcTokenAuthenticator implements TokenAuthenticator and SudoValidator).
func buildAuthClient(cfg *config.Config) (sharedauth.TokenAuthenticator, sharedauth.SudoValidator) {
	if cfg.Auth.Target == "" {
		return nil, nil
	}
	client, err := sharedauth.NewGrpcTokenAuthenticator(sharedauth.GrpcAuthDialConfig{
		Target:        cfg.Auth.Target,
		UseTLS:        cfg.Auth.UseTLS,
		TLSSkipVerify: cfg.Auth.TLSSkipVerify,
	})
	if err != nil {
		logging.Log.Fatal().Err(err).Msg("failed to initialize auth client")
	}
	return client, client
}

func authMiddleware(authenticator sharedauth.TokenAuthenticator, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if authenticator != nil {
			result, err := sharedauth.AuthenticateRequest(c.Request.Context(), authenticator, c.Request)
			if err == nil && result != nil {
				if tokenInfo, ok := sharedauth.ExtractToken(c.Request); ok {
					sharedauth.WithAuth(c, result, tokenInfo)
				}
				if accountID, ok := identity.ExtractAccountIDFromAuth(c); ok {
					identity.SetAccountID(c, accountID)
				}
			}
		}

		// Client-supplied identity headers are never trusted. The only escape
		// hatch is an explicit development flag, which logs every use.
		if _, exists := c.Get("account_id"); !exists && cfg.Auth.AllowDevIDs {
			if accountID := strings.TrimSpace(c.GetHeader("X-Account-Id")); accountID != "" {
				logging.Log.Warn().
					Str("account_id", accountID).
					Msg("trusting client-supplied X-Account-Id header because auth.allowDevIDs is enabled; disable it outside local development")
				identity.SetAccountID(c, accountID)
			}
		}

		c.Next()
	}
}
