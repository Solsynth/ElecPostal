// Package jmap exposes the ElecPostal mail store through JMAP (RFC 8620 and
// RFC 8621). It deliberately uses the IMAP folder/message tables as its
// source of truth so JMAP, IMAP, and POP3 all see the same messages.
package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

const (
	coreCapability = "urn:ietf:params:jmap:core"
	mailCapability = "urn:ietf:params:jmap:mail"
)

// Handler serves the JMAP session resource and API endpoint.
type Handler struct{ mail *service.EmailService }

func New(mail *service.EmailService) *Handler { return &Handler{mail: mail} }

// Session returns the authenticated user's JMAP account map. Account IDs are
// ElecPostal mailbox IDs; this makes a JMAP account correspond to one address.
func (h *Handler) Session(c *gin.Context) {
	accountID, ok := accountID(c)
	if !ok {
		return
	}
	mailboxes, err := h.mail.ListMailboxes(c.Request.Context(), accountID, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"type": "serverFail"})
		return
	}
	accounts := gin.H{}
	primary := ""
	for _, mailbox := range mailboxes {
		if primary == "" || mailbox.IsDefault {
			primary = mailbox.ID
		}
		accounts[mailbox.ID] = gin.H{"name": mailbox.Name, "isPersonal": true, "isReadOnly": false, "accountCapabilities": gin.H{mailCapability: gin.H{}}}
	}
	base := requestBaseURL(c)
	c.JSON(http.StatusOK, gin.H{
		"capabilities": gin.H{coreCapability: gin.H{"maxSizeRequest": 10000000, "maxConcurrentRequests": 4, "maxCallsInRequest": 16, "maxObjectsInGet": 500, "maxObjectsInSet": 500, "collationAlgorithms": []string{"i;unicode-casemap"}}, mailCapability: gin.H{"maxMailboxesPerEmail": 1, "maxSizeAttachmentsPerEmail": 0, "emailQuerySortOptions": []string{"receivedAt", "sentAt", "size", "from", "subject"}}},
		"accounts":     accounts, "primaryAccounts": gin.H{mailCapability: primary},
		"apiUrl": base + "/jmap/api", "downloadUrl": base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl": base + "/jmap/upload/{accountId}/", "eventSourceUrl": base + "/jmap/eventsource/", "state": h.state(c.Request.Context(), primary),
	})
}

