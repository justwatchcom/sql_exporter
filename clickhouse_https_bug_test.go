package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"
)

// TestClickHouseHTTPSURLBug tests the bug where clickhouse+https URLs get incorrectly prefixed with tcp://
func TestClickHouseHTTPSURLBug(t *testing.T) {
	// Create a job with a clickhouse+https connection
	job := &Job{
		log:         log.NewNopLogger(),
		mtlsSetup:   &MockMTLSSetup{ShouldFail: false},
		Connections: []string{"clickhouse+https://127.0.0.1:19999/default?secure=true&tls_config=spiffe&username=clickhouse"},
		Timeout:     1 * time.Second,
	}

	// Update connections to create the connection object
	job.updateConnections()

	// Verify we have exactly one connection
	require.Len(t, job.conns, 1, "Should have exactly one connection")
	conn := job.conns[0]

	// Test the connection with a fast timeout to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Try to ping the database - this should fail but we want to check the error message
	err := conn.connect(ctx, job)
	if err != nil {
		// Check if the error contains the incorrect "tcp://" prefix
		// This is the bug we're trying to reproduce
		if strings.Contains(err.Error(), "tcp://clickhouse+https") {
			t.Errorf("Bug reproduced: URL incorrectly prefixed with tcp://. Error: %v", err)
			// The error should contain the hostname, not the malformed URL
			if !strings.Contains(err.Error(), "127.0.0.1:19999") {
				t.Errorf("Error message should contain the correct hostname '127.0.0.1:19999', but got: %v", err)
			}
		} else {
			// If we don't see the tcp:// prefix bug, the connection attempt might fail for other reasons
			// which is fine - we just want to ensure the URL is processed correctly
			t.Logf("Connection failed as expected (no tcp:// prefix bug detected): %v", err)
		}
		return
	}
}
