package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AddressSuggestion is an address seen in this account's mail history.
type AddressSuggestion struct {
	Address          string    `json:"address"`
	Name             string    `json:"name"`
	LastSeen         time.Time `json:"last_seen"`
	InteractionCount int64     `json:"interaction_count"`
	AvatarURL        string    `json:"avatar_url"`
	AvatarSource     string    `json:"avatar_source"`
	GravatarURL      string    `json:"gravatar_url"`
	BIMIURL          string    `json:"bimi_url,omitempty"`
	WorkspaceID      string    `json:"workspace_id,omitempty"`
	Alias            bool      `json:"alias,omitempty"`
}

type addressHistoryRow struct {
	Address          string `gorm:"column:address"`
	Name             string `gorm:"column:name"`
	LastSeen         string `gorm:"column:last_seen"`
	InteractionCount int64  `gorm:"column:interaction_count"`
	WorkspaceID      string `gorm:"column:workspace_id"`
	Alias            int64  `gorm:"column:alias"`
}

// ListSenders returns distinct addresses that have sent non-archived mail to
// the account, ordered by latest activity. Search is case-insensitive.
func (s *EmailService) ListSenders(ctx context.Context, accountID uuid.UUID, query string, take int) ([]AddressSuggestion, error) {
	query = strings.TrimSpace(query)
	return s.listAddressHistory(ctx, accountID, query, take, `
		SELECT lower(trim(e.from_address)) AS address,
			max(COALESCE(NULLIF(trim(e.from_name), ''), NULLIF(ma.name, ''), mb.name, '')) AS name,
			max(e.created_at) AS last_seen, count(*) AS interaction_count,
			max(COALESCE(mb.workspace_id, '')) AS workspace_id,
			max(CASE WHEN ma.id IS NULL THEN 0 ELSE 1 END) AS alias
		FROM emails e
		LEFT JOIN mailbox_aliases ma ON lower(ma.address) = lower(trim(e.from_address))
			AND EXISTS (SELECT 1 FROM mailboxes owner WHERE owner.id = ma.mailbox_id AND owner.account_id = e.account_id)
		LEFT JOIN mailboxes mb ON mb.account_id = e.account_id AND (mb.id = ma.mailbox_id OR lower(mb.address) = lower(trim(e.from_address)))
		WHERE e.account_id = ? AND e.archived_at IS NULL AND e.is_draft = false AND e.is_dmarc_intake = false
			AND e.from_address <> '' AND e.folder NOT IN ('trash', 'spam', 'sent', 'drafts')
			AND (? = '' OR lower(e.from_address) LIKE ? OR lower(e.from_name) LIKE ?)
		GROUP BY lower(trim(e.from_address))
	`)
}

// ListContacts returns distinct recipients of sent mail for address
// autocomplete, ordered by latest activity. Search is case-insensitive.
func (s *EmailService) ListContacts(ctx context.Context, accountID uuid.UUID, query string, take int) ([]AddressSuggestion, error) {
	query = strings.TrimSpace(query)
	return s.listAddressHistory(ctx, accountID, query, take, `
		SELECT lower(trim(r.address)) AS address,
			max(COALESCE(NULLIF(trim(r.name), ''), NULLIF(ma.name, ''), mb.name, '')) AS name,
			max(e.created_at) AS last_seen, count(*) AS interaction_count,
			max(COALESCE(mb.workspace_id, '')) AS workspace_id,
			max(CASE WHEN ma.id IS NULL THEN 0 ELSE 1 END) AS alias
		FROM recipients r
		JOIN emails e ON e.id = r.email_id
		LEFT JOIN mailbox_aliases ma ON lower(ma.address) = lower(trim(r.address))
			AND EXISTS (SELECT 1 FROM mailboxes owner WHERE owner.id = ma.mailbox_id AND owner.account_id = e.account_id)
		LEFT JOIN mailboxes mb ON mb.account_id = e.account_id AND (mb.id = ma.mailbox_id OR lower(mb.address) = lower(trim(r.address)))
		WHERE e.account_id = ? AND e.archived_at IS NULL AND e.is_draft = false
			AND e.folder = 'sent' AND r.address <> ''
			AND (? = '' OR lower(r.address) LIKE ? OR lower(r.name) LIKE ?)
		GROUP BY lower(trim(r.address))
	`)
}

