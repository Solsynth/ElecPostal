package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

func newSettingsTestRouter(t *testing.T) (*gin.Engine, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(&database.AccountNotificationSettings{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	accountID := uuid.New()
	router := gin.New()
	group := router.Group("/api")
	group.Use(func(c *gin.Context) {
		identity.SetAccountID(c, accountID.String())
		c.Next()
	})
	RegisterRoutes(group, service.NewEmailService(&database.DB{DB: db}, nil))
	return router, accountID
}

func TestNotificationSettingsRoutesDefaultAndPartialUpdate(t *testing.T) {
	router, _ := newSettingsTestRouter(t)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/settings/notifications", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", response.Code, response.Body.String())
	}
	var settings struct {
		Highlight bool `json:"highlight"`
		Summarize bool `json:"summarize"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if !settings.Highlight || settings.Summarize {
		t.Fatalf("defaults = %+v, want highlighting on and summaries off", settings)
	}

	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/settings/notifications", strings.NewReader(`{"summarize":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode patched settings: %v", err)
	}
	if !settings.Highlight || !settings.Summarize {
		t.Fatalf("patched = %+v, want summarize on and highlight untouched", settings)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPatch, "/api/settings/notifications", strings.NewReader(`{"highlight":false}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode patched settings: %v", err)
	}
	if settings.Highlight || !settings.Summarize {
		t.Fatalf("patched = %+v, want highlight off and summarize kept", settings)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/settings/notifications", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode stored settings: %v", err)
	}
	if settings.Highlight || !settings.Summarize {
		t.Fatalf("stored = %+v, want both updates persisted", settings)
	}
}

func TestNotificationSettingsRoutesRejectMalformedBody(t *testing.T) {
	router, _ := newSettingsTestRouter(t)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/settings/notifications", strings.NewReader(`{"highlight":"yes"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PATCH status = %d, want 400 for a malformed body", response.Code)
	}
}
