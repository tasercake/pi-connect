package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func runProviderCommand(args []string) {
	if len(args) == 0 {
		printProviderUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		runProviderAdd(args[1:])
	case "list":
		runProviderList(args[1:])
	case "remove":
		runProviderRemove(args[1:])
	case "presets":
		runProviderPresets(args[1:])
	case "global":
		runProviderGlobal(args[1:])
	case "help", "--help", "-h":
		printProviderUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown provider subcommand: %s\n\n", args[0])
		printProviderUsage()
		os.Exit(1)
	}
}

func printProviderUsage() {
	fmt.Println(`Usage: cc-connect provider <command> [options]

Commands:
  add      Add a new API provider to a project
  list     List providers for a project
  remove   Remove a provider from a project
  presets  Browse recommended provider presets
  global   Manage global shared providers

Examples:
  cc-connect provider add --project my-backend --name relay --api-key sk-xxx
  cc-connect provider add --project my-backend --name bedrock --env CLAUDE_CODE_USE_BEDROCK=1,AWS_PROFILE=bedrock
  cc-connect provider list --project my-backend
  cc-connect provider remove --project my-backend --name relay
  cc-connect provider presets
  cc-connect provider global list
  cc-connect provider global add --name minimaxi --api-key sk-xxx --base-url https://api.minimaxi.chat/v1`)
}

// initConfigPath resolves the config path and sets config.ConfigPath.
func initConfigPath(flagValue string) {
	config.ConfigPath = resolveConfigPath(flagValue)
}

func runProviderAdd(args []string) {
	fs := flag.NewFlagSet("provider add", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project name (required)")
	name := fs.String("name", "", "provider name (required)")
	apiKey := fs.String("api-key", "", "API key")
	baseURL := fs.String("base-url", "", "API base URL (optional)")
	model := fs.String("model", "", "model name override (optional)")
	envStr := fs.String("env", "", "extra env vars as KEY=VAL,KEY2=VAL2 (optional)")
	_ = fs.Parse(args)

	if *project == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --project and --name are required")
		fs.Usage()
		os.Exit(1)
	}

	initConfigPath(*configFile)

	p := config.ProviderConfig{
		Name:    *name,
		APIKey:  *apiKey,
		BaseURL: *baseURL,
		Model:   *model,
	}
	if *envStr != "" {
		p.Env = parseEnvStr(*envStr)
	}

	if err := config.AddProviderToConfig(*project, p); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Provider %q added to project %q\n", *name, *project)
	if *baseURL != "" {
		fmt.Printf("   Base URL: %s\n", *baseURL)
	}
	if *model != "" {
		fmt.Printf("   Model: %s\n", *model)
	}
	if len(p.Env) > 0 {
		fmt.Printf("   Extra env: %v\n", p.Env)
	}
	fmt.Printf("\nTo activate: use /provider switch %s in chat.\n", *name)
}

