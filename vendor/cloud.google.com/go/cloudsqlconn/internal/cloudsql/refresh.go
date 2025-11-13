// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsql

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
	"cloud.google.com/go/cloudsqlconn/internal/trace"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

const (
	// PublicIP is the value for public IP Cloud SQL instances.
	PublicIP = "PUBLIC"
	// PrivateIP is the value for private IP Cloud SQL instances.
	PrivateIP = "PRIVATE"
	// PSC is the value for private service connect Cloud SQL instances.
	PSC = "PSC"
	// AutoIP selects public IP if available and otherwise selects private
	// IP.
	AutoIP = "AutoIP"

	// ClientProtocolTCP The value for the TCP client protocol type for MDX.
	ClientProtocolTCP = "tcp"
	// ClientProtocolUDS The value for the UDS client protocol type for MDX.
	ClientProtocolUDS = "uds"
	// ClientProtocolTLS The value for the TLS client protocol type for MDX.
	ClientProtocolTLS = "tls"
)

// metadata contains information about a Cloud SQL instance needed to create
// connections.
type metadata struct {
	ipAddrs            map[string]string
	serverCACert       []*x509.Certificate
	serverCAMode       string
	dnsName            string
	version            string
	mdxProtocolSupport []string
}

// fetchMetadata uses the Cloud SQL Admin APIs get method to retrieve the
// information about a Cloud SQL instance that is used to create secure
// connections.
func fetchMetadata(
	ctx context.Context, client *sqladmin.Service, inst instance.ConnName,
) (m metadata, err error) {

	var end trace.EndSpanFunc
	ctx, end = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.FetchMetadata")
	defer func() { end(err) }()

	db, err := retry50x(ctx, func(ctx2 context.Context) (*sqladmin.ConnectSettings, error) {
		return client.Connect.Get(
			inst.Project(), inst.Name(),
		).Context(ctx2).Do()
	}, exponentialBackoff)
	if err != nil {
		return metadata{}, errtype.NewRefreshError("failed to get instance metadata", inst.String(), err)
	}
	// validate the instance is supported for authenticated connections
	if db.Region != inst.Region() {
		msg := fmt.Sprintf(
			"provided region was mismatched - got %s, want %s",
			inst.Region(), db.Region,
		)
		return metadata{}, errtype.NewConfigError(msg, inst.String())
	}
	if db.BackendType != "SECOND_GEN" {
		return metadata{}, errtype.NewConfigError(
			"unsupported instance - only Second Generation instances are supported",
			inst.String(),
		)
	}

	// parse any ip addresses that might be used to connect
	ipAddrs := make(map[string]string)
	for _, ip := range db.IpAddresses {
		switch ip.Type {
		case "PRIMARY":
			ipAddrs[PublicIP] = ip.IpAddress
		case "PRIVATE":
			ipAddrs[PrivateIP] = ip.IpAddress
		}
	}

	// resolve DnsName into IP address for PSC
	// Note that we have to check for PSC enablement first because CAS instances also set the DnsName.
	if db.PscEnabled {
		// Search the dns_names field for the PSC DNS Name.
		pscDNSName := ""
		for _, dnm := range db.DnsNames {
			if dnm.Name != "" &&
				dnm.ConnectionType == "PRIVATE_SERVICE_CONNECT" && dnm.DnsScope == "INSTANCE" {
				pscDNSName = dnm.Name
				break
			}
		}

		// If the psc dns name was not found, use the legacy dns_name field
		if pscDNSName == "" && db.DnsName != "" {
			pscDNSName = db.DnsName
		}

		// If the psc dns name was found, add it to the ipaddrs map.
		if pscDNSName != "" {
			ipAddrs[PSC] = pscDNSName
		}
	}

	if len(ipAddrs) == 0 {
		return metadata{}, errtype.NewConfigError(
			"cannot connect to instance - it has no supported IP addresses",
			inst.String(),
		)
	}

	// parse the server-side CA certificate
	caCerts := []*x509.Certificate{}
	for b, rest := pem.Decode([]byte(db.ServerCaCert.Cert)); b != nil; b, rest = pem.Decode(rest) {
		if b == nil {
			return metadata{}, errtype.NewRefreshError("failed to decode valid PEM cert", inst.String(), nil)
		}
		caCert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return metadata{}, errtype.NewRefreshError(
				fmt.Sprintf("failed to parse as X.509 certificate: %v", err),
				inst.String(),
				nil,
			)
		}
		caCerts = append(caCerts, caCert)
	}

	// Find a DNS name to use to validate the certificate from the dns_names field. Any
	// name in the list may be used to validate the server TLS certificate.
	// Fall back to legacy dns_name field if necessary.
	var serverName string
	if len(db.DnsNames) > 0 {
		serverName = db.DnsNames[0].Name
	}
	if serverName == "" {
		serverName = db.DnsName
	}

	m = metadata{
		ipAddrs:            ipAddrs,
		serverCACert:       caCerts,
		version:            db.DatabaseVersion,
		dnsName:            serverName,
		serverCAMode:       db.ServerCaMode,
		mdxProtocolSupport: db.MdxProtocolSupport,
	}

	return m, nil
}

