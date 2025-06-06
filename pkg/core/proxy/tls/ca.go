//go:build linux

// Package tls provides functionality for handling tls connetions.
package tls

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

//go:embed asset/ca.crt
var caCrt []byte //certificate

//go:embed asset/ca.key
var caPKey []byte //private key

//go:embed asset
var _ embed.FS

var caStorePath = []string{
	"/usr/local/share/ca-certificates/",
	"/etc/pki/ca-trust/source/anchors/",
	"/etc/ca-certificates/trust-source/anchors/",
	"/etc/pki/trust/anchors/",
	"/usr/local/share/certs/",
	"/etc/ssl/certs/",
}

var caStoreUpdateCmd = []string{
	"update-ca-certificates",
	"update-ca-trust",
	"trust extract-compat",
	"tools-ca-trust extract",
	"certctl rehash",
}

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func updateCaStore(ctx context.Context) error {
	commandRun := false
	for _, cmd := range caStoreUpdateCmd {
		if commandExists(cmd) {
			commandRun = true
			c := exec.CommandContext(ctx, cmd)
			_, err := c.CombinedOutput()
			if err != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					return err
				}
			}
		}
	}
	if !commandRun {
		return fmt.Errorf("no valid CA store tools command found")
	}
	return nil
}

func getCaPaths() ([]string, error) {
	var caPaths []string
	for _, dir := range caStorePath {
		if util.IsDirectoryExist(dir) {
			caPaths = append(caPaths, dir)
		}
	}
	if len(caPaths) == 0 {
		return nil, fmt.Errorf("no valid CA store path found")
	}
	return caPaths, nil
}

// to extract ca certificate to temp
func extractCertToTemp() (string, error) {
	tempFile, err := os.CreateTemp("", "ca.crt")

	if err != nil {
		return "", err
	}
	defer func(tempFile *os.File) {
		err := tempFile.Close()
		if err != nil {
			return
		}
	}(tempFile)

	// Change the file permissions to allow read access for all users
	err = os.Chmod(tempFile.Name(), 0666)
	if err != nil {
		return "", err
	}

	// Write to the file
	_, err = tempFile.Write(caCrt)
	if err != nil {
		return "", err
	}

	// Close the file
	err = tempFile.Close()
	if err != nil {
		return "", err
	}
	return tempFile.Name(), nil
}

// isJavaCAExist checks if the CA is already installed in the specified Java keystore
func isJavaCAExist(ctx context.Context, alias, storepass, cacertsPath string) bool {
	cmd := exec.CommandContext(ctx, "keytool", "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)

	err := cmd.Run()
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return err == nil
}

// installJavaCA installs the CA in the Java keystore
func installJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	// check if java is installed
	if util.IsJavaInstalled() {
		logger.Debug("checking java path from default java home")
		javaHome, err := util.GetJavaHome(ctx)

		if err != nil {
			utils.LogError(logger, err, "Java detected but failed to find JAVA_HOME")
			return err
		}

		// Assuming modern Java structure (without /jre/)
		cacertsPath := fmt.Sprintf("%s/lib/security/cacerts", javaHome)
		// You can modify these as per your requirements
		storePass := "changeit"
		alias := "keployCA"

		logger.Debug("", zap.Any("java_home", javaHome), zap.Any("caCertsPath", cacertsPath), zap.Any("caPath", caPath))

		if isJavaCAExist(ctx, alias, storePass, cacertsPath) {
			logger.Debug("Java detected and CA already exists", zap.String("path", cacertsPath))
			return nil
		}

		cmd := exec.CommandContext(ctx, "keytool", "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)
		cmdOutput, err := cmd.CombinedOutput()

		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				utils.LogError(logger, err, "Java detected but failed to import CA", zap.String("output", string(cmdOutput)))
				return err
			}
		}

		logger.Debug("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
		logger.Debug("Successfully imported CA", zap.Any("", cmdOutput))
	} else {
		logger.Debug("Java is not installed on the system")
	}
	return nil
}

// TODO: This function should be used even before starting the proxy server. It should be called just after the keploy is started.
// because the custom ca in case of NODE is set via env variable NODE_EXTRA_CA_CERTS and env variables can be set only on startup.
// As in case of unit test integration, we are starting the proxy via api.

