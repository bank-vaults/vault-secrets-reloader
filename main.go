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
	"flag"
	"net/http"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/sample-controller/pkg/signals"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/bank-vaults/vault-secrets-reloader/pkg/reloader"
)

const (
	defaultSyncPeriod        = 30 * time.Second
	defaultReloaderRunPeriod = 60 * time.Second
)

func main() {
	// Register CLI flags
	collectorSyncPeriod := flag.Duration("collector_sync_period", defaultSyncPeriod,
		"Determines the minimum frequency at which watched resources are reconciled")
	reloaderRunPeriod := flag.Duration("reloader_run_period", defaultReloaderRunPeriod,
		"Determines the minimum frequency at which watched resources are reloaded")
	logLevel := flag.String("log_level", "info", "Log level (debug, info, warn, error, fatal, panic).")
	flag.Parse()

	// Set up signals so we handle the shutdown signal gracefully
	ctx := signals.SetupSignalHandler()
	var logger *logrus.Entry
	{
		l := logrus.New()

		lvl, err := logrus.ParseLevel(*logLevel)
		if err != nil {
			lvl = logrus.InfoLevel
		}
		l.SetLevel(lvl)

		logger = l.WithField("app", "vault-secrets-reloader")
	}

	// Handler for health checks
	port := os.Getenv("LISTEN_ADDRESS")
	if port == "" {
		port = ":8080"
	}

	go func() {
		_ = http.ListenAndServe(port, http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			},
		))
	}()

	// Create kubernetes client
	kubeConfig, err := config.GetConfig()
	if err != nil {
		logger.Fatalf("error building kubeconfig: %s", err)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		logger.Fatalf("error building kubernetes clientset: %s", err)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, *collectorSyncPeriod)

	controller := reloader.NewController(
		logger,
		kubeClient,
		kubeInformerFactory.Apps().V1().Deployments(),
		kubeInformerFactory.Apps().V1().DaemonSets(),
		kubeInformerFactory.Apps().V1().StatefulSets(),
	)

	kubeInformerFactory.Start(ctx.Done())

	if err = controller.Run(ctx, *reloaderRunPeriod); err != nil {
		logger.Fatalf("error running controller: %s", err)
	}
}
