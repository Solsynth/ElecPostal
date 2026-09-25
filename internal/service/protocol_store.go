package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

var defaultProtocolFolders = []struct{ name, use string }{
	{"INBOX", `\Inbox`}, {"Sent", `\Sent`}, {"Drafts", `\Drafts`},
	{"Spam", `\Junk`}, {"Trash", `\Trash`}, {"Archive", `\Archive`},
}

// ProtocolMessage is the lightweight protocol-facing mailbox view. The
// message bytes are rendered from its manifest when a client fetches it.
type ProtocolMessage struct {
	EmailID    string
	UID        uint32
	Flags      []string
	RFC822Size int64
	Subject    string
	Body       string
	ModSeq     uint64
}

// ProtocolStoreResult is returned by STORE so an IMAP session can emit the
// updated flags and mod-sequence without a follow-up query.
type ProtocolStoreResult struct {
	EmailID string
	UID     uint32
	Flags   []string
	ModSeq  uint64
}

func (s *EmailService) ListProtocolFolders(ctx context.Context, mailboxID string) ([]database.MailFolder, error) {
	var folders []database.MailFolder
	if err := s.db.WithContext(ctx).Where("mailbox_id = ?", mailboxID).Order("name ASC").Find(&folders).Error; err != nil {
		return nil, err
	}
	return folders, nil
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

// MigrateLegacyProtocolSources converts the pre-manifest raw MIME column in
// bounded batches. The raw column is dropped only after every row verifies.
func (s *EmailService) MigrateLegacyProtocolSources(ctx context.Context) error {
	if !s.db.Migrator().HasTable("message_sources") || !s.db.Migrator().HasColumn("message_sources", "raw") {
		return nil
	}
	if s.files == nil {
		var rows []struct {
			ID           string
			EmailID      string
			Raw          []byte
			EnvelopeFrom string
			ReceivedAt   time.Time
		}
		if err := s.db.WithContext(ctx).Table("message_sources").Select("id, email_id, raw, envelope_from, received_at").Where("raw IS NOT NULL").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			parsed, err := mailmime.ParseMessage(row.Raw, row.EnvelopeFrom, nil)
			if err != nil {
				return fmt.Errorf("parse legacy message source %s: %w", row.ID, err)
			}
			if len(parsed.Attachments) > 0 {
				return fmt.Errorf("legacy message source %s contains attachments but attachment byte store is not configured", row.ID)
			}
			if err := s.migrateLegacySource(ctx, row.ID, row.EmailID, row.Raw, row.EnvelopeFrom, row.ReceivedAt); err != nil {
				return err
			}
		}
		return s.db.Migrator().DropColumn("message_sources", "raw")
	}
	const batchSize = 100
	for {
		var rows []struct {
			ID           string
			EmailID      string
			Raw          []byte
			EnvelopeFrom string
			ReceivedAt   time.Time
			Manifest     datatypes.JSON
		}
		if err := s.db.WithContext(ctx).Table("message_sources").
			Select("id, email_id, raw, envelope_from, received_at, manifest").
			Where("raw IS NOT NULL AND (manifest IS NULL OR manifest = ?)", "{}").Order("id ASC").Limit(batchSize).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if len(row.Manifest) > 2 && string(row.Manifest) != "{}" {
				continue
			}
			if err := s.migrateLegacySource(ctx, row.ID, row.EmailID, row.Raw, row.EnvelopeFrom, row.ReceivedAt); err != nil {
				return fmt.Errorf("migrate legacy message source %s: %w", row.ID, err)
			}
		}
	}
	return s.db.Migrator().DropColumn("message_sources", "raw")
}

