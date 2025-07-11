// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/elastic/elastic-agent-libs/kibana"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/transport/httpcommon"
	"github.com/elastic/elastic-agent-libs/transport/tlscommon"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/enroll"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent/internal/pkg/agent/configuration"
	"github.com/elastic/elastic-agent/internal/pkg/agent/errors"
	"github.com/elastic/elastic-agent/internal/pkg/agent/storage"
	"github.com/elastic/elastic-agent/internal/pkg/cli"
	"github.com/elastic/elastic-agent/internal/pkg/config"
	"github.com/elastic/elastic-agent/internal/pkg/crypto"
	"github.com/elastic/elastic-agent/internal/pkg/fleetapi"
	fleetclient "github.com/elastic/elastic-agent/internal/pkg/fleetapi/client"
	"github.com/elastic/elastic-agent/internal/pkg/remote"
	"github.com/elastic/elastic-agent/pkg/component"
	"github.com/elastic/elastic-agent/pkg/core/logger"
	"github.com/elastic/elastic-agent/pkg/core/process"
	"github.com/elastic/elastic-agent/pkg/utils"
	"github.com/elastic/elastic-agent/version"
)

const (
	requestRetrySleepEnv     = "KIBANA_REQUEST_RETRY_SLEEP"
	maxRequestRetriesEnv     = "KIBANA_REQUEST_RETRY_COUNT"
	defaultRequestRetrySleep = "1s"                          // sleep 1 sec between retries for HTTP requests
	defaultMaxRequestRetries = "30"                          // maximum number of retries for HTTP requests
	agentBaseDirectory       = "/usr/share/elastic-agent"    // directory that holds all elastic-agent related files
	defaultStateDirectory    = agentBaseDirectory + "/state" // directory that will hold the state data

	logsPathPerms = 0775
)

// Used to strip the appended ({uuid}) from the name of an enrollment token. This makes much easier for
// a container to reference a token by name, without having to know what the generated UUID is for that name.
var tokenNameStrip = regexp.MustCompile(`\s\([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\)$`)

func newContainerCommand(_ []string, streams *cli.IOStreams) *cobra.Command {
	cmd := cobra.Command{
		Hidden: true, // not exposed over help; used by container entrypoint only
		Use:    "container",
		Short:  "Bootstrap Elastic Agent to run inside a container",
		Long: `This command should only be used as an entrypoint for a container. This will prepare the Elastic Agent using
environment variables to run inside of the container.

The following actions are possible and grouped based on the actions.

* Elastic Agent Fleet Enrollment
  This enrolls the Elastic Agent into a Fleet Server. It is also possible to have this create a new enrollment token
  for this specific Elastic Agent.

  FLEET_ENROLL - set to 1 for enrollment into Fleet Server. If not set, Elastic Agent is run in standalone mode.
  FLEET_URL - URL of the Fleet Server to enroll into
  FLEET_ENROLLMENT_TOKEN - token to use for enrollment. This is not needed in case FLEET_SERVER_ENABLED and FLEET_ENROLL is set. Then the token is fetched from Kibana.
  FLEET_ENROLL_TIMEOUT - The timeout duration for the enroll commnd. Defaults to 10m. A negative value disables the timeout.
  FLEET_CA - path to certificate authority to use with communicate with Fleet Server [$KIBANA_CA]
  FLEET_INSECURE - communicate with Fleet with either insecure HTTP or unverified HTTPS
  ELASTIC_AGENT_CERT - path to certificate to use for connecting to fleet-server.
  ELASTIC_AGENT_CERT_KEY - path to private key use for connecting to fleet-server.

  The following vars are need in the scenario that Elastic Agent should automatically fetch its own token.

  KIBANA_FLEET_HOST - Kibana host to enable create enrollment token on [$KIBANA_HOST]
  FLEET_TOKEN_NAME - token name to use for fetching token from Kibana. This requires Kibana configs to be set.
  FLEET_TOKEN_POLICY_NAME - token policy name to use for fetching token from Kibana. This requires Kibana configs to be set.

* Bootstrapping Fleet Server
  This bootstraps the Fleet Server to be run by this Elastic Agent. At least one Fleet Server is required in a Fleet
  deployment for other Elastic Agents to bootstrap. In case the Elastic Agent is run without Fleet Server, these variables
  are not needed.

  If FLEET_SERVER_ENABLE and FLEET_ENROLL is set but no FLEET_ENROLLMENT_TOKEN, the token is automatically fetched from Kibana.

  FLEET_SERVER_ENABLE - set to 1 enables bootstrapping of Fleet Server inside Elastic Agent (forces FLEET_ENROLL enabled)
  FLEET_SERVER_ELASTICSEARCH_HOST - Elasticsearch host for Fleet Server to communicate with [$ELASTICSEARCH_HOST]
  FLEET_SERVER_ELASTICSEARCH_CA - path to certificate authority to use to communicate with Elasticsearch [$ELASTICSEARCH_CA]
  FLEET_SERVER_ELASTICSEARCH_CA_TRUSTED_FINGERPRINT - The sha-256 fingerprint value of the certificate authority to trust
  FLEET_SERVER_ELASTICSEARCH_INSECURE - disables cert validation for communication with Elasticsearch
  FLEET_SERVER_SERVICE_TOKEN - service token to use for communication with Elasticsearch
  FLEET_SERVER_SERVICE_TOKEN_PATH - path to service token file to use for communication with Elasticsearch
  FLEET_SERVER_POLICY_ID - policy ID for Fleet Server to use for itself ("Default Fleet Server policy" used when undefined)
  FLEET_SERVER_HOST - binding host for Fleet Server HTTP (overrides the policy). By default this is 0.0.0.0.
  FLEET_SERVER_PORT - binding port for Fleet Server HTTP (overrides the policy)
  FLEET_SERVER_CERT - path to certificate to use for HTTPS endpoint
  FLEET_SERVER_CERT_KEY - path to private key for certificate to use for HTTPS endpoint
  FLEET_SERVER_CERT_KEY_PASSPHRASE - path to private key passphrase file for certificate to use for HTTPS endpoint
  FLEET_SERVER_ES_CERT - path to certificate to use for connecting to Elasticsearch
  FLEET_SERVER_ES_CERT_KEY - path to private key for certificate to use for connecting to Elasticsearch
  FLEET_SERVER_CLIENT_AUTH - fleet-server mTLS client authentication for connecting elastic-agents. Must be one of [none, optional, required]. A default of none is used.
  FLEET_SERVER_INSECURE_HTTP - expose Fleet Server over HTTP (not recommended; insecure)
  FLEET_SERVER_INIT_TIMEOUT - Sets the initial timeout when starting up the fleet server under agent. Default: 30s.

* Preparing Kibana for Fleet
  This prepares the Fleet plugin that exists inside of Kibana. This must either be enabled here or done externally
  before Fleet Server will actually successfully start. All the Kibana variables are not needed in case Elastic Agent
  should not setup Fleet.

  KIBANA_FLEET_HOST - Kibana host accessible from Fleet Server. [$KIBANA_HOST]
  KIBANA_FLEET_USERNAME - Kibana username to service token [$KIBANA_USERNAME]
  KIBANA_FLEET_PASSWORD - Kibana password to service token [$KIBANA_PASSWORD]
  KIBANA_FLEET_CA - path to certificate authority to use with communicate with Kibana [$KIBANA_CA]
  KIBANA_REQUEST_RETRY_SLEEP - sleep duration taken when agent performs a request to Kibana [default 1s]
  KIBANA_REQUEST_RETRY_COUNT - number of retries agent performs when executing a request to Kibana [default 30]

The following environment variables are provided as a convenience to prevent a large number of environment variables to
be used when the same credentials will be used across all the possible actions above.

  ELASTICSEARCH_HOST - Elasticsearch host [http://elasticsearch:9200]
  ELASTICSEARCH_USERNAME - Elasticsearch username [elastic]
  ELASTICSEARCH_PASSWORD - Elasticsearch password [changeme]
  ELASTICSEARCH_CA - path to certificate authority to use to communicate with Elasticsearch
  KIBANA_HOST - Kibana host [http://kibana:5601]
  KIBANA_FLEET_USERNAME - Kibana username to enable Fleet [$ELASTICSEARCH_USERNAME]
  KIBANA_FLEET_PASSWORD - Kibana password to enable Fleet [$ELASTICSEARCH_PASSWORD]
  KIBANA_CA - path to certificate authority to use with communicate with Kibana [$ELASTICSEARCH_CA]
  ELASTIC_AGENT_TAGS - user provided tags for the agent [linux,staging]


* Elastic-Agent event logging
  If EVENTS_TO_STDERR is set to true log entries containing event data or whole raw events will be logged to stderr alongside
all other log entries. If unset or set to false, the events will be logged to a separate file that is not collected alongside
the monitoring logs, however they will be present in diagnostics.

By default when this command starts it will check for an existing fleet.yml. If that file already exists then
all the above actions will be skipped, because the Elastic Agent has already been enrolled. To ensure that enrollment
occurs on every start of the container set FLEET_FORCE to 1.
`,
		Run: func(c *cobra.Command, args []string) {
			if err := logContainerCmd(streams); err != nil {
				logError(streams, err)
				os.Exit(1)
			}
		},
	}

	return &cmd
}

