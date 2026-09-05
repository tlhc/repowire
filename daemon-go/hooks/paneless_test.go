package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// paneLessEnvironment runs the hooks as a session that owns no tmux pane: tmux
// resolves nothing and the circle arrives from the environment instead.
func paneLessEnvironment(t *testing.T) string {
	t.Helper()
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	t.Setenv("TMUX_PANE", "")
	t.Setenv(runtimeKeyEnv, "")
	t.Setenv(circleEnv, "default")
	t.Setenv(claudeMessagingSocketEnv, "")
	return homeDir
}

func stubStdin(t *testing.T, payload map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.json")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = previous; file.Close() })
}

func TestPaneLessSessionRegistersWithEnvironmentCircle(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	var registration map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/peers" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&registration)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"peer_id": "repow-paneless", "display_name": "pi-claude-code",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{}})
		}
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)

	startedCircle, started := "", false
	previous := startSessionWSHook
	startSessionWSHook = func(paneID, _, _, _, _, circle string, _ int, _ *os.File) error {
		started, startedCircle = true, circle
		if paneID != "" {
			t.Errorf("pane-less session passed pane %q to the ws-hook", paneID)
		}
		return nil
	}
	t.Cleanup(func() { startSessionWSHook = previous })

	handleSession(map[string]any{
		"hook_event_name": "SessionStart", "session_id": "session-paneless", "cwd": homeDir,
	}, "claude-code", false)

	if registration == nil {
		t.Fatal("pane-less SessionStart never registered with the daemon")
	}
	if registration["circle"] != "default" {
		t.Fatalf("registered circle = %v, want default", registration["circle"])
	}
	if _, present := registration["pane_id"]; present {
		t.Fatalf("pane-less registration claimed a pane: %v", registration["pane_id"])
	}
	if !started || startedCircle != "default" {
		t.Fatalf("ws-hook started=%v circle=%q, want true/default", started, startedCircle)
	}
	if peerID := stringValue(ReadPaneRuntimeMetadata(""), "peer_id"); peerID != "repow-paneless" {
		t.Fatalf("runtime metadata peer_id = %q, want repow-paneless", peerID)
	}
}

func TestPaneLessCompactReusesLivePeer(t *testing.T) {
	t.Setenv(claudeMessagingSocketEnv, "uds:/tmp/stale-inbox")
	homeDir := paneLessEnvironment(t)
	bindRuntimeKey("", "session-compact")
	if err := writeMetadata("", map[string]any{
		"backend": "claude-code", "cwd": homeDir, "display_name": "pi-claude-code",
		"hook_session_id": "session-compact", "peer_id": "repow-paneless",
	}); err != nil {
		t.Fatal(err)
	}
	holdExclusiveLock(t, wsHookPath("", ".lock"))

	for _, status := range []string{"busy", "online"} {
		t.Run(status, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/peers" && r.Method == http.MethodPost:
					posts++
					_ = json.NewEncoder(w).Encode(map[string]any{
						"peer_id": "repow-paneless", "display_name": "pi-claude-code",
					})
				case r.URL.Path == "/peers":
					_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{
						map[string]any{"peer_id": "repow-paneless", "status": status},
					}})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{})
				}
			}))
			defer server.Close()
			configureHookTestDaemon(t, server.URL)
			started := stubWSHook(t)

			handleSession(map[string]any{
				"hook_event_name": "SessionStart", "session_id": "session-compact", "cwd": homeDir,
			}, "claude-code", false)

			if posts != 0 || *started {
				t.Fatalf("posts=%d started=%v, want reuse of the live %s peer", posts, *started, status)
			}
			if peerID := stringValue(ReadPaneRuntimeMetadata(""), "peer_id"); peerID != "repow-paneless" {
				t.Fatalf("compact cleared runtime metadata peer_id=%q", peerID)
			}
		})
	}
}

