package agent

import (
	"context"
	"strings"

	"github.com/charliek/craze/internal/codexrpc"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/routing"
)

// Read only the routing fields from the server's effective configuration for
// this workspace. Never log the response or include a configured endpoint in
// a failure (it may contain credentials).
func codexReviewBilling(ctx context.Context, client *codexrpc.Client, cwd string, account *codexrpc.AccountReadResult) (string, error) {
	if account != nil && !account.RequiresOpenaiAuth {
		return routing.Metered, newCodexErr(errkind.Auth, "paid_blocked: Codex uses a custom model provider")
	}
	if account == nil || account.Account == nil {
		return "", newCodexErr(errkind.Auth, "not_signed_in: Codex needs a ChatGPT subscription")
	}
	if account.Account.Type != "chatgpt" {
		return routing.APIKey, newCodexErr(errkind.Auth, "paid_blocked: Codex credentials are not a ChatGPT subscription")
	}
	var result struct {
		Config *struct {
			Provider    string `json:"model_provider"`
			Base        string `json:"openai_base_url"`
			ChatGPTBase string `json:"chatgpt_base_url"`
		} `json:"config"`
	}
	if err := client.Call(ctx, "config/read", map[string]any{"cwd": cwd, "includeLayers": false}, &result); err != nil || result.Config == nil {
		return "", newCodexErr(errkind.Config, "could not verify Codex billing configuration")
	}
	c := result.Config
	base := strings.TrimSuffix(c.ChatGPTBase, "/")
	if (c.Provider != "" && c.Provider != "openai") || (c.Base != "" && strings.TrimSuffix(c.Base, "/") != "https://chatgpt.com/backend-api/codex") || (base != "" && base != "https://chatgpt.com/backend-api" && base != "https://gov.chatgpt.com/backend-api") {
		return routing.Metered, newCodexErr(errkind.Auth, "paid_blocked: Codex has a custom model provider or endpoint")
	}
	return routing.Subscription, nil
}