func (s *EmailService) migrateLegacySource(ctx context.Context, sourceID, emailID string, raw []byte, envelopeFrom string, receivedAt time.Time) error {
	var email database.Email
	if err := s.db.WithContext(ctx).Where("id = ?", emailID).First(&email).Error; err != nil {
		return err
	}
	parsed, err := mailmime.ParseMessage(raw, envelopeFrom, nil)
	if err != nil {
		return err
	}
	var existing []database.Attachment
	references := make([]AttachmentReference, 0, len(parsed.Attachments))
	stagedIDs := make([]string, 0, len(parsed.Attachments))
	committed := false
	defer func() {
		if committed || s.files == nil {
			return
		}
		for _, id := range stagedIDs {
			_ = s.files.DeleteAttachment(context.Background(), id)
		}
	}()
	if err := s.db.WithContext(ctx).Where("email_id = ?", emailID).Order("position ASC").Find(&existing).Error; err != nil {
		return err
	}
	for position, attachment := range parsed.Attachments {
		if position < len(existing) {
			id := attachmentFileID(existing[position])
			if id != "" {
				references = append(references, AttachmentReference{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, StorageKey: id, File: existing[position].File, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Position: position})
				continue
			}
		}
		staged, stageErr := s.StageIncomingAttachments(ctx, email.MailboxID, []IncomingAttachment{{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Content: attachment.Content}})
		if stageErr != nil {
			return stageErr
		}
		references = append(references, staged[0])
		stagedIDs = append(stagedIDs, staged[0].StorageKey)
	}
	manifest := mailmime.Manifest{Version: mailmime.ManifestVersion, BodyType: parsed.BodyType, OmitContentType: parsed.OmitContentType}
	attachments := make(map[string]mailmime.Part, len(references))
	for _, reference := range references {
		part := mailmime.Part{AttachmentID: reference.StorageKey, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, ContentID: reference.ContentID, Disposition: reference.Disposition}
		manifest.Parts = append(manifest.Parts, part)
		attachments[reference.StorageKey] = part
	}
	recipients := make([]mailmime.Recipient, 0)
	var storedRecipients []database.Recipient
	if err := s.db.WithContext(ctx).Where("email_id = ?", emailID).Find(&storedRecipients).Error; err != nil {
		return err
	}
	for _, recipient := range storedRecipients {
		recipients = append(recipients, mailmime.Recipient{Address: recipient.Address, Name: recipient.Name, Kind: recipient.Kind})
	}
	source := mailmime.MessageSource{FromAddress: email.FromAddress, FromName: email.FromName, Subject: email.Subject, Body: parsed.Body, BodyType: parsed.BodyType, Recipients: recipients, Manifest: manifest, Attachments: attachments, Source: byteStoreSource{store: s.files}}
	wireSize, err := mailmime.Count(ctx, source)
	if err != nil {
		return err
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append(manifestBytes, []byte(fmt.Sprintf("\x00%d", wireSize))...))
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for position, reference := range references {
			id := reference.StorageKey
			file := reference.File
			var attachment database.Attachment
			query := tx.Where("email_id = ? AND position = ?", emailID, position).First(&attachment)
			if errors.Is(query.Error, gorm.ErrRecordNotFound) {
				if err := tx.Create(&database.Attachment{EmailID: emailID, Position: position, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, StorageKey: &id, File: file, ContentID: reference.ContentID, Disposition: reference.Disposition}).Error; err != nil {
					return err
				}
			} else if query.Error != nil {
				return query.Error
			} else if err := tx.Model(&attachment).Updates(map[string]any{"filename": reference.Filename, "mime_type": reference.MimeType, "size": reference.Size, "storage_key": id, "file": file, "content_id": reference.ContentID, "disposition": reference.Disposition}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&database.MessageSource{}).Where("id = ?", sourceID).Updates(map[string]any{"manifest": datatypes.JSON(manifestBytes), "wire_size_bytes": wireSize, "sha256": fmt.Sprintf("%x", sum[:]), "received_at": receivedAt}).Error
	}); err != nil {
		return err
	}
	for _, part := range manifest.Parts {
		reader, openErr := s.files.OpenAttachment(ctx, part.AttachmentID)
		if openErr != nil {
			return openErr
		}
		if closeErr := reader.Content.Close(); closeErr != nil {
			return closeErr
		}
	}
	committed = true
	return nil
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
		EmailID       string
		UID           uint32
		Flags         datatypes.JSON
		ModSeq        uint64
		WireSizeBytes int64
		Subject       string
		Body          string
	}
	var rows []row
	if err := s.db.WithContext(ctx).Table("folder_messages").
		Select("folder_messages.email_id, folder_messages.uid, folder_messages.flags, folder_messages.mod_seq, message_sources.wire_size_bytes, emails.subject, emails.body").
		Joins("JOIN message_sources ON message_sources.email_id = folder_messages.email_id").
		Joins("JOIN emails ON emails.id = folder_messages.email_id").
		Where("folder_messages.folder_id = ?", folder.ID).Order("folder_messages.uid").Scan(&rows).Error; err != nil {
		return nil, nil, err
	}
	items := make([]ProtocolMessage, 0, len(rows))
	for _, row := range rows {
		var flags []string
		_ = json.Unmarshal(row.Flags, &flags)
		items = append(items, ProtocolMessage{EmailID: row.EmailID, UID: row.UID, Flags: flags, RFC822Size: row.WireSizeBytes, Subject: row.Subject, Body: row.Body, ModSeq: row.ModSeq})
	}
	return items, &folder, nil
}

