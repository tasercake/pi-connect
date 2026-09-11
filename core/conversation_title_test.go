package core

import (
	"context"
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
	generator func(context.Context, string) (string, error)
}

func (p *titleSetterStubPlatform) SetConversationTitleGenerator(generator func(context.Context, string) (string, error)) {
	p.generator = generator
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
	title, err := platform.generator(context.Background(), "request")
	if err != nil || title != "title: request" {
		t.Fatalf("title/error = %q/%v", title, err)
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
