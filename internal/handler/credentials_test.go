package handler

import (
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

func newCredentialsTestRouter(t *testing.T, accountID uuid.UUID) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(&database.Mailbox{}, &database.MailProtocolCredential{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	router := gin.New()
	group := router.Group("/api")
	group.Use(func(c *gin.Context) {
		identity.SetAccountID(c, accountID.String())
		c.Next()
	})
	RegisterRoutes(group, service.NewEmailService(&database.DB{DB: db}, nil))
	return router, db
}

func createCredentialRequest(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/credentials", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// POST /credentials must not mint an app credential for a mailbox the caller
// does not own, even when the request supplies another account's mailbox id.
func TestCreateMailCredentialRejectsUnownedMailbox(t *testing.T) {
	caller, other := uuid.New(), uuid.New()
	router, db := newCredentialsTestRouter(t, caller)
	foreign := database.Mailbox{ID: database.NewID(), AccountID: other, WorkspaceID: "workspace-other", Address: "other@example.test"}
	if err := db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	response := createCredentialRequest(t, router, `{"mailbox_id":"`+foreign.ID+`","label":"laptop","protocols":["smtp"]}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("credential for unowned mailbox = %d, want 404 (body %s)", response.Code, response.Body.String())
	}
	var count int64
	if err := db.Model(&database.MailProtocolCredential{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unowned mailbox credential rows = %d, want 0", count)
	}
}

// The creation response still carries the one-time plaintext secret while the
// database keeps only a bcrypt hash.
func TestCreateMailCredentialReturnsOneTimeSecret(t *testing.T) {
	caller := uuid.New()
	router, db := newCredentialsTestRouter(t, caller)
	own := database.Mailbox{ID: database.NewID(), AccountID: caller, WorkspaceID: "workspace-own", Address: "owner@example.test"}
	if err := db.Create(&own).Error; err != nil {
		t.Fatal(err)
	}
	response := createCredentialRequest(t, router, `{"mailbox_id":"`+own.ID+`","label":"laptop","protocols":["smtp","imap"]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("credential for own mailbox = %d, want 201 (body %s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"secret":"`) || strings.Contains(body, `"secret":""`) {
		t.Fatalf("one-time secret missing from response: %s", body)
	}
	var credential database.MailProtocolCredential
	if err := db.First(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if credential.MailboxID != own.ID || credential.Hash == "" || credential.Legacy {
		t.Fatalf("stored credential = %#v", credential)
	}
}
