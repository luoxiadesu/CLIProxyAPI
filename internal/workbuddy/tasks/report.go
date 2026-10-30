package tasks

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	taskpkg "github.com/router-for-me/CLIProxyAPI/v8/internal/tasks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// reportTask reports a chat request event so upstream activity counters advance.
type reportTask struct {
	cfg  *config.Config
	spec taskpkg.Spec
}

// Spec returns the task schedule.
func (t *reportTask) Spec() taskpkg.Spec { return t.spec }

// Run posts one chat_request_send event. The upstream silently drops events
// without a userId, so a missing uid is treated as a no-op.
func (t *reportTask) Run(ctx context.Context, auth *coreauth.Auth) error {
	client := newTaskClient(t.cfg, auth, t.spec)
	if client.IsEnterpriseSwitch() {
		return nil
	}
	if client.uid == "" {
		log.WithField("auth_id", authID(auth)).Debug("workbuddy report: uid is empty; skipping")
		return nil
	}
	events := buildChatEvents(client.uid, defaultReportModel, client.nowTime().UnixMilli(), 1)
	if errReport := client.sendReport(ctx, events); errReport != nil {
		return fmt.Errorf("workbuddy report: %w", errReport)
	}
	setStateValue(taskState(ctx), "last_report_at", client.nowTime().UTC().Format("2006-01-02T15:04:05Z07:00"))
	return nil
}

// buildChatEvents builds count chat_request_send events.
func buildChatEvents(uid, model string, timestampMillis int64, count int) []map[string]any {
	if count <= 0 {
		return nil
	}
	events := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		events = append(events, map[string]any{
			"eventCode":        "chat_request_send",
			"timestamp":        timestampMillis,
			"conversationId":   newUUID(),
			"requestId":        newUUID(),
			"inputLength":      1,
			"requestModelId":   model,
			"requestModelName": model,
			"userId":           uid,
		})
	}
	return events
}

// sendReport posts activity events to the billing report endpoint.
func (c *Client) sendReport(ctx context.Context, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	_, errPost := c.PostArray(ctx, domainBilling, pathReport, c.billingHeaders(), events)
	return errPost
}

// authID returns the auth record id for logging.
func authID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}
