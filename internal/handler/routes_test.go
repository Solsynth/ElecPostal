package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
)

func TestEmailRoutesListPreviewAndDownloadSerializedEML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Email{}, &database.Mailbox{}, &database.MailboxAlias{}, &database.Recipient{}, &database.Attachment{}, &database.EmailLabel{}, &database.EmailLabelMapping{}, &database.MessageSource{}); err != nil {
		t.Fatal(err)
	}
	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-test", Address: "owner@example.test"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	body := `<html><head><style>.x{color:red}</style></head><body><p>Readable <b>message</b></p><script>alert(1)</script></body></html>`
	email := database.Email{ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID, Subject: "HTML mail", Body: body, ContentType: "text/html", FromAddress: "sender@example.test"}
	if err := db.Create(&email).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&database.MessageSource{EmailID: email.ID, Manifest: datatypes.JSON(`{"version":1,"body_type":"text/html"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	svc := service.NewEmailService(&database.DB{DB: db}, nil)
	router := gin.New()
	group := router.Group("/api")
	group.Use(func(c *gin.Context) {
		identity.SetAccountID(c, accountID.String())
		c.Next()
	})
	RegisterRoutes(group, svc)
	senderRequest := httptest.NewRequest(http.MethodGet, "/api/addresses/senders?q=sender", nil)
	senderResponse := httptest.NewRecorder()
	router.ServeHTTP(senderResponse, senderRequest)
	if senderResponse.Code != http.StatusOK || !strings.Contains(senderResponse.Body.String(), "sender@example.test") {
		t.Fatalf("sender suggestions = status %d, body %s", senderResponse.Code, senderResponse.Body.String())
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/emails", nil)
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", listResponse.Code, listResponse.Body.String())
	}
	var listed []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Body != "Readable message" {
		t.Fatalf("list preview = %#v", listed)
	}

	// The original single-email API must stay available alongside the EML
	// stream — the detail view fetches the full message by id.
	singleRequest := httptest.NewRequest(http.MethodGet, "/api/emails/"+email.ID, nil)
	singleResponse := httptest.NewRecorder()
	router.ServeHTTP(singleResponse, singleRequest)
	if singleResponse.Code != http.StatusOK {
		t.Fatalf("single email status = %d, body %s", singleResponse.Code, singleResponse.Body.String())
	}
	var fetched struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(singleResponse.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.ID != email.ID || fetched.Subject != "HTML mail" || fetched.Body != body {
		t.Fatalf("single email = %#v", fetched)
	}

	emlRequest := httptest.NewRequest(http.MethodGet, "/api/emails/"+email.ID+"/eml", nil)
	emlResponse := httptest.NewRecorder()
	router.ServeHTTP(emlResponse, emlRequest)
	if emlResponse.Code != http.StatusOK || emlResponse.Header().Get("Content-Type") != "message/rfc822" || !strings.Contains(emlResponse.Header().Get("Content-Disposition"), email.ID+".eml") {
		t.Fatalf("EML response = status %d, headers %#v", emlResponse.Code, emlResponse.Header())
	}
	if !strings.Contains(emlResponse.Body.String(), "Content-Type: text/html; charset=utf-8") || !strings.Contains(emlResponse.Body.String(), body) {
		t.Fatalf("serialized EML does not preserve canonical HTML body: %q", emlResponse.Body.String())
	}
	if _, err := svc.GetEmail(context.Background(), uuid.New(), email.ID); err == nil {
		t.Fatal("email lookup by another account unexpectedly succeeded")
	}
}

func TestEmailRoutesPermanentlyDeleteAndEmptyTrash(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Email{}, &database.Mailbox{}, &database.Recipient{}, &database.Attachment{}, &database.MessageSource{}, &database.MailFolder{}, &database.FolderMessage{}, &database.DmarcReport{}, &database.DmarcReportRecord{}); err != nil {
		t.Fatal(err)
	}
	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-test", Address: "owner@example.test"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	createMessage := func(subject, folder string) database.Email {
		email := database.Email{ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID, Subject: subject, Body: "body", Folder: folder, FromAddress: "sender@example.test"}
		if err := db.Create(&email).Error; err != nil {
			t.Fatal(err)
		}
		return email
	}
	trashed := createMessage("Trashed", "trash")
	kept := createMessage("Kept", "inbox")
	svc := service.NewEmailService(&database.DB{DB: db}, nil)
	svc.SetWorkspaceProvider(routeWorkspaceProvider{})
	router := gin.New()
	group := router.Group("/api")
	group.Use(func(c *gin.Context) {
		identity.SetAccountID(c, accountID.String())
		c.Next()
	})
	RegisterRoutes(group, svc)

	// A single message leaves Trash for good.
	permanentRequest := httptest.NewRequest(http.MethodDelete, "/api/emails/"+trashed.ID+"/permanent", nil)
	permanentResponse := httptest.NewRecorder()
	router.ServeHTTP(permanentResponse, permanentRequest)
	if permanentResponse.Code != http.StatusOK {
		t.Fatalf("permanent delete status = %d, body %s", permanentResponse.Code, permanentResponse.Body.String())
	}
	var rows int64
	if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", trashed.ID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("permanently deleted email rows = %d, want 0", rows)
	}
	// The soft delete route still moves a message to Trash instead of dropping it.
	if err := svc.MoveEmail(context.Background(), accountID, kept.ID, "trash"); err != nil {
		t.Fatal(err)
	}
	softRequest := httptest.NewRequest(http.MethodDelete, "/api/emails/"+kept.ID, nil)
	softResponse := httptest.NewRecorder()
	router.ServeHTTP(softResponse, softRequest)
	if softResponse.Code != http.StatusOK {
		t.Fatalf("soft delete status = %d, body %s", softResponse.Code, softResponse.Body.String())
	}
	if err := db.First(&database.Email{}, "id = ?", kept.ID).Error; err != nil {
		t.Fatalf("soft-deleted email is gone: %v", err)
	}
	// Unknown ids report 404 rather than pretending to delete.
	missingRequest := httptest.NewRequest(http.MethodDelete, "/api/emails/"+database.NewID()+"/permanent", nil)
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingRequest)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("permanent delete of an unknown email = %d, want 404", missingResponse.Code)
	}

	// Emptying the mailbox Trash reports how many messages went away.
	emptyRequest := httptest.NewRequest(http.MethodDelete, "/api/mailboxes/"+mailbox.ID+"/trash", nil)
	emptyResponse := httptest.NewRecorder()
	router.ServeHTTP(emptyResponse, emptyRequest)
	if emptyResponse.Code != http.StatusOK || !strings.Contains(emptyResponse.Body.String(), `"deleted":1`) {
		t.Fatalf("empty trash = status %d, body %s", emptyResponse.Code, emptyResponse.Body.String())
	}
	if rows := func() int64 {
		var count int64
		if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", kept.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}(); rows != 0 {
		t.Fatalf("emptied trash left %d rows", rows)
	}
}

// routeWorkspaceProvider stands in for Valve: every account is a member and no
// plan limit is reached.
type routeWorkspaceProvider struct{}

func (routeWorkspaceProvider) AuthorizeMember(context.Context, string, string) error { return nil }

func (routeWorkspaceProvider) PlanStorageBytes(context.Context, string) (int64, error) {
	return 1 << 30, nil
}

func (routeWorkspaceProvider) MailboxLimit(context.Context, string) (int64, error) { return 10, nil }

func (routeWorkspaceProvider) CustomDomainLimit(context.Context, string) (int64, error) { return 0, nil }

func (routeWorkspaceProvider) SendLimits(context.Context, string) (workspace.SendLimits, error) {
	return workspace.SendLimits{}, nil
}

func (routeWorkspaceProvider) Close() error { return nil }
