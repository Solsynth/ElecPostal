package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/identity"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

func listAddressSuggestions(c *gin.Context, emailSvc *service.EmailService, senders bool) {
	accountID, ok := identity.RequireAccountID(c)
	if !ok {
		return
	}
	take := 20
	if raw := c.Query("take"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "take must be a positive integer"})
			return
		}
		take = parsed
	}

	var items []service.AddressSuggestion
	var err error
	if senders {
		items, err = emailSvc.ListSenders(c.Request.Context(), uuid.MustParse(accountID), c.Query("q"), take)
	} else {
		items, err = emailSvc.ListContacts(c.Request.Context(), uuid.MustParse(accountID), c.Query("q"), take)
	}
	if err != nil {
		renderServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, items)
}