func logError(streams *cli.IOStreams, err error) {
	fmt.Fprintf(streams.Err, "Error: %v\n%s\n", err, troubleshootMessage())
}

func logInfo(streams *cli.IOStreams, a ...interface{}) {
	fmt.Fprintln(streams.Out, a...)
}

func logContainerCmd(streams *cli.IOStreams) error {
	logsPath := envWithDefault("", "LOGS_PATH")
	if logsPath != "" {
		// log this entire command to a file as well as to the passed streams
		if err := os.MkdirAll(logsPath, logsPathPerms); err != nil {
			return fmt.Errorf("preparing LOGS_PATH(%s) failed: %w", logsPath, err)
		}
		logPath := filepath.Join(logsPath, "elastic-agent-startup.log")
		w, err := os.Create(logPath)
		if err != nil {
			return fmt.Errorf("opening startup log(%s) failed: %w", logPath, err)
		}
		defer w.Close()
		streams.Out = io.MultiWriter(streams.Out, w)
		streams.Err = io.MultiWriter(streams.Out, w)
	}
	return containerCmd(streams)
}

func containerCmd(streams *cli.IOStreams) error {
	// set paths early so all action below use the defined paths
	if err := setPaths("", "", "", "", true); err != nil {
		return err
	}

	elasticCloud := envBool("ELASTIC_AGENT_CLOUD")
	// if not in cloud mode, always run the agent
	runAgent := !elasticCloud
	// create access configuration from ENV and config files
	cfg, err := defaultAccessConfig()
	if err != nil {
		return err
	}

	for _, f := range []string{"fleet-setup.yml", "credentials.yml"} {
		c, err := config.LoadFile(filepath.Join(paths.Config(), f))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("parsing config file(%s): %w", f, err)
		}
		if c != nil {
			err = c.UnpackTo(&cfg)
			if err != nil {
				return fmt.Errorf("unpacking config file(%s): %w", f, err)
			}
			// if in elastic cloud mode, only run the agent when configured
			runAgent = true
		}
	}

	// start apm-server legacy process when in cloud mode
	var wg sync.WaitGroup
	var apmProc *process.Info
	apmPath := os.Getenv("APM_SERVER_PATH")
	if elasticCloud {
		logInfo(streams, "Starting in elastic cloud mode")
		if elasticCloud && apmPath != "" {
			// run legacy APM Server as a daemon; send termination signal
			// to the main process if the daemon is stopped
			mainProc, err := os.FindProcess(os.Getpid())
			if err != nil {
				return errors.New(err, "finding current process")
			}
			if apmProc, err = runLegacyAPMServer(streams); err != nil {
				return errors.New(err, "starting legacy apm-server")
			}
			wg.Add(1) // apm-server legacy process
			logInfo(streams, "Legacy apm-server daemon started.")
			go func() {
				if err := func() error {
					apmProcState, err := apmProc.Process.Wait()
					if err != nil {
						return err
					}
					if apmProcState.ExitCode() != 0 {
						return fmt.Errorf("apm-server process exited with %d", apmProcState.ExitCode())
					}
					return nil
				}(); err != nil {
					logError(streams, err)
				}

				wg.Done()
				// sending kill signal to current process (elastic-agent)
				logInfo(streams, "Initiate shutdown elastic-agent.")
				mainProc.Signal(syscall.SIGTERM) //nolint:errcheck //not required
			}()

			defer func() {
				if apmProc != nil {
					apmProc.Stop() //nolint:errcheck //not required
					logInfo(streams, "Initiate shutdown legacy apm-server.")
				}
			}()
		}
	}

	if runAgent {
		// run the main elastic-agent container command
		err = runContainerCmd(streams, cfg)
	}
	// wait until APM Server shut down
	wg.Wait()
	return err
}

