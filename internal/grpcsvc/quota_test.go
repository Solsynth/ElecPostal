package grpcsvc

import (
	"context"
	"testing"
	"time"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	gen "src.solsynth.dev/sosys/go/proto"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGetUsedQuotaCountsActiveWorkspaceMail(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(&database.Mailbox{}, &database.Email{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	workspaceID := "workspace-1"
	mailboxID := database.NewID()
	if err := db.Create(&database.Mailbox{ID: mailboxID, AccountID: uuid.New(), WorkspaceID: workspaceID, Address: "alice@example.com"}).Error; err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	if err := db.Create(&database.Email{ID: database.NewID(), MailboxID: mailboxID, AccountID: uuid.New(), RawSizeBytes: 321}).Error; err != nil {
		t.Fatalf("create active email: %v", err)
	}
	archivedAt := time.Now()
	if err := db.Create(&database.Email{ID: database.NewID(), MailboxID: mailboxID, AccountID: uuid.New(), RawSizeBytes: 654, ArchivedAt: &archivedAt}).Error; err != nil {
		t.Fatalf("create archived email: %v", err)
	}

	server := &quotaServer{mail: service.NewEmailService(&database.DB{DB: db}, nil)}
	response, err := server.GetUsedQuota(context.Background(), &gen.DyGetUsedQuotaRequest{WorkspaceId: workspaceID})
	if err != nil {
		t.Fatalf("GetUsedQuota() error = %v", err)
	}
	if response.GetUsedBytes() != 321 {
		t.Fatalf("used_bytes = %d, want 321", response.GetUsedBytes())
	}
}
