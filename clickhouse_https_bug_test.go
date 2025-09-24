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

	// Helper function to check for the bug
	checkConnectionError := func(attemptNum int, err error) bool {
		if err == nil {
			t.Logf("Attempt %d: Connection unexpectedly succeeded", attemptNum)
			return false
		}

		// Check for different manifestations of the URL corruption bug
		if strings.Contains(err.Error(), "tcp://clickhouse+https") {
			t.Errorf("Attempt %d: Bug reproduced: URL incorrectly prefixed with tcp://. Error: %v", attemptNum, err)
			return true // Bug found
		} else if strings.Contains(err.Error(), "lookup clickhouse+https") {
			t.Errorf("Attempt %d: Bug reproduced: URL corruption causing DNS lookup of 'clickhouse+https'. Error: %v", attemptNum, err)
			return true // Bug found
		} else if strings.Contains(err.Error(), "clickhouse+https") && !strings.Contains(err.Error(), "127.0.0.1:19999") {
			t.Errorf("Attempt %d: Bug reproduced: URL contains malformed 'clickhouse+https' without proper hostname. Error: %v", attemptNum, err)
			return true // Bug found
		} else {
			// If we don't see any URL corruption bug, the connection attempt might fail for other reasons
			// which is fine - we just want to ensure the URL is processed correctly
			t.Logf("Attempt %d: Connection failed as expected (no URL corruption bug detected): %v", attemptNum, err)
			return false
		}
	}

	// Try to ping the database twice - the bug seems to occur on the retry mechanism
	// First attempt
	err1 := conn.connect(ctx, job)
	bugFound := checkConnectionError(1, err1)
	if bugFound {
		return
	}

	// Second attempt - this is where the bug typically manifests
	err2 := conn.connect(ctx, job)
	bugFound = checkConnectionError(2, err2)
	if bugFound {
		return
	}

	// If neither attempt reproduced the bug, log that both attempts completed
	t.Logf("Both connection attempts completed without reproducing the tcp:// prefix bug")
}
