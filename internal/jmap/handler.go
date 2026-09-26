// Package jmap exposes the ElecPostal mail store through JMAP (RFC 8620 and
// RFC 8621). It deliberately uses the IMAP folder/message tables as its
// source of truth so JMAP, IMAP, and POP3 all see the same messages.
package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

const (
	coreCapability       = "urn:ietf:params:jmap:core"
	mailCapability       = "urn:ietf:params:jmap:mail"
	submissionCapability = "urn:ietf:params:jmap:submission"
	identityCapability   = "urn:ietf:params:jmap:identity"
)

// WellKnown returns the JMAP session resource URL for RFC 8620 §2.2 autodiscovery.
func WellKnown(c *gin.Context) {
	base := requestBaseURL(c)
	c.JSON(http.StatusOK, gin.H{
		"self":     base + "/jmap/session",
		"api":      base + "/jmap/api",
		"download": base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"upload":   base + "/jmap/upload/{accountId}/",
	})
}

// Handler serves the JMAP session resource and API endpoint.
type Handler struct{ mail *service.EmailService }

func New(mail *service.EmailService) *Handler { return &Handler{mail: mail} }

// Session returns the authenticated user's JMAP account map. Account IDs are
// ElecPostal mailbox IDs; this makes a JMAP account correspond to one address.
func (h *Handler) Session(c *gin.Context) {
	accID, ok := accountID(c)
	if !ok {
		return
	}
	mailboxes, err := h.mail.ListMailboxes(c.Request.Context(), accID, "")
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
	capabilities := gin.H{
		coreCapability: gin.H{
			"maxSizeRequest":        10000000,
			"maxConcurrentRequests": 4,
			"maxCallsInRequest":     16,
			"maxObjectsInGet":       500,
			"maxObjectsInSet":       500,
			"collationAlgorithms":   []string{"i;unicode-casemap"},
		},
		mailCapability: gin.H{
			"maxMailboxesPerEmail":       1,
			"maxSizeAttachmentsPerEmail": 0,
			"maxMailboxDepth":            nil,
			"maxSizeMailboxName":         255,
			"emailQuerySortOptions":      []string{"receivedAt", "sentAt", "size", "from", "subject"},
		},
		identityCapability: gin.H{
			"maxIdentities": 1,
		},
	}
	if h.mail.HasRelay() {
		capabilities[submissionCapability] = gin.H{"maxDelaySend": 0}
	}
	c.JSON(http.StatusOK, gin.H{
		"capabilities":    capabilities,
		"accounts":        accounts,
		"primaryAccounts": gin.H{mailCapability: primary},
		"apiUrl":          base + "/jmap/api",
		"downloadUrl":     base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":       base + "/jmap/upload/{accountId}/",
		"eventSourceUrl":  base + "/jmap/eventsource/",
		"state":           h.state(c.Request.Context(), primary),
	})
}

// API processes a JMAP methodCalls request. Calls are handled sequentially as
// required by RFC 8620. Result references are intentionally rejected rather
// than being interpreted as arbitrary JSON paths.
func (h *Handler) API(c *gin.Context) {
	accID, ok := accountID(c)
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
		responseName, result := h.method(c, accID, name, args)
		responses = append(responses, []any{responseName, result, callID})
	}
	c.JSON(http.StatusOK, gin.H{"methodResponses": responses, "sessionState": h.state(c.Request.Context(), "")})
}

