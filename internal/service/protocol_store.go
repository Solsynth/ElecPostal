package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

var defaultProtocolFolders = []struct{ name, use string }{
	{"INBOX", `\\Inbox`}, {"Sent", `\\Sent`}, {"Drafts", `\\Drafts`},
	{"Spam", `\\Junk`}, {"Trash", `\\Trash`}, {"Archive", `\\Archive`},
}

// ProtocolMessage is the immutable, protocol-facing mailbox view.
type ProtocolMessage struct {
	EmailID string
	UID     uint32
	Flags   []string
	Raw     []byte
	ModSeq  uint64
}

// ProtocolStoreResult is returned by STORE so an IMAP session can emit the
// updated flags and mod-sequence without a follow-up query.
type ProtocolStoreResult struct {
	EmailID string
	UID     uint32
	Flags   []string
	ModSeq  uint64
}

// BackfillProtocolStorage gives messages created before IMAP/POP3 support a
// canonical source and a membership in their existing HTTP folder.  It is
// idempotent and intentionally runs in bounded batches at startup while this
// service is still pre-release.
func (s *EmailService) BackfillProtocolStorage(ctx context.Context) (int, error) {
	const batchSize = 250
	created := 0
	for {
		var emails []database.Email
		if err := s.db.WithContext(ctx).
			Joins("LEFT JOIN message_sources ON message_sources.email_id = emails.id").
			Where("message_sources.id IS NULL").Order("emails.created_at ASC").Limit(batchSize).Find(&emails).Error; err != nil {
			return created, err
		}
		if len(emails) == 0 {
			return created, nil
		}
		for _, email := range emails {
			if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := s.storeProtocolSourceTx(tx, &email, nil, email.FromAddress); err != nil {
					return err
				}
				return s.addFolderMembershipTx(tx, email.MailboxID, protocolFolderName(email.Folder), email.ID)
			}); err != nil {
				return created, err
			}
			created++
		}
	}
}

func protocolFolderName(folder string) string {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "sent":
		return "Sent"
	case "drafts":
		return "Drafts"
	case "spam":
		return "Spam"
	case "trash":
		return "Trash"
	case "archive":
		return "Archive"
	default:
		return "INBOX"
	}
}

func (s *EmailService) ListProtocolFolder(ctx context.Context, mailboxID, name string) ([]ProtocolMessage, *database.MailFolder, error) {
	name = strings.TrimSpace(name)
	// The empty string is the IMAP hierarchy root, not a selectable mailbox.
	// Reject it before querying so exploratory client commands do not create
	// misleading GORM "record not found" warnings.
	if name == "" {
		return nil, nil, ErrNotFound
	}
	var folder database.MailFolder
	result := s.db.WithContext(ctx).Where("mailbox_id = ? AND name = ?", mailboxID, name).Find(&folder)
	if result.Error != nil {
		return nil, nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil, ErrNotFound
	}
	type row struct {
		EmailID string
		UID     uint32
		Flags   datatypes.JSON
		ModSeq  uint64
		Raw     []byte
	}
	var rows []row
	if err := s.db.WithContext(ctx).Table("folder_messages").
		Select("folder_messages.email_id, folder_messages.uid, folder_messages.flags, folder_messages.mod_seq, message_sources.raw").
		Joins("JOIN message_sources ON message_sources.email_id = folder_messages.email_id").
		Where("folder_messages.folder_id = ?", folder.ID).Order("folder_messages.uid").Scan(&rows).Error; err != nil {
		return nil, nil, err
	}
	items := make([]ProtocolMessage, 0, len(rows))
	for _, row := range rows {
		var flags []string
		_ = json.Unmarshal(row.Flags, &flags)
		items = append(items, ProtocolMessage{EmailID: row.EmailID, UID: row.UID, Flags: flags, Raw: row.Raw, ModSeq: row.ModSeq})
	}
	return items, &folder, nil
}

func (s *EmailService) MoveProtocolMessages(ctx context.Context, mailboxID, from, to string, emailIDs []string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source, target database.MailFolder
		if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, from).First(&source).Error; err != nil {
			return ErrNotFound
		}
		if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, to).First(&target).Error; err != nil {
			return ErrNotFound
		}
		var rows []database.FolderMessage
		if err := tx.Where("folder_id = ? AND email_id IN ?", source.ID, emailIDs).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			row.FolderID, row.UID, row.ModSeq = target.ID, target.NextUID, target.HighestModSeq+1
			target.NextUID++
			target.HighestModSeq++
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		source.HighestModSeq++
		if err := tx.Where("folder_id = ? AND email_id IN ?", source.ID, emailIDs).Delete(&database.FolderMessage{}).Error; err != nil {
			return err
		}
		if err := tx.Save(&source).Error; err != nil {
			return err
		}
		return tx.Save(&target).Error
	})
}