func runContainerCmd(streams *cli.IOStreams, cfg setupConfig) error {
	var err error

	initTimeout := envTimeout(fleetInitTimeoutName)

	if cfg.FleetServer.Enable {
		err = ensureServiceToken(streams, &cfg)
		if err != nil {
			return err
		}
	}

	shouldEnroll, err := shouldFleetEnroll(cfg)
	if err != nil {
		return err
	}
	if shouldEnroll {
		var policy *kibanaPolicy
		token := cfg.Fleet.EnrollmentToken
		if token == "" && !cfg.FleetServer.Enable {
			client, err := kibanaClient(cfg.Kibana, cfg.Kibana.Headers)
			if err != nil {
				return err
			}
			policy, err = kibanaFetchPolicy(cfg, client, streams)
			if err != nil {
				return err
			}
			token, err = kibanaFetchToken(cfg, client, policy, streams, cfg.Fleet.TokenName)
			if err != nil {
				return err
			}
		}
		policyID := cfg.FleetServer.PolicyID
		if policy != nil {
			policyID = policy.ID
		}
		if policyID != "" {
			logInfo(streams, "Policy selected for enrollment: ", policyID)
		}

		executable, err := os.Executable()
		if err != nil {
			return err
		}

		cmdArgs, err := buildEnrollArgs(cfg, token, policyID)
		if err != nil {
			return err
		}
		enroll := exec.Command(executable, cmdArgs...)
		enroll.Stdout = streams.Out
		enroll.Stderr = streams.Err
		err = enroll.Start()
		if err != nil {
			return errors.New("failed to execute enrollment command", err)
		}
		err = enroll.Wait()
		if err != nil {
			return errors.New("enrollment failed", err)
		}
	}

	return run(containerCfgOverrides, false, initTimeout, isContainer)
}

// TokenResp is used to decode a response for generating a service token
type TokenResp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ensureServiceToken will ensure that the cfg specified has the service_token attributes filled.
//
// If no token is specified it will try to use the value from service_token_path
// If no filepath is specified it will use the elasticsearch username/password to request a new token from Kibana
func ensureServiceToken(streams *cli.IOStreams, cfg *setupConfig) error {
	// There's already a service token
	if cfg.Kibana.Fleet.ServiceToken != "" || cfg.FleetServer.Elasticsearch.ServiceToken != "" {
		return nil
	}
	// read from secret file
	if cfg.FleetServer.Elasticsearch.ServiceTokenPath != "" {
		p, err := os.ReadFile(cfg.FleetServer.Elasticsearch.ServiceTokenPath)
		if err != nil {
			return fmt.Errorf("unable to open service_token_path: %w", err)
		}
		cfg.Kibana.Fleet.ServiceToken = string(p)
		cfg.FleetServer.Elasticsearch.ServiceToken = string(p)
		return nil
	}
	if cfg.Kibana.Fleet.ServiceTokenPath != "" {
		p, err := os.ReadFile(cfg.Kibana.Fleet.ServiceTokenPath)
		if err != nil {
			return fmt.Errorf("unable to open service_token_path: %w", err)
		}
		cfg.Kibana.Fleet.ServiceToken = string(p)
		cfg.FleetServer.Elasticsearch.ServiceToken = string(p)
		return nil
	}
	// request new token
	if cfg.Kibana.Fleet.Username == "" || cfg.Kibana.Fleet.Password == "" {
		return fmt.Errorf("username/password must be provided to retrieve service token")
	}

	logInfo(streams, "Requesting service_token from Kibana.")

	// Client is not passed in to this function because this function will use username/password and then
	// all the following clients will use the created service token.
	client, err := kibanaClient(cfg.Kibana, cfg.Kibana.Headers)
	if err != nil {
		return err
	}
	code, r, err := client.Request("POST", "/api/fleet/service_tokens", nil, nil, nil)
	if err != nil {
		return fmt.Errorf("request to get security token from Kibana failed: %w", err)
	}
	if code >= 400 {
		return fmt.Errorf("request to get security token from Kibana failed with status %d, body: %s", code, string(r))
	}
	t := TokenResp{}
	err = json.Unmarshal(r, &t)
	if err != nil {
		return fmt.Errorf("unable to decode response: %w", err)
	}
	logInfo(streams, "Created service_token named:", t.Name)
	cfg.Kibana.Fleet.ServiceToken = t.Value
	cfg.FleetServer.Elasticsearch.ServiceToken = t.Value
	return nil
}

