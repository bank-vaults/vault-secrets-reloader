# Vault Secrets Reloader

Vault Secrets Reloader watches opted-in workloads in team namespaces and initiates rollouts when Vault KV v2 secret versions change. It reads secret metadata using its own Kubernetes ServiceAccount; application secret delivery remains the responsibility of Vault Secrets Webhook.

Teams configure their Vault role and add `secrets-reloader.security.bank-vaults.io/vault-role` to the Pod template. The operator uses its projected JWT with audience `vault` and requires a Vault policy granting `read` on the relevant KV v2 metadata paths. It does not request tokens for application ServiceAccounts. Only direct `vault:` environment references and `vault-from-path` are tracked; references inside `valueFrom`/`envFrom` resources are not discovered.

See the [installation, authentication and operational guide](docs/namespaced-operator.md) and [team opt-in example](examples/namespaced/team.yaml). The operator is experimental and requires validation in the target environment before production use.

Run local checks with `go test ./...` (Helm is required for chart tests).

The project is licensed under the [Apache 2.0 License](LICENSE).