// OpenProtocolMessage reloads the indexed message and returns a replayable
// source. Every attachment Open call creates a new DysonFS stream.
func (s *EmailService) OpenProtocolMessage(ctx context.Context, emailID string) (mailmime.MessageSource, error) {
	if strings.TrimSpace(emailID) == "" {
		return mailmime.MessageSource{}, fmt.Errorf("email_id is required")
	}
	var email database.Email
	if err := s.db.WithContext(ctx).Preload("Recipients").Preload("Attachments").Where("id = ?", emailID).First(&email).Error; err != nil {
		return mailmime.MessageSource{}, err
	}
	var stored database.MessageSource
	if err := s.db.WithContext(ctx).Where("email_id = ?", emailID).First(&stored).Error; err != nil {
		return mailmime.MessageSource{}, err
	}
	var manifest mailmime.Manifest
	if err := json.Unmarshal(stored.Manifest, &manifest); err != nil {
		return mailmime.MessageSource{}, fmt.Errorf("decode message manifest: %w", err)
	}
	attachments := make(map[string]mailmime.Part, len(email.Attachments))
	for _, attachment := range email.Attachments {
		id := attachmentFileID(attachment)
		if id == "" {
			return mailmime.MessageSource{}, fmt.Errorf("attachment %s has no DysonFS file ID", attachment.ID)
		}
		attachments[id] = mailmime.Part{AttachmentID: id, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition}
	}
	recipients := make([]mailmime.Recipient, 0, len(email.Recipients))
	for _, recipient := range email.Recipients {
		recipients = append(recipients, mailmime.Recipient{Address: recipient.Address, Name: recipient.Name, Kind: recipient.Kind})
	}
	return mailmime.MessageSource{
		FromAddress: email.FromAddress,
		FromName:    email.FromName,
		Subject:     email.Subject,
		Body:        email.Body,
		BodyType:    email.ContentType,
		Recipients:  recipients,
		Manifest:    manifest,
		Attachments: attachments,
		Source:      byteStoreSource{store: s.files},
	}, nil
}

func attachmentFileID(attachment database.Attachment) string {
	if attachment.StorageKey != nil && strings.TrimSpace(*attachment.StorageKey) != "" {
		return strings.TrimSpace(*attachment.StorageKey)
	}
	if attachment.File != nil {
		return strings.TrimSpace(attachment.File.ID)
	}
	return ""
}

type byteStoreSource struct{ store filesystem.ByteStore }

