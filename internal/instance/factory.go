package instance

import (
	"context"
	"fmt"

	"github.com/mapleafgo/acp-bridge/internal/client"
	"github.com/mapleafgo/acp-bridge/internal/config"
	"github.com/mapleafgo/acp-bridge/internal/driver"
)

type ClientFactory func(context.Context, driver.AgentType) (ACPClient, error)

// DefaultFactory 构造生产环境 ACP Client。
func DefaultFactory(cfg *config.Config) ClientFactory {
	return func(ctx context.Context, agentType driver.AgentType) (ACPClient, error) {
		drv, err := driver.NewDriver(agentType, cfg)
		if err != nil {
			return nil, fmt.Errorf("create driver: %w", err)
		}
		return client.New(ctx, drv)
	}
}
