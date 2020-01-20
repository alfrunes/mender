package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
	"golang.org/x/net/http2"
)

var (
	// 	                  http.Client.Timeout
	// +--------------------------------------------------------+
	// +--------+  +---------+  +-------+  +--------+  +--------+
	// |  Dial  |  |   TLS   |  |Request|  |Response|  |Response|
	// |        |  |handshake|  |       |  |headers |  |body    |
	// +--------+  +---------+  +-------+  +--------+  +--------+
	// +--------+  +---------+             +--------+
	//  Dial        TLS                     Response
	//  timeout     handshake               header
	//  timeout                             timeout
	//
	//  It covers the entire exchange, from Dial (if a connection is not reused)
	// to reading the body. This is to timeout long lasting connections.
	//
	// 4 hours should be enough to download a 2GB image file with the
	// average download speed ~1 mbps
	defaultClientReadingTimeout = 4 * time.Hour

	// connection keepalive options
	connectionKeepaliveTime = 10 * time.Second
)

// MenderServer describes a server entity for which the client
// can communicate with. The client can be set up towards multiple
// server entities for redundancy or for migration purposes.
type MenderServer struct {
	// ServerURL is the url of the mender-server.
	ServerURL string
	// TenantToken is the JWT identity token for Hosted Mender or servers
	// with multi-tenancy support (Mender Enterprise).
	TenantToken []byte
	// APIToken is the JWT token generated by the server once a device is
	// authenticated to use the device-API.
	APIToken []byte
}
type UpdateInfo struct {
	DeviceType   string
	ArtifactName string
}

// MenderClient contains all necessary underlying data structure to communicate
// with the server's device API.
type MenderClient struct {
	servers    []MenderServer
	privateKey []byte
	identity   []byte
	httpClient *http.Client
	updateInfo UpdateInfo
	// Intended for other multi-server policies
	activeServer int
}

// NewMenderClient initializes a new mender client towards the server(s)
// given. The id is used for the Authorization endpoint and the will
// appear in the UI as the device's identity. The privateKey is the actual
// identity of the device and is used for cryptographic authentication with the
// server. isHTTPS determines if the server speaks HTTPS, if false non-encrypted
// HTTP will be used and the remaining arguments does not apply. skipVerify
// determines if the certificates should be verified against trusted
// certificates. serverCert is the path to the file containing the chain of
// trusted certificates, this value may be nil if the certificate is known by a
// public authority.
func NewMenderClient(servers []MenderServer, id map[string]interface{},
	privateKey []byte, isHTTPS, skipVerify bool,
	serverCert *string) (*MenderClient, error) {

	client := http.DefaultClient
	transport := http.DefaultTransport.(*http.Transport)

	idStr, err := json.Marshal(id)
	if err != nil {
		return nil, errors.Wrap(err,
			"error converting identity to raw json")
	}

	menderClient := &MenderClient{
		httpClient: client,
		privateKey: privateKey,
		identity:   idStr,
		servers:    servers,
	}

	if isHTTPS {
		// Setup HTTPS client
		var err error
		var trustedcerts *x509.CertPool
		if serverCert != nil {
			trustedcerts, err = loadServerTrust(*serverCert)
			if err != nil {
				return nil, err
			}
		}

		if skipVerify {
			log.Warnf("certificate verification skipped..")
		}
		tlsc := tls.Config{
			RootCAs:            trustedcerts,
			InsecureSkipVerify: skipVerify,
		}
		transport.TLSClientConfig = &tlsc

		client.Transport = transport
		if err != nil {
			return nil, err
		}
	}

	// set connection timeout
	client.Timeout = defaultClientReadingTimeout

	//set keepalive options
	transport.DialContext = (&net.Dialer{
		KeepAlive: connectionKeepaliveTime,
	}).DialContext

	if err := http2.ConfigureTransport(transport); err != nil {
		log.Warnf("failed to enable HTTP/2 for client: %v", err)
	}

	return menderClient, nil
}