func buildEnrollArgs(cfg setupConfig, token string, policyID string) ([]string, error) {
	args := []string{
		"enroll", "-f",
		"-c", paths.ConfigFile(),
		"--path.home", paths.Top(), // --path.home actually maps to paths.Top()
		"--path.config", paths.Config(),
		"--path.logs", paths.Logs(),
		"--path.socket", paths.ControlSocket(),
		"--skip-daemon-reload",
	}
	if paths.Downloads() != "" {
		args = append(args, "--path.downloads", paths.Downloads())
	}
	if !paths.IsVersionHome() {
		args = append(args, "--path.home.unversioned")
	}
	if tags := envWithDefault("", "ELASTIC_AGENT_TAGS"); tags != "" {
		args = append(args, "--tag", tags)
	}
	if cfg.FleetServer.Enable {
		connStr, err := buildFleetServerConnStr(cfg.FleetServer)
		if err != nil {
			return nil, err
		}
		args = append(args, "--fleet-server-es", connStr)
		if cfg.FleetServer.Elasticsearch.ServiceTokenPath != "" {
			args = append(args, "--fleet-server-service-token-path", cfg.FleetServer.Elasticsearch.ServiceTokenPath)
		} else if cfg.FleetServer.Elasticsearch.ServiceTokenPath == "" && cfg.FleetServer.Elasticsearch.ServiceToken != "" {
			args = append(args, "--fleet-server-service-token", cfg.FleetServer.Elasticsearch.ServiceToken)
		}
		if policyID != "" {
			args = append(args, "--fleet-server-policy", policyID)
		}
		if cfg.FleetServer.Elasticsearch.CA != "" {
			args = append(args, "--fleet-server-es-ca", cfg.FleetServer.Elasticsearch.CA)
		}
		if cfg.FleetServer.Elasticsearch.CATrustedFingerprint != "" {
			args = append(args, "--fleet-server-es-ca-trusted-fingerprint", cfg.FleetServer.Elasticsearch.CATrustedFingerprint)
		}
		if cfg.FleetServer.Elasticsearch.Cert != "" {
			args = append(args, "--fleet-server-es-cert", cfg.FleetServer.Elasticsearch.Cert)
		}
		if cfg.FleetServer.Elasticsearch.CertKey != "" {
			args = append(args, "--fleet-server-es-cert-key", cfg.FleetServer.Elasticsearch.CertKey)
		}
		if cfg.FleetServer.Host != "" {
			args = append(args, "--fleet-server-host", cfg.FleetServer.Host)
		}
		if cfg.FleetServer.Port != "" {
			args = append(args, "--fleet-server-port", cfg.FleetServer.Port)
		}
		if cfg.FleetServer.Cert != "" {
			args = append(args, "--fleet-server-cert", cfg.FleetServer.Cert)
		}
		if cfg.FleetServer.CertKey != "" {
			args = append(args, "--fleet-server-cert-key", cfg.FleetServer.CertKey)
		}
		if cfg.FleetServer.PassphrasePath != "" {
			args = append(args, "--fleet-server-cert-key-passphrase", cfg.FleetServer.PassphrasePath)
		}
		if cfg.FleetServer.ClientAuth != "" {
			args = append(args, "--fleet-server-client-auth", cfg.FleetServer.ClientAuth)
		}

		for k, v := range cfg.FleetServer.Headers {
			args = append(args, "--header", k+"="+v)
		}

		if cfg.Fleet.URL != "" {
			args = append(args, "--url", cfg.Fleet.URL)
		}
		if cfg.FleetServer.InsecureHTTP {
			args = append(args, "--fleet-server-insecure-http")
		}
		if cfg.FleetServer.InsecureHTTP || cfg.Fleet.Insecure {
			args = append(args, "--insecure")
		}
		if cfg.FleetServer.Elasticsearch.Insecure {
			args = append(args, "--fleet-server-es-insecure")
		}
		if cfg.FleetServer.Timeout != 0 {
			args = append(args, "--fleet-server-timeout")
			args = append(args, cfg.FleetServer.Timeout.String())
		}
	} else {
		if cfg.Fleet.URL == "" {
			return nil, errors.New("FLEET_URL is required when FLEET_ENROLL is true without FLEET_SERVER_ENABLE")
		}
		args = append(args, "--url", cfg.Fleet.URL)
		if cfg.Fleet.Insecure {
			args = append(args, "--insecure")
		}
		for k, v := range cfg.Fleet.Headers {
			args = append(args, "--header", k+"="+v)
		}
	}
	if cfg.Fleet.CA != "" {
		args = append(args, "--certificate-authorities", cfg.Fleet.CA)
	}
	if token != "" {
		args = append(args, "--enrollment-token", token)
	}
	if cfg.Fleet.ID != "" {
		args = append(args, "--id", cfg.Fleet.ID)
	}
	if cfg.Fleet.ReplaceToken != "" {
		args = append(args, "--replace-token", cfg.Fleet.ReplaceToken)
	}
	if cfg.Fleet.DaemonTimeout != 0 {
		args = append(args, "--daemon-timeout")
		args = append(args, cfg.Fleet.DaemonTimeout.String())
	}
	if cfg.Fleet.EnrollTimeout != 0 {
		args = append(args, "--enroll-timeout")
		args = append(args, cfg.Fleet.EnrollTimeout.String())
	}
	if cfg.Fleet.Cert != "" {
		args = append(args, "--elastic-agent-cert", cfg.Fleet.Cert)
	}
	if cfg.Fleet.CertKey != "" {
		args = append(args, "--elastic-agent-cert-key", cfg.Fleet.CertKey)
	}
	return args, nil
}

