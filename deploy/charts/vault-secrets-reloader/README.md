# Vault Secrets Reloader

This chart installs the namespaced Vault Secrets Reloader. It observes Vault KV v2 metadata using the operator ServiceAccount and starts workload rollouts when versions change. Vault Secrets Webhook delivers the secret values.

Teams configure Vault Kubernetes auth to accept the operator ServiceAccount and its JWT audience `vault`, with policy granting only required metadata reads. Each workload uses one Vault, auth mount and reloader role. Add these Pod-template annotations:

```yaml
secrets-reloader.security.bank-vaults.io/reload-on-secret-change: "true"
secrets-reloader.security.bank-vaults.io/vault-role: "reloader-metadata-role"
```

The operator discovers direct `vault:` references in container and init-container `env.value` and `vault-from-path`. It ignores `valueFrom` and `envFrom`; Vault references inside Kubernetes Secrets or ConfigMaps are not discovered.

Set the trusted Vault origins in `namespaced.vaultAddressAllowlist` and install the CRD separately or set `namespaced.installCRD=true`. Teams opt in their namespace using a `VaultReloader` resource and RoleBinding. See the [operator guide](https://github.com/anisimov-es/vault-secrets-reloader/blob/feature/namespaced-operator/docs/namespaced-operator.md) for installation and security details.
