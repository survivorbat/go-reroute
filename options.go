package reroute

import (
	"log/slog"
)

// Option allows options to be supplied in the NewReRouter
type Option func(*ReRouter) error

func WithLogger(logger *slog.Logger) Option {
	return func(rr *ReRouter) error {
		rr.Logger = logger
		return nil
	}
}
