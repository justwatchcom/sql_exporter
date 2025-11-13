// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsql

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
)

// verifyPeerCertificateFunc creates a VerifyPeerCertificate function with the
// custom ClientProtocolTLS verification logic to gracefully and securely handle deviations
// from standard ClientProtocolTLS hostname verification in existing Cloud SQL instance
// server certificates.
//
// This is the verification algorithm:
//
//  1. Verify the server cert CA, using the CA certs from the instance metadata.
//     Reject the certificate if the CA is invalid.
//
//  2. Check that the server cert contains a SubjectAlternativeName matching the
//     DNS name in the connector configuration OR the DNS Name from the instance
//     metadata
//
//  3. If the SubjectAlternativeName does not match, and if the server cert
//     Subject.CN field is not empty, check that the Subject.CN field contains
//     the instance name.
//
//     Reject the certificate if both the #2 SAN check and #3 CN checks fail.
//
// To summarize the deviations from standard ClientProtocolTLS hostname verification:
//
// Historically, Cloud SQL creates server certificates with the instance name in
// the Subject.CN field in the format "my-project:my-instance". The connector is
// expected to check that the instance name that the connector was configured to
// dial matches the server certificate Subject.CN field. Thus, the Subject.CN
// field for most Cloud SQL instances does not contain a well-formed DNS Name.
//
// The default Go ClientProtocolTLS hostname verification TLSConfig.serverName may be compared
// with the Subject.CN field if Subject.CN contains a well-formed DNS name.
// So the Cloud SQL server certs break the standard hostname verification in Go.
// See:
// - https://github.com/GoogleCloudPlatform/cloudsql-proxy/issues/194
// - https://tip.golang.org/doc/go1.11#crypto/x509
//
// Also, there are times when the instance metadata reports that an instance has
// a DNS name, but that DNS name does not yet appear in the SAN records of the
// server certificate. The client should fall back to validating the hostname
// using the instance name in the Subject.CN field.
func verifyPeerCertificateFunc(
	serverName string, cn instance.ConnName, roots *x509.CertPool,
) func(certs [][]byte, chain [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errtype.NewDialError(
				"no certificate to verify", cn.String(), nil,
			)
		}
		// Parse the raw certificates
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		var err error
		for _, certBytes := range rawCerts {
			cert, err := x509.ParseCertificate(certBytes)
			if err != nil {
				return errtype.NewDialError(
					"failed to parse X.509 certificate", cn.String(), err,
				)
			}
			certs = append(certs, cert)
		}
		serverCert := certs[0]

		// Verify the validity of the certificate chain
		_, err = serverCert.Verify(x509.VerifyOptions{
			Roots: roots,
		})
		if err != nil {
			err = &tls.CertificateVerificationError{
				UnverifiedCertificates: certs,
				Err:                    err,
			}
			return errtype.NewDialError(
				"failed to verify certificate", cn.String(), err,
			)
		}

		var serverNameErr error

		if serverName == "" {
			// The instance has no DNS name.
			// Verify only the CN
			return verifyCn(cn, serverCert)
		}

		// The instance has a DNS name.
		// First, verify the server hostname
		serverNameErr = serverCert.VerifyHostname(serverName)
		if serverNameErr != nil {
			// If that failed, verify the CN field.
			cnErr := verifyCn(cn, serverCert)
			if cnErr != nil {
				// If both failed, return the server hostname error.
				serverNameErr = &tls.CertificateVerificationError{
					UnverifiedCertificates: certs,
					Err:                    serverNameErr,
				}
				return serverNameErr
			}
		}

		// All checks passed
		return nil
	}
}

func verifyCn(cn instance.ConnName, cert *x509.Certificate) error {
	// Reject CN check if the certificate CN field is empty
	if cert.Subject.CommonName == "" {
		return errtype.NewDialError(
			fmt.Sprintf(
				"certificate CN was empty, expected %q",
				cert.Subject.CommonName,
			),
			cn.String(),
			nil,
		)
	}

	// Verify the CN field matches the instance name
	certInstanceName := fmt.Sprintf("%s:%s", cn.Project(), cn.Name())
	if cert.Subject.CommonName != certInstanceName {
		return errtype.NewDialError(
			fmt.Sprintf(
				"certificate had CN %q, expected %q",
				cert.Subject.CommonName, certInstanceName,
			),
			cn.String(),
			nil,
		)
	}
	return nil
}
