package core

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageRouteDispositionUsesCoreRegistries(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	e.AddCommand("prompted", "", "Review {{args}}", "", "", "config")
	e.AddCommand("direct", "", "", "printf ok", "", "config")
	e.AddCommand("quiet", "", "shadowed", "", "", "config")
	e.AddAlias("/q", "/quiet")

	skillRoot := t.TempDir()
	writeSkillFile(t, filepath.Join(skillRoot, "review", "SKILL.md"), "Review skill")
	e.skills.SetDirs([]string{skillRoot})

	tests := []struct {
		name    string
		content string
		want    MessageRouteDisposition
	}{
		{name: "global builtin", content: "/quiet compact", want: MessageRouteInPlace},
		{name: "session builtin", content: "/new private", want: MessageRouteInPlace},
		{name: "start registration", content: "/start", want: MessageRouteInPlace},
		{name: "unique builtin prefix", content: "/vers", want: MessageRouteInPlace},
		{name: "builtin wins custom collision", content: "/quiet", want: MessageRouteInPlace},
		{name: "alias inherits builtin", content: "/q", want: MessageRouteInPlace},
		{name: "custom exec", content: "/direct", want: MessageRouteInPlace},
		{name: "custom prompt", content: "/prompted code", want: MessageRouteDefault},
		{name: "skill", content: "/review code", want: MessageRouteDefault},
		{name: "unknown extension", content: "/plugin-command secret", want: MessageRouteDefault},
		{name: "ambiguous builtin prefix", content: "/s", want: MessageRouteDefault},
		{name: "ordinary text", content: "hello", want: MessageRouteDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.messageRouteDisposition(&Message{Content: tt.content, UserID: "7"}); got != tt.want {
				t.Fatalf("disposition = %v, want %v", got, tt.want)
			}
		})
	}

	e.SetDisabledCommands([]string{"prompted", "review"})
	for _, content := range []string{"/prompted private", "/review private"} {
		if got := e.messageRouteDisposition(&Message{Content: content, UserID: "7"}); got != MessageRouteInPlace {
			t.Fatalf("disabled %q disposition = %v, want in-place", content, got)
		}
	}
}

func TestMessageRouteDispositionKeepsPendingControlInputInPlace(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	e.initFlows = map[string]*workspaceInitFlow{
		workspaceChannelKey("telegram", "-100123:1"): {state: "awaiting_url"},
	}
	msg := &Message{SessionKey: "telegram:-100123:1:7", Platform: "telegram", ChannelKey: "-100123:1", Content: "secret follow-up"}
	if got := e.messageRouteDisposition(msg); got != MessageRouteInPlace {
		t.Fatalf("pending workspace flow = %v, want in-place", got)
	}
	clear(e.initFlows)

	e.interactiveStates[msg.SessionKey] = &interactiveState{pendingProviderAdd: &pendingProviderAddState{phase: "preset"}}
	if got := e.messageRouteDisposition(msg); got != MessageRouteInPlace {
		t.Fatalf("pending provider flow = %v, want in-place", got)
	}
}

func TestBuiltinSessionRequirements(t *testing.T) {
	tests := []struct {
		id       string
		typed    string
		args     []string
		required bool
	}{
		{id: "new", typed: "new", required: true},
		{id: "switch", typed: "switch", required: true},
		{id: "name", typed: "name-current", required: true},
		{id: "name", typed: "name", args: []string{"topic"}, required: true},
		{id: "name", typed: "name", args: []string{"2", "archived"}},
		{id: "name", typed: "name", args: []string{"0", "invalid"}, required: true},
		{id: "current", typed: "current", required: true},
		{id: "history", typed: "history", required: true},
		{id: "stop", typed: "stop", required: true},
		{id: "compress", typed: "compress", required: true},
		{id: "ps", typed: "ps", required: true},
		{id: "cron", typed: "cron", args: []string{"add"}, required: true},
		{id: "cron", typed: "cron", args: []string{"addexec"}, required: true},
		{id: "cron", typed: "cron", args: []string{"list"}},
		{id: "quiet", typed: "quiet"},
	}
	for _, tt := range tests {
		if got := builtinCommandRequiresSession(tt.id, tt.typed, tt.args); got != tt.required {
			t.Errorf("%s %v requires session = %v, want %v", tt.typed, tt.args, got, tt.required)
		}
	}
}

func TestSessionlessBuiltinsDoNotCreateGeneralSession(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	p := &stubPlatformEngine{n: "telegram"}
	key := "telegram:-100123:1:7"

	e.handleMessage(p, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Content: "/quiet quiet", Sessionless: true})
	if got := e.sessions.ActiveSessionID(key); got != "" {
		t.Fatalf("/quiet created General session %q", got)
	}

	p.clearSent()
	e.handleMessage(p, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Content: "/new do not leak", Sessionless: true})
	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "inside the topic") || strings.Contains(sent[0], "do not leak") {
		t.Fatalf("/new reply = %v", sent)
	}
	if got := e.sessions.ActiveSessionID(key); got != "" {
		t.Fatalf("/new created General session %q", got)
	}

	p.clearSent()
	e.SetDisabledCommands([]string{"new"})
	e.handleMessage(p, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Content: "/new private argument", Sessionless: true})
	sent = p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "disabled") || strings.Contains(sent[0], "private argument") {
		t.Fatalf("disabled /new reply = %v", sent)
	}

	p.clearSent()
	e.SetDisabledCommands(nil)
	e.SetAdminFrom("")
	e.handleMessage(p, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Content: "/shell private argument", Sessionless: true})
	sent = p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "admin") || strings.Contains(sent[0], "private argument") {
		t.Fatalf("unauthorized /shell reply = %v", sent)
	}
	if got := e.sessions.ActiveSessionID(key); got != "" {
		t.Fatalf("blocked command created General session %q", got)
	}
}

func TestSessionlessProviderAddDoesNotCollectLaterSecret(t *testing.T) {
	e := NewEngine("project", &stubProviderAgent{}, nil, "", LangEnglish)
	p := &stubPlatformEngine{n: "telegram"}
	key := "telegram:-100123:1:7"
	e.cmdProviderAdd(p, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Sessionless: true}, e.agent.(ProviderSwitcher), []string{"preset"})
	if state := e.interactiveStates[key]; state != nil && state.pendingProviderAdd != nil {
		t.Fatal("General provider command retained secret-collection state")
	}
	if got := e.sessions.ActiveSessionID(key); got != "" {
		t.Fatalf("provider command created General session %q", got)
	}
}

func TestSessionCommandOutsideGeneralKeepsExistingBehavior(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	key := "telegram:-100123:77:7"
	e.handleMessage(&stubPlatformEngine{n: "telegram"}, &Message{SessionKey: key, Platform: "telegram", UserID: "7", Content: "/new topic session"})
	if got := e.sessions.ActiveSessionID(key); got == "" {
		t.Fatal("/new outside General did not create topic session")
	}
}

func TestStartIsRegisteredBuiltin(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	p := &stubPlatformEngine{n: "telegram"}
	e.handleMessage(p, &Message{SessionKey: "telegram:7", Platform: "telegram", UserID: "7", Content: "/start"})
	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "project") {
		t.Fatalf("/start reply = %v", sent)
	}
}