func parseRequestError(err error) error {
	// checking the detailed reason of the failure
	if urlErr, ok := err.(*url.Error); ok {
		switch certErr := urlErr.Err.(type) {
		case x509.UnknownAuthorityError:
			log.Error("Certificate is signed by unknown authority.")
			log.Error("If you are using a self-signed certificate, make sure it is " +
				"available locally to the Mender client in /etc/mender/server.crt and " +
				"is configured properly in /etc/mender/mender.conf.")
			log.Error("See https://docs.mender.io/troubleshooting/mender-client#" +
				"certificate-signed-by-unknown-authority for more information.")

			return errors.Wrapf(err, "certificate signed by unknown authority")

		case x509.CertificateInvalidError:
			switch certErr.Reason {
			case x509.Expired:
				log.Error("Certificate has expired or is not yet valid.")
				log.Errorf("Current clock is %s", time.Now())
				log.Error("Verify that the clock on the device is correct " +
					"and/or certificate expiration date is valid.")
				log.Error("See https://docs.mender.io/troubleshooting/mender-client#" +
					"certificate-expired-or-not-yet-valid for more information.")

				return errors.Wrapf(err, "certificate has expired")
			default:
				log.Errorf("Server certificate is invalid, reason: %#v", certErr.Reason)
			}
			return errors.Wrapf(err, "certificate exists, but is invalid")
		default:
			log.Errorf("Error performing request: %v", certErr)
		}
	}
	return err
}

func (client *MenderClient) Do(request *http.Request) (*http.Response, error) {
	var err error
	var rsp *http.Response

	for i := 0; i < len(client.servers); i++ {
		// Compute active server
		s := (i + client.activeServer) % len(client.servers)
		// Replace Hostname to that of the server
		newURL, err := url.Parse(client.servers[s].ServerURL)
		if err != nil {
			return nil, errors.Wrap(err, "error initializing url")
		}
		request.URL.Host = newURL.Host
		request.URL.Scheme = newURL.Scheme
		request.Host = newURL.Host
		request.Header.Set("Authorization",
			"Bearer "+string(client.
				servers[client.activeServer].APIToken))

		// Perform request
		rsp, err = client.httpClient.Do(request)
		if err != nil {
			return rsp, err
		}
		if rsp.StatusCode == 401 {
			log.Info("Client not authorized with %s; "+
				"re-authorizing...", client.servers[s].ServerURL)
			rsp, err = client.authorize(&client.servers[s])
			if err != nil {
				log.Warnf("Re-authorization failed: %s",
					err.Error())
				continue
			} else if rsp.StatusCode >= 400 {
				log.Warnf("Re-authorization failed, "+
					"HTTP status: %s", rsp.Status)
				continue
			}
			log.Info("Successfully re-authorized")
			request.Header.Set("Authorization",
				"Bearer "+string(client.
					servers[client.activeServer].APIToken))
			rsp, err = client.httpClient.Do(request)
			if err != nil {
				return rsp, err
			} else if rsp.StatusCode < 400 {
				break
			}
		}
	}
	return rsp, err
}

func loadServerTrust(serverCert string) (*x509.CertPool, error) {
	if serverCert == "" {
		// Returning nil will make tls.Config.RootCAs nil, which causes
		// tls module to use system certs.
		return nil, nil
	}

	syscerts, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}

	// Read certificate file.
	servcert, err := ioutil.ReadFile(serverCert)
	if err != nil {
		log.Errorf("%s is inaccessible: %s", serverCert, err.Error())
		return nil, err
	}

	if len(servcert) == 0 {
		log.Errorf("Both %s and the system certificate pool are empty.",
			serverCert)
		return nil, errors.New("server certificate is empty")
	}

	block, _ := pem.Decode([]byte(servcert))
	if block != nil {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil {
			log.Infof("API Gateway certificate (in PEM format): \n%s", string(servcert))
			log.Infof("Issuer: %s, Valid from: %s, Valid to: %s",
				cert.Issuer.Organization, cert.NotBefore, cert.NotAfter)
		}
	}

	if syscerts == nil {
		log.Warn("No system certificates found.")
		syscerts = x509.NewCertPool()
	}

	syscerts.AppendCertsFromPEM(servcert)

	if len(syscerts.Subjects()) == 0 {
		return nil, errors.New(
			"error adding trusted server certificate to pool")
	}
	return syscerts, nil
}
