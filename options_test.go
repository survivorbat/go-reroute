package reroute

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithLogger_SetsLogger(t *testing.T) {
	t.Parallel()
	// Arrange
	reRouter := new(ReRouter)

	logger := slog.Default()

	// Act
	err := WithLogger(logger)(reRouter)

	// Assert
	require.NoError(t, err)

	assert.Same(t, logger, reRouter.Logger)
}