func (s *EmailService) CopyProtocolMessages(ctx context.Context, mailboxID, from, to string, emailIDs []string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source, target database.MailFolder
		if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, from).First(&source).Error; err != nil {
			return ErrNotFound
		}
		if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, to).First(&target).Error; err != nil {
			return ErrNotFound
		}
		var rows []database.FolderMessage
		if err := tx.Where("folder_id = ? AND email_id IN ?", source.ID, emailIDs).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			row.FolderID, row.UID, row.ModSeq = target.ID, target.NextUID, target.HighestModSeq+1
			target.NextUID++
			target.HighestModSeq++
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return tx.Save(&target).Error
	})
}

func (s *EmailService) StoreProtocolFlags(ctx context.Context, mailboxID, folderName string, emailIDs []string, flags []string, mode string, unchangedSince uint64) ([]ProtocolStoreResult, error) {
	var result []ProtocolStoreResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var folder database.MailFolder
		if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, folderName).First(&folder).Error; err != nil {
			return ErrNotFound
		}
		var rows []database.FolderMessage
		if err := tx.Where("folder_id = ? AND email_id IN ?", folder.ID, emailIDs).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if unchangedSince > 0 && row.ModSeq > unchangedSince {
				continue
			}
			var previous []string
			_ = json.Unmarshal(row.Flags, &previous)
			row.Flags = datatypes.JSON(marshalFlags(mergeFlags(previous, flags, mode)))
			folder.HighestModSeq++
			row.ModSeq = folder.HighestModSeq
			if err := tx.Save(&row).Error; err != nil {
				return err
			}
			var updated []string
			_ = json.Unmarshal(row.Flags, &updated)
			result = append(result, ProtocolStoreResult{EmailID: row.EmailID, UID: row.UID, Flags: updated, ModSeq: row.ModSeq})
		}
		return tx.Save(&folder).Error
	})
	return result, err
}

func mergeFlags(current, requested []string, mode string) []string {
	set := map[string]bool{}
	for _, value := range current {
		set[value] = true
	}
	switch mode {
	case "replace":
		set = map[string]bool{}
		for _, value := range requested {
			set[value] = true
		}
	case "add":
		for _, value := range requested {
			set[value] = true
		}
	case "remove":
		for _, value := range requested {
			delete(set, value)
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func marshalFlags(flags []string) []byte { value, _ := json.Marshal(flags); return value }

// ensureProtocolFoldersTx establishes the stable per-address IMAP namespace.
func (s *EmailService) ensureProtocolFoldersTx(tx *gorm.DB, mailboxID string) error {
	for _, spec := range defaultProtocolFolders {
		var count int64
		if err := tx.Model(&database.MailFolder{}).Where("mailbox_id = ? AND name = ?", mailboxID, spec.name).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			folder := database.MailFolder{MailboxID: mailboxID, Name: spec.name, UIDValidity: uint32(time.Now().Unix()), NextUID: 1, HighestModSeq: 1, SpecialUse: spec.use, Subscribed: true}
			if err := tx.Create(&folder).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *EmailService) addInboxMembershipTx(tx *gorm.DB, mailboxID, emailID string) error {
	return s.addFolderMembershipTx(tx, mailboxID, "INBOX", emailID)
}

func (s *EmailService) addFolderMembershipTx(tx *gorm.DB, mailboxID, name, emailID string) error {
	if err := s.ensureProtocolFoldersTx(tx, mailboxID); err != nil {
		return err
	}
	var folder database.MailFolder
	if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, name).First(&folder).Error; err != nil {
		return err
	}
	uid, modseq := folder.NextUID, folder.HighestModSeq+1
	if err := tx.Create(&database.FolderMessage{FolderID: folder.ID, EmailID: emailID, UID: uid, Flags: datatypes.JSON([]byte("[]")), ModSeq: modseq}).Error; err != nil {
		return err
	}
	return tx.Model(&database.MailFolder{}).Where("id = ?", folder.ID).Updates(map[string]any{"next_uid": uid + 1, "highest_mod_seq": modseq}).Error
}

// clauseLock is kept local so protocol mutations consistently serialize UID
// allocation on PostgreSQL without spreading SQL strings through handlers.
func clauseLock() clause.Locking { return clause.Locking{Strength: "UPDATE"} }

func (s *EmailService) storeProtocolSourceTx(tx *gorm.DB, email *database.Email, raw []byte, envelopeFrom string) error {
	if len(raw) == 0 {
		raw = renderCanonicalSource(*email)
	}
	sum := sha256.Sum256(raw)
	return tx.Create(&database.MessageSource{EmailID: email.ID, Raw: raw, SHA256: fmt.Sprintf("%x", sum[:]), EnvelopeFrom: envelopeFrom, ReceivedAt: time.Now()}).Error
}

func renderCanonicalSource(email database.Email) []byte {
	var b bytes.Buffer
	from := email.FromAddress
	if email.FromName != "" {
		from = (&mail.Address{Name: email.FromName, Address: from}).String()
	}
	fmt.Fprintf(&b, "From: %s\r\nSubject: %s\r\nContent-Type: %s; charset=utf-8\r\n\r\n%s", from, strings.ReplaceAll(email.Subject, "\n", " "), normalizeContentType(email.ContentType), email.Body)
	return b.Bytes()
}