func (source byteStoreSource) Open(ctx context.Context, fileID string) (mailmime.AttachmentReader, error) {
	if source.store == nil {
		return mailmime.AttachmentReader{}, errors.New("attachment byte store is not configured")
	}
	reader, err := source.store.OpenAttachment(ctx, fileID)
	if err != nil {
		return mailmime.AttachmentReader{}, err
	}
	return mailmime.AttachmentReader{
		Metadata: mailmime.AttachmentMetadata{ID: reader.File.ID, Name: reader.File.Name, MimeType: reader.File.MimeType, Size: reader.File.Size, Hash: reader.File.Hash},
		Content:  reader.Content,
	}, nil
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

// AppendProtocolMessage stores a client-supplied RFC 5322 message into a
// protocol folder. Parsed headers and MIME parts feed the normalized HTTP and
// protocol indexes; protocol retrieval renders the canonical normalized form.
// It returns the assigned UID and mod-sequence.
func (s *EmailService) AppendProtocolMessage(ctx context.Context, mailboxID, folderName string, raw []byte, flags []string, date time.Time) (uint32, uint64, error) {
	if strings.TrimSpace(mailboxID) == "" {
		return 0, 0, fmt.Errorf("mailbox_id is required")
	}
	folderName = strings.TrimSpace(folderName)
	if folderName == "" {
		return 0, 0, fmt.Errorf("folder name is required")
	}
	if len(raw) == 0 {
		return 0, 0, fmt.Errorf("message source is required")
	}
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", mailboxID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	if date.IsZero() {
		date = time.Now()
	}

	parsed, err := mailmime.ParseMessage(raw, mailbox.Address, nil)
	if err != nil {
		return 0, 0, err
	}
	incoming := make([]IncomingAttachment, 0, len(parsed.Attachments))
	for _, attachment := range parsed.Attachments {
		incoming = append(incoming, IncomingAttachment{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition, Content: attachment.Content})
	}
	staged, err := s.StageIncomingAttachments(ctx, mailbox.ID, incoming)
	if err != nil {
		return 0, 0, err
	}
	threadID := database.NewID()
	httpFolder := httpFolderName(folderName)
	email := database.Email{
		AccountID: mailbox.AccountID, MailboxID: mailbox.ID, ThreadID: &threadID,
		Subject: mailtext.ToValidUTF8(parsed.Subject), Body: mailtext.ToValidUTF8(parsed.Body),
		FromAddress: mailtext.ToValidUTF8(parsed.FromAddress), FromName: mailtext.ToValidUTF8(parsed.FromName),
		Folder: httpFolder, ContentType: mailtext.ToValidUTF8(parsed.BodyType),
		OmitContentType: parsed.OmitContentType,
		IsDraft:         httpFolder == folderDrafts || hasSystemFlag(flags, `\Draft`),
		IsRead:          hasSystemFlag(flags, `\Seen`), SentAt: &date,
		RawSizeBytes:   rawStringSize(parsed.Subject, parsed.Body, parsed.FromAddress, parsed.FromName),
		DeliveryStatus: "draft",
	}
	if httpFolder != folderDrafts {
		email.DeliveryStatus = ""
	}
	if parsed.ID != "" {
		email.MessageID = &parsed.ID
	}
	var uid uint32
	var modSeq uint64
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&email).Error; err != nil {
			return err
		}
		for _, recipient := range parsed.To {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: mailtext.ToValidUTF8(recipient.Address), Name: mailtext.ToValidUTF8(recipient.Name), Kind: normalizeKind(recipient.Kind, "to")}).Error; err != nil {
				return err
			}
		}
		for _, recipient := range parsed.Cc {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: mailtext.ToValidUTF8(recipient.Address), Name: mailtext.ToValidUTF8(recipient.Name), Kind: normalizeKind(recipient.Kind, "cc")}).Error; err != nil {
				return err
			}
		}
		for position, reference := range staged {
			id := reference.StorageKey
			if err := tx.Create(&database.Attachment{EmailID: email.ID, Position: position, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, StorageKey: &id, File: reference.File, ContentID: reference.ContentID, Disposition: reference.Disposition}).Error; err != nil {
				return err
			}
		}
		if err := s.storeProtocolSourceTx(tx, &email, nil, ""); err != nil {
			return err
		}
		uid, modSeq, err = s.appendFolderMembershipTx(tx, mailbox.ID, folderName, email.ID, flags, date)
		return err
	})
	if err != nil {
		for _, reference := range staged {
			if s.files != nil && reference.StorageKey != "" {
				_ = s.files.DeleteAttachment(context.Background(), reference.StorageKey)
			}
		}
		return 0, 0, err
	}
	s.publishMailEvent(ctx, email.AccountID.String(), "mail.created", &email)
	return uid, modSeq, nil
}

