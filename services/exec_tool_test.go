package services

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSecretEnvKey(t *testing.T) {
	secret := []string{"AGENT_TOKEN", "NNTP_PASS", "NNTP_USER", "VPN_USER", "VPN_PASS",
		"nntp_pass", "MY_API_TOKEN", "DB_PASSWORD", "SOME_SECRET", "AWS_CRED", "TLS_KEY", "APIKEY"}
	for _, k := range secret {
		if !secretEnvKey(k) {
			t.Errorf("secretEnvKey(%q) = false, want true", k)
		}
	}
	safe := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "NVIDIA_VISIBLE_DEVICES",
		"CUDA_HOME", "LD_LIBRARY_PATH", "NNTP_SERVER", "SITE_URL", "TERM"}
	for _, k := range safe {
		if secretEnvKey(k) {
			t.Errorf("secretEnvKey(%q) = true, want false (would strip a needed var)", k)
		}
	}
}

func TestToolEnv_StripsSecretsKeepsRest(t *testing.T) {
	t.Setenv("NNTP_PASS", "hunter2")
	t.Setenv("AGENT_TOKEN", "tok_secret")
	t.Setenv("PATH_MARKER_TEST", "keepme") // non-secret sentinel

	env := toolEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "NNTP_PASS=") || strings.HasPrefix(kv, "AGENT_TOKEN=") {
			t.Errorf("toolEnv leaked a secret: %q", kv)
		}
	}
	var kept bool
	for _, kv := range env {
		if kv == "PATH_MARKER_TEST=keepme" {
			kept = true
		}
	}
	if !kept {
		t.Error("toolEnv dropped a non-secret var (PATH_MARKER_TEST)")
	}
	// The real process PATH must survive — tools need it to find deps.
	if os.Getenv("PATH") != "" {
		var hasPath bool
		for _, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				hasPath = true
			}
		}
		if !hasPath {
			t.Error("toolEnv dropped PATH")
		}
	}
}

func TestExitSignal(t *testing.T) {
	if got := exitSignal(errors.New("signal: segmentation fault")); got != "signal: segmentation fault" {
		t.Errorf("exitSignal segfault = %q", got)
	}
	if got := exitSignal(errors.New("exit status 1")); got != "" {
		t.Errorf("exitSignal non-signal = %q, want empty", got)
	}
	if got := exitSignal(nil); got != "" {
		t.Errorf("exitSignal(nil) = %q, want empty", got)
	}
}

// TestEscalateToolCrash pins that a signal kill reaches ToolFailureSink
// (the site) while a plain non-zero exit and a nil error do not — the
// crash-visibility contract, and the guarantee that adding this call
// anywhere is behavior-safe for non-crash paths.
func TestEscalateToolCrash(t *testing.T) {
	var got string
	prev := ToolFailureSink
	ToolFailureSink = func(level, message string) { got = level + ":" + message }
	defer func() { ToolFailureSink = prev }()

	escalateToolCrash("ffmpeg", "bad.mkv", []byte("boom"), errors.New("signal: segmentation fault"))
	if !strings.HasPrefix(got, "error:") || !strings.Contains(got, "CRASHED") {
		t.Errorf("signal kill did not escalate to sink: %q", got)
	}

	got = ""
	escalateToolCrash("ffmpeg", "bad.mkv", []byte("boom"), errors.New("exit status 1"))
	if got != "" {
		t.Errorf("plain exit wrongly escalated to sink: %q", got)
	}

	got = ""
	escalateToolCrash("ffmpeg", "ok.mkv", nil, nil)
	if got != "" {
		t.Errorf("nil error wrongly escalated to sink: %q", got)
	}
}
