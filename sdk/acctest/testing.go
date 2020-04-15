package acctest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/internalshared/reloadutil"
	"github.com/hashicorp/vault/vault"
	"github.com/y0ssar1an/q"
	"golang.org/x/net/http2"

	docker "github.com/docker/docker/client"
)

// DockerCluster is used to managing the lifecycle of the test Vault cluster
type DockerCluster struct {
	RaftStorage        bool
	ClientAuthRequired bool
	BarrierKeys        [][]byte
	RecoveryKeys       [][]byte
	CACertBytes        []byte
	CACertPEM          []byte
	CAKeyPEM           []byte
	CACertPEMFile      string
	ID                 string
	RootToken          string
	TempDir            string
	ClusterName        string
	RootCAs            *x509.CertPool
	CACert             *x509.Certificate
	CAKey              *ecdsa.PrivateKey
	CleanupFunc        func()
	SetupFunc          func()
	ClusterNodes       []*DockerClusterNode
}

// Cleanup stops all the containers.
// TODO: error/logging
func (rc *DockerCluster) Cleanup() {
	for _, node := range rc.ClusterNodes {
		node.Cleanup()
	}
}

func (rc *DockerCluster) GetBarrierOrRecoveryKeys() [][]byte {
	return rc.GetBarrierKeys()
}

func (rc *DockerCluster) GetCACertPEMFile() string {
	return rc.CACertPEMFile
}

func (rc *DockerCluster) ClusterID() string {
	return rc.ID
}

func (n *DockerClusterNode) Name() string {
	return n.Cluster.ClusterName + "-" + n.NodeID
}

type VaultClusterNode interface {
	Name() string
	APIClient() *api.Client
}

func (rc *DockerCluster) Nodes() []VaultClusterNode {
	ret := make([]VaultClusterNode, len(rc.ClusterNodes))
	for i, core := range rc.ClusterNodes {
		ret[i] = core
	}
	return ret
}

func (rc *DockerCluster) GetBarrierKeys() [][]byte {
	ret := make([][]byte, len(rc.BarrierKeys))
	for i, k := range rc.BarrierKeys {
		ret[i] = vault.TestKeyCopy(k)
	}
	return ret
}

func (rc *DockerCluster) GetRecoveryKeys() [][]byte {
	ret := make([][]byte, len(rc.RecoveryKeys))
	for i, k := range rc.RecoveryKeys {
		ret[i] = vault.TestKeyCopy(k)
	}
	return ret
}

func (rc *DockerCluster) SetBarrierKeys(keys [][]byte) {
	rc.BarrierKeys = make([][]byte, len(keys))
	for i, k := range keys {
		rc.BarrierKeys[i] = vault.TestKeyCopy(k)
	}
}

func (rc *DockerCluster) SetRecoveryKeys(keys [][]byte) {
	rc.RecoveryKeys = make([][]byte, len(keys))
	for i, k := range keys {
		rc.RecoveryKeys[i] = vault.TestKeyCopy(k)
	}
}

