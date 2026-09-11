package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type titleGeneratingStubAgent struct {
	stubAgent
}

func (a *titleGeneratingStubAgent) GenerateConversationTitle(_ context.Context, content string) (string, error) {
	return "title: " + content, nil
}

type titleSetterStubPlatform struct {
	stubPlatformEngine
	generator      func(context.Context, string) (string, error)
	routePreflight func(*Message) MessageRouteDisposition
}

func (p *titleSetterStubPlatform) SetConversationTitleGenerator(generator func(context.Context, string) (string, error)) {
	p.generator = generator
}

func (p *titleSetterStubPlatform) SetMessageRoutePreflight(preflight func(*Message) MessageRouteDisposition) {
	p.routePreflight = preflight
}

func TestEngineInjectsConversationTitleGeneratorBeforePlatformStart(t *testing.T) {
	agent := &titleGeneratingStubAgent{}
	platform := &titleSetterStubPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	engine := NewEngine("project", agent, []Platform{platform}, "", LangEnglish)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Stop() })

	if platform.generator == nil {
		t.Fatal("conversation title generator was not injected")
	}
	if platform.routePreflight == nil || platform.routePreflight(&Message{Content: "/quiet"}) != MessageRouteInPlace {
		t.Fatal("message route preflight was not injected")
	}
	title, err := platform.generator(context.Background(), "request")
	if err != nil || title != "title: request" {
		t.Fatalf("title/error = %q/%v", title, err)
	}
}

func TestEngineUsesWorkspaceBindingSnapshotFromRoutePreflight(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "a")
	workspaceB := filepath.Join(base, "b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	engine := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	engine.SetMultiWorkspace(base, filepath.Join(base, "bindings.json"))
	source := workspaceChannelKey("telegram", "-100123:1")
	engine.workspaceBindings.Bind("project:project", source, "General", workspaceA)

	msg := &Message{Platform: "telegram", SessionKey: "telegram:-100123:1:7", ChannelKey: "-100123:1", Content: "/help"}
	engine.messageRouteDisposition(msg)
	engine.workspaceBindings.Bind("project:project", source, "General", workspaceB)
	msg.SessionKey = "telegram:-100123:77:7"
	msg.ChannelKey = "-100123:77"
	msg.WorkspaceSourceChannelKey = "-100123:1"
	engine.copyWorkspaceSourceBinding(msg)

	binding := engine.workspaceBindings.Lookup("project:project", workspaceChannelKey("telegram", "-100123:77"))
	if binding == nil || binding.Workspace != workspaceA {
		t.Fatalf("final workspace binding = %#v, want intake workspace %q", binding, workspaceA)
	}
}

func TestEngineDoesNotRestoreStaleWorkspaceSnapshotForInPlaceRoute(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "a")
	workspaceB := filepath.Join(base, "b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	engine := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	engine.SetMultiWorkspace(base, filepath.Join(base, "bindings.json"))
	channel := workspaceChannelKey("telegram", "-100123:1")
	engine.workspaceBindings.Bind("project:project", channel, "General", workspaceA)

	msg := &Message{Platform: "telegram", SessionKey: "telegram:-100123:1:7", ChannelKey: "-100123:1", Content: "/help"}
	engine.messageRouteDisposition(msg)
	engine.workspaceBindings.Bind("project:project", channel, "General", workspaceB)
	engine.copyWorkspaceSourceBinding(msg)

	binding := engine.workspaceBindings.Lookup("project:project", channel)
	if binding == nil || binding.Workspace != workspaceB {
		t.Fatalf("in-place workspace binding = %#v, want newer binding %q", binding, workspaceB)
	}
}

func TestEngineCopiesWorkspaceSourceBindingBeforeFinalRouteResolution(t *testing.T) {
	dir := t.TempDir()
	engine := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })
	engine.SetMultiWorkspace(dir, filepath.Join(dir, "bindings.json"))
	source := workspaceChannelKey("test", "source")
	engine.workspaceBindings.Bind("project:project", source, "General", dir)

	msg := &Message{
		Platform:                  "test",
		SessionKey:                "test:final",
		ChannelKey:                "final",
		WorkspaceSourceChannelKey: "source",
		Content:                   "/help",
	}
	engine.ReceiveMessage(&stubPlatformEngine{n: "test"}, msg)

	final := workspaceChannelKey("test", "final")
	binding := engine.workspaceBindings.Lookup("project:project", final)
	if binding == nil || binding.Workspace != dir {
		t.Fatalf("final workspace binding = %#v", binding)
	}
}