func buildFleetServerConnStr(cfg fleetServerConfig) (string, error) {
	u, err := url.Parse(cfg.Elasticsearch.Host)
	if err != nil {
		return "", err
	}
	path := ""
	if u.Path != "" {
		path += "/" + strings.TrimLeft(u.Path, "/")
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, path), nil
}

func kibanaFetchPolicy(cfg setupConfig, client *kibana.Client, streams *cli.IOStreams) (*kibanaPolicy, error) {
	var policies kibanaPolicies
	err := performGET(cfg, client, "/api/fleet/agent_policies", &policies, streams.Err, "Kibana fetch policy")
	if err != nil {
		return nil, err
	}
	return findPolicy(cfg, policies.Items)
}

func kibanaFetchToken(cfg setupConfig, client *kibana.Client, policy *kibanaPolicy, streams *cli.IOStreams, tokenName string) (string, error) {
	var keys kibanaAPIKeys
	err := performGET(cfg, client, "/api/fleet/enrollment_api_keys", &keys, streams.Err, "Kibana fetch token")
	if err != nil {
		return "", err
	}
	key, err := findKey(keys.Items, policy, tokenName)
	if err != nil {
		return "", err
	}
	var keyDetail kibanaAPIKeyDetail
	err = performGET(cfg, client, fmt.Sprintf("/api/fleet/enrollment_api_keys/%s", key.ID), &keyDetail, streams.Err, "Kibana fetch token detail")
	if err != nil {
		return "", err
	}
	return keyDetail.Item.APIKey, nil
}

func kibanaClient(cfg kibanaConfig, headers map[string]string) (*kibana.Client, error) {
	var tls *tlscommon.Config
	if cfg.Fleet.CA != "" {
		tls = &tlscommon.Config{
			CAs: []string{cfg.Fleet.CA},
		}
	}

	transport := httpcommon.DefaultHTTPTransportSettings()
	transport.TLS = tls

	return kibana.NewClientWithConfigDefault(&kibana.ClientConfig{
		Host:          cfg.Fleet.Host,
		Username:      cfg.Fleet.Username,
		Password:      cfg.Fleet.Password,
		ServiceToken:  cfg.Fleet.ServiceToken,
		IgnoreVersion: true,
		Transport:     transport,
		Headers:       headers,
	}, 0, "Elastic-Agent", version.GetDefaultVersion(), version.Commit(), version.BuildTime().String())
}

func findPolicy(cfg setupConfig, policies []kibanaPolicy) (*kibanaPolicy, error) {
	policyID := ""
	policyName := cfg.Fleet.TokenPolicyName
	if cfg.FleetServer.Enable {
		policyID = cfg.FleetServer.PolicyID
	}
	for _, policy := range policies {
		if policyID != "" {
			if policyID == policy.ID {
				return &policy, nil
			}
		} else if policyName != "" {
			if policyName == policy.Name {
				return &policy, nil
			}
		} else if cfg.FleetServer.Enable {
			if policy.IsDefaultFleetServer {
				return &policy, nil
			}
		} else {
			if policy.IsDefault {
				return &policy, nil
			}
		}
	}
	return nil, fmt.Errorf(`unable to find policy named "%s"`, policyName)
}

func findKey(keys []kibanaAPIKey, policy *kibanaPolicy, tokenName string) (*kibanaAPIKey, error) {
	for _, key := range keys {
		name := strings.TrimSpace(tokenNameStrip.ReplaceAllString(key.Name, ""))
		if name == tokenName && key.PolicyID == policy.ID {
			return &key, nil
		}
	}
	return nil, fmt.Errorf(`unable to find enrollment token named "%s" in policy "%s"`, tokenName, policy.Name)
}

func envWithDefault(def string, keys ...string) string {
	for _, key := range keys {
		val, ok := os.LookupEnv(key)
		if ok {
			return val
		}
	}
	return def
}

func envBool(keys ...string) bool {
	for _, key := range keys {
		val, ok := os.LookupEnv(key)
		if ok && isTrue(val) {
			return true
		}
	}
	return false
}

func envTimeout(keys ...string) time.Duration {
	for _, key := range keys {
		val, ok := os.LookupEnv(key)
		if ok {
			dur, err := time.ParseDuration(val)
			if err == nil {
				return dur
			}
		}
	}
	return 0
}

func envMap(key string) map[string]string {
	m := make(map[string]string)
	prefix := key + "="
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, prefix) {
			continue
		}

		envVal := strings.TrimPrefix(env, prefix)

		keyValue := strings.SplitN(envVal, "=", 2)
		if len(keyValue) != 2 {
			continue
		}

		m[keyValue[0]] = keyValue[1]
	}

	return m
}

func isTrue(val string) bool {
	trueVals := []string{"1", "true", "yes", "y"}
	val = strings.ToLower(val)
	for _, v := range trueVals {
		if val == v {
			return true
		}
	}
	return false
}

func performGET(cfg setupConfig, client *kibana.Client, path string, response interface{}, writer io.Writer, msg string) error {
	var lastErr error
	for i := 0; i < cfg.Kibana.RetryMaxCount; i++ {
		code, result, err := client.Request("GET", path, nil, nil, nil)
		if err != nil || code != 200 {
			if err != nil {
				err = fmt.Errorf("http GET request to %s%s fails: %w. Response: %s",
					client.URL, path, err, truncateString(result))
			} else {
				err = fmt.Errorf("http GET request to %s%s fails. StatusCode: %d Response: %s",
					client.URL, path, code, truncateString(result))
			}
			fmt.Fprintf(writer, "%s failed: %s\n", msg, err)
			<-time.After(cfg.Kibana.RetrySleepDuration)
			continue
		}
		if response == nil {
			return nil
		}
		return json.Unmarshal(result, response)
	}
	return lastErr
}

