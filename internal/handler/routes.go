package handler

import (
	"errors"
	"net/http"
	"strconv"

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
	}

	emails := r.Group("/emails")
	{
		emails.GET("", func(c *gin.Context) { listEmails(c, emailSvc) })
		emails.POST("", func(c *gin.Context) { sendEmail(c, emailSvc) })
		emails.GET("/:id", func(c *gin.Context) { getEmail(c, emailSvc) })
		emails.DELETE("/:id", func(c *gin.Context) { deleteEmail(c, emailSvc) })
		emails.POST("/:id/read", func(c *gin.Context) { markRead(c, emailSvc, true) })
		emails.POST("/:id/unread", func(c *gin.Context) { markRead(c, emailSvc, false) })
	}

	credentials := r.Group("/credentials")
	{
		credentials.GET("", func(c *gin.Context) { listMailCredentials(c, emailSvc) })
		credentials.POST("", func(c *gin.Context) { createMailCredential(c, emailSvc) })
		credentials.DELETE("/:id", func(c *gin.Context) { deleteMailCredential(c, emailSvc) })
	}
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
	return service.ListInput{Take: take, Offset: offset}
}

func renderServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}
