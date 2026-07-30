package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

// RegisterRoutes registers HTTP handlers.
func RegisterRoutes(r *gin.RouterGroup, emailSvc *service.EmailService) {
	mailboxes := r.Group("/mailboxes")
	{
		mailboxes.GET("", func(c *gin.Context) { listMailboxes(c, emailSvc) })
		mailboxes.POST("", func(c *gin.Context) { createMailbox(c, emailSvc) })
		mailboxes.GET("/:id/emails", func(c *gin.Context) { listMailboxEmails(c, emailSvc) })
		mailboxes.GET("/:id/threads", func(c *gin.Context) { listThreads(c, emailSvc, c.Param("id")) })
		mailboxes.GET("/:id/stats", func(c *gin.Context) { getMailboxStats(c, emailSvc) })
		mailboxes.GET("/:id/quota", func(c *gin.Context) { getMailboxQuota(c, emailSvc) })
	}

	emails := r.Group("/emails")
	{
		emails.GET("", func(c *gin.Context) { listEmails(c, emailSvc) })
		emails.GET("/stats", func(c *gin.Context) { getMailboxStats(c, emailSvc) })
		emails.POST("", func(c *gin.Context) { sendEmail(c, emailSvc) })
		emails.GET("/:id", func(c *gin.Context) { getEmail(c, emailSvc) })
		emails.POST("/:id/resend", func(c *gin.Context) { resendEmail(c, emailSvc) })
		emails.DELETE("/:id", func(c *gin.Context) { deleteEmail(c, emailSvc) })
		emails.POST("/:id/read", func(c *gin.Context) { markRead(c, emailSvc, true) })
		emails.POST("/:id/unread", func(c *gin.Context) { markRead(c, emailSvc, false) })
		emails.POST("/:id/star", func(c *gin.Context) { markStarred(c, emailSvc, true) })
		emails.POST("/:id/unstar", func(c *gin.Context) { markStarred(c, emailSvc, false) })
		emails.POST("/:id/move", func(c *gin.Context) { moveEmail(c, emailSvc) })
		emails.POST("/:id/spam", func(c *gin.Context) { moveEmailTo(c, emailSvc, "spam") })
		emails.POST("/:id/not-spam", func(c *gin.Context) { moveEmailTo(c, emailSvc, "inbox") })
		emails.POST("/:id/labels/:labelID", func(c *gin.Context) { setEmailLabel(c, emailSvc, true) })
		emails.DELETE("/:id/labels/:labelID", func(c *gin.Context) { setEmailLabel(c, emailSvc, false) })
	}

	threads := r.Group("/threads")
	{
		threads.GET("", func(c *gin.Context) { listThreads(c, emailSvc, "") })
		threads.GET("/:id", func(c *gin.Context) { getThread(c, emailSvc) })
	}

	blocklist := r.Group("/blocklist")
	{
		blocklist.GET("", func(c *gin.Context) { listBlockRules(c, emailSvc) })
		blocklist.POST("", func(c *gin.Context) { createBlockRule(c, emailSvc) })
		blocklist.DELETE("/:id", func(c *gin.Context) { deleteBlockRule(c, emailSvc) })
	}

	labels := r.Group("/labels")
	{
		labels.GET("", func(c *gin.Context) { listLabels(c, emailSvc) })
		labels.POST("", func(c *gin.Context) { createLabel(c, emailSvc) })
		labels.DELETE("/:id", func(c *gin.Context) { deleteLabel(c, emailSvc) })
	}

	credentials := r.Group("/credentials")
	{
		credentials.GET("", func(c *gin.Context) { listMailCredentials(c, emailSvc) })
		credentials.POST("", func(c *gin.Context) { createMailCredential(c, emailSvc) })
		credentials.DELETE("/:id", func(c *gin.Context) { deleteMailCredential(c, emailSvc) })
	}

	connections := r.Group("/mail-connections")
	{
		connections.GET("", func(c *gin.Context) { listMailConnections(c, emailSvc) })
		connections.POST("", func(c *gin.Context) { createMailConnection(c, emailSvc) })
		connections.POST("/:id/refresh", func(c *gin.Context) { refreshMailConnection(c, emailSvc) })
		connections.DELETE("/:id", func(c *gin.Context) { deleteMailConnection(c, emailSvc) })
	}
}

