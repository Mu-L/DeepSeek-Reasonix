package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/provider"
)

// TestProviderModel sends a bounded, tool-free probe through the configured
// adapter. It does not create a session or change persisted settings.
func (a *App) TestProviderModel(p ProviderView, model, key string) error {
	root := a.activeWorkspaceRoot()
	cfg, err := config.LoadForRootWithoutCredentialsReadOnly(root)
	if err != nil {
		return err
	}
	if err := saveProviderConfig(cfg, p); err != nil {
		return err
	}
	entry, ok := cfg.ResolveModel(p.Name + "/" + strings.TrimSpace(model))
	if !ok {
		return fmt.Errorf("model is not in this provider's configured list")
	}
	entry.ResolveAPIKeyForRoot(root)
	if strings.TrimSpace(key) != "" {
		copied := entry.WithAPIKeyForProbe(key)
		entry = &copied
	}
	client, err := boot.NewProviderWithProxy(entry, withProbeDirectHost(a.networkProxySpecForRoot(root), entry.BaseURL, p.NoProxy))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.reqCtx(), 20*time.Second)
	defer cancel()
	chunks, err := client.Stream(ctx, provider.Request{Messages: []provider.Message{{Role: "user", Content: "Reply with OK."}}, MaxTokens: 16})
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, open := <-chunks:
			if !open {
				return fmt.Errorf("provider closed the connection without a response")
			}
			if chunk.Err != nil {
				return chunk.Err
			}
			if chunk.Type == provider.ChunkText && strings.TrimSpace(chunk.Text) != "" {
				return nil
			}
			if chunk.Type == provider.ChunkDone {
				return nil
			}
		}
	}
}