func truncateString(b []byte) string {
	const maxLength = 250
	runes := bytes.Runes(b)
	if len(runes) > maxLength {
		runes = append(runes[:maxLength], []rune("... (truncated)")...)
	}

	return strings.ReplaceAll(string(runes), "\n", " ")
}

// runLegacyAPMServer extracts the bundled apm-server from elastic-agent
// to path and runs it with args.
func runLegacyAPMServer(streams *cli.IOStreams) (*process.Info, error) {
	name := "apm"
	logInfo(streams, "Preparing apm-server for legacy mode.")

	platform, err := component.LoadPlatformDetail(isContainer)
	if err != nil {
		return nil, fmt.Errorf("failed to gather system information: %w", err)
	}

	specs, err := component.LoadRuntimeSpecs(paths.Components(), platform)
	if err != nil {
		return nil, fmt.Errorf("failed to detect inputs and outputs: %w", err)
	}

	spec, err := specs.GetInput(name)
	if err != nil {
		return nil, fmt.Errorf("failed to detect apm-server input: %w", err)
	}

	// add APM Server specific configuration
	var args []string
	addEnv := func(arg, env string) {
		if v := os.Getenv(env); v != "" {
			args = append(args, arg, v)
		}
	}
	addSettingEnv := func(arg, env string) {
		if v := os.Getenv(env); v != "" {
			args = append(args, "-E", fmt.Sprintf("%v=%v", arg, v))
		}
	}

	addEnv("--path.home", "HOME_PATH")
	addEnv("--path.config", "CONFIG_PATH")
	addEnv("--path.data", "DATA_PATH")
	addEnv("--path.logs", "LOGS_PATH")
	addEnv("--httpprof", "HTTPPROF")
	addSettingEnv("gc_percent", "APMSERVER_GOGC")
	logInfo(streams, "Starting legacy apm-server daemon as a subprocess."+spec.BinaryPath)
	options := []process.StartOption{process.WithArgs(args)}
	wdir := filepath.Dir(spec.BinaryPath)
	if wdir != "." {
		options = append(options, process.WithCmdOptions(func(c *exec.Cmd) error {
			c.Dir = wdir
			return nil
		}))
	}
	return process.Start(spec.BinaryPath, options...)
}

func containerCfgOverrides(cfg *configuration.Configuration) {
	logsPath := envWithDefault("", "LOGS_PATH")
	if logsPath == "" {
		// when no LOGS_PATH defined the container should log to stderr
		cfg.Settings.LoggingConfig.ToStderr = true
		cfg.Settings.LoggingConfig.ToFiles = false
	}

	eventsToStderrEnv := envWithDefault("false", "EVENTS_TO_STDERR")
	eventsToStderr, err := strconv.ParseBool(eventsToStderrEnv)
	if err != nil {
		logp.Warn("cannot parse EVENS_TO_STDERR='%s' as boolean, logging events to file'", eventsToStderrEnv)
	}
	if eventsToStderr {
		cfg.Settings.EventLoggingConfig.ToFiles = false
		cfg.Settings.EventLoggingConfig.ToStderr = true
	}

	configuration.OverrideDefaultContainerGRPCPort(cfg.Settings.GRPC)
}

func setPaths(statePath, configPath, logsPath, socketPath string, writePaths bool) error {
	statePath = envWithDefault(statePath, "STATE_PATH")
	if statePath == "" {
		statePath = defaultStateDirectory
	}

	topPath := filepath.Join(statePath, "data")
	configPath = envWithDefault(configPath, "CONFIG_PATH")
	if configPath == "" {
		configPath = statePath
	}
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(configPath, 0755); err != nil {
			return fmt.Errorf("cannot create folders for config path '%s': %w", configPath, err)
		}
	}

	if socketPath == "" {
		socketPath = utils.SocketURLWithFallback(statePath, topPath)
	}
	// ensure that the directory and sub-directory data exists
	if err := os.MkdirAll(topPath, 0755); err != nil {
		return fmt.Errorf("preparing STATE_PATH(%s) failed: %w", statePath, err)
	}
	// ensure that the elastic-agent.yml exists in the state directory or if given in the config directory
	baseConfig := filepath.Join(configPath, paths.DefaultConfigName)
	if _, err := os.Stat(baseConfig); os.IsNotExist(err) {
		if err := copyFile(baseConfig, paths.ConfigFile(), 0); err != nil {
			return err
		}
	}

	originalInstall := paths.Install()
	paths.SetTop(topPath)
	paths.SetConfig(configPath)
	paths.SetControlSocket(socketPath)
	// when custom top path is provided the home directory is not versioned
	paths.SetVersionHome(false)
	// install path stays on container default mount (otherwise a bind mounted directory could have noexec set)
	paths.SetInstall(originalInstall)
	// set LOGS_PATH is given
	logsPath = envWithDefault(logsPath, "LOGS_PATH")
	if logsPath != "" {
		paths.SetLogs(logsPath)
		// ensure that the logs directory exists
		if err := os.MkdirAll(filepath.Join(logsPath), logsPathPerms); err != nil {
			return fmt.Errorf("preparing LOGS_PATH(%s) failed: %w", logsPath, err)
		}
	}

	// ensure that the internal logger directory exists
	loggerPath := filepath.Join(paths.Home(), logger.DefaultLogDirectory)
	if err := os.MkdirAll(loggerPath, logsPathPerms); err != nil {
		return fmt.Errorf("preparing internal log path(%s) failed: %w", loggerPath, err)
	}

	// persist the paths so other commands in the container will use the correct paths
	if writePaths {
		if err := writeContainerPaths(statePath, configPath, logsPath, socketPath); err != nil {
			return err
		}
	}
	return nil
}

