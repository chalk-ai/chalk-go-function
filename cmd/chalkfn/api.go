package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	customImageService  = "chalk.sandbox.v1.CustomImageService"
	scalingGroupService = "chalk.scalinggroup.v1.ScalingGroupManagerService"
	catalogService      = "chalk.externalfunctioncatalog.v1.ExternalFunctionCatalogService"
)

// credentials identify a Chalk client and the environment it acts in.
type credentials struct {
	APIServer     string
	ClientID      string
	ClientSecret  string
	EnvironmentID string
}

// loadCredentials reads CHALK_API_SERVER, CHALK_CLIENT_ID,
// CHALK_CLIENT_SECRET and CHALK_ENVIRONMENT_ID, filling anything unset from
// the token `chalk login` stored in ~/.chalk.yml for the working directory
// (or its nearest ancestor with a token), falling back to the default token.
func loadCredentials() (credentials, error) {
	c := credentials{
		APIServer:     os.Getenv("CHALK_API_SERVER"),
		ClientID:      os.Getenv("CHALK_CLIENT_ID"),
		ClientSecret:  os.Getenv("CHALK_CLIENT_SECRET"),
		EnvironmentID: os.Getenv("CHALK_ENVIRONMENT_ID"),
	}
	if c.APIServer == "" || c.ClientID == "" || c.ClientSecret == "" || c.EnvironmentID == "" {
		if tok, ok := chalkYAMLToken(); ok {
			c.APIServer = firstNonEmpty(c.APIServer, tok.APIServer)
			c.ClientID = firstNonEmpty(c.ClientID, tok.ClientID)
			c.ClientSecret = firstNonEmpty(c.ClientSecret, tok.ClientSecret)
			c.EnvironmentID = firstNonEmpty(c.EnvironmentID, tok.ActiveEnvironment)
		}
	}
	if c.APIServer == "" {
		c.APIServer = "https://api.chalk.ai"
	}
	c.APIServer = strings.TrimRight(c.APIServer, "/")
	if c.ClientID == "" || c.ClientSecret == "" {
		return c, fmt.Errorf("no Chalk credentials: set CHALK_CLIENT_ID and CHALK_CLIENT_SECRET, or run `chalk login`")
	}
	if c.EnvironmentID == "" {
		return c, fmt.Errorf("no Chalk environment: set CHALK_ENVIRONMENT_ID")
	}
	return c, nil
}

type chalkYAMLTokenEntry struct {
	ClientID          string `yaml:"clientId"`
	ClientSecret      string `yaml:"clientSecret"`
	APIServer         string `yaml:"apiServer"`
	ActiveEnvironment string `yaml:"activeEnvironment"`
}

