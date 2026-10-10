package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

func newTestRouter(cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	if cfg == nil {
		cfg = &config.Config{}
	}
	return NewRouter(cfg, &service.EmailService{})
}

// An unauthenticated caller can no longer impersonate an account by sending
// X-Account-Id: only a validated token establishes identity.
func TestRouterIgnoresClientSuppliedAccountID(t *testing.T) {
	router := newTestRouter(nil)
	request := httptest.NewRequest(http.MethodGet, "/api/mailboxes", nil)
	request.Header.Set("X-Account-Id", uuid.NewString())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("X-Account-Id alone = %d, want 401 (body %s)", response.Code, response.Body.String())
	}
}

// /api/mail/host discloses the internal mail domain, so anonymous callers are
// rejected.
func TestMailHostRequiresAuthentication(t *testing.T) {
	router := newTestRouter(nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/mail/host", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/mail/host = %d, want 401 (body %s)", response.Code, response.Body.String())
	}
}

// The development flag keeps local workflows usable and trusts the header only
// when it is explicitly enabled.
func TestMailHostAllowsDevHeaderWhenEnabled(t *testing.T) {
	router := newTestRouter(&config.Config{Auth: config.AuthConfig{AllowDevIDs: true}})
	request := httptest.NewRequest(http.MethodGet, "/api/mail/host", nil)
	request.Header.Set("X-Account-Id", uuid.NewString())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("/api/mail/host with allowDevIDs = %d, want 200 (body %s)", response.Code, response.Body.String())
	}
}
