// Copyright © 2023 Cisco
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/bank-vaults/vault-secrets-reloader/pkg/operator"
	"k8s.io/client-go/dynamic"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	slogmulti "github.com/samber/slog-multi"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

func main() {
	// Register CLI flags
	options := operator.DefaultOptions()
	allowedVaults := flag.String("vault-address-allowlist", "", "Comma-separated exact Vault origins (namespaced mode)")
	allowedCAs := flag.String("vault-ca-secret-allowlist", "", "Approved namespace/name pairs for CA Secret reads")
	flag.DurationVar(&options.PollInterval, "vault-poll-interval", options.PollInterval, "Metadata polling interval")
	flag.DurationVar(&options.ReconcileInterval, "namespace-reconcile-interval", options.ReconcileInterval, "Periodic namespace reconciliation interval")
	flag.DurationVar(&options.AccessInterval, "access-check-interval", options.AccessInterval, "Namespace access check interval")
	flag.DurationVar(&options.RequestTimeout, "request-timeout", options.RequestTimeout, "API/Vault request timeout")
	flag.DurationVar(&options.SyncTimeout, "cache-sync-timeout", options.SyncTimeout, "Namespace cache synchronization timeout")
	flag.DurationVar(&options.RolloutTimeout, "rollout-timeout", options.RolloutTimeout, "Time before reporting a stalled rollout")
	flag.IntVar(&options.MaxNamespaces, "max-namespaces", options.MaxNamespaces, "Maximum active namespace runtimes")
	flag.IntVar(&options.MaxTargets, "max-targets", options.MaxTargets, "Maximum targets per namespace")
	flag.IntVar(&options.MaxPaths, "max-paths", options.MaxPaths, "Maximum paths per workload")
	flag.IntVar(&options.MaxEntries, "max-cache-entries", options.MaxEntries, "Maximum shared metadata cache entries")
	flag.IntVar(&options.MaxStateBytes, "max-state-bytes", options.MaxStateBytes, "Maximum serialized status patch size")
	flag.IntVar(&options.Workers, "poll-workers", options.Workers, "Global metadata worker limit")
	flag.IntVar(&options.PerVaultWorkers, "poll-workers-per-vault", options.PerVaultWorkers, "Per-Vault worker limit")
	flag.IntVar(&options.PerTenantWorkers, "poll-workers-per-namespace", options.PerTenantWorkers, "Per-namespace worker limit")
	flag.Float64Var(&options.VaultQPS, "vault-qps", options.VaultQPS, "Request starts per second per Vault origin")
	flag.StringVar(&options.JWTFile, "vault-jwt-file", options.JWTFile, "Projected controller JWT file for Vault login")
	flag.StringVar(&options.CAFile, "vault-ca-file", os.Getenv("VAULT_CACERT"), "Platform-provided Vault CA bundle file")
	flag.BoolVar(&options.AllowCASecrets, "allow-ca-secret-reads", false, "Explicitly enable approved local CA Secret reads")
	flag.BoolVar(&options.AllowHTTP, "allow-http-vault", false, "Platform opt-in for HTTP Vault origins (development only)")
	flag.BoolVar(&options.LeaderElection, "leader-elect", options.LeaderElection, "Enable namespaced-mode leader election")
	flag.StringVar(&options.LeaderNamespace, "leader-election-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the leader-election Lease")
	flag.StringVar(&options.LeaderName, "leader-election-name", options.LeaderName, "Leader-election Lease name")
	flag.StringVar(&options.Identity, "leader-election-identity", os.Getenv("POD_NAME"), "Unique identity for this process/pod")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error).")
	enableJSONLog := flag.Bool("enable-json-log", false, "Enable JSON logging")
	flag.Parse()

	// Set up signals so we handle the shutdown signal gracefully
	ctx := signals.SetupSignalHandler()

	// Setup logger
	var logger *slog.Logger
	{
		var level slog.Level

		err := level.UnmarshalText([]byte(*logLevel))
		if err != nil { // Silently fall back to info level
			level = slog.LevelInfo
		}

		levelFilter := func(levels ...slog.Level) func(ctx context.Context, r slog.Record) bool {
			return func(_ context.Context, r slog.Record) bool {
				return slices.Contains(levels, r.Level)
			}
		}

		router := slogmulti.Router()

		if *enableJSONLog {
			// Send logs with level higher than warning to stderr
			router = router.Add(
				slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
				levelFilter(slog.LevelWarn, slog.LevelError),
			)

			// Send info and debug logs to stdout
			router = router.Add(
				slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}),
				levelFilter(slog.LevelDebug, slog.LevelInfo),
			)
		} else {
			// Send logs with level higher than warning to stderr
			router = router.Add(
				slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
				levelFilter(slog.LevelWarn, slog.LevelError),
			)

			// Send info and debug logs to stdout
			router = router.Add(
				slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}),
				levelFilter(slog.LevelDebug, slog.LevelInfo),
			)
		}

		// TODO: add level filter handler
		logger = slog.New(router.Handler())
		logger = logger.With(slog.String("app", "vault-secrets-reloader"))

		slog.SetDefault(logger)
	}

	var namespaced atomic.Pointer[operator.Controller]
	// Handler for health checks
	port := os.Getenv("LISTEN_ADDRESS")
	if port == "" {
		port = ":8080"
	}

	go func() {
		_ = http.ListenAndServe(port, http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				current := namespaced.Load()
				if request.URL.Path == "/metrics" && current != nil {
					current.ServeMetrics(w)
					return
				}
				isReady := false
				if current != nil {
					isReady = current.Ready()
				}
				if request.URL.Path == "/readyz" && !isReady {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte("ok"))
			},
		))
	}()

	// Create kubernetes client
	kubeConfig, err := config.GetConfig()
	if err != nil {
		logger.Error(fmt.Errorf("error building kubeconfig: %s", err).Error())
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		logger.Error(fmt.Errorf("error building kubernetes clientset: %s", err).Error())
		os.Exit(1)
	}

	{
		options.Logger = logger
		options.AllowedAddresses = splitCSV(*allowedVaults)
		options.AllowedCASecrets = splitCSV(*allowedCAs)
		if v := os.Getenv("VAULT_ADDR"); v != "" {
			options.DefaultAddress = v
		}
		options.DefaultNamespace = os.Getenv("VAULT_NAMESPACE")
		if v := os.Getenv("VAULT_PATH"); v != "" {
			options.DefaultAuthMount = v
		}
		if v := os.Getenv("VAULT_AUTH_METHOD"); v != "" {
			options.DefaultAuthMethod = v
		}
		dyn, e := dynamic.NewForConfig(kubeConfig)
		if e != nil {
			logger.Error("cannot create custom-resource client")
			os.Exit(1)
		}
		controller, e := operator.NewController(options, kubeClient, dyn, nil)
		if e != nil {
			logger.Error(e.Error())
			os.Exit(1)
		}
		namespaced.Store(controller)
		if e = controller.Run(ctx); e != nil && ctx.Err() == nil {
			logger.Error("namespaced controller stopped", "error", e.Error())
			os.Exit(1)
		}
		return
	}
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