func (rc *DockerCluster) Initialize(ctx context.Context) error {
	client, err := rc.ClusterNodes[0].CreateAPIClient()
	if err != nil {
		return err
	}

	var resp *api.InitResponse
	for ctx.Err() == nil {
		resp, err = client.Sys().Init(&api.InitRequest{
			SecretShares:    3,
			SecretThreshold: 3,
		})
		if err == nil && resp != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("nil response to init request")
	}
	q.Q("--> docker setup init response:", resp)
	for _, k := range resp.Keys {
		raw, err := hex.DecodeString(k)
		if err != nil {
			return err
		}
		rc.BarrierKeys = append(rc.BarrierKeys, raw)
	}
	for _, k := range resp.RecoveryKeys {
		raw, err := hex.DecodeString(k)
		if err != nil {
			return err
		}
		rc.RecoveryKeys = append(rc.RecoveryKeys, raw)
	}
	rc.RootToken = resp.RootToken
	q.Q("--> docker init root token:", rc.RootToken)

	// Write root token and barrier keys
	err = ioutil.WriteFile(filepath.Join(rc.TempDir, "root_token"), []byte(rc.RootToken), 0755)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, key := range rc.BarrierKeys {
		// TODO handle errors?
		_, _ = buf.Write(key)
		_, _ = buf.WriteRune('\n')
	}
	err = ioutil.WriteFile(filepath.Join(rc.TempDir, "barrier_keys"), buf.Bytes(), 0755)
	if err != nil {
		return err
	}
	for _, key := range rc.RecoveryKeys {
		// TODO handle errors?
		_, _ = buf.Write(key)
		_, _ = buf.WriteRune('\n')
	}
	err = ioutil.WriteFile(filepath.Join(rc.TempDir, "recovery_keys"), buf.Bytes(), 0755)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Unseal
	for j, node := range rc.ClusterNodes {
		// copy the index value, so we're not reusing it in deeper scopes
		i := j
		client, err := node.CreateAPIClient()
		if err != nil {
			return err
		}
		node.Client = client

		if i > 0 && rc.RaftStorage {
			leader := rc.ClusterNodes[0]
			resp, err := client.Sys().RaftJoin(&api.RaftJoinRequest{
				LeaderAPIAddr:    fmt.Sprintf("https://%s:%d", rc.ClusterNodes[0].Name(), leader.Address.Port),
				LeaderCACert:     string(rc.CACertPEM),
				LeaderClientCert: string(node.ServerCertPEM),
				LeaderClientKey:  string(node.ServerKeyPEM),
			})
			if err != nil {
				return err
			}
			if resp == nil || !resp.Joined {
				return fmt.Errorf("nil or negative response from raft join request: %v", resp)
			}
		}

		var unsealed bool
		for _, key := range rc.BarrierKeys {
			resp, err := client.Sys().Unseal(hex.EncodeToString(key))
			if err != nil {
				return err
			}
			unsealed = !resp.Sealed
		}
		if i == 0 && !unsealed {
			return fmt.Errorf("could not unseal node %d", i)
		}
		client.SetToken(rc.RootToken)

		err = TestWaitHealthMatches(ctx, node.Client, func(health *api.HealthResponse) error {
			if health.Sealed {
				return fmt.Errorf("node %d is sealed: %#v", i, health)
			}
			if health.ClusterID == "" {
				return fmt.Errorf("node %d has no cluster ID", i)
			}

			rc.ID = health.ClusterID
			return nil
		})
		if err != nil {
			return err
		}

		if i == 0 {
			err = TestWaitLeaderMatches(ctx, node.Client, func(leader *api.LeaderResponse) error {
				if !leader.IsSelf {
					// TODO node name here?
					// return fmt.Errorf("node %d leader=%v, expected=%v", leader.IsSelf, true)
					return fmt.Errorf("node leader=%v, expected=%v", leader.IsSelf, true)
				}

				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	for i, node := range rc.ClusterNodes {
		expectLeader := i == 0
		err = TestWaitLeaderMatches(ctx, node.Client, func(leader *api.LeaderResponse) error {
			if expectLeader != leader.IsSelf {
				// TODO node name here?
				// return fmt.Errorf("node %d leader=%v, expected=%v", leader.IsSelf, expectLeader)
				return fmt.Errorf("node leader=%v, expected=%v", leader.IsSelf, expectLeader)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (rc *DockerCluster) setupCA(opts *DockerClusterOptions) error {
	var err error

	certIPs := []net.IP{
		net.IPv6loopback,
		net.ParseIP("127.0.0.1"),
	}

	var caKey *ecdsa.PrivateKey
	if opts != nil && opts.CAKey != nil {
		caKey = opts.CAKey
	} else {
		caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
	}
	rc.CAKey = caKey

	var caBytes []byte
	if opts != nil && len(opts.CACert) > 0 {
		caBytes = opts.CACert
	} else {
		caCertTemplate := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: "localhost",
			},
			DNSNames:              []string{"localhost"},
			IPAddresses:           certIPs,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			SerialNumber:          big.NewInt(mathrand.Int63()),
			NotBefore:             time.Now().Add(-30 * time.Second),
			NotAfter:              time.Now().Add(262980 * time.Hour),
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		caBytes, err = x509.CreateCertificate(rand.Reader, caCertTemplate, caCertTemplate, caKey.Public(), caKey)
		if err != nil {
			return err
		}
	}
	caCert, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return err
	}
	rc.CACert = caCert
	rc.CACertBytes = caBytes

	rc.RootCAs = x509.NewCertPool()
	rc.RootCAs.AddCert(caCert)

	caCertPEMBlock := &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	}
	rc.CACertPEM = pem.EncodeToMemory(caCertPEMBlock)

	rc.CACertPEMFile = filepath.Join(rc.TempDir, "ca", "ca.pem")
	err = ioutil.WriteFile(rc.CACertPEMFile, rc.CACertPEM, 0755)
	if err != nil {
		return err
	}

	marshaledCAKey, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return err
	}
	caKeyPEMBlock := &pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: marshaledCAKey,
	}
	rc.CAKeyPEM = pem.EncodeToMemory(caKeyPEMBlock)

	// We don't actually need this file, but it may be helpful for debugging.
	err = ioutil.WriteFile(filepath.Join(rc.TempDir, "ca", "ca_key.pem"), rc.CAKeyPEM, 0755)
	if err != nil {
		return err
	}

	return nil
}

// TODO: unused at this point
// func (rc *DockerCluster) raftJoinConfig() []api.RaftJoinRequest {
// 	ret := make([]api.RaftJoinRequest, len(rc.ClusterNodes))
// 	for _, node := range rc.ClusterNodes {
// 		ret = append(ret, api.RaftJoinRequest{
// 			LeaderAPIAddr:    fmt.Sprintf("https://%s:%d", node.Address.IP, node.Address.Port),
// 			LeaderCACert:     string(rc.CACertPEM),
// 			LeaderClientCert: string(node.ServerCertPEM),
// 			LeaderClientKey:  string(node.ServerKeyPEM),
// 		})
// 	}
// 	return ret
// }

// Don't call this until n.Address.IP is populated
func (n *DockerClusterNode) setupCert() error {
	var err error

	n.ServerKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	certTemplate := &x509.Certificate{
		Subject: pkix.Name{
			CommonName: n.Name(),
		},
		// Include host.docker.internal for the sake of benchmark-vault running on MacOS/Windows.
		// This allows Prometheus running in docker to scrape the cluster for metrics.
		DNSNames:    []string{"localhost", "host.docker.internal", n.Name()},
		IPAddresses: []net.IP{net.IPv6loopback, net.ParseIP("127.0.0.1")}, // n.Address.IP,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		SerialNumber: big.NewInt(mathrand.Int63()),
		NotBefore:    time.Now().Add(-30 * time.Second),
		NotAfter:     time.Now().Add(262980 * time.Hour),
	}
	n.ServerCertBytes, err = x509.CreateCertificate(rand.Reader, certTemplate, n.Cluster.CACert, n.ServerKey.Public(), n.Cluster.CAKey)
	if err != nil {
		return err
	}
	n.ServerCert, err = x509.ParseCertificate(n.ServerCertBytes)
	if err != nil {
		return err
	}
	n.ServerCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: n.ServerCertBytes,
	})

	marshaledKey, err := x509.MarshalECPrivateKey(n.ServerKey)
	if err != nil {
		return err
	}
	n.ServerKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: marshaledKey,
	})

	n.ServerCertPEMFile = filepath.Join(n.WorkDir, "cert.pem")
	err = ioutil.WriteFile(n.ServerCertPEMFile, n.ServerCertPEM, 0755)
	if err != nil {
		return err
	}

	n.ServerKeyPEMFile = filepath.Join(n.WorkDir, "key.pem")
	err = ioutil.WriteFile(n.ServerKeyPEMFile, n.ServerKeyPEM, 0755)
	if err != nil {
		return err
	}

	tlsCert, err := tls.X509KeyPair(n.ServerCertPEM, n.ServerKeyPEM)
	if err != nil {
		return err
	}

	certGetter := reloadutil.NewCertificateGetter(n.ServerCertPEMFile, n.ServerKeyPEMFile, "")
	if err := certGetter.Reload(nil); err != nil {
		// TODO error handle or panic?
		panic(err)
	}
	tlsConfig := &tls.Config{
		Certificates:   []tls.Certificate{tlsCert},
		RootCAs:        n.Cluster.RootCAs,
		ClientCAs:      n.Cluster.RootCAs,
		ClientAuth:     tls.RequestClientCert,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: certGetter.GetCertificate,
	}
	tlsConfig.BuildNameToCertificate()
	if n.Cluster.ClientAuthRequired {
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}
	n.TLSConfig = tlsConfig

	return nil
}