// SetupCA setups custom certificate authority to handle TLS connections
func SetupCA(ctx context.Context, logger *zap.Logger) error {
	caPaths, err := getCaPaths()
	if err != nil {
		utils.LogError(logger, err, "Failed to find the CA store path")
		return err
	}

	for _, path := range caPaths {
		caPath := filepath.Join(path, "ca.crt")

		fs, err := os.Create(caPath)
		if err != nil {
			utils.LogError(logger, err, "Failed to create path for ca certificate", zap.Any("root store path", path))
			return err
		}

		_, err = fs.Write(caCrt)
		if err != nil {
			utils.LogError(logger, err, "Failed to write custom ca certificate", zap.Any("root store path", path))
			return err
		}

		// install CA in the java keystore if java is installed
		err = installJavaCA(ctx, logger, caPath)
		if err != nil {
			utils.LogError(logger, err, "Failed to install CA in the java keystore")
			return err
		}
	}

	// Update the trusted CAs store
	err = updateCaStore(ctx)
	if err != nil {
		utils.LogError(logger, err, "Failed to update the CA store")
		return err
	}

	tempCertPath, err := extractCertToTemp()
	if err != nil {
		utils.LogError(logger, err, "Failed to extract certificate to tmp folder")
		return err
	}

	// for node
	err = os.Setenv("NODE_EXTRA_CA_CERTS", tempCertPath)
	if err != nil {
		utils.LogError(logger, err, "Failed to set environment variable NODE_EXTRA_CA_CERTS")
		return err
	}

	// for python
	err = os.Setenv("REQUESTS_CA_BUNDLE", tempCertPath)
	if err != nil {
		utils.LogError(logger, err, "Failed to set environment variable REQUESTS_CA_BUNDLE")
		return err
	}
	return nil
}

// SrcPortToDstURL map is used to store the mapping between source port and DstURL for the TLS connection
var SrcPortToDstURL = sync.Map{}

var setLogLevelOnce sync.Once

func CertForClient(logger *zap.Logger, clientHello *tls.ClientHelloInfo, caPrivKey any, caCertParsed *x509.Certificate, backdate time.Time) (*tls.Certificate, error) {

	// Ensure log level is set only once

	/*
	* Since multiple goroutines can call this function concurrently, we need to ensure that the log level is set only once.
	 */
	setLogLevelOnce.Do(func() {

		// * Set the log level to error to avoid unnecessary logs. like below...

		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] signed certificate with serial number 435398774381835435678674951099961010543769077102

		cfsslLog.Level = cfsslLog.LevelError
	})

	// Generate a new server certificate and private key for the given hostname
	dstURL := clientHello.ServerName

	remoteAddr := clientHello.Conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	SrcPortToDstURL.Store(sourcePort, dstURL)

	serverReq := &csr.CertificateRequest{
		//Make the name accordng to the ip of the request
		CN: clientHello.ServerName,
		Hosts: []string{
			clientHello.ServerName,
		},
		KeyRequest: csr.NewKeyRequest(),
	}

	serverCsr, serverKey, err := csr.ParseRequest(serverReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create server CSR: %v", err)
	}
	cryptoSigner, ok := caPrivKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("failed to typecast the caPrivKey")
	}
	signerd, err := local.NewSigner(cryptoSigner, caCertParsed, signer.DefaultSigAlgo(cryptoSigner), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	if backdate.IsZero() {
		logger.Debug("backdate is zero, using current time")
		backdate = time.Now()
	}

	// Case: time freezing (an Ent. feature) is enabled,
	// If application time is frozen in past, and the certificate is signed today, then the certificate will be invalid.
	// This results in a certificate error during tls handshake.
	// To avoid this, we set the certificate’s validity period (NotBefore and NotAfter)
	// by referencing the testcase request time of the application (backdate) instead of the current real time.
	//
	// Note: If you have recorded test cases before April 20, 2024 (http://www.sslchecker.com/certdecoder?su=269725513dfeb137f6f29b8488f17ca9)
	// and are using time freezing, please reach out to us if you get tls handshake error.
	signReq := signer.SignRequest{
		Hosts:     serverReq.Hosts,
		Request:   string(serverCsr),
		Profile:   "web",
		NotBefore: backdate.AddDate(-1, 0, 0),
		NotAfter:  time.Now().AddDate(1, 0, 0),
	}

	serverCert, err := signerd.Sign(signReq)
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %v", err)
	}

	logger.Debug("signed the certificate for a duration of 2 years", zap.Any("notBefore", signReq.NotBefore.String()), zap.Any("notAfter", signReq.NotAfter.String()))

	// Load the server certificate and private key
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %v", err)
	}

	return &serverTLSCert, nil
}