func chalkYAMLToken() (chalkYAMLTokenEntry, bool) {
	path := os.Getenv("CHALK_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return chalkYAMLTokenEntry{}, false
		}
		path = filepath.Join(home, ".chalk.yml")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return chalkYAMLTokenEntry{}, false
	}
	var cfg struct {
		Tokens map[string]chalkYAMLTokenEntry `yaml:"tokens"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return chalkYAMLTokenEntry{}, false
	}
	if dir, err := os.Getwd(); err == nil {
		for {
			if tok, ok := cfg.Tokens[dir]; ok {
				return tok, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	tok, ok := cfg.Tokens["default"]
	return tok, ok
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiClient calls Chalk API services over the Connect protocol with JSON
// bodies, so the CLI needs no generated stubs for them.
type apiClient struct {
	http  *http.Client
	creds credentials
	token string
}

func newAPIClient() (*apiClient, error) {
	creds, err := loadCredentials()
	if err != nil {
		return nil, err
	}
	c := &apiClient{http: &http.Client{}, creds: creds}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = c.post(creds.APIServer+"/v1/oauth/token", map[string]any{
		"client_id":     creds.ClientID,
		"client_secret": creds.ClientSecret,
		"grant_type":    "client_credentials",
	}, &tok, 30*time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("exchanging client credentials at %s: %w", creds.APIServer, err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("oauth response from %s has no access_token", creds.APIServer)
	}
	c.token = tok.AccessToken
	return c, nil
}

// rpc invokes service/method with a JSON request and decodes the JSON response into out.
func (c *apiClient) rpc(service, method string, req, out any, timeout time.Duration) error {
	if err := c.post(fmt.Sprintf("%s/%s/%s", c.creds.APIServer, service, method), req, out, timeout, true); err != nil {
		return fmt.Errorf("%s/%s: %w", service, method, err)
	}
	return nil
}

func (c *apiClient) post(url string, req, out any, timeout time.Duration, authed bool) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Chalk-Server", "go-api")
	httpReq.Header.Set("User-Agent", "chalkfn")
	if authed {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
		httpReq.Header.Set("X-Chalk-Env-Id", c.creds.EnvironmentID)
	}
	client := *c.http
	client.Timeout = timeout
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &httpError{Status: resp.StatusCode, Message: connectErrorMessage(respBody)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decoding response: %w: %s", err, respBody)
	}
	return nil
}

type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func connectErrorMessage(body []byte) string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body))
}

type imageBuild struct {
	Status  string `json:"status"`
	Image   string `json:"image"`
	BuildID string `json:"buildId"`
	Error   string `json:"error"`
}

// getOrBuildImage returns the URI of the image built from spec, waiting for a
// build if the content-addressed cache misses. cached reports whether the
// image was available without waiting.
func (c *apiClient) getOrBuildImage(spec map[string]any, timeout time.Duration) (string, bool, error) {
	var b imageBuild
	if err := c.rpc(customImageService, "GetOrBuildCustomImage", map[string]any{"imageSpec": spec}, &b, 60*time.Second); err != nil {
		return "", false, err
	}
	switch b.Status {
	case "exists", "succeeded":
		return b.Image, true, nil
	case "failed":
		return "", false, fmt.Errorf("image build failed: %s", b.Error)
	}
	if b.BuildID == "" {
		return "", false, fmt.Errorf("image build has status %q and no build id", b.Status)
	}
	slog.Info("building image", "build_id", b.BuildID)
	deadline := time.Now().Add(timeout)
	failures := 0
	last := b.Status
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		var poll imageBuild
		if err := c.rpc(customImageService, "GetCustomImage", map[string]any{"buildId": b.BuildID}, &poll, 30*time.Second); err != nil {
			failures++
			if failures > 5 {
				return "", false, err
			}
			slog.Warn("polling image build", "error", err)
			continue
		}
		failures = 0
		if poll.Status != last {
			slog.Info("image build", "status", poll.Status)
			last = poll.Status
		}
		switch poll.Status {
		case "succeeded":
			return poll.Image, false, nil
		case "failed":
			return "", false, fmt.Errorf("image build %s failed: %s", b.BuildID, poll.Error)
		}
	}
	return "", false, fmt.Errorf("image build %s did not finish within %s", b.BuildID, timeout)
}

// waitScalingGroupReady polls until the named scaling group has a ready replica.
func (c *apiClient) waitScalingGroupReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		var resp struct {
			ScalingGroup struct {
				Status        string `json:"status"`
				StatusMessage string `json:"statusMessage"`
				ReadyReplicas int    `json:"readyReplicas"`
			} `json:"scalingGroup"`
		}
		if err := c.rpc(scalingGroupService, "GetScalingGroup", map[string]any{"name": name}, &resp, 30*time.Second); err != nil {
			return err
		}
		sg := resp.ScalingGroup
		if sg.Status != last {
			slog.Info("scaling group", "name", name, "status", sg.Status, "message", sg.StatusMessage)
			last = sg.Status
		}
		switch sg.Status {
		case "Running", "Available":
			if sg.ReadyReplicas > 0 {
				return nil
			}
		case "Failed", "Error":
			return fmt.Errorf("scaling group %s is %s: %s", name, sg.Status, sg.StatusMessage)
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("scaling group %s not ready after %s", name, timeout)
}

// waitCallable calls the function with an empty batch until the API server can
// route to it. A scaling group reports ready replicas before its route is
// programmed, and calls in that window fail with 503.
func (c *apiClient) waitCallable(name string, emptyBatch []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	req := map[string]any{
		"function":          map[string]any{"functionName": name},
		"remoteCallRequest": map[string]any{"name": name, "featherStream": emptyBatch},
	}
	for {
		err := c.rpc(catalogService, "CallExternalFunction", req, nil, time.Minute)
		var he *httpError
		if err == nil || !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("function %s is not callable after %s: %w", name, timeout, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// currentScalingGroup returns the scaling group serving the named function's
// current version, or "" if the function does not exist.
func (c *apiClient) currentScalingGroup(name string) (string, error) {
	var resp struct {
		ExternalFunction struct {
			CurrentVersion struct {
				ScalingGroupName string `json:"scalingGroupName"`
			} `json:"currentVersion"`
		} `json:"externalFunction"`
	}
	err := c.rpc(catalogService, "GetExternalFunction", map[string]any{"functionName": name}, &resp, 30*time.Second)
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return resp.ExternalFunction.CurrentVersion.ScalingGroupName, nil
}