type DockerClusterNode struct {
	NodeID            string
	Address           *net.TCPAddr
	HostPort          string
	Client            *api.Client
	ServerCert        *x509.Certificate
	ServerCertBytes   []byte
	ServerCertPEM     []byte
	ServerCertPEMFile string
	ServerKey         *ecdsa.PrivateKey
	ServerKeyPEM      []byte
	ServerKeyPEMFile  string
	TLSConfig         *tls.Config
	WorkDir           string
	Cluster           *DockerCluster
	container         *types.ContainerJSON
	dockerAPI         *docker.Client
}

func (n *DockerClusterNode) APIClient() *api.Client {
	return n.Client
}

func (n *DockerClusterNode) CreateAPIClient() (*api.Client, error) {
	transport := cleanhttp.DefaultPooledTransport()
	transport.TLSClientConfig = n.TLSConfig.Clone()
	if err := http2.ConfigureTransport(transport); err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// This can of course be overridden per-test by using its own client
			return fmt.Errorf("redirects not allowed in these tests")
		},
	}
	config := api.DefaultConfig()
	if config.Error != nil {
		return nil, config.Error
	}
	config.Address = fmt.Sprintf("https://127.0.0.1:%s", n.HostPort)
	config.HttpClient = client
	config.MaxRetries = 0
	apiClient, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}
	apiClient.SetToken(n.Cluster.RootToken)
	return apiClient, nil
}