type containerPaths struct {
	StatePath  string `config:"state_path" yaml:"state_path"`
	ConfigPath string `config:"config_path" yaml:"config_path,omitempty"`
	LogsPath   string `config:"logs_path" yaml:"logs_path,omitempty"`
	SocketPath string `config:"socket_path" yaml:"socket_path,omitempty"`
}

func writeContainerPaths(statePath, configPath, logsPath, socketPath string) error {
	pathFile := filepath.Join(statePath, "container-paths.yml")
	fp, err := os.Create(pathFile)
	if err != nil {
		return fmt.Errorf("failed creating %s: %w", pathFile, err)
	}
	b, err := yaml.Marshal(containerPaths{
		StatePath:  statePath,
		ConfigPath: configPath,
		LogsPath:   logsPath,
		SocketPath: socketPath,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal for %s: %w", pathFile, err)
	}
	_, err = fp.Write(b)
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", pathFile, err)
	}
	return nil
}

func tryContainerLoadPaths() error {
	statePath := envWithDefault("", "STATE_PATH")
	if statePath == "" {
		statePath = defaultStateDirectory
	}
	pathFile := filepath.Join(statePath, "container-paths.yml")
	_, err := os.Stat(pathFile)
	if os.IsNotExist(err) {
		// no container-paths.yml file exists, so nothing to do
		return nil
	}
	cfg, err := config.LoadFile(pathFile)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", pathFile, err)
	}
	var paths containerPaths
	err = cfg.UnpackTo(&paths)
	if err != nil {
		return fmt.Errorf("failed to unpack %s: %w", pathFile, err)
	}
	return setPaths(paths.StatePath, paths.ConfigPath, paths.LogsPath, paths.SocketPath, false)
}

func copyFile(destPath string, srcPath string, mode os.FileMode) error {
	// if mode is unset; set to the same as the source file
	if mode == 0 {
		info, err := os.Stat(srcPath)
		if err == nil {
			// ignoring error because; os.Open will also error if the file cannot be stat'd
			mode = info.Mode()
		}
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer dest.Close()
	_, err = io.Copy(dest, src)
	return err
}

type kibanaPolicy struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Status               string `json:"status"`
	IsDefault            bool   `json:"is_default"`
	IsDefaultFleetServer bool   `json:"is_default_fleet_server"`
}

type kibanaPolicies struct {
	Items []kibanaPolicy `json:"items"`
}

type kibanaAPIKey struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Active   bool   `json:"active"`
	PolicyID string `json:"policy_id"`
	APIKey   string `json:"api_key"`
}

type kibanaAPIKeys struct {
	Items []kibanaAPIKey `json:"items"`
}

type kibanaAPIKeyDetail struct {
	Item kibanaAPIKey `json:"item"`
}

func envDurationWithDefault(defVal string, keys ...string) (time.Duration, error) {
	valStr := defVal
	for _, key := range keys {
		val, ok := os.LookupEnv(key)
		if ok {
			valStr = val
			break
		}
	}

	return time.ParseDuration(valStr)
}

func envIntWithDefault(defVal string, keys ...string) (int, error) {
	valStr := defVal
	for _, key := range keys {
		val, ok := os.LookupEnv(key)
		if ok {
			valStr = val
			break
		}
	}

	return strconv.Atoi(valStr)
}

// isContainer changes the platform details to be a container.
//
// Runtime specifications can provide unique configurations when running in a container, this ensures that
// those configurations are used versus the standard Linux configurations.
func isContainer(detail component.PlatformDetail) component.PlatformDetail {
	detail.OS = component.Container
	return detail
}

var (
	newFleetClient = func(log *logger.Logger, apiKey string, cfg remote.Config) (fleetclient.Sender, error) {
		return fleetclient.NewAuthWithConfig(log, apiKey, cfg)
	}
	newEncryptedDiskStore = storage.NewEncryptedDiskStore
	statAgentConfigFile   = os.Stat
)

// agentInfo implements the AgentInfo interface, and it used in shouldFleetEnroll.
type agentInfo struct {
	id string
}

func (a *agentInfo) AgentID() string {
	return a.id
}

