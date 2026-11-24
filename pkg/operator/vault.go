// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/time/rate"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	api "github.com/bank-vaults/vault-secrets-reloader/api/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PollError struct {
	Code       string
	RetryAfter time.Duration
	Stage      string
	HTTPStatus int
}

func (e *PollError) Error() string { return e.Code }

func pollStage(err error, stage string) error {
	var e *PollError
	if errors.As(err, &e) {
		copy := *e
		copy.Stage = stage
		return &copy
	}
	return &PollError{Code: "VaultPollFailed", Stage: stage}
}

type MetadataReader interface {
	Read(context.Context, Source, string) (api.Version, error)
	Close()
}
type vaultSession struct {
	mu           sync.Mutex
	client       *http.Client
	token        string
	expires      time.Time
	used         time.Time
	credentialID string
	jwtRevision  string
	retired      atomic.Bool
}
type VaultReader struct {
	options  Options
	kube     kubernetes.Interface
	mu       sync.Mutex
	sessions map[string]*vaultSession
	limiters map[string]*rate.Limiter
}

func NewVaultReader(o Options, k kubernetes.Interface) *VaultReader {
	return &VaultReader{options: o, kube: k, sessions: map[string]*vaultSession{}, limiters: map[string]*rate.Limiter{}}
}
func (v *VaultReader) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, s := range v.sessions {
		s.mu.Lock()
		if s.client != nil {
			s.client.CloseIdleConnections()
		}
		s.token = ""
		s.mu.Unlock()
	}
	v.sessions = map[string]*vaultSession{}
}
func (v *VaultReader) session(s Source) (*vaultSession, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	for key, item := range v.sessions {
		if !item.mu.TryLock() {
			continue
		}
		if now.Sub(item.used) > 5*v.options.PollInterval {
			if item.client != nil {
				item.client.CloseIdleConnections()
			}
			item.token = ""
			delete(v.sessions, key)
		}
		item.mu.Unlock()
	}
	key := s.ContextKey()
	item := v.sessions[key]
	if item == nil {
		if len(v.sessions) >= v.options.MaxEntries {
			return nil, &PollError{Code: "ClientLimitExceeded"}
		}
		item = &vaultSession{used: now}
		v.sessions[key] = item
	}
	return item, nil
}
func (v *VaultReader) Read(parent context.Context, s Source, path string) (api.Version, error) {
	var zero api.Version
	origin, originErr := canonicalAddress(s.Address, v.options.AllowHTTP)
	approved := false
	for _, entry := range v.options.AllowedAddresses {
		a, e := canonicalAddress(entry, v.options.AllowHTTP)
		if e == nil && a == origin {
			approved = true
		}
	}
	if originErr != nil || !approved {
		return zero, &PollError{Code: "VaultOriginNotApproved"}
	}
	metadata, err := metadataPath(path)
	if err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(parent, v.options.RequestTimeout)
	defer cancel()
	session, err := v.session(s)
	if err != nil {
		return zero, err
	}
	defer func() {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.retired.Load() {
			session.token = ""
			if session.client != nil {
				session.client.CloseIdleConnections()
			}
		}
	}()
	if session.retired.Load() {
		return zero, &PollError{Code: "ContextRetired"}
	}
	// Login/refresh is per identity, never a global mutex. Waiting requests remain bounded by poller limits.
	session.mu.Lock()
	client, token, err := v.prepare(ctx, s, session)
	session.mu.Unlock()
	if err != nil {
		var pe *PollError
		if errors.As(err, &pe) && pe.Stage == "" {
			return zero, pollStage(err, "prepare")
		}
		return zero, err
	}
	raw, err := v.request(ctx, client, s, "GET", metadata, nil, token)
	if err != nil {
		var pe *PollError
		if errors.As(err, &pe) && (pe.Code == "VaultUnauthorized" || pe.Code == "VaultAccessDenied") {
			session.mu.Lock()
			if session.token == token {
				session.token = ""
			}
			session.mu.Unlock()
		}
		return zero, pollStage(err, "metadata")
	}
	var response struct {
		Data struct {
			Current  int64  `json:"current_version"`
			Created  string `json:"created_time"`
			Versions map[string]struct {
				Created   string `json:"created_time"`
				Deleted   string `json:"deletion_time"`
				Destroyed bool   `json:"destroyed"`
			} `json:"versions"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data.Current <= 0 || response.Data.Created == "" {
		return zero, &PollError{Code: "InvalidMetadata"}
	}
	current, ok := response.Data.Versions[strconv.FormatInt(response.Data.Current, 10)]
	if !ok || current.Created == "" {
		return zero, &PollError{Code: "InvalidMetadata"}
	}
	if current.Destroyed {
		return zero, &PollError{Code: "VersionDestroyed"}
	}
	if current.Deleted != "" {
		t, e := time.Parse(time.RFC3339Nano, current.Deleted)
		if e != nil {
			return zero, &PollError{Code: "InvalidMetadata"}
		}
		if !t.After(time.Now()) {
			return zero, &PollError{Code: "VersionDeleted"}
		}
	}
	return api.Version{Version: response.Data.Current, Created: current.Created, KeyCreated: response.Data.Created}, nil
}
func (v *VaultReader) prepare(ctx context.Context, s Source, item *vaultSession) (*http.Client, string, error) {
	item.used = time.Now()
	// CA bytes are public trust material; Secret access still requires explicit opt-in and a local grant.
	var ca []byte
	var err error
	if !s.SkipTLSVerify && s.CAFile != "" {
		ca, err = os.ReadFile(s.CAFile)
		if err != nil {
			return nil, "", &PollError{Code: "CAFileUnavailable"}
		}
	}
	if !s.SkipTLSVerify && s.CASecret != "" {
		if !v.options.AllowCASecrets || !contains(v.options.AllowedCASecrets, s.Tenant+"/"+s.CASecret) {
			return nil, "", &PollError{Code: "CASecretNotApproved"}
		}
		secret, e := v.kube.CoreV1().Secrets(s.Tenant).Get(ctx, s.CASecret, meta.GetOptions{})
		if e != nil {
			return nil, "", &PollError{Code: "CASecretUnavailable"}
		}
		ca = secret.Data["ca.crt"]
		if len(ca) == 0 {
			return nil, "", &PollError{Code: "InvalidCA"}
		}
	}
	trust := digest(ca)
	if item.client == nil || item.credentialID != trust {
		if item.client != nil {
			item.client.CloseIdleConnections()
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if len(ca) > 0 && !roots.AppendCertsFromPEM(ca) {
			return nil, "", &PollError{Code: "InvalidCA"}
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// Explicit per-workload opt-in; the origin allowlist still applies.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, InsecureSkipVerify: s.SkipTLSVerify} //nolint:gosec // Requested per-workload TLS verification bypass.
		transport.MaxConnsPerHost = v.options.PerVaultWorkers
		transport.MaxIdleConnsPerHost = v.options.PerVaultWorkers
		item.client = &http.Client{Transport: transport, Timeout: v.options.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are disabled") }}
		item.credentialID = trust
		item.token = ""
	}
	b, e := os.ReadFile(v.options.JWTFile)
	if e != nil {
		item.token = ""
		return nil, "", &PollError{Code: "JWTUnavailable"}
	}
	jwt := strings.TrimSpace(string(b))
	if jwt == "" {
		item.token = ""
		return nil, "", &PollError{Code: "JWTUnavailable"}
	}
	id := digest(jwt)
	if item.jwtRevision != id {
		item.token = ""
		item.jwtRevision = id
	}
	if item.token != "" && time.Now().Before(item.expires) {
		return item.client, item.token, nil
	}

	raw, err := v.request(ctx, item.client, s, "POST", "auth/"+escapePath(s.AuthMount)+"/login", map[string]string{"role": s.Role, "jwt": jwt}, "")
	if err != nil {
		return nil, "", pollStage(err, "login")
	}
	var login struct {
		Auth struct {
			Token    string `json:"client_token"`
			Duration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &login) != nil || login.Auth.Token == "" || login.Auth.Duration <= 0 {
		return nil, "", &PollError{Code: "InvalidAuthResponse"}
	}
	item.token = login.Auth.Token
	ttl := time.Duration(login.Auth.Duration) * time.Second * 8 / 10
	if ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	item.expires = time.Now().Add(ttl)
	v.options.Logger.Debug("Vault authentication succeeded", "namespace", s.Tenant, "source_id", s.ContextKey()[:12])
	return item.client, item.token, nil
}
func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func vaultRequest(ctx context.Context, client *http.Client, s Source, method, path string, data interface{}, token string) ([]byte, error) {
	var body io.Reader
	if data != nil {
		b, e := json.Marshal(data)
		if e != nil {
			return nil, &PollError{Code: "InvalidRequest"}
		}
		body = bytes.NewReader(b)
	}
	request, e := http.NewRequestWithContext(ctx, method, s.Address+"/v1/"+path, body)
	if e != nil {
		return nil, &PollError{Code: "InvalidRequest"}
	}
	if token != "" {
		request.Header.Set("X-Vault-Token", token)
	}
	if s.VaultNamespace != "" {
		request.Header.Set("X-Vault-Namespace", s.VaultNamespace)
	}
	request.Header.Set("Content-Type", "application/json")
	response, e := client.Do(request)
	if e != nil {
		var authority x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var certificate x509.CertificateInvalidError
		switch {
		case errors.As(e, &authority):
			return nil, &PollError{Code: "VaultTLSUnknownAuthority"}
		case errors.As(e, &hostname):
			return nil, &PollError{Code: "VaultTLSHostnameMismatch"}
		case errors.As(e, &certificate):
			return nil, &PollError{Code: "VaultTLSCertificateInvalid"}
		case errors.Is(e, context.DeadlineExceeded):
			return nil, &PollError{Code: "VaultRequestTimeout"}
		}
		return nil, &PollError{Code: "VaultTransportError"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		// Do not expose upstream bodies: Vault/proxies may echo credentials or user-supplied data.
		code := fmt.Sprintf("VaultHTTP%d", response.StatusCode)
		switch response.StatusCode {
		case 401:
			code = "VaultUnauthorized"
		case 403:
			code = "VaultAccessDenied"
		case 404:
			code = "VaultPathMissing"
		case 429:
			code = "VaultRateLimited"
		}
		retry := time.Duration(0)
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			retry = time.Duration(seconds) * time.Second
		} else if t, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
			retry = time.Until(t)
		}
		return nil, &PollError{Code: code, RetryAfter: retry, HTTPStatus: response.StatusCode}
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if e != nil || len(b) > 2*1024*1024 {
		return nil, &PollError{Code: "MetadataResponseTooLarge"}
	}
	return b, nil
}

func (v *VaultReader) request(ctx context.Context, client *http.Client, s Source, method, path string, data interface{}, token string) ([]byte, error) {
	v.mu.Lock()
	limiter := v.limiters[s.Address]
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(v.options.VaultQPS), 1)
		v.limiters[s.Address] = limiter
	}
	v.mu.Unlock()
	if err := limiter.Wait(ctx); err != nil {
		return nil, &PollError{Code: "VaultRateBudgetExceeded"}
	}
	return vaultRequest(ctx, client, s, method, path, data, token)
}

// Forget retires an unused identity without waiting for another identity's requests.
func (v *VaultReader) Forget(key string) {
	v.mu.Lock()
	item := v.sessions[key]
	delete(v.sessions, key)
	v.mu.Unlock()
	if item == nil {
		return
	}
	item.retired.Store(true)
	if item.mu.TryLock() {
		item.token = ""
		if item.client != nil {
			item.client.CloseIdleConnections()
		}
		item.mu.Unlock()
	}
}
