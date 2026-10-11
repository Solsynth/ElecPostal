package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

// scopedKeywordPrefix marks the keyword namespace clients own: `$Junk`,
// `$NotJunk`, and friends carry meaning the server must not reinterpret as a
// user tag.
const scopedKeywordPrefix = "$"

// SplitProtocolFlags separates a client's flag list into the names that belong
// to the membership's own flag set and the names that are tag keywords.
// `\`-prefixed system flags and `$`-prefixed scoped keywords stay with the
// membership; every other name names a tag, which a client creates by using it.
func SplitProtocolFlags(flags []string) (stored, keywords []string) {
	for _, flag := range flags {
		flag = strings.TrimSpace(flag)
		if flag == "" {
			continue
		}
		if strings.HasPrefix(flag, `\`) || strings.HasPrefix(flag, scopedKeywordPrefix) {
			stored = append(stored, flag)
			continue
		}
		keywords = append(keywords, flag)
	}
	return stored, keywords
}

// IsIMAPKeyword reports whether a tag name can travel as an IMAP keyword. RFC
// 3501 defines flag-keyword as an atom, so the ASCII specials and the control
// characters cannot appear. Octets at or above 0x80 are accepted because every
// deployed server carries UTF-8 tag names in practice.
func IsIMAPKeyword(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for i := range len(name) {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return false
		} else if c < 0x80 {
			switch c {
			case '(', ')', '{', ' ', '%', '*', '"', '\\', ']':
				return false
			}
		}
	}
	return true
}

// KeywordsForEmails returns the tag names each listed message carries, keyed by
// email ID. Names starting with `$` are reserved for scoped protocol keywords
// and never project a tag.
func (s *EmailService) KeywordsForEmails(ctx context.Context, emailIDs []string) (map[string][]string, error) {
	return s.keywordsForEmails(s.db.WithContext(ctx), emailIDs)
}

// protocolKeywordsByEmail is the IMAP view of KeywordsForEmails: names IMAP
// cannot carry are dropped rather than sent to a client that would misparse
// them.
func (s *EmailService) protocolKeywordsByEmail(ctx context.Context, emailIDs []string) (map[string][]string, error) {
	all, err := s.keywordsForEmails(s.db.WithContext(ctx), emailIDs)
	if err != nil {
		return nil, err
	}
	for id, names := range all {
		projected := make([]string, 0, len(names))
		for _, name := range names {
			if IsIMAPKeyword(name) {
				projected = append(projected, name)
			}
		}
		all[id] = projected
	}
	return all, nil
}

func (s *EmailService) keywordsForEmails(db *gorm.DB, emailIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(emailIDs) == 0 {
		return out, nil
	}
	type row struct {
		EmailID string
		Name    string
	}
	var rows []row
	if err := db.Table("email_label_mappings").
		Select("email_label_mappings.email_id, email_labels.name").
		Joins("JOIN email_labels ON email_labels.id = email_label_mappings.label_id").
		Where("email_label_mappings.email_id IN ? AND email_labels.deleted_at IS NULL", emailIDs).
		Order("email_labels.name").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if strings.HasPrefix(row.Name, scopedKeywordPrefix) {
			continue
		}
		out[row.EmailID] = append(out[row.EmailID], row.Name)
	}
	return out, nil
}

// SetProtocolKeywords applies a keyword change to the messages of one protocol
// folder. mode is "add", "remove", or "replace". Keywords name account tags: a
// keyword a client stores creates that tag on first use, and removing it drops
// the tag from those messages only — emptying a tag never deletes it, because
// the tag outlives the mail that carried it.
//
// replace makes the listed keywords the message's whole tag set, but only
// among the names the calling surface can carry: a client cannot be authoritative
// about a tag it was never able to observe, so IMAP leaves tags whose names are
// not valid keywords untouched.
func (s *EmailService) SetProtocolKeywords(ctx context.Context, mailboxID, folderName string, emailIDs []string, keywords []string, mode string) error {
	if len(emailIDs) == 0 {
		return nil
	}
	if names := normalizeKeywords(keywords); len(names) == 0 && mode != "replace" {
		return nil
	}
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", mailboxID).First(&mailbox).Error; err != nil {
		return ErrNotFound
	}
	var folder database.MailFolder
	if err := s.db.WithContext(ctx).Where("mailbox_id = ? AND name = ?", mailboxID, folderName).First(&folder).Error; err != nil {
		return ErrNotFound
	}
	members, err := s.folderMembers(ctx, folder.ID, emailIDs)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.setProtocolKeywordsTx(tx, mailbox.AccountID, members, normalizeKeywords(keywords), mode)
	})
}

// normalizeKeywords trims the names, drops the ones that cannot name a tag, and
// folds case because protocol keywords are case-insensitive.
func normalizeKeywords(keywords []string) []string {
	names := make([]string, 0, len(keywords))
	seen := make(map[string]bool, len(keywords))
	for _, keyword := range keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" || strings.HasPrefix(keyword, `\`) || strings.HasPrefix(keyword, scopedKeywordPrefix) {
			continue
		}
		lower := strings.ToLower(keyword)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		names = append(names, keyword)
	}
	return names
}

// folderMembers narrows the requested message ids to the ones that really sit in
// the folder, so a stale client request cannot tag mail it never saw.
func (s *EmailService) folderMembers(ctx context.Context, folderID string, emailIDs []string) ([]string, error) {
	var members []string
	if err := s.db.WithContext(ctx).Table("folder_messages").
		Where("folder_id = ? AND email_id IN ?", folderID, emailIDs).
		Pluck("email_id", &members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

// setProtocolKeywordsTx applies the keyword change inside the caller's
// transaction.
func (s *EmailService) setProtocolKeywordsTx(tx *gorm.DB, accountID uuid.UUID, members, names []string, mode string) error {
	switch mode {
	case "remove":
		return s.detachKeywordsTx(tx, accountID, members, names)
	case "replace":
		if err := s.releaseKeywordsTx(tx, accountID, members, names); err != nil {
			return err
		}
		return s.attachKeywordsTx(tx, accountID, members, names)
	default:
		return s.attachKeywordsTx(tx, accountID, members, names)
	}
}

// attachKeywordsTx files the named tags onto the messages, creating a tag the
// account does not have yet.
func (s *EmailService) attachKeywordsTx(tx *gorm.DB, accountID uuid.UUID, members, names []string) error {
	for _, name := range names {
		label, err := findOrCreateLabelTx(tx, accountID, name)
		if err != nil {
			return err
		}
		for _, emailID := range members {
			mapping := database.EmailLabelMapping{EmailID: emailID, LabelID: label.ID}
			if err := tx.Where("email_id = ? AND label_id = ?", emailID, label.ID).FirstOrCreate(&mapping).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// detachKeywordsTx drops the named tags from the messages without touching the
// tags themselves.
func (s *EmailService) detachKeywordsTx(tx *gorm.DB, accountID uuid.UUID, members, names []string) error {
	ids, err := s.labelIDsTx(tx, accountID, names)
	if err != nil || len(ids) == 0 {
		return err
	}
	return tx.Where("email_id IN ? AND label_id IN ?", members, ids).Delete(&database.EmailLabelMapping{}).Error
}

// releaseKeywordsTx drops every tag the caller could have observed and did not
// list, which is what a replace-style STORE asks for.
func (s *EmailService) releaseKeywordsTx(tx *gorm.DB, accountID uuid.UUID, members, names []string) error {
	type row struct {
		LabelID string
		Name    string
	}
	var rows []row
	if err := tx.Table("email_label_mappings").
		Select("email_label_mappings.label_id, email_labels.name").
		Joins("JOIN email_labels ON email_labels.id = email_label_mappings.label_id").
		Where("email_label_mappings.email_id IN ? AND email_labels.account_id = ? AND email_labels.deleted_at IS NULL", members, accountID).
		Scan(&rows).Error; err != nil {
		return err
	}
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[strings.ToLower(name)] = true
	}
	drop := map[string]bool{}
	for _, row := range rows {
		if !IsIMAPKeyword(row.Name) || keep[strings.ToLower(row.Name)] {
			continue
		}
		drop[row.LabelID] = true
	}
	if len(drop) == 0 {
		return nil
	}
	ids := make([]string, 0, len(drop))
	for id := range drop {
		ids = append(ids, id)
	}
	return tx.Where("email_id IN ? AND label_id IN ?", members, ids).Delete(&database.EmailLabelMapping{}).Error
}

func (s *EmailService) labelIDsTx(tx *gorm.DB, accountID uuid.UUID, names []string) ([]string, error) {
	var ids []string
	if len(names) == 0 {
		return ids, nil
	}
	lowered := make([]string, 0, len(names))
	for _, name := range names {
		lowered = append(lowered, strings.ToLower(name))
	}
	// Tags carry no case-insensitive uniqueness constraint, so a duplicate name
	// resolves to every row it matches.
	if err := tx.Model(&database.EmailLabel{}).
		Where("account_id = ? AND LOWER(name) IN ?", accountID, lowered).
		Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// findOrCreateLabelTx resolves the account tag a keyword names, creating it the
// first time a client uses the keyword. Resolution ignores case, because
// protocol keywords are case-insensitive. The lookup reports a miss through
// RowsAffected instead of an error, so a first use is not logged as a failed
// query.
func findOrCreateLabelTx(tx *gorm.DB, accountID uuid.UUID, name string) (database.EmailLabel, error) {
	var label database.EmailLabel
	result := tx.Where("account_id = ? AND LOWER(name) = ?", accountID, strings.ToLower(name)).Limit(1).Find(&label)
	if result.Error != nil {
		return label, result.Error
	}
	if result.RowsAffected > 0 {
		return label, nil
	}
	label = database.EmailLabel{AccountID: accountID, Name: name}
	if err := tx.Create(&label).Error; err != nil {
		return label, err
	}
	return label, nil
}