// shouldFleetEnroll returns true if the elastic-agent should enroll to fleet.
func shouldFleetEnroll(setupCfg setupConfig) (bool, error) {
	if !setupCfg.Fleet.Enroll {
		// Enrollment is explicitly disabled in the setup configuration.
		return false, nil
	}

	if setupCfg.Fleet.Force {
		// Enrollment is explicitly enforced by the setup configuration.
		return true, nil
	}

	agentCfgFilePath := paths.AgentConfigFile()
	_, err := statAgentConfigFile(agentCfgFilePath)
	if os.IsNotExist(err) {
		// The agent configuration file does not exist, so enrollment is required.
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := newEncryptedDiskStore(ctx, agentCfgFilePath)
	if err != nil {
		return false, fmt.Errorf("failed to instantiate encrypted disk store: %w", err)
	}

	reader, err := store.Load()
	if err != nil {
		return false, fmt.Errorf("failed to load from disk store: %w", err)
	}

	cfg, err := config.NewConfigFrom(reader)
	if err != nil {
		return false, fmt.Errorf("failed to read from disk store: %w", err)
	}

	storedConfig, err := configuration.NewFromConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("failed to read from disk store: %w", err)
	}

	// Check if enrolling with a specifically defined Elastic Agent ID.
	// If the ID's don't match then it needs to enroll.
	if setupCfg.Fleet.ID != "" && (storedConfig.Fleet.Info == nil || storedConfig.Fleet.Info.ID != setupCfg.Fleet.ID) {
		// ID is a mismatch
		return true, nil
	}

	storedFleetHosts := storedConfig.Fleet.Client.GetHosts()
	if len(storedFleetHosts) == 0 || !slices.Contains(storedFleetHosts, setupCfg.Fleet.URL) {
		// The Fleet URL in the setup does not exist in the stored configuration, so enrollment is required.
		return true, nil
	}

	// Evaluate the stored enrollment token hash against the setup enrollment token if both are present.
	// Note that when "upgrading" from an older agent version the enrollment token hash will not exist
	// in the stored configuration.
	if len(storedConfig.Fleet.EnrollmentTokenHash) > 0 && len(setupCfg.Fleet.EnrollmentToken) > 0 {
		enrollmentHashBytes, err := base64.StdEncoding.DecodeString(storedConfig.Fleet.EnrollmentTokenHash)
		if err != nil {
			return false, fmt.Errorf("failed to decode enrollment token hash: %w", err)
		}

		err = crypto.ComparePBKDF2HashAndPassword(enrollmentHashBytes, []byte(setupCfg.Fleet.EnrollmentToken))
		switch {
		case errors.Is(err, crypto.ErrMismatchedHashAndPassword):
			// The stored enrollment token hash does not match the new token, so enrollment is required.
			return true, nil
		case err != nil:
			return false, fmt.Errorf("failed to compare enrollment token hash: %w", err)
		}
	}

	// Evaluate the stored replace token hash against the setup replace token if both are present.
	// Note that when "upgrading" from an older agent version the replace token hash will not exist
	// in the stored configuration.
	if len(storedConfig.Fleet.ReplaceTokenHash) > 0 && len(setupCfg.Fleet.ReplaceToken) > 0 {
		replaceHashBytes, err := base64.StdEncoding.DecodeString(storedConfig.Fleet.ReplaceTokenHash)
		if err != nil {
			return false, fmt.Errorf("failed to decode replace token hash: %w", err)
		}

		err = crypto.ComparePBKDF2HashAndPassword(replaceHashBytes, []byte(setupCfg.Fleet.ReplaceToken))
		switch {
		case errors.Is(err, crypto.ErrMismatchedHashAndPassword):
			// The stored enrollment token hash does not match the new token, so enrollment is required.
			return true, nil
		case err != nil:
			return false, fmt.Errorf("failed to compare replace token hash: %w", err)
		}
	}

	// Validate the stored API token to check if the agent is still authorized with Fleet.
	log, err := logger.New("fleet_client", false)
	if err != nil {
		return false, fmt.Errorf("failed to create logger: %w", err)
	}
	fc, err := newFleetClient(log, storedConfig.Fleet.AccessAPIKey, storedConfig.Fleet.Client)
	if err != nil {
		return false, fmt.Errorf("failed to create fleet client: %w", err)
	}

	// Perform an ACK request with **empty events** to verify the validity of the API token.
	// If the agent has been manually un-enrolled through the Kibana UI, the ACK request will fail due to an invalid API token.
	// In such cases, the agent should automatically re-enroll and "recover" their enrollment status without manual intervention,
	// maintaining seamless operation.
	err = ackFleet(ctx, fc, storedConfig.Fleet.Info.ID)
	switch {
	case errors.Is(err, fleetclient.ErrInvalidAPIKey):
		// The API key is invalid, so enrollment is required.
		return true, nil
	case err != nil:
		return false, fmt.Errorf("failed to validate api token: %w", err)
	}

	saveConfig := false

	// Update the stored enrollment token hash if there is no previous enrollment token hash
	// (can happen when "upgrading" from an older version of the agent) and setup enrollment token is present.
	if len(storedConfig.Fleet.EnrollmentTokenHash) == 0 && len(setupCfg.Fleet.EnrollmentToken) > 0 {
		enrollmentHashBytes, err := crypto.GeneratePBKDF2FromPassword([]byte(setupCfg.Fleet.EnrollmentToken))
		if err != nil {
			return false, errors.New("failed to generate enrollment token hash")
		}
		enrollmentTokenHash := base64.StdEncoding.EncodeToString(enrollmentHashBytes)
		storedConfig.Fleet.EnrollmentTokenHash = enrollmentTokenHash
		saveConfig = true
	}

	// Update the stored replace token hash if there is no previous replace token hash
	// (can happen when "upgrading" from an older version of the agent) and setup replace token is present.
	if len(storedConfig.Fleet.ReplaceTokenHash) == 0 && len(setupCfg.Fleet.ReplaceToken) > 0 {
		replaceHashBytes, err := crypto.GeneratePBKDF2FromPassword([]byte(setupCfg.Fleet.ReplaceToken))
		if err != nil {
			return false, errors.New("failed to generate replace token hash")
		}
		replaceTokenHash := base64.StdEncoding.EncodeToString(replaceHashBytes)
		storedConfig.Fleet.ReplaceTokenHash = replaceTokenHash
		saveConfig = true
	}

	if saveConfig {
		data, err := yaml.Marshal(storedConfig)
		if err != nil {
			return false, errors.New("could not marshal config")
		}

		if err := enroll.SafelyStoreAgentInfo(store, bytes.NewReader(data)); err != nil {
			return false, fmt.Errorf("failed to store agent config: %w", err)
		}
	}

	return false, nil
}

// ackFleet performs an ACK request to the fleet server with **empty events**.
func ackFleet(ctx context.Context, client fleetclient.Sender, agentID string) error {
	const retryInterval = time.Second
	const maxRetries = 3
	ackRequest := &fleetapi.AckRequest{Events: nil}
	ackCMD := fleetapi.NewAckCmd(&agentInfo{agentID}, client)
	retries := 0
	return backoff.Retry(func() error {
		retries++
		_, err := ackCMD.Execute(ctx, ackRequest)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, fleetclient.ErrInvalidAPIKey) || retries == maxRetries:
			return backoff.Permanent(err)
		default:
			return err
		}
	}, &backoff.ConstantBackOff{Interval: retryInterval})
}
