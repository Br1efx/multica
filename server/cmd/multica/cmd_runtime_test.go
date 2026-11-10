package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newRuntimeDeleteTestCmd(serverURL string) *cobra.Command {
	cmd := &cobra.Command{Use: "delete"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().Bool("cascade", false, "")
	cmd.Flags().String("output", "table", "")
	_ = cmd.Flags().Set("server-url", serverURL)
	_ = cmd.Flags().Set("workspace-id", "ws-1")
	return cmd
}

func captureRuntimeStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	// Read while fn runs. A pipe nobody is draining stops accepting writes long
	// before a command's output ends -- after 512 bytes on macOS -- so reading
	// only once fn has returned deadlocks on anything that prints more.
	type captured struct {
		out []byte
		err error
	}
	drained := make(chan captured, 1)
	go func() {
		out, err := io.ReadAll(r)
		drained <- captured{out, err}
	}()

	runErr := fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	got := <-drained
	if got.err != nil {
		t.Fatalf("read stdout: %v", got.err)
	}
	return string(got.out), runErr
}

func TestRunRuntimeDeleteStrictSuccessPrintsJSON(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	var deleteCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/api/runtimes/rt-1" {
			t.Fatalf("path = %q, want /api/runtimes/rt-1", r.URL.Path)
		}
		if r.Header.Get("X-Workspace-ID") != "ws-1" {
			t.Fatalf("X-Workspace-ID = %q, want ws-1", r.Header.Get("X-Workspace-ID"))
		}
		deleteCount++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	cmd := newRuntimeDeleteTestCmd(srv.URL)
	_ = cmd.Flags().Set("output", "json")

	out, err := captureRuntimeStdout(t, func() error {
		return runRuntimeDelete(cmd, []string{"rt-1"})
	})
	if err != nil {
		t.Fatalf("runRuntimeDelete: %v", err)
	}
	if deleteCount != 1 {
		t.Fatalf("deleteCount = %d, want 1", deleteCount)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode stdout JSON %q: %v", out, err)
	}
	if got["id"] != "rt-1" || got["deleted"] != true {
		t.Fatalf("stdout = %#v, want deleted result for rt-1", got)
	}
}

func TestRunRuntimeDeleteConflictSuggestsCascade(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/runtimes/rt-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":  "runtime_has_active_agents",
			"error": "cannot delete runtime: it has active agents bound to it.",
			"active_agents": []map[string]any{
				{"id": "agent-1", "name": "Codex"},
			},
		})
	}))
	defer srv.Close()

	err := runRuntimeDelete(newRuntimeDeleteTestCmd(srv.URL), []string{"rt-1"})
	if err == nil {
		t.Fatal("expected active-agent conflict")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Codex (agent-1)") || !strings.Contains(msg, "--cascade") {
		t.Fatalf("error = %q, want agent name and --cascade guidance", msg)
	}
}