// appendFolderMembershipTx allocates the next UID and mod-sequence and links an
// existing message into a folder with the flags supplied by APPEND.
func (s *EmailService) appendFolderMembershipTx(tx *gorm.DB, mailboxID, name, emailID string, flags []string, date time.Time) (uint32, uint64, error) {
	if err := s.ensureProtocolFoldersTx(tx, mailboxID); err != nil {
		return 0, 0, err
	}
	var folder database.MailFolder
	if err := tx.Clauses(clauseLock()).Where("mailbox_id = ? AND name = ?", mailboxID, name).First(&folder).Error; err != nil {
		return 0, 0, err
	}
	uid, modSeq := folder.NextUID, folder.HighestModSeq+1
	row := database.FolderMessage{FolderID: folder.ID, EmailID: emailID, UID: uid, Flags: datatypes.JSON(marshalFlags(mergeFlags(nil, flags, "add"))), ModSeq: modSeq, CreatedAt: date}
	if err := tx.Create(&row).Error; err != nil {
		return 0, 0, err
	}
	if err := tx.Model(&database.MailFolder{}).Where("id = ?", folder.ID).Updates(map[string]any{"next_uid": uid + 1, "highest_mod_seq": modSeq}).Error; err != nil {
		return 0, 0, err
	}
	return uid, modSeq, nil
}

// httpFolderName maps an IMAP folder name back to the HTTP mailbox folder.
func httpFolderName(folder string) string {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "sent":
		return folderSent
	case "drafts":
		return folderDrafts
	case "spam":
		return folderSpam
	case "trash":
		return folderTrash
	case "archive":
		return folderArchive
	default:
		return folderInbox
	}
}

func hasSystemFlag(flags []string, flag string) bool {
	for _, value := range flags {
		if strings.EqualFold(value, flag) {
			return true
		}
	}
	return false
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

func (s *EmailService) storeProtocolSourceTx(tx *gorm.DB, email *database.Email, _ []byte, envelopeFrom string) error {
	var attachments []database.Attachment
	if err := tx.Where("email_id = ?", email.ID).Order("position ASC").Find(&attachments).Error; err != nil {
		return err
	}
	manifest := mailmime.Manifest{Version: mailmime.ManifestVersion, BodyType: normalizeContentType(email.ContentType), OmitContentType: email.OmitContentType}
	attachmentParts := make(map[string]mailmime.Part, len(attachments))
	for _, attachment := range attachments {
		id := attachmentFileID(attachment)
		if id == "" {
			return fmt.Errorf("attachment %s has no DysonFS file ID", attachment.ID)
		}
		part := mailmime.Part{AttachmentID: id, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition}
		manifest.Parts = append(manifest.Parts, part)
		attachmentParts[id] = part
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	var recipients []database.Recipient
	if err := tx.Where("email_id = ?", email.ID).Find(&recipients).Error; err != nil {
		return err
	}
	mimeRecipients := make([]mailmime.Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		mimeRecipients = append(mimeRecipients, mailmime.Recipient{Address: recipient.Address, Name: recipient.Name, Kind: recipient.Kind})
	}
	wireSize, err := mailmime.Count(context.Background(), mailmime.MessageSource{
		FromAddress: email.FromAddress, FromName: email.FromName, Subject: email.Subject,
		Body: email.Body, BodyType: email.ContentType, Recipients: mimeRecipients,
		Manifest: manifest, Attachments: attachmentParts, Source: byteStoreSource{store: s.files},
	})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append(manifestBytes, []byte(fmt.Sprintf("\x00%d", wireSize))...))
	return tx.Create(&database.MessageSource{EmailID: email.ID, Manifest: datatypes.JSON(manifestBytes), WireSizeBytes: wireSize, SHA256: fmt.Sprintf("%x", sum[:]), EnvelopeFrom: envelopeFrom, ReceivedAt: time.Now()}).Error
}