// fetchEphemeralCert uses the Cloud SQL Admin API's createEphemeral method to
// create a signed ClientProtocolTLS certificate that authorized to connect via the Cloud SQL
// instance's serverside proxy. The cert if valid for approximately one hour.
func fetchEphemeralCert(
	ctx context.Context,
	client *sqladmin.Service,
	inst instance.ConnName,
	key *rsa.PrivateKey,
	tp auth.TokenProvider,
) (c tls.Certificate, err error) {
	var end trace.EndSpanFunc
	ctx, end = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.FetchEphemeralCert")
	defer func() { end(err) }()
	clientPubKey, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	req := sqladmin.GenerateEphemeralCertRequest{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Bytes: clientPubKey, Type: "RSA PUBLIC KEY"})),
	}
	var tok *auth.Token
	if tp != nil {
		var tokErr error
		tok, tokErr = tp.Token(ctx)
		if tokErr != nil {
			return tls.Certificate{}, errtype.NewRefreshError(
				"failed to retrieve Oauth2 token",
				inst.String(),
				tokErr,
			)
		}
		req.AccessToken = tok.Value
	}
	resp, err := retry50x(ctx, func(ctx2 context.Context) (*sqladmin.GenerateEphemeralCertResponse, error) {
		return client.Connect.GenerateEphemeralCert(
			inst.Project(), inst.Name(), &req,
		).Context(ctx2).Do()
	}, exponentialBackoff)
	if err != nil {
		return tls.Certificate{}, errtype.NewRefreshError(
			"create ephemeral cert failed",
			inst.String(),
			err,
		)
	}

	// parse the client cert
	b, _ := pem.Decode([]byte(resp.EphemeralCert.Cert))
	if b == nil {
		return tls.Certificate{}, errtype.NewRefreshError(
			"failed to decode valid PEM cert",
			inst.String(),
			nil,
		)
	}
	clientCert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return tls.Certificate{}, errtype.NewRefreshError(
			fmt.Sprintf("failed to parse as X.509 certificate: %v", err),
			inst.String(),
			nil,
		)
	}
	if tp != nil {
		// Adjust the certificate's expiration to be the earliest of
		// the token's expiration or the certificate's expiration.
		if tok.Expiry.Before(clientCert.NotAfter) {
			clientCert.NotAfter = tok.Expiry
		}
	}

	c = tls.Certificate{
		Certificate: [][]byte{clientCert.Raw},
		PrivateKey:  key,
		Leaf:        clientCert,
	}
	return c, nil
}

// newAdminAPIClient creates a Refresher.
func newAdminAPIClient(
	l debug.ContextLogger,
	svc *sqladmin.Service,
	key *rsa.PrivateKey,
	tp auth.TokenProvider,
	dialerID string,
) adminAPIClient {
	return adminAPIClient{
		dialerID: dialerID,
		logger:   l,
		key:      key,
		client:   svc,
		tp:       tp,
	}
}

// adminAPIClient manages the SQL Admin API access to instance metadata and to
// ephemeral certificates.
type adminAPIClient struct {
	// dialerID is the unique ID of the associated dialer.
	dialerID string
	logger   debug.ContextLogger
	// key is used to generate the client certificate
	key    *rsa.PrivateKey
	client *sqladmin.Service
	// tp is the TokenProvider used for IAM DB AuthN.
	tp auth.TokenProvider
}

// ConnectionInfo immediately performs a full refresh operation using the Cloud
// SQL Admin API.
func (c adminAPIClient) ConnectionInfo(
	ctx context.Context, cn instance.ConnName, iamAuthNDial bool,
) (ci ConnectionInfo, err error) {

	var refreshEnd trace.EndSpanFunc
	ctx, refreshEnd = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.RefreshConnection",
		trace.AddInstanceName(cn.String()),
	)
	defer func() {
		go trace.RecordRefreshResult(context.Background(), cn.String(), c.dialerID, err)
		refreshEnd(err)
	}()

	// start async fetching the instance's metadata
	type mdRes struct {
		md  metadata
		err error
	}
	mdC := make(chan mdRes, 1)
	go func() {
		defer close(mdC)
		md, err := fetchMetadata(ctx, c.client, cn)
		mdC <- mdRes{md, err}
	}()

	// start async fetching the certs
	type ecRes struct {
		ec  tls.Certificate
		err error
	}
	ecC := make(chan ecRes, 1)
	go func() {
		defer close(ecC)
		var iamTP auth.TokenProvider
		if iamAuthNDial {
			iamTP = c.tp
		}
		ec, err := fetchEphemeralCert(ctx, c.client, cn, c.key, iamTP)
		ecC <- ecRes{ec, err}
	}()

	// wait for the results of each operation
	var md metadata
	select {
	case r := <-mdC:
		if r.err != nil {
			return ConnectionInfo{}, fmt.Errorf("failed to get instance: %w", r.err)
		}
		md = r.md
	case <-ctx.Done():
		return ci, fmt.Errorf("refresh failed: %w", ctx.Err())
	}
	if iamAuthNDial {
		if vErr := supportsAutoIAMAuthN(md.version); vErr != nil {
			return ConnectionInfo{}, vErr
		}
	}

	var ec tls.Certificate
	select {
	case r := <-ecC:
		if r.err != nil {
			return ConnectionInfo{}, fmt.Errorf("fetch ephemeral cert failed: %w", r.err)
		}
		ec = r.ec
	case <-ctx.Done():
		return ConnectionInfo{}, fmt.Errorf("refresh failed: %w", ctx.Err())
	}

	return NewConnectionInfo(
		cn, md.dnsName, md.serverCAMode, md.version, md.ipAddrs, md.serverCACert, ec, md.mdxProtocolSupport,
	), nil
}

// supportsAutoIAMAuthN checks that the engine support automatic IAM authn. If
// auto IAM authn was not request, this is a no-op.
func supportsAutoIAMAuthN(version string) error {
	switch {
	case strings.HasPrefix(version, "POSTGRES"):
		return nil
	case strings.HasPrefix(version, "MYSQL"):
		return nil
	default:
		return fmt.Errorf("%s does not support Auto IAM DB Authentication", version)
	}
}
