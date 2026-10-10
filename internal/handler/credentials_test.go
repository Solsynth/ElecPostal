package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	sharedauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

// stubSudoValidator returns the elevation result the test dictates.
type stubSudoValidator struct {
	valid   bool
	factors []string
	err     error
}

func (s stubSudoValidator) ValidateSudo(context.Context, string) (bool, []string, error) {
	return s.valid, s.factors, s.err
}

func newCredentialsTestRouter(t *testing.T, accountID uuid.UUID) (*gin.Engine, *gorm.DB) {
	t.Helper()
	return newCredentialsTestRouterWithValidator(t, accountID, stubSudoValidator{valid: true})
}

func newCredentialsTestRouterWithValidator(t *testing.T, accountID uuid.UUID, validator sharedauth.SudoValidator) (*gin.Engine, *gorm.DB) {
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
		sharedauth.WithAuth(c, &sharedauth.AuthResult{
			Account: &gen.DyAccount{Id: accountID.String()},
			Session: &gen.DyAuthSession{Id: "session-" + accountID.String()},
		}, sharedauth.TokenInfo{Token: "test-token", Type: sharedauth.TokenTypeAuthKey})
		c.Next()
	})
	RegisterRoutes(group, service.NewEmailService(&database.DB{DB: db}, nil), validator)
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

// decodeApiError decodes the shared errs.ApiError body.
func decodeApiError(t *testing.T, body []byte) struct {
	Code   string  `json:"code"`
	Status int     `json:"status"`
	Detail *string `json:"detail"`
} {
	t.Helper()
	var decoded struct {
		Code   string  `json:"code"`
		Status int     `json:"status"`
		Detail *string `json:"detail"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return decoded
}

// An unelevated session is rejected with the factor hint and nothing is minted.
func TestCreateMailCredentialRequiresElevation(t *testing.T) {
	caller := uuid.New()
	router, db := newCredentialsTestRouterWithValidator(t, caller,
		stubSudoValidator{valid: false, factors: []string{"password", "timed_code"}})
	own := database.Mailbox{ID: database.NewID(), AccountID: caller, WorkspaceID: "workspace-own", Address: "owner@example.test"}
	if err := db.Create(&own).Error; err != nil {
		t.Fatal(err)
	}
	response := createCredentialRequest(t, router, `{"mailbox_id":"`+own.ID+`","label":"laptop","protocols":["smtp"]}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unelevated credential create = %d, want 403 (body %s)", response.Code, response.Body.String())
	}
	apiErr := decodeApiError(t, response.Body.Bytes())
	if apiErr.Code != "AUTH_SUDO_REQUIRED" {
		t.Fatalf("code = %q, want AUTH_SUDO_REQUIRED", apiErr.Code)
	}
	if apiErr.Detail == nil || *apiErr.Detail != "password,timed_code" {
		t.Fatalf("detail = %v, want password,timed_code", apiErr.Detail)
	}
	var count int64
	if err := db.Model(&database.MailProtocolCredential{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("credential rows after 403 = %d, want 0", count)
	}
}

// The delete half of the gate: an unelevated session cannot drop a credential.
func TestDeleteMailCredentialRequiresElevation(t *testing.T) {
	caller := uuid.New()
	router, db := newCredentialsTestRouterWithValidator(t, caller, stubSudoValidator{valid: false})
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: caller, WorkspaceID: "workspace-own", Address: "owner@example.test"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	credential := database.MailProtocolCredential{ID: database.NewID(), MailboxID: mailbox.ID, Label: "laptop", Hash: "hash", Protocols: datatypes.JSON(`["smtp"]`)}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/credentials/"+credential.ID, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("unelevated credential delete = %d, want 403 (body %s)", response.Code, response.Body.String())
	}
	var count int64
	if err := db.Model(&database.MailProtocolCredential{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("credential rows after 403 = %d, want 1", count)
	}
}

// An unresolvable elevation check fails closed with 503 rather than allowing.
func TestCredentialsFailClosedWhenElevationUnavailable(t *testing.T) {
	cases := map[string]sharedauth.SudoValidator{
		"validator error": stubSudoValidator{err: errors.New("auth service is down")},
		"nil validator":   nil,
	}
	for name, validator := range cases {
		t.Run(name, func(t *testing.T) {
			caller := uuid.New()
			router, _ := newCredentialsTestRouterWithValidator(t, caller, validator)
			response := createCredentialRequest(t, router, `{"mailbox_id":"`+uuid.NewString()+`","label":"laptop","protocols":["smtp"]}`)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("gated create = %d, want 503 (body %s)", response.Code, response.Body.String())
			}
		})
	}
}

// Unauthenticated callers keep the 401 from the shared auth middleware.
func TestCredentialsGateRejectsUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	router := gin.New()
	group := router.Group("/api")
	RegisterRoutes(group, service.NewEmailService(&database.DB{DB: db}, nil), stubSudoValidator{valid: true})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/credentials", strings.NewReader(`{"label":"laptop"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated gated create = %d, want 401 (body %s)", response.Code, response.Body.String())
	}
}