func (n *DockerClusterNode) Cleanup() {
	if err := n.dockerAPI.ContainerKill(context.Background(), n.container.ID, "KILL"); err != nil {
		// TODO handle
		panic(err)
	}
}

func (n *DockerClusterNode) Start(cli *docker.Client, caDir, netName string, netCIDR *DockerClusterNode, pluginBinPath string) error {
	n.dockerAPI = cli

	err := n.setupCert()
	if err != nil {
		return err
	}
	//joinConfig := n.Cluster.raftJoinConfig()
	//joinConfigStr, err := jsonutil.EncodeJSON(joinConfig)
	//if err != nil {
	//	return err
	//}
	vaultCfg := map[string]interface{}{
		"listener": map[string]interface{}{
			"tcp": map[string]interface{}{
				"address":       fmt.Sprintf("%s:%d", "0.0.0.0", 8200),
				"tls_cert_file": "/vault/config/cert.pem",
				"tls_key_file":  "/vault/config/key.pem",
				"telemetry": map[string]interface{}{
					"unauthenticated_metrics_access": true,
				},
			},
		},
		"telemetry": map[string]interface{}{
			"disable_hostname": true,
		},
		"storage": map[string]interface{}{
			"raft": map[string]interface{}{
				"path":    "/vault/file",
				"node_id": n.NodeID,
				//"retry_join": string(joinConfigStr),
			},
		},
		"cluster_name":         netName,
		"log_level":            "TRACE",
		"raw_storage_endpoint": true,
		"plugin_directory":     "/vault/config",
		// These are being provided by docker-entrypoint now, since we don't know
		// the address before the container starts.
		//"api_addr": fmt.Sprintf("https://%s:%d", n.Address.IP, n.Address.Port),
		//"cluster_addr": fmt.Sprintf("https://%s:%d", n.Address.IP, n.Address.Port+1),
	}
	cfgJSON, err := json.Marshal(vaultCfg)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(n.WorkDir, "local.json"), cfgJSON, 0644)
	if err != nil {
		return err
	}
	// setup plugin bin copy if needed
	copyFromTo := map[string]string{
		n.WorkDir: "/vault/config",
		caDir:     "/usr/local/share/ca-certificates/",
		// "/Users/ncc/bin/vault-1.4-linux-prem": "/bin/vault",
		// TODO: find plugin binary pth and copy into container
	}
	if pluginBinPath != "" {
		// from -> to
		// strip "test" from the source
		base := path.Base(pluginBinPath)
		// dest := strings.TrimSuffix(base, ".test")
		// q.Q("--> acctest dest:", dest)
		copyFromTo[pluginBinPath] = filepath.Join("/vault/config", base)
	}
	q.Q("end copyFrom:", copyFromTo)
	r := &Runner{
		DockerAPI: cli,
		ContainerConfig: &container.Config{
			Image: "vault",
			Entrypoint: []string{"/bin/sh", "-c", "update-ca-certificates && " +
				"exec /usr/local/bin/docker-entrypoint.sh vault server -log-level=trace -dev-plugin-dir=./vault/config -config /vault/config/local.json"},
			//Cmd:	[]string{"vault", "server", "-config=/vault/config/local.json"},
			Env: []string{
				"VAULT_CLUSTER_INTERFACE=eth0",
				//"VAULT_REDIRECT_INTERFACE=eth0",
				//"VAULT_REDIRECT_ADDR=https://0.0.0.0:8200",
				fmt.Sprintf("VAULT_REDIRECT_ADDR=https://%s:8200", n.Name()),
			},
			Labels:       nil,
			ExposedPorts: nat.PortSet{"8200/tcp": {}, "8201/tcp": {}},
		},
		ContainerName: n.Name(),
		NetName:       netName,
		//IP:              n.Address.IP.String(),
		CopyFromTo: copyFromTo,
	}

	//if vaultPath, err := exec.LookPath("vault"); err != nil {
	//	r.CopyFromTo[vaultPath] = "/bin/"
	//}

	n.container, err = r.Start(context.Background())
	if err != nil {
		return err
	}

	n.Address = &net.TCPAddr{
		IP:   net.ParseIP(n.container.NetworkSettings.IPAddress),
		Port: 8200,
	}
	ports := n.container.NetworkSettings.NetworkSettingsBase.Ports[nat.Port("8200/tcp")]
	if len(ports) == 0 {
		n.Cleanup()
		return fmt.Errorf("could not find port binding for 8200/tcp")
	}
	n.HostPort = ports[0].HostPort

	return nil
}