func (s *EmailService) listAddressHistory(ctx context.Context, accountID uuid.UUID, query string, take int, groupedQuery string) ([]AddressSuggestion, error) {
	if take <= 0 {
		take = 20
	}
	if take > 50 {
		take = 50
	}
	pattern := "%" + strings.ToLower(query) + "%"
	var rows []addressHistoryRow
	sql := fmt.Sprintf("SELECT address, name, last_seen, interaction_count, workspace_id, alias FROM (%s) AS history WHERE ? = '' OR address LIKE ? OR lower(name) LIKE ? ORDER BY last_seen DESC, address ASC LIMIT ?", groupedQuery)
	if err := s.db.WithContext(ctx).Raw(sql, accountID, query, pattern, pattern, query, pattern, pattern, take).Scan(&rows).Error; err != nil {
		return nil, err
	}

	results := make([]AddressSuggestion, len(rows))
	bimiByDomain := make(map[string]string)
	workspaceAvatarByID := make(map[string]string)
	for i, row := range rows {
		email := strings.ToLower(strings.TrimSpace(row.Address))
		hash := md5.Sum([]byte(email)) // Gravatar's documented email-address identifier.
		gravatar := "https://www.gravatar.com/avatar/" + hex.EncodeToString(hash[:]) + "?d=identicon&s=80"
		result := AddressSuggestion{
			Address: email, Name: row.Name, LastSeen: parseHistoryTime(row.LastSeen),
			InteractionCount: row.InteractionCount, AvatarURL: gravatar,
			AvatarSource: "gravatar", GravatarURL: gravatar,
			WorkspaceID: row.WorkspaceID, Alias: row.Alias != 0,
		}
		if strings.HasSuffix(email, "@solarpass.one") && row.WorkspaceID != "" {
			avatar, lookedUp := workspaceAvatarByID[row.WorkspaceID]
			if !lookedUp {
				avatar = s.lookupWorkspaceAvatar(ctx, row.WorkspaceID)
				workspaceAvatarByID[row.WorkspaceID] = avatar
			}
			if avatar != "" {
				result.AvatarURL = avatar
				result.AvatarSource = "workspace"
			}
		}
		if result.AvatarSource != "workspace" {
			if at := strings.LastIndexByte(email, '@'); at >= 0 && at < len(email)-1 {
				domain := email[at+1:]
				bimiURL, lookedUp := bimiByDomain[domain]
				if !lookedUp {
					bimiURL = s.lookupBIMILogo(ctx, domain)
					bimiByDomain[domain] = bimiURL
				}
				if bimiURL != "" {
					result.BIMIURL = bimiURL
					result.AvatarURL = bimiURL
					result.AvatarSource = "bimi"
				}
			}
		}
		results[i] = result
	}
	return results, nil
}

func parseHistoryTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed
	}
	parsed, _ = time.Parse("2006-01-02 15:04:05-07:00", value)
	return parsed
}
func (s *EmailService) lookupWorkspaceAvatar(ctx context.Context, workspaceID string) string {
	provider, ok := s.workspace.(interface {
		WorkspaceAvatar(context.Context, string) (string, error)
	})
	if !ok {
		return ""
	}
	avatar, err := provider.WorkspaceAvatar(ctx, workspaceID)
	if err != nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(avatar))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return parsed.String()
}

func (s *EmailService) lookupBIMILogo(ctx context.Context, domain string) string {
	if s.dns == nil || net.ParseIP(domain) != nil || domain == "" {
		return ""
	}
	records, err := s.dns.LookupTXT(ctx, "default._bimi."+domain)
	if err != nil {
		return ""
	}
	for _, record := range records {
		for _, field := range strings.Split(record, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "l") {
				continue
			}
			logo, err := url.Parse(strings.TrimSpace(value))
			if err == nil && logo.Scheme == "https" && logo.Host != "" && logo.User == nil {
				return logo.String()
			}
		}
	}
	return ""
}
