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
	r.Use(authMiddleware(cfg))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "service": "elecpostal"})
	})

	// JMAP uses fixed discovery/API paths rather than the REST /api namespace.
	jmapHandler := jmap.New(emailSvc)
	r.GET("/jmap/session", jmapHandler.Session)
	r.POST("/jmap/api", jmapHandler.API)

	api := r.Group("/api")
	handler.RegisterRoutes(api, emailSvc)

	api.GET("/mail/host", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"host": emailSvc.MailHost()})
	})

	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	return r
}

func authMiddleware(cfg *config.Config) gin.HandlerFunc {
	var authenticator sharedauth.TokenAuthenticator
	if cfg.Auth.Target != "" {
		client, err := sharedauth.NewGrpcTokenAuthenticator(sharedauth.GrpcAuthDialConfig{
			Target:        cfg.Auth.Target,
			UseTLS:        cfg.Auth.UseTLS,
			TLSSkipVerify: cfg.Auth.TLSSkipVerify,
		})
		if err != nil {
			logging.Log.Fatal().Err(err).Msg("failed to initialize auth client")
		}
		authenticator = client
	}

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

		if _, exists := c.Get("account_id"); !exists {
			accountID := strings.TrimSpace(c.GetHeader("X-Account-Id"))
			if accountID != "" {
				identity.SetAccountID(c, accountID)
			}
		}

		c.Next()
	}
}
