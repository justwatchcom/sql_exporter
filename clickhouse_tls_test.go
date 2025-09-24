package main

import (
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClickHouseTLSConfigurationIntegration(t *testing.T) {
	// Test the ClickHouse TLS configuration through the actual updateConnections method
	tests := []struct {
		name           string
		driver         string
		connectionURL  string
		shouldSetupTLS bool
		expectedURL    string
	}{
		{
			name:           "clickhouse with tls_config=spiffe should setup TLS",
			driver:         "clickhouse",
			connectionURL:  "clickhouse+https://localhost:9000/default?tls_config=spiffe&other_param=value",
			shouldSetupTLS: true,
			expectedURL:    "clickhouse+https://localhost:9000/default?other_param=value",
		},
		{
			name:           "clickhouse without tls_config should not setup TLS",
			driver:         "clickhouse",
			connectionURL:  "clickhouse://localhost:9000/default?other_param=value",
			shouldSetupTLS: false,
			expectedURL:    "clickhouse://localhost:9000/default?other_param=value",
		},
		{
			name:           "non-clickhouse driver should not setup TLS",
			driver:         "postgres",
			connectionURL:  "postgres://localhost:5432/db?tls_config=spiffe",
			shouldSetupTLS: false,
			expectedURL:    "postgres://localhost:5432/db?tls_config=spiffe",
		},
		{
			name:           "clickhouse with different tls_config should not setup TLS",
			driver:         "clickhouse",
			connectionURL:  "clickhouse://localhost:9000/default?tls_config=custom",
			shouldSetupTLS: false,
			expectedURL:    "clickhouse://localhost:9000/default?tls_config=custom",
		},
		{
			name:           "remove multiple tls_config parameters",
			driver:         "clickhouse",
			connectionURL:  "clickhouse://localhost:9000/default?tls_config=spiffe&param1=value1&tls_config=other&param2=value2",
			shouldSetupTLS: true,
			expectedURL:    "clickhouse://localhost:9000/default?param1=value1&param2=value2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a mock that doesn't fail for this test
			mockSetup := &MockMTLSSetup{ShouldFail: false}
			job := &Job{
				log:         log.NewNopLogger(),
				mtlsSetup:   mockSetup,
				Connections: []string{tt.connectionURL}, // Set the connection URL
			}

			// Test the actual updateConnections method (this is what production code uses)
			job.updateConnections()

			// Verify the results by checking the created connections
			if tt.shouldSetupTLS {
				// Should have exactly one connection
				require.Len(t, job.conns, 1, "Should have exactly one connection")
				conn := job.conns[0]

				assert.NotNil(t, conn.tlsConfig, "TLS config should be set")
				assert.Equal(t, uint16(tls.VersionTLS12), conn.tlsConfig.MinVersion, "TLS minimum version should be TLS 1.2")
				assert.True(t, mockSetup.SetupCalled, "SetupMTLS should have been called")
				assert.NotNil(t, conn.tlsConfig.GetClientCertificate, "GetClientCertificate should be set")
				assert.Equal(t, tt.expectedURL, conn.url, "URL should match expected after processing")
			} else {
				require.Len(t, job.conns, 1, "Should have exactly one connection")
				conn := job.conns[0]
				assert.Nil(t, conn.tlsConfig, "TLS config should not be set for non-ClickHouse drivers")
				assert.False(t, mockSetup.SetupCalled, "SetupMTLS should not have been called for non-ClickHouse drivers")
				assert.Equal(t, tt.expectedURL, conn.url, "URL should remain unchanged for non-ClickHouse drivers")
			}
		})
	}
}

type MockMTLSSetup struct {
	ShouldFail  bool
	SetupCalled bool
}

func (m *MockMTLSSetup) SetupMTLS(job *Job, tlsConfig *tls.Config) error {
	m.SetupCalled = true
	if m.ShouldFail {
		return errors.New("mock setup failure")
	}
	// Mock the GetClientCertificate function
	tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &tls.Certificate{}, nil
	}
	return nil
}

func TestClickHouseTLSConfigurationWithMocking(t *testing.T) {
	tests := []struct {
		name            string
		connectionURL   string
		setupShouldFail bool
		shouldSetupTLS  bool
		expectedURL     string
		expectError     bool
	}{
		{
			name:            "successful TLS setup",
			connectionURL:   "clickhouse://localhost:9000/default?tls_config=spiffe&other=value",
			setupShouldFail: false,
			shouldSetupTLS:  true,
			expectedURL:     "clickhouse://localhost:9000/default?other=value",
			expectError:     false,
		},
		{
			name:            "mTLS setup failure",
			connectionURL:   "clickhouse://localhost:9000/default?tls_config=spiffe",
			setupShouldFail: true,
			shouldSetupTLS:  false,
			expectError:     true,
		},
		{
			name:            "no TLS setup needed",
			connectionURL:   "clickhouse://localhost:9000/default?other=value",
			setupShouldFail: false,
			shouldSetupTLS:  false,
			expectedURL:     "clickhouse://localhost:9000/default?other=value",
			expectError:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSetup := &MockMTLSSetup{ShouldFail: tt.setupShouldFail}
			job := &Job{
				log:       log.NewNopLogger(),
				mtlsSetup: mockSetup,
			}

			result := job.configureClickHouseTLS(tt.connectionURL, tt.connectionURL)

			if tt.expectError {
				assert.Error(t, result.Error)
				return
			}

			assert.NoError(t, result.Error)
			assert.Equal(t, tt.expectedURL, result.ModifiedURL)

			if tt.shouldSetupTLS {
				assert.NotNil(t, result.TLSConfig)
				assert.Equal(t, uint16(tls.VersionTLS12), result.TLSConfig.MinVersion)
				assert.True(t, mockSetup.SetupCalled)
				assert.NotNil(t, result.TLSConfig.GetClientCertificate)
			} else {
				assert.Nil(t, result.TLSConfig)
				if strings.Contains(tt.connectionURL, "tls_config=spiffe") {
					// Should have been called but failed
					assert.True(t, mockSetup.SetupCalled)
				} else {
					// Should not have been called at all
					assert.False(t, mockSetup.SetupCalled)
				}
			}
		})
	}
}