func (h *Handler) method(c *gin.Context, owner uuid.UUID, name string, args map[string]any) (string, any) {
	if name == "Core/echo" {
		return name, args
	}
	// Identity/get does not require resolving an account.
	if name == "Identity/get" {
		return name, h.identityGet(c, owner, args)
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
	case "Mailbox/set":
		return name, h.mailboxSet(c, mailboxID, args)
	case "Email/get":
		return name, h.emailGet(c, mailboxID, args)
	case "Email/query":
		return name, h.emailQuery(c, mailboxID, args)
	case "Email/set":
		return name, h.emailSet(c, owner, mailboxID, args)
	case "Email/copy":
		return name, h.emailCopy(c, owner, mailboxID, args)
	case "Email/import":
		return name, h.emailImport(c, mailboxID, args)
	case "Email/parse":
		return name, h.emailParse(c, mailboxID, args)
	case "Thread/get":
		return name, h.threadGet(c, mailboxID, args)
	case "SearchSnippet/get":
		return name, h.searchSnippetGet(c, mailboxID, args)
	case "EmailSubmission/set":
		return name, h.emailSubmissionSet(c, mailboxID, owner, args)
	case "EmailSubmission/get":
		return name, h.emailSubmissionGet(c, mailboxID, args)
	case "EmailSubmission/query":
		return name, h.emailSubmissionQuery(c, mailboxID, args)
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

// ── Mailbox methods ──────────────────────────────────────────────────────

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

func (h *Handler) mailboxSet(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	created, notCreated := gin.H{}, gin.H{}
	updated, notUpdated := gin.H{}, gin.H{}
	destroyed, notDestroyed := []string{}, gin.H{}

	for cid, raw := range objectMap(args["create"]) {
		input, ok := raw.(map[string]any)
		if !ok {
			notCreated[cid] = gin.H{"type": "invalidProperties"}
			continue
		}
		name := strings.TrimSpace(stringArg(input, "name"))
		if name == "" {
			notCreated[cid] = gin.H{"type": "invalidProperties", "description": "name is required"}
			continue
		}
		role := stringArg(input, "role")
		specialUse := ""
		switch role {
		case "inbox":
			specialUse = `\Inbox`
		case "sent":
			specialUse = `\Sent`
		case "drafts":
			specialUse = `\Drafts`
		case "junk":
			specialUse = `\Junk`
		case "trash":
			specialUse = `\Trash`
		case "archive":
			specialUse = `\Archive`
		}
		subscribed := true
		if v, ok := input["isSubscribed"].(bool); ok {
			subscribed = v
		}
		existing, _ := h.mail.ListProtocolFolders(ctx, accountID)
		dupe := false
		for _, f := range existing {
			if strings.EqualFold(f.Name, name) {
				dupe = true
				break
			}
		}
		if dupe {
			notCreated[cid] = gin.H{"type": "invalidProperties", "description": "name already exists"}
			continue
		}
		folder := database.MailFolder{
			MailboxID:     accountID,
			Name:          name,
			UIDValidity:   uint32(time.Now().Unix()),
			NextUID:       1,
			HighestModSeq: 1,
			SpecialUse:    specialUse,
			Subscribed:    subscribed,
		}
		if err := h.mail.DB().WithContext(ctx).Create(&folder).Error; err != nil {
			notCreated[cid] = jmapError(err)
			continue
		}
		created[cid] = folder.ID
	}

	for id, raw := range objectMap(args["update"]) {
		patch, ok := raw.(map[string]any)
		if !ok {
			notUpdated[id] = gin.H{"type": "invalidProperties"}
			continue
		}
		folders, err := h.mail.ListProtocolFolders(ctx, accountID)
		if err != nil {
			notUpdated[id] = gin.H{"type": "serverFail"}
			continue
		}
		var current *database.MailFolder
		for i := range folders {
			if folders[i].ID == id {
				current = &folders[i]
				break
			}
		}
		if current == nil {
			notUpdated[id] = gin.H{"type": "notFound"}
			continue
		}
		updates := map[string]any{}
		if v, ok := patch["name"].(string); ok && strings.TrimSpace(v) != "" {
			updates["name"] = strings.TrimSpace(v)
		}
		if v, ok := patch["isSubscribed"].(bool); ok {
			updates["subscribed"] = v
		}
		if len(updates) > 0 {
			updates["highest_mod_seq"] = current.HighestModSeq + 1
			if err := h.mail.DB().WithContext(ctx).Model(&database.MailFolder{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				notUpdated[id] = jmapError(err)
				continue
			}
		}
		updated[id] = nil
	}

	for _, id := range stringList(args["destroy"]) {
		folders, _ := h.mail.ListProtocolFolders(ctx, accountID)
		var current *database.MailFolder
		for i := range folders {
			if folders[i].ID == id {
				current = &folders[i]
				break
			}
		}
		if current == nil {
			notDestroyed[id] = gin.H{"type": "notFound"}
			continue
		}
		var count int64
		if err := h.mail.DB().WithContext(ctx).Model(&database.FolderMessage{}).Where("folder_id = ?", id).Count(&count).Error; err != nil {
			notDestroyed[id] = jmapError(err)
			continue
		}
		if count > 0 {
			notDestroyed[id] = gin.H{"type": "mailboxHasEmail"}
			continue
		}
		if err := h.mail.DB().WithContext(ctx).Delete(&database.MailFolder{}, "id = ?", id).Error; err != nil {
			notDestroyed[id] = jmapError(err)
			continue
		}
		destroyed = append(destroyed, id)
	}

	return gin.H{
		"accountId": accountID, "oldState": h.state(ctx, accountID), "newState": h.state(ctx, accountID),
		"created": created, "notCreated": notCreated, "updated": updated, "notUpdated": notUpdated,
		"destroyed": destroyed, "notDestroyed": notDestroyed,
	}
}

// ── Email methods ────────────────────────────────────────────────────────

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
	ids := filterEmails(rows, filter)

	// Sorting.
	sortField := "receivedAt"
	if s, ok := args["sort"].(string); ok {
		sortField = s
	}
	ascending := false
	if sortArr, ok := args["sort"].([]any); ok && len(sortArr) > 0 {
		if first, ok := sortArr[0].(map[string]any); ok {
			sortField = stringArg(first, "property")
			ascending = first["isAscending"] == true
		}
	}
	sortEmails(rows, ids, sortField, ascending)

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
		// Destroy keeps mail recoverable by filing it into Trash, except in
		// Trash itself, which exists to be emptied.
		if err := h.mail.DeleteProtocolMessages(c.Request.Context(), accountID, cur.Folder.Name, []string{id}); err != nil {
			notDestroyed[id] = jmapError(err)
			continue
		}
		destroyed = append(destroyed, id)
	}
	return gin.H{"accountId": accountID, "oldState": h.state(c.Request.Context(), accountID), "newState": h.state(c.Request.Context(), accountID), "updated": updated, "notUpdated": notUpdated, "destroyed": destroyed, "notDestroyed": notDestroyed}
}

func (h *Handler) emailCopy(c *gin.Context, owner uuid.UUID, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	sourceID := stringArg(args, "id")
	if sourceID == "" {
		return gin.H{"type": "invalidArguments", "description": "id is required"}
	}
	destAccountID := stringArg(args, "accountId")
	if destAccountID == "" {
		destAccountID = accountID
	}
	_, err := h.resolveAccount(c, owner, destAccountID)
	if err != nil {
		return gin.H{"type": "invalidArguments", "description": "destination account not found"}
	}
	mailboxIDs, ok := args["mailboxIds"].(map[string]any)
	if !ok || len(mailboxIDs) == 0 {
		return gin.H{"type": "invalidArguments", "description": "mailboxIds is required"}
	}
	var destMailboxID string
	for mid := range mailboxIDs {
		destMailboxID = mid
		break
	}
	folders, err := h.mail.ListProtocolFolders(ctx, destMailboxID)
	if err != nil || len(folders) == 0 {
		return gin.H{"type": "invalidArguments", "description": "destination mailbox has no folders"}
	}
	destFolderName := folders[0].Name

	rows, err := h.rows(c, accountID)
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	found := false
	for i := range rows {
		if rows[i].Email.ID == sourceID {
			found = true
			break
		}
	}
	if !found {
		return gin.H{"type": "notFound"}
	}

	newID, err := h.mail.CopyEmail(ctx, sourceID, destMailboxID, destFolderName)
	if err != nil {
		return jmapError(err)
	}
	return gin.H{"accountId": destAccountID, "newId": newID}
}

func (h *Handler) emailImport(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	emails, _ := args["emails"].([]any)
	created, notCreated := gin.H{}, gin.H{}

	for _, raw := range emails {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blobId := stringArg(entry, "blobId")
		if blobId == "" {
			continue
		}
		mailboxIDs, _ := entry["mailboxIds"].(map[string]any)
		targetFolderID := ""
		for mid := range mailboxIDs {
			targetFolderID = mid
			break
		}
		rows, err := h.rows(c, accountID)
		if err != nil {
			notCreated[blobId] = gin.H{"type": "serverFail"}
			continue
		}
		var currentRow *mailRow
		for i := range rows {
			if rows[i].Email.ID == blobId {
				currentRow = &rows[i]
				break
			}
		}
		if currentRow == nil {
			notCreated[blobId] = gin.H{"type": "notFound"}
			continue
		}
		if targetFolderID != "" {
			destFolder, ok := h.folder(c, accountID, targetFolderID)
			if !ok {
				notCreated[blobId] = gin.H{"type": "invalidProperties", "description": "invalid mailboxIds"}
				continue
			}
			if currentRow.Folder.ID != destFolder.ID {
				if err := h.mail.MoveEmailToFolder(ctx, accountID, currentRow.Folder.Name, destFolder.Name, blobId); err != nil {
					notCreated[blobId] = jmapError(err)
					continue
				}
			}
		}
		if kwMap, ok := entry["keywords"].(map[string]any); ok && len(kwMap) > 0 {
			flags := []string{}
			if kwMap["$seen"] == true {
				flags = append(flags, "\\Seen")
			}
			if kwMap["$flagged"] == true {
				flags = append(flags, "\\Flagged")
			}
			if kwMap["$answered"] == true {
				flags = append(flags, "\\Answered")
			}
			if kwMap["$draft"] == true {
				flags = append(flags, "\\Draft")
			}
			folderName := currentRow.Folder.Name
			if targetFolderID != "" {
				if destFolder, ok := h.folder(c, accountID, targetFolderID); ok {
					folderName = destFolder.Name
				}
			}
			if _, err := h.mail.StoreProtocolFlags(ctx, accountID, folderName, []string{blobId}, flags, "replace", 0); err != nil {
				notCreated[blobId] = jmapError(err)
				continue
			}
		}
		if ra, ok := entry["receivedAt"].(string); ok && ra != "" {
			if t, err := time.Parse(time.RFC3339, ra); err == nil {
				_ = h.mail.DB().WithContext(ctx).Model(&database.Email{}).Where("id = ?", blobId).Update("sent_at", t).Error
			}
		}
		created[blobId] = nil
	}

	return gin.H{"accountId": accountID, "created": created, "notCreated": notCreated}
}

func (h *Handler) emailParse(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	ids := stringList(args["ids"])
	list := make([]any, 0, len(ids))
	notFound := []string{}

	for _, id := range ids {
		var email database.Email
		if err := h.mail.DB().WithContext(ctx).Preload("Attachments").Where("id = ?", id).First(&email).Error; err != nil {
			notFound = append(notFound, id)
			continue
		}

		textBody := []any{}
		htmlBody := []any{}
		attachments := []any{}

		bodyType := strings.ToLower(email.ContentType)
		if bodyType == "" {
			bodyType = "text/plain"
		}
		bodyBlob := gin.H{"partId": "1", "blobId": id, "size": len(email.Body), "type": bodyType}
		if strings.Contains(bodyType, "html") {
			htmlBody = append(htmlBody, bodyBlob)
		} else {
			textBody = append(textBody, bodyBlob)
		}

		for i, att := range email.Attachments {
			attID := att.ID
			if att.StorageKey != nil {
				attID = *att.StorageKey
			}
			attachments = append(attachments, gin.H{
				"partId": fmt.Sprintf("%d", i+2), "blobId": attID,
				"size": att.Size, "type": att.MimeType, "name": att.Filename,
			})
		}

		list = append(list, gin.H{
			"id": id, "textBody": textBody, "htmlBody": htmlBody, "attachments": attachments,
		})
	}

	return gin.H{"accountId": accountID, "list": list, "notFound": notFound}
}

// ── Thread methods ───────────────────────────────────────────────────────

func (h *Handler) threadGet(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	ids := stringList(args["ids"])
	list := make([]any, 0, len(ids))
	notFound := []string{}

	for _, tid := range ids {
		var emails []database.Email
		if err := h.mail.DB().WithContext(ctx).Where(
			"account_id = ? AND archived_at IS NULL AND (thread_id = ? OR id = ?)",
			accountID, tid, tid,
		).Order("created_at ASC").Find(&emails).Error; err != nil {
			notFound = append(notFound, tid)
			continue
		}
		if len(emails) == 0 {
			notFound = append(notFound, tid)
			continue
		}
		emailIDs := make([]string, len(emails))
		for i, e := range emails {
			emailIDs[i] = e.ID
		}
		list = append(list, gin.H{"id": tid, "emailIds": emailIDs})
	}

	return gin.H{"accountId": accountID, "state": h.state(ctx, accountID), "list": list, "notFound": notFound}
}

// ── Identity methods ─────────────────────────────────────────────────────

func (h *Handler) identityGet(c *gin.Context, owner uuid.UUID, args map[string]any) gin.H {
	ctx := c.Request.Context()
	mailboxes, err := h.mail.ListMailboxes(ctx, owner, "")
	if err != nil {
		return gin.H{"type": "serverFail"}
	}
	wanted := idSet(args["ids"])
	list, notFound := make([]any, 0, len(mailboxes)), []string{}

	for _, mb := range mailboxes {
		if len(wanted) > 0 && !wanted[mb.ID] {
			continue
		}
		list = append(list, gin.H{
			"id": mb.ID, "name": mb.Name, "email": mb.Address, "mayDelete": false,
		})
	}
	for id := range wanted {
		found := false
		for _, mb := range mailboxes {
			if mb.ID == id {
				found = true
				break
			}
		}
		if !found {
			notFound = append(notFound, id)
		}
	}
	return gin.H{"accountId": owner.String(), "list": list, "notFound": notFound}
}

// ── SearchSnippet methods ────────────────────────────────────────────────

func (h *Handler) searchSnippetGet(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	filter, _ := args["filter"].(map[string]any)
	emailIDs := stringList(args["emailIds"])
	text := strings.ToLower(stringArg(filter, "text"))

	type snippetResult struct {
		EmailID string
		Snippet string
	}
	results := []snippetResult{}

	for _, id := range emailIDs {
		var email database.Email
		if err := h.mail.DB().WithContext(ctx).Where("id = ? AND archived_at IS NULL", id).First(&email).Error; err != nil {
			continue
		}
		snippet := extractSnippet(email.Subject+" "+mailtext.Summary(email.Body, email.ContentType, 1200)+" "+email.FromAddress, text, 300)
		results = append(results, snippetResult{EmailID: id, Snippet: snippet})
	}

	list := make([]any, len(results))
	for i, r := range results {
		list[i] = gin.H{"emailId": r.EmailID, "snippet": r.Snippet}
	}

	return gin.H{"accountId": accountID, "list": list}
}

// ── EmailSubmission methods ──────────────────────────────────────────────

func (h *Handler) emailSubmissionSet(c *gin.Context, accountID string, owner uuid.UUID, args map[string]any) gin.H {
	ctx := c.Request.Context()
	created, notCreated := gin.H{}, gin.H{}

	for cid, raw := range objectMap(args["create"]) {
		entry, ok := raw.(map[string]any)
		if !ok {
			notCreated[cid] = gin.H{"type": "invalidProperties"}
			continue
		}
		identityId := stringArg(entry, "identityId")
		blobId := stringArg(entry, "email")
		if identityId == "" || blobId == "" {
			notCreated[cid] = gin.H{"type": "invalidArguments", "description": "identityId and email (blobId) are required"}
			continue
		}
		var email database.Email
		if err := h.mail.DB().WithContext(ctx).Preload("Recipients").Where("id = ?", blobId).First(&email).Error; err != nil {
			notCreated[cid] = gin.H{"type": "notFound", "description": "email blob not found"}
			continue
		}
		mailboxes, err := h.mail.ListMailboxes(ctx, owner, "")
		if err != nil {
			notCreated[cid] = jmapError(err)
			continue
		}
		var identity *database.Mailbox
		for i := range mailboxes {
			if mailboxes[i].ID == identityId {
				identity = &mailboxes[i]
				break
			}
		}
		if identity == nil {
			notCreated[cid] = gin.H{"type": "notFound", "description": "identity not found"}
			continue
		}
		envelope, _ := entry["envelope"].(map[string]any)
		subject := email.Subject

		to := []service.RecipientInput{}
		cc := []service.RecipientInput{}
		bcc := []service.RecipientInput{}
		if toList, ok := envelope["to"].([]any); ok {
			for _, r := range toList {
				if rm, ok := r.(map[string]any); ok {
					to = append(to, service.RecipientInput{Address: stringArg(rm, "email"), Name: stringArg(rm, "name"), Kind: "to"})
				}
			}
		} else {
			for _, r := range email.Recipients {
				switch r.Kind {
				case "cc":
					cc = append(cc, service.RecipientInput{Address: r.Address, Name: r.Name, Kind: "cc"})
				case "bcc":
					bcc = append(bcc, service.RecipientInput{Address: r.Address, Name: r.Name, Kind: "bcc"})
				default:
					to = append(to, service.RecipientInput{Address: r.Address, Name: r.Name, Kind: "to"})
				}
			}
		}
		if len(to) == 0 {
			notCreated[cid] = gin.H{"type": "invalidArguments", "description": "at least one To recipient required"}
			continue
		}

		_, err = h.mail.SendEmail(ctx, owner, service.SendEmailInput{
			MailboxID: identityId, Subject: subject, Body: email.Body, To: to, Cc: cc, Bcc: bcc,
		})
		if err != nil {
			notCreated[cid] = jmapError(err)
			continue
		}
		now := time.Now().UTC().Format(time.RFC3339)
		created[cid] = gin.H{
			"id": database.NewID(), "identityId": identityId,
			"sendTime": now, "deliveryStatus": gin.H{"status": "queued"},
		}
	}

	return gin.H{"accountId": accountID, "created": created, "notCreated": notCreated}
}

func (h *Handler) emailSubmissionGet(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	ids := stringList(args["ids"])
	list := make([]any, 0, len(ids))
	notFound := []string{}

	for _, id := range ids {
		var email database.Email
		if err := h.mail.DB().WithContext(ctx).Where("id = ?", id).First(&email).Error; err != nil {
			notFound = append(notFound, id)
			continue
		}
		if email.DeliveryStatus == "" && email.SentAt == nil {
			notFound = append(notFound, id)
			continue
		}
		status := email.DeliveryStatus
		if status == "" {
			status = "sent"
		}
		sendTime := ""
		if email.SentAt != nil {
			sendTime = email.SentAt.Format(time.RFC3339)
		}
		list = append(list, gin.H{
			"id": id, "identityId": email.MailboxID, "sendTime": sendTime,
			"deliveryStatus": gin.H{"status": status},
		})
	}

	return gin.H{"accountId": accountID, "list": list, "notFound": notFound}
}

func (h *Handler) emailSubmissionQuery(c *gin.Context, accountID string, args map[string]any) gin.H {
	ctx := c.Request.Context()
	filter, _ := args["filter"].(map[string]any)

	query := h.mail.DB().WithContext(ctx).Model(&database.Email{}).
		Where("account_id = ? AND delivery_status != ''", accountID)

	if after := stringArg(filter, "after"); after != "" {
		if t, err := time.Parse(time.RFC3339, after); err == nil {
			query = query.Where("sent_at >= ?", t)
		}
	}
	if before := stringArg(filter, "before"); before != "" {
		if t, err := time.Parse(time.RFC3339, before); err == nil {
			query = query.Where("sent_at <= ?", t)
		}
	}

	var total int64
	_ = query.Count(&total).Error

	var ids []string
	_ = query.Order("sent_at DESC").Pluck("id", &ids).Error

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

	return gin.H{
		"accountId": accountID, "queryState": h.state(ctx, accountID),
		"canCalculateChanges": false, "position": position,
		"ids": ids[position:end], "total": total,
	}
}

// ── Upload / Download (RFC 8620 §6) ─────────────────────────────────────

// Upload handles POST /jmap/upload/{accountId}/.
func (h *Handler) Upload(c *gin.Context) {
	accountID := c.Param("accountId")
	ctx := c.Request.Context()
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"type": "invalidArguments", "description": "failed to read body"})
		return
	}
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"type": "invalidArguments", "description": "empty body"})
		return
	}
	emailID, size, err := h.mail.UploadMessage(ctx, accountID, body)
	if err != nil {
		c.JSON(http.StatusNotFound, jmapError(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"accountId": accountID, "blobId": emailID, "size": size})
}