func holdExclusiveLock(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", `
import fcntl, sys, time
f = open(sys.argv[1], "a")
fcntl.flock(f.fileno(), fcntl.LOCK_EX)
sys.stdout.write("held\n")
sys.stdout.flush()
time.sleep(3)
`, path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	got := make([]byte, 5)
	if _, err := stdout.Read(got); err != nil {
		t.Fatalf("lock holder: %v", err)
	}
}

func TestPaneLessStateFilesAreScopedPerSession(t *testing.T) {
	paneLessEnvironment(t)
	t.Setenv(runtimeKeyEnv, "")
	bindRuntimeKey("", "session-a")
	first := wsHookPath("", ".lock")

	t.Setenv(runtimeKeyEnv, "")
	bindRuntimeKey("", "session-b")
	second := wsHookPath("", ".lock")

	if first == second {
		t.Fatalf("two pane-less sessions share one lock file: %s", first)
	}
}

func TestPaneLessStopAddressesPeerByID(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	bindRuntimeKey("", "session-stop")
	if err := writeMetadata("", map[string]any{
		"backend": "claude-code", "cwd": homeDir, "display_name": "pi-claude-code",
		"peer_id": "repow-paneless", "circle": "default",
	}); err != nil {
		t.Fatal(err)
	}

	var update, chat map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/update":
			_ = json.NewDecoder(r.Body).Decode(&update)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/events/chat":
			_ = json.NewDecoder(r.Body).Decode(&chat)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/deliveries/pending":
			if r.URL.Query().Get("peer_id") != "repow-paneless" {
				t.Errorf("queued deliveries queried with %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deliveries": []any{}})
		case "/asks/pending":
			if r.URL.Query().Get("peer_id") != "repow-paneless" {
				t.Errorf("pending asks queried with %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"asks": []any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)

	stubStdin(t, map[string]any{
		"hook_event_name": "Stop", "session_id": "session-stop", "cwd": homeDir,
		"prompt_response": "done",
	})
	runStop("claude-code")

	if update == nil {
		t.Fatal("pane-less Stop never reported status to the daemon")
	}
	if update["peer_name"] != "repow-paneless" {
		t.Fatalf("Stop addressed peer_name=%v, want repow-paneless", update["peer_name"])
	}
	if _, present := update["pane_id"]; present {
		t.Fatalf("pane-less Stop claimed a pane: %v", update["pane_id"])
	}
	if chat["peer_id"] != "repow-paneless" || chat["peer"] != "pi-claude-code" {
		t.Fatalf("chat payload = %v, want peer_id=repow-paneless peer=pi-claude-code", chat)
	}
}

func TestArgvHostsAppServerMatchesSubcommandOnly(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"/opt/homebrew/bin/codex", "app-server", "--listen", "unix://"}, true},
		{[]string{"/Applications/ChatGPT.app/Contents/Resources/codex", "-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled"}, true},
		{[]string{"codex", "-c", "developer_instructions=Use concise replies", "app-server", "--listen", "unix://"}, true},
		{[]string{"codex", "--dangerously-bypass-approvals-and-sandbox", "Explain app-server connection failures"}, false},
		{[]string{"codex", "Explain", "codex", "app-server", "connection", "failures"}, false},
		{[]string{"codex", "--", "app-server"}, false},
		{[]string{"codex", "-c", "features.code_mode_host=true", "resume", "--last"}, false},
	}
	for _, tc := range cases {
		if got := argvHostsAppServer(tc.argv); got != tc.want {
			t.Errorf("argvHostsAppServer(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestProcessArgvSelf(t *testing.T) {
	argv, err := processArgv(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 {
		t.Fatal("processArgv returned no arguments")
	}
}

func TestRetireKeepsPaneLessLock(t *testing.T) {
	paneLessEnvironment(t)
	bindRuntimeKey("", "session-lock")
	lock := wsHookPath("", ".lock")
	if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	retire("")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("pane-less lock removed on retire: %v", err)
	}
}

func TestPaneProbeIsInconclusiveWithoutPane(t *testing.T) {
	paneLessEnvironment(t)
	if safe := paneSafe("", ""); safe != nil {
		t.Fatalf("pane probe returned %v for a pane-less peer; a false verdict retires it", *safe)
	}
}

func TestPaneTokenDigestsKeysWithoutSafeCharacters(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	t.Setenv(runtimeKeyEnv, "...")
	dots := paneToken("")
	if dots == "unknown" || dots == "" {
		t.Fatalf("paneToken for runtime key %q = %q, want a digest", "...", dots)
	}
	t.Setenv(runtimeKeyEnv, "/ /")
	if slashes := paneToken(""); slashes == dots || slashes == "unknown" {
		t.Fatalf("paneToken for runtime key %q = %q, collides with %q", "/ /", slashes, dots)
	}
}

func TestClearPaneRuntimeStateRemovesSessionFiles(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	bindRuntimeKey("", "session-clear")
	cert := map[string]any{"nonce": "clear-proof"}
	if err := WriteRuntimeIdentity("codex", "session-clear", map[string]any{"birth_certificate": cert}); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata("", map[string]any{
		"backend": "codex", "cwd": homeDir, "hook_session_id": "session-clear",
		"agent_pid": 4242, "birth_certificate": cert,
	}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".log", ".pid"} {
		if err := os.WriteFile(wsHookPath("", suffix), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ClearPaneRuntimeState("")

	for _, path := range []string{
		wsHookPath("", ".pid"), wsHookPath("", ".meta.json"), wsHookPath("", ".log"),
		birthCertificatePath("codex", 4242, ""), runtimeIdentityPath("codex", "session-clear"),
	} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s survived ClearPaneRuntimeState", filepath.Base(path))
		}
	}
}

type fakeDaemon struct {
	registrations int
	registration  map[string]any
	update        map[string]any
}

// serveFakeDaemon answers the requests a standalone Codex session makes while
// registering, reporting status, and validating its certificate.
func serveFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/peers" && r.Method == http.MethodPost:
			d.registrations++
			_ = json.NewDecoder(r.Body).Decode(&d.registration)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"peer_id": "repow-codex-cli", "display_name": "repowire-codex",
				"birth_certificate": map[string]any{"nonce": "codex-proof", "peer_id": "repow-codex-cli"},
			})
		case r.URL.Path == "/peers/identity/validate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			cert, _ := body["birth_certificate"].(map[string]any)
			if stringValue(cert, "nonce") != "codex-proof" {
				http.Error(w, "unknown certificate", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"peer": map[string]any{"peer_id": "repow-codex-cli", "display_name": "repowire-codex"}})
		case r.URL.Path == "/session/update":
			_ = json.NewDecoder(r.Body).Decode(&d.update)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{}, "deliveries": []any{}, "asks": []any{}})
		}
	}))
	t.Cleanup(server.Close)
	configureHookTestDaemon(t, server.URL)
	return d
}

func stubWSHook(t *testing.T) *bool {
	t.Helper()
	started := false
	previous := startSessionWSHook
	startSessionWSHook = func(_, _, _, _, _, _ string, _ int, _ *os.File) error {
		started = true
		return nil
	}
	t.Cleanup(func() { startSessionWSHook = previous })
	return &started
}

func writeSpawnHint(t *testing.T, path, backend string, hint map[string]any) {
	t.Helper()
	abs, _ := filepath.Abs(path)
	hash := sha256.Sum256([]byte(abs + "::" + backend))
	target := cachePath("spawn-hints", hex.EncodeToString(hash[:])[:16]+".json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	hint["ts"] = float64(time.Now().Unix())
	raw, _ := json.Marshal([]map[string]any{hint})
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSessionStartHonorsSpawnHintWithoutPane(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	t.Setenv(circleEnv, "")
	writeSpawnHint(t, homeDir, "codex", map[string]any{"circle": "hinted", "peer_id": "repow-hinted"})
	daemon := serveFakeDaemon(t)
	stubWSHook(t)

	handleSession(map[string]any{"hook_event_name": "SessionStart", "session_id": "thread-hinted", "cwd": homeDir}, "codex", false)

	t.Run("hint peer_id and circle", func(t *testing.T) {
		r := daemon.registration
		if r["peer_id"] != "repow-hinted" || r["circle"] != "hinted" || r["circle_source"] != "spawn_hint" {
			t.Fatalf("registration = %v, want the hinted peer_id and circle", r)
		}
	})
	t.Run("thread identity", func(t *testing.T) {
		cert, _ := ReadRuntimeIdentity("codex", "thread-hinted")["birth_certificate"].(map[string]any)
		if stringValue(cert, "nonce") != "codex-proof" {
			t.Fatalf("runtime identity for the thread = %v, want the registration certificate", cert)
		}
	})
}

func TestPaneLessSessionFallsBackToProjectCircle(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	t.Setenv(circleEnv, "")
	daemon := serveFakeDaemon(t)
	stubWSHook(t)

	handleSession(map[string]any{"hook_event_name": "SessionStart", "session_id": "thread-project", "cwd": homeDir}, "codex", false)

	if want := projectCircle(homeDir); daemon.registration["circle"] != want || daemon.registration["circle_source"] != "fallback" {
		t.Fatalf("registration circle = %v (%v), want %s (fallback)", daemon.registration["circle"], daemon.registration["circle_source"], want)
	}
}

func TestStopRegistersPaneLessSessionThatMissedSessionStart(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	daemon := serveFakeDaemon(t)
	started := stubWSHook(t)
	stubStdin(t, map[string]any{"hook_event_name": "Stop", "session_id": "session-late", "cwd": homeDir, "prompt_response": "done"})

	runStop("codex")

	if daemon.registrations != 1 || !*started {
		t.Fatalf("registrations=%d ws-hook started=%v, want one repair registration", daemon.registrations, *started)
	}
	if daemon.update["peer_name"] != "repow-codex-cli" {
		t.Fatalf("Stop addressed %v, want the repaired peer id", daemon.update["peer_name"])
	}
}

func TestAppServerThreadsAreLeftToTheBridge(t *testing.T) {
	homeDir := paneLessEnvironment(t)
	previous := hostedByAppServer
	hostedByAppServer = func(int) bool { return true }
	t.Cleanup(func() { hostedByAppServer = previous })
	daemon := serveFakeDaemon(t)
	started := stubWSHook(t)

	t.Run("session start", func(t *testing.T) {
		handleSession(map[string]any{"hook_event_name": "SessionStart", "session_id": "thread-app", "cwd": homeDir}, "codex", false)
		if daemon.registrations != 0 || *started {
			t.Fatalf("registrations=%d started=%v, want the bridge to own the thread", daemon.registrations, *started)
		}
	})
	t.Run("stop", func(t *testing.T) {
		stubStdin(t, map[string]any{"hook_event_name": "Stop", "session_id": "thread-app", "cwd": homeDir, "prompt_response": "done"})
		runStop("codex")
		if daemon.registrations != 0 || daemon.update != nil {
			t.Fatalf("registrations=%d update=%v, want no lifecycle traffic", daemon.registrations, daemon.update)
		}
	})
	t.Run("mcp identity", func(t *testing.T) {
		t.Setenv("REPOWIRE_BACKEND", "codex")
		t.Setenv("CODEX_THREAD_ID", "")
		identity, proof := MCPIdentityProofForThread("thread-app")
		if proof != "" || daemon.registrations != 0 {
			t.Fatalf("identity=%q proof=%q registrations=%d, want an anonymous call", identity, proof, daemon.registrations)
		}
	})
}

func TestMCPIdentityRegistersStandaloneCodexThread(t *testing.T) {
	paneLessEnvironment(t)
	t.Setenv(circleEnv, "")
	t.Setenv("REPOWIRE_BACKEND", "codex")
	t.Setenv("CODEX_THREAD_ID", "")
	daemon := serveFakeDaemon(t)

	identity, proof := MCPIdentityProofForThread("thread-mcp")

	t.Run("project circle without a pane", func(t *testing.T) {
		if identity != "repow-codex-cli" || proof != "codex-proof" {
			t.Fatalf("identity=%q proof=%q, want the registered peer", identity, proof)
		}
		if want := projectCircle(mustGetwd()); daemon.registration["circle"] != want {
			t.Fatalf("registered circle = %v, want %s", daemon.registration["circle"], want)
		}
		if _, present := daemon.registration["pane_id"]; present {
			t.Fatalf("pane-less shim claimed a pane: %v", daemon.registration["pane_id"])
		}
	})
	t.Run("thread stays bound", func(t *testing.T) {
		identity, proof := MCPIdentityProofForThread("thread-mcp")
		if identity != "repow-codex-cli" || proof != "codex-proof" || daemon.registrations != 1 {
			t.Fatalf("identity=%q proof=%q registrations=%d, want the bound thread without a second registration", identity, proof, daemon.registrations)
		}
	})
}

func TestWriteBirthCertificateSkipsUnknownToken(t *testing.T) {
	paneLessEnvironment(t)
	t.Setenv(runtimeKeyEnv, "")
	t.Setenv("TMUX_PANE", "")
	writeBirthCertificate("codex", 54807, "", map[string]any{"nonce": "orphan"})
	path := birthCertificatePath("codex", 54807, "")
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("wrote %s for an unscoped pane-less process", path)
	}
}