func TestRunRuntimeDeleteCascadeConfirmsActiveAgentSnapshot(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	var gotExpectedIDs []string
	var deleteCount, cascadeCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/runtimes/rt-1":
			deleteCount++
			w.WriteHeader(http.StatusConflict)
			// The real 409 embeds a full agent snapshot per active agent, including
			// instructions. Two of those at 40 KiB each push the body past the old
			// 64 KiB cap; a truncated body neither parses nor yields a complete id set,
			// so the command used to print the prefix and never confirm (GH #10187).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":  "runtime_has_active_agents",
				"error": "cannot delete runtime: it has active agents bound to it.",
				"active_agents": []map[string]any{
					{"id": "agent-1", "name": "Codex", "instructions": strings.Repeat("a", 40<<10)},
					{"id": "agent-2", "name": "Claude", "instructions": strings.Repeat("b", 40<<10)},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/runtimes/rt-1/unbind-agents-and-delete":
			cascadeCount++
			var body struct {
				ExpectedActiveAgentIDs []string `json:"expected_active_agent_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode cascade body: %v", err)
			}
			gotExpectedIDs = body.ExpectedActiveAgentIDs
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":          "ok",
				"agents_unbound":  2,
				"tasks_cancelled": 1,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cmd := newRuntimeDeleteTestCmd(srv.URL)
	_ = cmd.Flags().Set("cascade", "true")
	_ = cmd.Flags().Set("output", "json")

	out, err := captureRuntimeStdout(t, func() error {
		return runRuntimeDelete(cmd, []string{"rt-1"})
	})
	if err != nil {
		t.Fatalf("runRuntimeDelete: %v", err)
	}
	if deleteCount != 1 || cascadeCount != 1 {
		t.Fatalf("deleteCount/cascadeCount = %d/%d, want 1/1", deleteCount, cascadeCount)
	}
	if strings.Join(gotExpectedIDs, ",") != "agent-1,agent-2" {
		t.Fatalf("expected_active_agent_ids = %#v", gotExpectedIDs)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode stdout JSON %q: %v", out, err)
	}
	if got["id"] != "rt-1" || got["deleted"] != true || got["agents_unbound"] != float64(2) {
		t.Fatalf("stdout = %#v, want cascade result", got)
	}
}

// A 409 that claims the active-agent refusal but does not parse is not a
// snapshot. The command must stop without confirming, and without printing the
// body: that is where agent instructions would leak.
func TestRunRuntimeDeleteCascadeUnreadableConflictDoesNotConfirm(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	const instructions = "SECRET-AGENT-INSTRUCTIONS-do-not-print"
	var cascadeCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/runtimes/rt-1":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"runtime_has_active_agents","error":"cannot delete","active_agents":[{"id":"agent-1","instructions":"`+instructions+`"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runtimes/rt-1/unbind-agents-and-delete":
			cascadeCount++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cmd := newRuntimeDeleteTestCmd(srv.URL)
	_ = cmd.Flags().Set("cascade", "true")

	err := runRuntimeDelete(cmd, []string{"rt-1"})
	if err == nil {
		t.Fatal("expected the unreadable conflict to stop the command")
	}
	if cascadeCount != 0 {
		t.Fatalf("cascadeCount = %d, want 0: an unreadable snapshot must not be confirmed", cascadeCount)
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be read") {
		t.Fatalf("error = %q, want an actionable read failure", msg)
	}
	if strings.Contains(msg, instructions) || strings.Contains(msg, "active_agents") {
		t.Fatalf("error leaked the response body: %q", msg)
	}
}

// A profile-backed instance refusal is not something --cascade can retry past,
// and the server already explains what to do instead. Wrapping it in
// HTTPError.Error() used to print the whole JSON response at the user, burying
// that guidance (GH #8456).
func TestRunRuntimeDeleteProfileInstanceConflictShowsServerGuidance(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	const guidance = `cannot delete "MSI-S3TEST" on its own: it is registered from the custom runtime profile "Devin CLI (WSL)". It is offline, and Multica removes offline runtimes automatically after 7 days.`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":                    "runtime_profile_instance_delete_unsupported",
			"error":                   guidance,
			"profile_name":            "Devin CLI (WSL)",
			"auto_cleanup_after_days": 7,
		})
	}))
	defer srv.Close()

	err := runRuntimeDelete(newRuntimeDeleteTestCmd(srv.URL), []string{"rt-1"})
	if err == nil {
		t.Fatal("expected the profile-instance refusal to surface as an error")
	}
	if err.Error() != guidance {
		t.Fatalf("error = %q, want the server's guidance verbatim", err.Error())
	}
	if strings.Contains(err.Error(), "auto_cleanup_after_days") {
		t.Fatalf("raw JSON leaked into the user-facing error: %q", err.Error())
	}
}

// A 409 with no readable sentence must still fall back to the wrapper rather
// than surfacing an empty error.
func TestRunRuntimeDeleteConflictWithoutMessageKeepsWrapper(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"something_new"}`))
	}))
	defer srv.Close()

	err := runRuntimeDelete(newRuntimeDeleteTestCmd(srv.URL), []string{"rt-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "delete runtime:") {
		t.Fatalf("error = %q, want the generic wrapper", err.Error())
	}
}