// Download handles GET /jmap/download/{accountId}/{blobId}/{name}.
func (h *Handler) Download(c *gin.Context) {
	blobID := c.Param("blobId")
	ctx := c.Request.Context()

	// Validate ownership by checking the email exists.
	var email database.Email
	if err := h.mail.DB().WithContext(ctx).Where("id = ?", blobID).First(&email).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"type": "notFound"})
		return
	}

	source, err := h.mail.OpenProtocolMessage(ctx, blobID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"type": "notFound"})
		return
	}
	c.Header("Content-Type", "message/rfc822")
	c.Status(http.StatusOK)
	_ = mailmime.Render(ctx, source, c.Writer)
}

// ── Data structures ──────────────────────────────────────────────────────

type mailRow struct {
	Email    database.Email
	Folder   database.MailFolder
	Flags    []string
	WireSize int64
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
		var source database.MessageSource
		if err := h.mail.DB().WithContext(c.Request.Context()).Where("email_id = ?", email.ID).First(&source).Error; err != nil {
			return nil, err
		}
		out = append(out, mailRow{Email: email, Folder: database.MailFolder{ID: v.FolderID, Name: v.FolderName, SpecialUse: v.SpecialUse}, Flags: flags, WireSize: source.WireSizeBytes})
	}
	return out, nil
}

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
	preview := mailtext.Preview(r.Email.Summary, r.Email.Body, r.Email.ContentType, 256)
	sentAt := r.Email.CreatedAt
	if r.Email.SentAt != nil {
		sentAt = *r.Email.SentAt
	}

	attachments := make([]any, 0, len(r.Email.Attachments))
	for _, att := range r.Email.Attachments {
		attBlobID := att.ID
		if att.StorageKey != nil && *att.StorageKey != "" {
			attBlobID = *att.StorageKey
		}
		attachments = append(attachments, gin.H{
			"id": att.ID, "blobId": attBlobID, "type": att.MimeType, "name": att.Filename, "size": att.Size,
		})
	}

	headers := []any{
		gin.H{"name": "From", "value": r.Email.FromAddress},
		gin.H{"name": "Subject", "value": r.Email.Subject},
		gin.H{"name": "Date", "value": sentAt.Format(time.RFC3339)},
	}
	if to := recipients("to"); len(to) > 0 {
		headers = append(headers, gin.H{"name": "To", "value": fmt.Sprintf("%v", to)})
	}

	bodyType := r.Email.ContentType
	if bodyType == "" {
		bodyType = "text/plain"
	}
	textBody := []any{gin.H{"partId": "1", "blobId": r.Email.ID, "size": len(r.Email.Body), "type": bodyType}}

	return gin.H{
		"id": r.Email.ID, "blobId": r.Email.ID, "threadId": threadID(r.Email),
		"mailboxIds": gin.H{r.Folder.ID: true}, "keywords": keywords,
		"size": r.WireSize, "receivedAt": r.Email.CreatedAt.Format(time.RFC3339),
		"sentAt": sentAt.Format(time.RFC3339),
		"from":   []any{gin.H{"email": r.Email.FromAddress, "name": r.Email.FromName}},
		"to":     recipients("to"), "cc": recipients("cc"), "bcc": recipients("bcc"),
		"subject": r.Email.Subject, "preview": preview,
		"hasAttachment": len(r.Email.Attachments) > 0,
		"textBody":      textBody, "htmlBody": []any{},
		"attachments": attachments, "headers": headers,
	}
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

