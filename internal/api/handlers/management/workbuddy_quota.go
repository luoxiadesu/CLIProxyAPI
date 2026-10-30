package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	workbuddyTasks "github.com/router-for-me/CLIProxyAPI/v8/internal/workbuddy/tasks"
	log "github.com/sirupsen/logrus"
)

// workBuddyQuotaResponse is the live balance view of a WorkBuddy credential.
type workBuddyQuotaResponse struct {
	AuthIndex   string    `json:"auth_index"`
	Provider    string    `json:"provider"`
	Enterprise  bool      `json:"enterprise"`
	Credit      float64   `json:"credit"`
	CycleCredit float64   `json:"cycle_credit"`
	Accounts    int       `json:"accounts"`
	ExpiredAt   string    `json:"expired_at"`
	ObservedAt  time.Time `json:"observed_at"`
}

// GetWorkBuddyQuota fetches the live balance of one WorkBuddy credential. It is
// read-only: it never writes task state or the auth file.
func (h *Handler) GetWorkBuddyQuota(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if !strings.EqualFold(auth.Provider, workbuddyauth.Provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth is not a workbuddy credential"})
		return
	}
	if storage, ok := auth.Storage.(*workbuddyauth.TokenStorage); ok && storage != nil && storage.NeedsRefresh(2*time.Minute) && manager != nil {
		refreshed, errRefresh := manager.ForceRefreshAuth(c.Request.Context(), auth.ID)
		if errRefresh != nil {
			log.Warnf("management: refresh workbuddy auth %s: %v", auth.ID, errRefresh)
		} else if refreshed != nil {
			auth = refreshed
		}
	}

	cfg := h.configSnapshot()
	client := workbuddyTasks.NewClient(cfg, auth, 30*time.Second)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	snapshot, errFetch := client.FetchBalance(ctx)
	if errFetch != nil {
		log.Errorf("management: fetch workbuddy quota for auth %s: %v", auth.ID, errFetch)
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota fetch failed"})
		return
	}

	c.JSON(http.StatusOK, workBuddyQuotaResponse{
		AuthIndex:   authIndex,
		Provider:    workbuddyauth.Provider,
		Enterprise:  snapshot.Enterprise,
		Credit:      snapshot.Credit,
		CycleCredit: snapshot.CycleCredit,
		Accounts:    snapshot.Accounts,
		ExpiredAt:   snapshot.ExpiredAt,
		ObservedAt:  time.Now().UTC(),
	})
}
