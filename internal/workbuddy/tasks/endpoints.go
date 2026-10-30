// Package tasks implements the concrete WorkBuddy (CodeBuddy CN) account
// maintenance tasks scheduled by internal/tasks.
package tasks

import (
	"fmt"
	"time"
)

// Upstream domains. A domain selects the base URL a request is sent to.
const (
	// domainChat selects the chat and growth upstream origin.
	domainChat = "chat"
	// domainBilling selects the billing upstream origin.
	domainBilling = "billing"
	// domainWeb selects the web activity upstream origin.
	domainWeb = "web"
)

// Default upstream origins.
const (
	// ChatBaseURL is the default chat and growth origin.
	ChatBaseURL = "https://copilot.tencent.com"
	// BillingBaseURL is the default billing origin.
	BillingBaseURL = "https://www.codebuddy.cn"
	// WebBaseURL is the default web activity origin.
	WebBaseURL = "https://www.workbuddy.cn"
)

// Request paths. All upstream paths are defined here so tasks never embed
// endpoint literals.
const (
	pathDailyCheckin       = "/v2/billing/meter/daily-checkin"
	pathUserResource       = "/v2/billing/meter/get-user-resource"
	pathEnterpriseUsage    = "/v2/billing/meter/get-enterprise-user-usage"
	pathReport             = "/v2/report"
	pathChatCompletions    = "/v2/chat/completions"
	pathGrowthTasks        = "/v2/activity/growth/tasks"
	pathGrowthAccept       = "/v2/activity/growth/tasks/accept"
	pathGrowthClaimFormat  = "/activity/growth/tasks/%s/claim"
	pathBuddyInfo          = "/activity/growth/buddy/info"
	pathBuddyTravelStatus  = "/activity/growth/buddy/travel/status"
	pathBuddyAgreement     = "/activity/growth/buddy/agreement"
	pathBuddyFirst         = "/activity/growth/buddy/first"
	pathBuddyTravelDepart  = "/activity/growth/buddy/travel/depart"
	pathBuddyTravelClaim   = "/activity/growth/buddy/travel/claim"
	pathCompensationStatus = "/billing/meter/compensation-status"
	pathClaimCompensation  = "/billing/meter/claim-compensation"
	pathHeatmap            = "/activity/growth/heatmap"
	pathStreak             = "/activity/growth/streak"
	pathMakeupCards        = "/activity/growth/makeup-cards/use"
	pathRedeem             = "/activity/growth/redeem"
	pathLotterySummary     = "/activity/growth/lottery/summary"
	pathLotteryDraw        = "/activity/growth/lottery/draw"
	pathClaimGift          = "/billing/meter/claim-gift"
)

// Outbound identity values. They mirror the official WorkBuddy clients.
const (
	productName      = "WorkBuddy"
	clientVersion    = "5.5.4"
	desktopVersion   = "5.5.6"
	cliVersion       = "2.137.1"
	desktopUserAgent = "WorkBuddy/" + desktopVersion + " WorkBuddy/" + desktopVersion + " CLI/" + cliVersion
	chatUserAgent    = "WorkBuddy/" + clientVersion + " WorkBuddy/" + clientVersion + " CLI/" + cliVersion
	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	webOrigin        = "https://www.workbuddy.cn"
	chatOrigin       = "https://www.codebuddy.cn"
	defaultDomain    = "copilot.tencent.com"

	// productCodePersonal is the personal resource product code.
	productCodePersonal = "p_tcaca"

	// defaultReportModel is the model reported by activity events.
	defaultReportModel = "glm-5.2"
	// nightOwlModel is the model used to generate night-owl activity.
	nightOwlModel = "glm-5.2"
	// nightOwlPrompt is the short prompt sent by the night-owl task.
	nightOwlPrompt = "1+1等于几？直接回答。"
	// nightOwlMaxMessages caps one night-owl run.
	nightOwlMaxMessages = 50
)

// Timing constants that are not part of the task schedule.
const (
	// nightOwlMessageInterval is the delay between night-owl chat messages.
	nightOwlMessageInterval = 4 * time.Second
	// travelUnlockDelay is the delay between the unlock report and adoption.
	travelUnlockDelay = 1050 * time.Millisecond
)

// pathGrowthClaim renders the growth claim path for a task code.
func pathGrowthClaim(code string) string {
	return fmt.Sprintf(pathGrowthClaimFormat, code)
}