// API processes a JMAP methodCalls request. Calls are handled sequentially as
// required by RFC 8620. Result references are intentionally rejected rather
// than being interpreted as arbitrary JSON paths.
func (h *Handler) API(c *gin.Context) {
	accountID, ok := accountID(c)
	if !ok {
		return
	}
	var request struct {
		Using       []string `json:"using"`
		MethodCalls [][]any  `json:"methodCalls"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || len(request.MethodCalls) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"type": "invalidArguments", "description": "methodCalls is required"})
		return
	}
	responses := make([][]any, 0, len(request.MethodCalls))
	for _, call := range request.MethodCalls {
		if len(call) != 3 {
			responses = append(responses, []any{"error", gin.H{"type": "invalidArguments"}, ""})
			continue
		}
		name, nameOK := call[0].(string)
		callID, idOK := call[2].(string)
		args, argsOK := call[1].(map[string]any)
		if !nameOK || !idOK || !argsOK {
			responses = append(responses, []any{"error", gin.H{"type": "invalidArguments"}, ""})
			continue
		}
		responseName, result := h.method(c, accountID, name, args)
		responses = append(responses, []any{responseName, result, callID})
	}
	c.JSON(http.StatusOK, gin.H{"methodResponses": responses, "sessionState": h.state(c.Request.Context(), "")})
}

func (h *Handler) method(c *gin.Context, owner uuid.UUID, name string, args map[string]any) (string, any) {
	if name == "Core/echo" {
		return name, args
	}
	mailboxID, err := h.resolveAccount(c, owner, stringArg(args, "accountId"))
	if err != nil {
		return "error", jmapError(err)
	}
	switch name {
	case "Mailbox/get":
		return name, h.mailboxGet(c, mailboxID, args)
	case "Mailbox/query":
		return name, h.mailboxQuery(c, mailboxID)
	case "Email/get":
		return name, h.emailGet(c, mailboxID, args)
	case "Email/query":
		return name, h.emailQuery(c, mailboxID, args)
	case "Email/set":
		return name, h.emailSet(c, owner, mailboxID, args)
	default:
		return "error", gin.H{"type": "unknownMethod", "description": "unsupported JMAP method " + name}
	}
}

func (h *Handler) resolveAccount(c *gin.Context, owner uuid.UUID, id string) (string, error) {
	mailboxes, err := h.mail.ListMailboxes(c.Request.Context(), owner, "")
	if err != nil {
		return "", err
	}
	if id == "" && len(mailboxes) == 1 {
		return mailboxes[0].ID, nil
	}
	for _, mailbox := range mailboxes {
		if mailbox.ID == id {
			return id, nil
		}
	}
	return "", service.ErrNotFound
}

func (h *Handler) mailboxGet(c *gin.Context, accountID string, args map[string]any) gin.H {
	folders, err := h.mail.ListProtocolFolders(c.Request.Context(), accountID)
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	wanted := idSet(args["ids"])
	list, notFound := make([]any, 0, len(folders)), []string{}
	for _, folder := range folders {
		if len(wanted) > 0 && !wanted[folder.ID] {
			continue
		}
		list = append(list, folderObject(folder))
	}
	for id := range wanted {
		found := false
		for _, f := range folders {
			if f.ID == id {
				found = true
				break
			}
		}
		if !found {
			notFound = append(notFound, id)
		}
	}
	return gin.H{"accountId": accountID, "state": h.state(c.Request.Context(), accountID), "list": list, "notFound": notFound}
}
func (h *Handler) mailboxQuery(c *gin.Context, accountID string) gin.H {
	folders, err := h.mail.ListProtocolFolders(c.Request.Context(), accountID)
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	ids := make([]string, 0, len(folders))
	for _, f := range folders {
		ids = append(ids, f.ID)
	}
	return gin.H{"accountId": accountID, "queryState": h.state(c.Request.Context(), accountID), "canCalculateChanges": false, "position": 0, "ids": ids, "total": len(ids)}
}

func (h *Handler) emailGet(c *gin.Context, accountID string, args map[string]any) gin.H {
	rows, err := h.rows(c, accountID)
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	wanted := idSet(args["ids"])
	list := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if len(wanted) > 0 && !wanted[row.Email.ID] {
			continue
		}
		seen[row.Email.ID] = true
		list = append(list, emailObject(row))
	}
	notFound := []string{}
	for id := range wanted {
		if !seen[id] {
			notFound = append(notFound, id)
		}
	}
	return gin.H{"accountId": accountID, "state": h.state(c.Request.Context(), accountID), "list": list, "notFound": notFound}
}
func (h *Handler) emailQuery(c *gin.Context, accountID string, args map[string]any) gin.H {
	rows, err := h.rows(c, accountID)
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	filter, _ := args["filter"].(map[string]any)
	inMailbox := stringArg(filter, "inMailbox")
	text := strings.ToLower(stringArg(filter, "text"))
	ids := []string{}
	for _, row := range rows {
		if inMailbox != "" && row.Folder.ID != inMailbox {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(row.Email.Subject+" "+row.Email.Body+" "+row.Email.FromAddress), text) {
			continue
		}
		ids = append(ids, row.Email.ID)
	}
	position := int(numberArg(args, "position"))
	limit := int(numberArg(args, "limit"))
	if position < 0 {
		position = 0
	}
	if position > len(ids) {
		position = len(ids)
	}
	end := len(ids)
	if limit > 0 && position+limit < end {
		end = position + limit
	}
	return gin.H{"accountId": accountID, "queryState": h.state(c.Request.Context(), accountID), "canCalculateChanges": false, "position": position, "ids": ids[position:end], "total": len(ids)}
}

func (h *Handler) emailSet(c *gin.Context, owner uuid.UUID, accountID string, args map[string]any) gin.H {
	updated, notUpdated := gin.H{}, gin.H{}
	for id, raw := range objectMap(args["update"]) {
		patch, ok := raw.(map[string]any)
		if !ok {
			notUpdated[id] = gin.H{"type": "invalidProperties"}
			continue
		}
		rows, err := h.rows(c, accountID)
		if err != nil {
			notUpdated[id] = gin.H{"type": "serverFail"}
			continue
		}
		var current *mailRow
		for i := range rows {
			if rows[i].Email.ID == id {
				current = &rows[i]
				break
			}
		}
		if current == nil {
			notUpdated[id] = gin.H{"type": "notFound"}
			continue
		}
		flags := current.Flags
		if value, exists := patch["keywords/$seen"]; exists {
			flags = setFlag(flags, "\\Seen", value == true)
		}
		if value, exists := patch["keywords/$flagged"]; exists {
			flags = setFlag(flags, "\\Flagged", value == true)
		}
		if _, err := h.mail.StoreProtocolFlags(c.Request.Context(), accountID, current.Folder.Name, []string{id}, flags, "replace", 0); err != nil {
			notUpdated[id] = jmapError(err)
			continue
		}
		if target := mailboxPatch(patch); target != "" && target != current.Folder.ID {
			dest, ok := h.folder(c, accountID, target)
			if !ok {
				notUpdated[id] = gin.H{"type": "invalidProperties"}
				continue
			}
			if err := h.mail.MoveProtocolMessages(c.Request.Context(), accountID, current.Folder.Name, dest.Name, []string{id}); err != nil {
				notUpdated[id] = jmapError(err)
				continue
			}
		}
		updated[id] = nil
	}
	destroyed, notDestroyed := []string{}, gin.H{}
	for _, id := range stringList(args["destroy"]) {
		rows, _ := h.rows(c, accountID)
		var cur *mailRow
		for i := range rows {
			if rows[i].Email.ID == id {
				cur = &rows[i]
				break
			}
		}
		if cur == nil {
			notDestroyed[id] = gin.H{"type": "notFound"}
			continue
		}
		if err := h.mail.MoveProtocolMessages(c.Request.Context(), accountID, cur.Folder.Name, "Trash", []string{id}); err != nil {
			notDestroyed[id] = jmapError(err)
			continue
		}
		destroyed = append(destroyed, id)
	}
	return gin.H{"accountId": accountID, "oldState": h.state(c.Request.Context(), accountID), "newState": h.state(c.Request.Context(), accountID), "updated": updated, "notUpdated": notUpdated, "destroyed": destroyed, "notDestroyed": notDestroyed}
}

type mailRow struct {
	Email  database.Email
	Folder database.MailFolder
	Flags  []string
}

func (h *Handler) rows(c *gin.Context, accountID string) ([]mailRow, error) {
	type row struct {
		database.Email
		FolderID   string
		FolderName string
		SpecialUse string
		Flags      databaseJSON
	}
	var values []row
	err := h.mail.DB().WithContext(c.Request.Context()).Table("folder_messages").Select("emails.*, mail_folders.id AS folder_id, mail_folders.name AS folder_name, mail_folders.special_use, folder_messages.flags").Joins("JOIN mail_folders ON mail_folders.id = folder_messages.folder_id").Joins("JOIN emails ON emails.id = folder_messages.email_id").Where("mail_folders.mailbox_id = ? AND emails.archived_at IS NULL", accountID).Order("emails.created_at DESC").Find(&values).Error
	if err != nil {
		return nil, err
	}
	out := make([]mailRow, 0, len(values))
	for _, v := range values {
		var flags []string
		_ = v.Flags.decode(&flags)
		email := v.Email
		if err := h.mail.DB().WithContext(c.Request.Context()).Preload("Recipients").Preload("Attachments").First(&email, "id = ?", email.ID).Error; err != nil {
			return nil, err
		}
		out = append(out, mailRow{Email: email, Folder: database.MailFolder{ID: v.FolderID, Name: v.FolderName, SpecialUse: v.SpecialUse}, Flags: flags})
	}
	return out, nil
}

// databaseJSON is a Scanner-compatible alias for JSON columns without adding a
// JMAP-specific persistence model.
type databaseJSON []byte

func (j *databaseJSON) Scan(value any) error {
	switch v := value.(type) {
	case []byte:
		*j = append((*j)[:0], v...)
	case string:
		*j = append((*j)[:0], v...)
	}
	return nil
}
func (j databaseJSON) decode(dst any) error { return json.Unmarshal(j, dst) }

func (h *Handler) folder(c *gin.Context, accountID, id string) (database.MailFolder, bool) {
	folders, err := h.mail.ListProtocolFolders(c.Request.Context(), accountID)
	if err != nil {
		return database.MailFolder{}, false
	}
	for _, f := range folders {
		if f.ID == id {
			return f, true
		}
	}
	return database.MailFolder{}, false
}
func folderObject(f database.MailFolder) gin.H {
	role := ""
	switch f.SpecialUse {
	case `\Inbox`:
		role = "inbox"
	case `\Sent`:
		role = "sent"
	case `\Drafts`:
		role = "drafts"
	case `\Junk`:
		role = "junk"
	case `\Trash`:
		role = "trash"
	case `\Archive`:
		role = "archive"
	}
	return gin.H{"id": f.ID, "name": f.Name, "parentId": nil, "role": role, "sortOrder": 0, "isSubscribed": f.Subscribed, "myRights": gin.H{"mayReadItems": true, "mayAddItems": false, "mayRemoveItems": true, "maySetSeen": true, "maySetKeywords": true, "mayCreateChild": false, "mayRename": false, "mayDelete": false, "maySubmit": true}}
}
func emailObject(r mailRow) gin.H {
	keywords := gin.H{}
	for _, f := range r.Flags {
		switch f {
		case "\\Seen":
			keywords["$seen"] = true
		case "\\Flagged":
			keywords["$flagged"] = true
		case "\\Answered":
			keywords["$answered"] = true
		case "\\Draft":
			keywords["$draft"] = true
		}
	}
	recipients := func(kind string) []any {
		out := []any{}
		for _, x := range r.Email.Recipients {
			if x.Kind == kind {
				out = append(out, gin.H{"email": x.Address, "name": x.Name})
			}
		}
		return out
	}
	preview := r.Email.Body
	if len(preview) > 256 {
		preview = preview[:256]
	}
	sentAt := r.Email.CreatedAt
	if r.Email.SentAt != nil {
		sentAt = *r.Email.SentAt
	}
	return gin.H{"id": r.Email.ID, "blobId": r.Email.ID, "threadId": threadID(r.Email), "mailboxIds": gin.H{r.Folder.ID: true}, "keywords": keywords, "size": r.Email.RawSizeBytes, "receivedAt": r.Email.CreatedAt.Format(time.RFC3339), "sentAt": sentAt.Format(time.RFC3339), "from": []any{gin.H{"email": r.Email.FromAddress, "name": r.Email.FromName}}, "to": recipients("to"), "cc": recipients("cc"), "bcc": recipients("bcc"), "subject": r.Email.Subject, "preview": preview, "hasAttachment": len(r.Email.Attachments) > 0}
}
func threadID(e database.Email) string {
	if e.ThreadID != nil {
		return *e.ThreadID
	}
	return e.ID
}
func (h *Handler) state(ctx context.Context, id string) string {
	if id == "" {
		return "session"
	}
	folders, err := h.mail.ListProtocolFolders(ctx, id)
	if err != nil {
		return "unavailable"
	}
	// The counters are advanced in the same transaction as every folder
	// membership or flag mutation. Unlike a timestamp, this state remains
	// stable while the account has not changed.
	var version uint64
	for _, folder := range folders {
		version += folder.HighestModSeq
	}
	return fmt.Sprintf("%s-%d", id, version)
}
func accountID(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get("account_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"type": "authenticationRequired"})
		return uuid.Nil, false
	}
	id, err := uuid.Parse(fmt.Sprint(v))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"type": "authenticationRequired"})
		return uuid.Nil, false
	}
	return id, true
}
func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}
func jmapError(err error) gin.H {
	if err == service.ErrNotFound {
		return gin.H{"type": "accountNotFound"}
	}
	if err == service.ErrForbidden {
		return gin.H{"type": "forbidden"}
	}
	return gin.H{"type": "serverFail", "description": err.Error()}
}
func stringArg(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	v, _ := m[k].(string)
	return v
}
func numberArg(m map[string]any, k string) float64 { v, _ := m[k].(float64); return v }
func objectMap(v any) map[string]any               { m, _ := v.(map[string]any); return m }
func idSet(v any) map[string]bool {
	out := map[string]bool{}
	for _, x := range stringList(v) {
		out[x] = true
	}
	return out
}
func stringList(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func setFlag(flags []string, flag string, on bool) []string {
	found := false
	out := []string{}
	for _, f := range flags {
		if f == flag {
			found = true
			if !on {
				continue
			}
		}
		out = append(out, f)
	}
	if on && !found {
		out = append(out, flag)
	}
	sort.Strings(out)
	return out
}
func mailboxPatch(p map[string]any) string {
	for k, v := range p {
		if strings.HasPrefix(k, "mailboxIds/") && v == true {
			return strings.TrimPrefix(k, "mailboxIds/")
		}
	}
	return ""
}
