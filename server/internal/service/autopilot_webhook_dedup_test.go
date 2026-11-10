package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// webhookDedupSeq keeps generated names unique across parallel subtests.
var webhookDedupSeq atomic.Int64

// webhookDedupFixture builds one create_issue autopilot whose rendered issue
// title never changes between runs: no issue_title_template, so every event
// would share a title and the old guard would treat any two of them inside 60
// seconds as the same event.
type webhookDedupFixture struct {
	principalFixture
	autopilotID string
	triggerID   string
}

func newWebhookDedupFixture(t *testing.T) webhookDedupFixture {
	t.Helper()
	fx, ownerID := newPrincipalFixture(t)
	n := webhookDedupSeq.Add(1)
	runtimeID := fx.Runtime(t, fmt.Sprintf("dedup-rt-%d", n), testutil.Cols{"owner_id": ownerID})
	agentID := fx.Agent(t, fmt.Sprintf("dedup-agent-%d", n), runtimeID, testutil.Cols{
		"owner_id":        ownerID,
		"permission_mode": "public_to",
		"visibility":      "workspace",
	})
	fx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id":    agentID,
		"target_type": "workspace",
		"target_id":   fx.WorkspaceID,
	})
	autopilotID := fx.Insert(t, "autopilot", testutil.Cols{
		"workspace_id":    fx.WorkspaceID,
		"title":           "GitLab MR review",
		"assignee_type":   "agent",
		"assignee_id":     agentID,
		"status":          "active",
		"execution_mode":  "create_issue",
		"created_by_type": "member",
		"created_by_id":   ownerID,
	})
	triggerID := fx.Insert(t, "autopilot_trigger", testutil.Cols{
		"autopilot_id":    autopilotID,
		"kind":            "webhook",
		"enabled":         true,
		"created_by_type": "member",
		"created_by_id":   ownerID,
	})
	return webhookDedupFixture{principalFixture: fx, autopilotID: autopilotID, triggerID: triggerID}
}

// gitlabMergeRequestPayload is the envelope a GitLab merge-request hook lands
// in autopilot_run.trigger_payload as. Only object_kind and the object's iid
// decide the subject; action is what GitLab varies across the burst one change
// produces, so it must not.
func gitlabMergeRequestPayload(iid int, action string) []byte {
	return []byte(fmt.Sprintf(`{"event":"gitlab.Merge Request Hook","eventPayload":{"object_kind":"merge_request","object_attributes":{"iid":%d,"action":"%s"}}}`, iid, action))
}

