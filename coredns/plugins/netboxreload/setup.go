package netboxreload

import (
	"context"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

func init() {
	plugin.Register(pluginName, setup)
}

func setup(c *caddy.Controller) error {
	p, err := parseConfig(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		p.Next = next
		return p
	})

	c.OnStartup(func() error {
		if err := p.reloadZones(); err != nil {
			return plugin.Error(pluginName, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.OnShutdown(func() error {
			cancel()
			p.stopGRPC()
			return nil
		})
		go p.startGRPC()
		go p.pollLoop(ctx)
		return nil
	})

	return nil
}

func parseConfig(c *caddy.Controller) (*Plugin, error) {
	p := &Plugin{
		Dir:          "/zones",
		Port:         ":8054",
		PollInterval: 60 * time.Second,
	}

	for c.Next() {
		for c.NextBlock() {
			switch c.Val() {
			case "directory":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				p.Dir = c.Val()
			case "grpc_port":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				p.Port = c.Val()
			case "reload":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid reload duration %q: %v", c.Val(), err)
				}
				p.PollInterval = d
			default:
				return nil, c.Errf("unknown option %q", c.Val())
			}
		}
	}
	return p, nil
}
