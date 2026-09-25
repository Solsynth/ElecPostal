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
)

func TestEmailRoutesListPreviewAndDownloadSerializedEML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Email{}, &database.Mailbox{}, &database.Recipient{}, &database.Attachment{}, &database.EmailLabel{}, &database.EmailLabelMapping{}, &database.MessageSource{}); err != nil {
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