func (f webhookDedupFixture) dispatchWebhook(t *testing.T, payload []byte) *db.AutopilotRun {
	t.Helper()
	ap, err := f.q.GetAutopilot(context.Background(), util.MustParseUUID(f.autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, err := f.svc.DispatchAutopilot(context.Background(), ap, util.MustParseUUID(f.triggerID), "webhook", payload)
	if err != nil {
		t.Fatalf("DispatchAutopilot: %v", err)
	}
	if run == nil {
		t.Fatal("dispatch returned no run")
	}
	return run
}

func (f webhookDedupFixture) issuesTitled(t *testing.T, title string) int {
	t.Helper()
	return f.Count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2`, f.WorkspaceID, title)
}

// TestWebhookAutopilotDedup_DistinctEventsCreateDistinctIssues is the bug from
// issue #10181: two webhook deliveries inside the 60 second window, about two
// different merge requests, must each create an issue. Before the fix the
// second was skipped with reason_code=already_active because both rendered the
// autopilot's own title.
func TestWebhookAutopilotDedup_DistinctEventsCreateDistinctIssues(t *testing.T) {
	f := newWebhookDedupFixture(t)

	first := f.dispatchWebhook(t, gitlabMergeRequestPayload(658, "update"))
	if first.Status != "issue_created" {
		t.Fatalf("first delivery status = %q, want issue_created", first.Status)
	}

	second := f.dispatchWebhook(t, gitlabMergeRequestPayload(704, "open"))
	if second.Status != "issue_created" {
		t.Fatalf("second delivery status = %q (%s), want issue_created: a different merge request must not be swallowed", second.Status, second.FailureReason.String)
	}
	if second.IssueID == first.IssueID {
		t.Fatal("second delivery reused the first delivery's issue")
	}
	if got := f.issuesTitled(t, "GitLab MR review"); got != 2 {
		t.Fatalf("issues titled %q = %d, want 2", "GitLab MR review", got)
	}
}

// TestWebhookAutopilotDedup_SameEventBurstCollapses is the reason the guard
// exists. GitLab answers one merge-request action with open, then update, then
// merge_status, seconds apart and with a fresh event UUID each time. All of
// them are about the same merge request, so only the first creates an issue and
// the rest are recorded as already active.
func TestWebhookAutopilotDedup_SameEventBurstCollapses(t *testing.T) {
	f := newWebhookDedupFixture(t)

	first := f.dispatchWebhook(t, gitlabMergeRequestPayload(658, "open"))
	if first.Status != "issue_created" {
		t.Fatalf("first delivery status = %q, want issue_created", first.Status)
	}

	for _, action := range []string{"update", "merge_status"} {
		retry := f.dispatchWebhook(t, gitlabMergeRequestPayload(658, action))
		if retry.Status != "skipped" {
			t.Fatalf("action %s status = %q, want skipped", action, retry.Status)
		}
		if !retry.ReasonCode.Valid || retry.ReasonCode.String != "already_active" {
			t.Fatalf("action %s reason_code = %q, want already_active", action, retry.ReasonCode.String)
		}
	}
	if got := f.issuesTitled(t, "GitLab MR review"); got != 1 {
		t.Fatalf("issues titled %q = %d, want 1: the burst must collapse onto one issue", "GitLab MR review", got)
	}
}

// TestWebhookAutopilotDedup_UnrecognisedPayloadKeepsTitleGuard pins the
// fallback. A webhook whose payload names no subject has nothing to tell two
// deliveries apart, so the guard keeps matching on the rendered title and a
// second delivery inside the window is still merged.
func TestWebhookAutopilotDedup_UnrecognisedPayloadKeepsTitleGuard(t *testing.T) {
	f := newWebhookDedupFixture(t)
	payload := []byte(`{"event":"webhook.received","eventPayload":{"hello":"world"}}`)

	first := f.dispatchWebhook(t, payload)
	if first.Status != "issue_created" {
		t.Fatalf("first delivery status = %q, want issue_created", first.Status)
	}
	retry := f.dispatchWebhook(t, payload)
	if retry.Status != "skipped" || !retry.ReasonCode.Valid || retry.ReasonCode.String != "already_active" {
		t.Fatalf("retry status = %q reason_code = %q, want skipped / already_active", retry.Status, retry.ReasonCode.String)
	}
}

func TestWebhookDedupKey(t *testing.T) {
	gitlab := func(body string) []byte {
		return []byte(`{"event":"gitlab.Merge Request Hook","eventPayload":` + body + `}`)
	}
	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "gitlab merge request ignores the action",
			payload: gitlabMergeRequestPayload(704, "open"),
			want:    "gitlab:merge_request:704",
		},
		{
			name:    "gitlab note hangs off the merge request it comments on",
			payload: gitlab(`{"object_kind":"note","object_attributes":{"id":9,"noteable_type":"MergeRequest"},"merge_request":{"iid":658}}`),
			want:    "gitlab:note:658",
		},
		{
			name:    "gitlab push uses the commit",
			payload: gitlab(`{"object_kind":"push","checkout_sha":"abc123"}`),
			want:    "gitlab:push:abc123",
		},
		{
			name:    "github pull request uses its number",
			payload: []byte(`{"event":"github.pull_request.opened","eventPayload":{"action":"opened","pull_request":{"number":12}}}`),
			want:    "github:pull_request:12",
		},
		{
			name:    "webhook without a subject has no key",
			payload: []byte(`{"event":"webhook.received","eventPayload":{"hello":"world"}}`),
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webhookDedupKey(tc.payload)
			if got != tc.want {
				t.Fatalf("webhookDedupKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// Referenced so a future edit that drops the pgtype import fails here rather
// than inside a helper this file grows later.
var _ = pgtype.UUID{}
