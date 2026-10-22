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

package reloader

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

func TestCheckVaultHealthConcurrentTokenRenewal(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false}`))
	}))
	defer server.Close()
	cfg := vaultapi.DefaultConfig()
	cfg.Address = server.URL
	client, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetNamespace("ns1")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				client.SetToken("review-test-token")
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 30; i++ {
		if err := checkVaultHealth(client); err != nil {
			t.Fatal(err)
		}
	}
}