func runProviderList(args []string) {
	fs := flag.NewFlagSet("provider list", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project name (lists all projects if empty)")
	_ = fs.Parse(args)

	initConfigPath(*configFile)

	if *project != "" {
		listProjectProviders(*project)
		return
	}

	projects, err := config.ListProjects()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	for _, p := range projects {
		fmt.Printf("── %s ──\n", p)
		listProjectProviders(p)
		fmt.Println()
	}
}

func listProjectProviders(projectName string) {
	providers, active, err := config.GetProjectProviders(projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	if len(providers) == 0 {
		fmt.Println("  (no providers)")
		return
	}

	for _, p := range providers {
		marker := "  "
		if p.Name == active {
			marker = "▶ "
		}
		info := p.Name
		if p.BaseURL != "" {
			info += fmt.Sprintf(" (base_url: %s)", p.BaseURL)
		}
		if p.Model != "" {
			info += fmt.Sprintf(" [model: %s]", p.Model)
		}
		apiKeyHint := "(not set)"
		if p.APIKey != "" {
			if len(p.APIKey) > 8 {
				apiKeyHint = p.APIKey[:4] + "..." + p.APIKey[len(p.APIKey)-4:]
			} else {
				apiKeyHint = "****"
			}
		}
		fmt.Printf("%s%s  api_key: %s\n", marker, info, apiKeyHint)
	}
}

func runProviderRemove(args []string) {
	fs := flag.NewFlagSet("provider remove", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project name (required)")
	name := fs.String("name", "", "provider name (required)")
	_ = fs.Parse(args)

	if *project == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --project and --name are required")
		fs.Usage()
		os.Exit(1)
	}

	initConfigPath(*configFile)

	if err := config.RemoveProviderFromConfig(*project, *name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Provider %q removed from project %q\n", *name, *project)
}

func parseEnvStr(s string) map[string]string {
	env := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if idx := strings.IndexByte(pair, '='); idx > 0 {
			env[pair[:idx]] = pair[idx+1:]
		}
	}
	return env
}

// ── Presets ────────────────────────────────────────────────────

func runProviderPresets(args []string) {
	fs := flag.NewFlagSet("provider presets", flag.ExitOnError)
	url := fs.String("url", "", "override presets URL")
	_ = fs.Parse(args)

	if *url != "" {
		core.SetPresetsURL(*url)
	}

	data, err := core.FetchProviderPresets()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(data.Providers) == 0 {
		fmt.Println("No presets available.")
		return
	}

	fmt.Printf("Provider Presets (v%d)\n\n", data.Version)
	for i, p := range data.Providers {
		sponsor := ""
		if p.Tier <= 1 {
			sponsor = " ⭐ SPONSOR"
		}
		fmt.Printf("%d. %s%s\n", i+1, p.DisplayName, sponsor)
		if p.Description != "" {
			fmt.Printf("   %s\n", p.Description)
		}
		agentTypes := make([]string, 0, len(p.Agents))
		for at := range p.Agents {
			agentTypes = append(agentTypes, at)
		}
		if len(agentTypes) > 0 {
			fmt.Printf("   Agents: %s\n", strings.Join(agentTypes, ", "))
		}
		for at, ac := range p.Agents {
			fmt.Printf("   [%s] %s · %s\n", at, ac.BaseURL, ac.Model)
		}
		if p.InviteURL != "" {
			fmt.Printf("   Register: %s\n", p.InviteURL)
		}
		fmt.Println()
	}
}

// ── Global provider management ─────────────────────────────────

func runProviderGlobal(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `Usage: cc-connect provider global <command>

Commands:
  list     List global providers
  add      Add a global provider
  remove   Remove a global provider`)
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		runGlobalProviderList(args[1:])
	case "add":
		runGlobalProviderAdd(args[1:])
	case "remove":
		runGlobalProviderRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown global subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func runGlobalProviderList(args []string) {
	fs := flag.NewFlagSet("provider global list", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	_ = fs.Parse(args)

	initConfigPath(*configFile)

	providers, err := config.ListGlobalProviders()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if len(providers) == 0 {
		fmt.Println("No global providers configured.")
		fmt.Println("\nAdd one with: cc-connect provider global add --name <name> --api-key <key>")
		return
	}

	fmt.Printf("Global Providers (%d)\n\n", len(providers))
	for _, p := range providers {
		info := p.Name
		if p.BaseURL != "" {
			info += fmt.Sprintf(" (%s)", p.BaseURL)
		}
		if p.Model != "" {
			info += fmt.Sprintf(" [%s]", p.Model)
		}
		apiKeyHint := "(not set)"
		if p.APIKey != "" {
			if len(p.APIKey) > 8 {
				apiKeyHint = p.APIKey[:4] + "..." + p.APIKey[len(p.APIKey)-4:]
			} else {
				apiKeyHint = "****"
			}
		}
		fmt.Printf("  %s  api_key: %s\n", info, apiKeyHint)
	}
}

func runGlobalProviderAdd(args []string) {
	fs := flag.NewFlagSet("provider global add", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	name := fs.String("name", "", "provider name (required)")
	apiKey := fs.String("api-key", "", "API key")
	baseURL := fs.String("base-url", "", "API base URL")
	model := fs.String("model", "", "default model")
	thinking := fs.String("thinking", "", "thinking override: enabled/disabled")
	envStr := fs.String("env", "", "extra env vars as KEY=VAL,KEY2=VAL2")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required")
		fs.Usage()
		os.Exit(1)
	}

	initConfigPath(*configFile)

	p := config.ProviderConfig{
		Name:     *name,
		APIKey:   *apiKey,
		BaseURL:  *baseURL,
		Model:    *model,
		Thinking: *thinking,
	}
	if *envStr != "" {
		p.Env = parseEnvStr(*envStr)
	}

	if err := config.AddGlobalProvider(p); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Global provider %q added\n", *name)
	fmt.Println("\nTo use in a project, add to config.toml:")
	fmt.Printf("  [projects.agent]\n  provider_refs = [\"%s\"]\n", *name)
}

func runGlobalProviderRemove(args []string) {
	fs := flag.NewFlagSet("provider global remove", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")
	name := fs.String("name", "", "provider name (required)")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required")
		fs.Usage()
		os.Exit(1)
	}

	initConfigPath(*configFile)

	if err := config.RemoveGlobalProvider(*name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Global provider %q removed\n", *name)
}