func listMailConnections(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	items, err := emailSvc.ListMailConnections(c.Request.Context(), uuid.MustParse(accountID), c.Query("workspace_id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, items)
}

func createMailConnection(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	var input service.CreateMailConnectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	connection, err := emailSvc.CreateMailConnection(c.Request.Context(), uuid.MustParse(accountID), input)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, connection)
}

func refreshMailConnection(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	connection, err := emailSvc.RefreshMailConnection(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, connection)
}

func deleteMailConnection(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.DeleteMailConnection(c.Request.Context(), uuid.MustParse(accountID), c.Param("id")); err != nil {
		renderServiceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func getMailboxQuota(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	quota, err := emailSvc.GetMailboxQuota(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, quota)
}

func listMailboxes(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	items, err := emailSvc.ListMailboxes(c.Request.Context(), uuid.MustParse(accountID), c.Query("workspace_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("X-Total", strconv.Itoa(len(items)))
	c.JSON(http.StatusOK, items)
}

func createMailbox(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	var req struct {
		WorkspaceID string `json:"workspace_id" binding:"required"`
		Address     string `json:"address" binding:"required"`
		Name        string `json:"name"`
		IsDefault   bool   `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	mailbox, err := emailSvc.CreateMailbox(c.Request.Context(), uuid.MustParse(accountID), req.WorkspaceID, req.Address, req.Name, req.IsDefault)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, mailbox)
}

func listMailboxEmails(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	input := parseListInput(c)
	items, total, err := emailSvc.ListEmails(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"), input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("X-Total", strconv.FormatInt(total, 10))
	c.JSON(http.StatusOK, items)
}

func listEmails(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	input := parseListInput(c)
	items, total, err := emailSvc.ListEmails(c.Request.Context(), uuid.MustParse(accountID), "", input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("X-Total", strconv.FormatInt(total, 10))
	c.JSON(http.StatusOK, items)
}

func listThreads(c *gin.Context, emailSvc *service.EmailService, mailboxID string) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	items, total, err := emailSvc.ListThreads(c.Request.Context(), uuid.MustParse(accountID), mailboxID, parseListInput(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("X-Total", strconv.FormatInt(total, 10))
	c.JSON(http.StatusOK, items)
}

func getThread(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	items, err := emailSvc.GetThread(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, items)
}

func getMailboxStats(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	stats, err := emailSvc.GetMailboxStats(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func sendEmail(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	var input service.SendEmailInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	email, err := emailSvc.SendEmail(c.Request.Context(), uuid.MustParse(accountID), input)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, email)
}

func getEmail(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	email, err := emailSvc.GetEmail(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, email)
}

func resendEmail(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	email, err := emailSvc.ResendEmail(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"))
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, email)
}

func deleteEmail(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	if err := emailSvc.DeleteEmail(c.Request.Context(), uuid.MustParse(accountID), c.Param("id")); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func markRead(c *gin.Context, emailSvc *service.EmailService, isRead bool) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}

	if err := emailSvc.MarkRead(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"), isRead); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func markStarred(c *gin.Context, emailSvc *service.EmailService, isStarred bool) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.MarkStarred(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"), isStarred); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func listLabels(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	items, err := emailSvc.ListLabels(c.Request.Context(), uuid.MustParse(accountID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, items)
}

func createLabel(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	var input struct {
		Name  string `json:"name" binding:"required"`
		Color string `json:"color"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	label, err := emailSvc.CreateLabel(c.Request.Context(), uuid.MustParse(accountID), input.Name, input.Color)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, label)
}

func deleteLabel(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.DeleteLabel(c.Request.Context(), uuid.MustParse(accountID), c.Param("id")); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func setEmailLabel(c *gin.Context, emailSvc *service.EmailService, assigned bool) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.SetEmailLabel(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"), c.Param("labelID"), assigned); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func moveEmail(c *gin.Context, emailSvc *service.EmailService) {
	var input struct {
		Folder string `json:"folder" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	moveEmailTo(c, emailSvc, input.Folder)
}

func moveEmailTo(c *gin.Context, emailSvc *service.EmailService, folder string) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.MoveEmail(c.Request.Context(), uuid.MustParse(accountID), c.Param("id"), folder); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func listBlockRules(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	items, err := emailSvc.ListBlockRules(c.Request.Context(), uuid.MustParse(accountID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, items)
}

func createBlockRule(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	var input service.CreateBlockRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rule, err := emailSvc.CreateBlockRule(c.Request.Context(), uuid.MustParse(accountID), input)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, rule)
}

func deleteBlockRule(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.DeleteBlockRule(c.Request.Context(), uuid.MustParse(accountID), c.Param("id")); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func listMailCredentials(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	credentials, err := emailSvc.ListMailProtocolCredentials(c.Request.Context(), uuid.MustParse(accountID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, credentials)
}

func createMailCredential(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	var input service.CreateMailProtocolCredentialInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	credential, err := emailSvc.CreateMailProtocolCredential(c.Request.Context(), uuid.MustParse(accountID), input)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, credential)
}

func deleteMailCredential(c *gin.Context, emailSvc *service.EmailService) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	if err := emailSvc.DeleteMailProtocolCredential(c.Request.Context(), uuid.MustParse(accountID), c.Param("id")); err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func parseListInput(c *gin.Context) service.ListInput {
	take := 20
	offset := 0
	if parsed, err := strconv.Atoi(c.DefaultQuery("take", "20")); err == nil && parsed > 0 && parsed <= 200 {
		take = parsed
	}
	if parsed, err := strconv.Atoi(c.DefaultQuery("offset", "0")); err == nil && parsed >= 0 {
		offset = parsed
	}
	status := strings.TrimSpace(c.Query("delivery_status"))
	if status == "" {
		status = strings.TrimSpace(c.Query("status"))
	}
	input := service.ListInput{Take: take, Offset: offset, MailboxID: strings.TrimSpace(c.Query("mailbox_id")), WorkspaceID: strings.TrimSpace(c.Query("workspace_id")), Query: c.Query("q"), From: strings.TrimSpace(c.Query("from")), To: strings.TrimSpace(c.Query("to")), DeliveryStatus: status, LabelID: strings.TrimSpace(c.Query("label_id")), Folder: strings.TrimSpace(c.Query("folder"))}
	for key, target := range map[string]**bool{"is_read": &input.IsRead, "is_draft": &input.IsDraft, "has_attachments": &input.HasAttachments} {
		if raw, exists := c.GetQuery(key); exists {
			if value, err := strconv.ParseBool(raw); err == nil {
				*target = &value
			}
		}
	}
	if raw, exists := c.GetQuery("is_starred"); exists {
		if value, err := strconv.ParseBool(raw); err == nil {
			input.IsStarred = &value
		}
	} else if raw, exists := c.GetQuery("is_flagged"); exists {
		if value, err := strconv.ParseBool(raw); err == nil {
			input.IsStarred = &value
		}
	}
	return input
}

func renderServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrSendLimitExceeded):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}