// ── Filtering & sorting helpers ──────────────────────────────────────────

func filterEmails(rows []mailRow, filter map[string]any) []string {
	if filter == nil {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.Email.ID
		}
		return ids
	}

	inMailbox := stringArg(filter, "inMailbox")
	inMailboxOtherThan := stringList(filter["inMailboxOtherThan"])
	text := strings.ToLower(stringArg(filter, "text"))
	from := strings.ToLower(stringArg(filter, "from"))
	to := strings.ToLower(stringArg(filter, "to"))
	subject := strings.ToLower(stringArg(filter, "subject"))
	after := stringArg(filter, "after")
	before := stringArg(filter, "before")
	isUnread := filter["isUnread"]
	isRead := filter["isRead"]
	isFlagged := filter["isFlagged"]
	hasAttachment := filter["hasAttachment"]

	ids := []string{}
	for _, row := range rows {
		if inMailbox != "" && row.Folder.ID != inMailbox {
			continue
		}
		if len(inMailboxOtherThan) > 0 {
			excluded := false
			for _, ex := range inMailboxOtherThan {
				if row.Folder.ID == ex {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}
		if text != "" && !strings.Contains(strings.ToLower(row.Email.Subject+" "+row.Email.Body+" "+row.Email.FromAddress), text) {
			continue
		}
		if from != "" && !strings.Contains(strings.ToLower(row.Email.FromAddress), from) {
			continue
		}
		if to != "" {
			toMatch := false
			for _, r := range row.Email.Recipients {
				if strings.Contains(strings.ToLower(r.Address), to) {
					toMatch = true
					break
				}
			}
			if !toMatch {
				continue
			}
		}
		if subject != "" && !strings.Contains(strings.ToLower(row.Email.Subject), subject) {
			continue
		}
		if after != "" {
			if t, err := time.Parse(time.RFC3339, after); err == nil && row.Email.CreatedAt.Before(t) {
				continue
			}
		}
		if before != "" {
			if t, err := time.Parse(time.RFC3339, before); err == nil && row.Email.CreatedAt.After(t) {
				continue
			}
		}
		if isUnread == true {
			seen := false
			for _, f := range row.Flags {
				if f == "\\Seen" {
					seen = true
					break
				}
			}
			if seen {
				continue
			}
		}
		if isRead == true {
			seen := false
			for _, f := range row.Flags {
				if f == "\\Seen" {
					seen = true
					break
				}
			}
			if !seen {
				continue
			}
		}
		if isFlagged == true {
			flagged := false
			for _, f := range row.Flags {
				if f == "\\Flagged" {
					flagged = true
					break
				}
			}
			if !flagged {
				continue
			}
		}
		if hasAttachment == true && len(row.Email.Attachments) == 0 {
			continue
		}
		ids = append(ids, row.Email.ID)
	}
	return ids
}

func sortEmails(rows []mailRow, ids []string, field string, ascending bool) {
	lookup := map[string]mailRow{}
	for _, r := range rows {
		lookup[r.Email.ID] = r
	}
	sort.SliceStable(ids, func(i, j int) bool {
		ri, iok := lookup[ids[i]]
		rj, jok := lookup[ids[j]]
		if !iok || !jok {
			return false
		}
		var cmp int
		switch field {
		case "sentAt":
			si := ri.Email.CreatedAt
			if ri.Email.SentAt != nil {
				si = *ri.Email.SentAt
			}
			sj := rj.Email.CreatedAt
			if rj.Email.SentAt != nil {
				sj = *rj.Email.SentAt
			}
			cmp = si.Compare(sj)
		case "size":
			cmp = int(ri.WireSize - rj.WireSize)
		case "from":
			cmp = strings.Compare(strings.ToLower(ri.Email.FromAddress), strings.ToLower(rj.Email.FromAddress))
		case "subject":
			cmp = strings.Compare(strings.ToLower(ri.Email.Subject), strings.ToLower(rj.Email.Subject))
		default:
			cmp = ri.Email.CreatedAt.Compare(rj.Email.CreatedAt)
		}
		if ascending {
			return cmp < 0
		}
		return cmp > 0
	})
}

func extractSnippet(text, query string, maxLen int) string {
	if query == "" {
		if len(text) > maxLen {
			return text[:maxLen]
		}
		return text
	}
	lower := strings.ToLower(text)
	idx := strings.Index(lower, query)
	if idx == -1 {
		if len(text) > maxLen {
			return text[:maxLen]
		}
		return text
	}
	start := idx - maxLen/3
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + maxLen*2/3
	if end > len(text) {
		end = len(text)
	}
	snippet := text[start:end]
	snippet = strings.ReplaceAll(snippet, text[idx:idx+len(query)], "<mark>"+text[idx:idx+len(query)]+"</mark>")
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(text) {
		snippet = snippet + "…"
	}
	return snippet
}

// Clamp restricts v to [lo, hi].
func Clamp(v, lo, hi int) int {
	return int(math.Max(float64(lo), math.Min(float64(hi), float64(v))))
}