// DockerClusterOptions has options for setting up the docker cluster
type DockerClusterOptions struct {
	KeepStandbysSealed bool
	RequireClientAuth  bool
	SkipInit           bool
	CACert             []byte
	NumCores           int
	TempDir            string
	PluginTestBin      string
	// SetupFunc is called after the cluster is started.
	SetupFunc func(t testing.T, c *DockerCluster)
	CAKey     *ecdsa.PrivateKey
	// TODO: plugin source dir here?
}

//
// test methods/functions
//

// TestWaitHealthMatches checks health TODO: update docs
func TestWaitHealthMatches(ctx context.Context, client *api.Client, ready func(response *api.HealthResponse) error) error {
	var health *api.HealthResponse
	var err error
	for ctx.Err() == nil {
		// TODO ideally Health method would take a context
		health, err = client.Sys().Health()
		switch {
		case err != nil:
		case health == nil:
			err = fmt.Errorf("nil response to health check")
		default:
			err = ready(health)
			if err == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("error checking health: %v", err)
}

func TestWaitLeaderMatches(ctx context.Context, client *api.Client, ready func(response *api.LeaderResponse) error) error {
	var leader *api.LeaderResponse
	var err error
	for ctx.Err() == nil {
		// TODO ideally Leader method would take a context
		leader, err = client.Sys().Leader()
		switch {
		case err != nil:
		case leader == nil:
			err = fmt.Errorf("nil response to leader check")
		default:
			err = ready(leader)
			if err == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("error checking leader: %v", err)
}

// end test helper methods

// TODO: change back to 3
// var DefaultNumCores = 3
var DefaultNumCores = 1

// NewDockerCluster creates a managed docker container running Vault
func NewDockerCluster(name string, base *vault.CoreConfig, opts *DockerClusterOptions) (rc *DockerCluster, err error) {
	cluster := DockerCluster{
		ClusterName: name,
		RaftStorage: true,
	}

	if opts != nil && opts.TempDir != "" {
		if _, err := os.Stat(opts.TempDir); os.IsNotExist(err) {
			if err := os.MkdirAll(opts.TempDir, 0700); err != nil {
				return nil, err
			}
		}
		cluster.TempDir = opts.TempDir
	} else {
		tempDir, err := ioutil.TempDir("", "vault-test-cluster-")
		if err != nil {
			return nil, err
		}
		cluster.TempDir = tempDir
	}
	caDir := filepath.Join(cluster.TempDir, "ca")
	if err := os.MkdirAll(caDir, 0755); err != nil {
		return nil, err
	}

	// // TODO: compiling with command here
	// buildDir := filepath.Join(cluster.TempDir, "build")
	// if err := os.MkdirAll(caDir, 0755); err != nil {
	// 	return nil, err
	// }

	var numCores int
	if opts == nil || opts.NumCores == 0 {
		numCores = DefaultNumCores
	} else {
		numCores = opts.NumCores
	}

	if opts != nil && opts.RequireClientAuth {
		cluster.ClientAuthRequired = true
	}

	cidr := "192.168.128.0/20"
	//baseIP, _, err := net.ParseCIDR(cidr)
	//baseIPv4 := baseIP.To4()
	//if err != nil {
	//	return nil, err
	//}
	for i := 0; i < numCores; i++ {
		nodeID := fmt.Sprintf("vault-%d", i)
		node := &DockerClusterNode{
			NodeID: nodeID,
			//Address: &net.TCPAddr{
			//	IP: net.IPv4(baseIPv4[0], baseIPv4[1], baseIPv4[2], byte(i+2)),
			//	Port: 8200,
			//},
			Cluster: &cluster,
			WorkDir: filepath.Join(cluster.TempDir, nodeID),
		}
		cluster.ClusterNodes = append(cluster.ClusterNodes, node)
		if err := os.MkdirAll(node.WorkDir, 0700); err != nil {
			return nil, err
		}
	}

	err = cluster.setupCA(opts)
	if err != nil {
		return nil, err
	}

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithVersion("1.40"))
	if err != nil {
		return nil, err
	}
	netName := "vault-test"
	_, err = SetupNetwork(cli, netName, cidr)
	if err != nil {
		return nil, err
	}

	for _, node := range cluster.ClusterNodes {
		// TODO: add test image path here to copy-from-CopyFromToto
		pluginBinPath := ""
		if opts != nil {
			q.Q("opts bin path in testing/new:", opts.PluginTestBin)
			pluginBinPath = opts.PluginTestBin
		} else {
			q.Q("opts nil in cluster node start")
		}

		// TODO: maybe don't need plugin here due to replication.. but need it on 1
		// at least
		err := node.Start(cli, caDir, netName, node, pluginBinPath)
		if err != nil {
			return nil, err
		}
	}

	if opts == nil || !opts.SkipInit {
		if err := cluster.Initialize(context.Background()); err != nil {
			return nil, err
		}
	}

	return &cluster, nil
}

// Docker networking functions
// SetupNetwork establishes networking for the Docker container
func SetupNetwork(cli *docker.Client, netName, cidr string) (string, error) {
	ctx := context.Background()

	netResources, err := cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return "", err
	}
	for _, netRes := range netResources {
		if netRes.Name == netName {
			if len(netRes.IPAM.Config) > 0 && netRes.IPAM.Config[0].Subnet == cidr {
				return netRes.ID, nil
			}
			err = cli.NetworkRemove(ctx, netRes.ID)
			if err != nil {
				return "", err
			}
		}
	}

	id, err := createNetwork(cli, netName, cidr)
	if err != nil {
		return "", fmt.Errorf("couldn't create network %s on %s: %w", netName, cidr, err)
	}
	return id, nil
}

func createNetwork(cli *docker.Client, netName, cidr string) (string, error) {
	resp, err := cli.NetworkCreate(context.Background(), netName, types.NetworkCreate{
		CheckDuplicate: true,
		Driver:         "bridge",
		Options:        map[string]string{},
		IPAM: &network.IPAM{
			Driver:  "default",
			Options: map[string]string{},
			Config: []network.IPAMConfig{
				{
					Subnet: cidr,
				},
			},
		},
	})
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}
