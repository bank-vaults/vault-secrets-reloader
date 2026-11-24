# Experimental namespaced operator

The operator watches opted-in namespaces and reads Vault KV v2 metadata using its own ServiceAccount. Its API group and status contract have not been approved as an upstream stable API.

## Installation model

The platform installs one operator and its ServiceAccount in an infrastructure namespace. Its only cluster-wide application permission is `list/watch` on `VaultReloader` custom resources. Each team creates `VaultReloader/default` and a RoleBinding in its namespace. The operator starts separate Deployment, StatefulSet and DaemonSet informers there and does not list/watch workloads across all namespaces. No Namespace or RoleBinding watcher is used.

The operator checks workload access with SelfSubjectAccessReview before starting a namespace and periodically thereafter. Kubernetes normally permits authenticated identities to check their own access; if a deployment disables that API, access checks fail closed and the operator reports the problem. No privileged SubjectAccessReview permission is installed. A missing RoleBinding is retried with bounded backoff. Failed namespaces do not stop other namespaces. Initial cache sync must finish for all three workload kinds before polling or patching.

Deleting or suspending the CR stops its runtime and cancels its requests. Deleting the CR does not revoke the RoleBinding. Removing the binding stops future authorized calls; an existing watch may remain open until the access check/reconnect observes revocation. A request already in flight may finish. Delete both CR and binding to remove opt-in and the underlying grant. Re-enabling access works without editing the CR. These lifecycles are tested locally with fake clients and HTTP protocol stubs, not a real API server yet.

For a platform installation, use your own exact trusted Vault origin and release names:

```sh
# Examples only: running these commands changes a cluster.
# Install the CRD separately, or opt in to Helm-managed CRD installation below.
kubectl --context <context> apply -f config/crd/bases/secrets-reloader.security.bank-vaults.io_vaultreloaders.yaml
helm upgrade --install reloader deploy/charts/vault-secrets-reloader \
  --kube-context <context> --namespace infra-ns --create-namespace \
  --set mode=namespaced \
  --set 'namespaced.vaultAddressAllowlist[0]=https://vault.example.internal:8200'
```

Alternatively set `namespaced.installCRD=true`. The CRD is then a regular Helm template, with `helm.sh/resource-policy: keep`. Helm's `crds/` mechanism is deliberately not used for conditional installation. Review CRD changes separately; it remains after uninstall. Reinstallation/adoption and schema upgrades must be planned by the platform, not assumed automatic.

The reusable namespace ClusterRole has the fixed name `vault-secrets-reloader-operator` independent of the release name. `helm get notes` prints that name and the actual operator ServiceAccount. Teams substitute them in [the opt-in example](../examples/namespaced/team.yaml). The chart installs no workload ClusterRoleBinding. Teams need permission to create their CR and bind the role (existing equivalent rights or a narrowly scoped `bind` grant); the operator does not create team bindings or grant itself access.

## Workload annotations

Enable a Deployment, StatefulSet or DaemonSet with the opt-in annotation on workload metadata or Pod template metadata. Vault connection settings are read from the Pod template, as with the webhook:

```yaml
spec:
  template:
    metadata:
      annotations:
        secrets-reloader.security.bank-vaults.io/reload-on-secret-change: "true"
        secrets-reloader.security.bank-vaults.io/vault-role: "reloader-metadata-role"
        secrets-webhook.security.bank-vaults.io/vault-role: "application-role"
        # Optional; shared with the Vault Secrets Webhook.
        secrets-webhook.security.bank-vaults.io/vault-skip-verify: "true"
```

The deprecated webhook prefix `vault.security.banzaicloud.io/` is also supported; if both are present, the current prefix takes precedence. TLS defaults to verified and its value must be `"true"` or `"false"`. Labels and the former reloader-specific TLS annotation are not used. When migrating existing workloads, move label-based opt-in to an annotation before upgrading.

TLS verification mode is part of the Vault authentication/cache context. Toggling it establishes a new RecordOnly baseline without a rollout; a version change during the transition may not trigger one.

## Logging

Set chart value `logLevel: info` (default) for namespace lifecycle, secret version changes, requested/completed rollouts and errors. `logLevel: debug` additionally records namespace reconciliation, access checks and individual Vault polling/authentication activity. Error status and logs include the operation (`login` or `metadata`) and HTTP status when available. Secret paths are represented by hashes; credentials, secret values and Vault response bodies are not logged. Debug logging can be verbose.

## Vault authentication

Vault addresses must match the platform's exact origin allowlist. HTTPS verification is enabled by default. The reloader reads the webhook's `vault-skip-verify` Pod-template annotation and uses the same setting for its own Vault connection. Disabling verification lets a network attacker impersonate an allowed Vault endpoint and receive the operator JWT and Vault token; prefer a trusted CA. The setting does not bypass the origin allowlist or install a CA in the application. Redirects are disabled. `namespaced.allowHTTPVault=true` is a deliberate platform exception for local development. A CA file supplied through platform-managed volumes can be selected with `namespaced.vaultCAFile` / `VAULT_CACERT`. Changing a CA file replaces the cached transport on the next request.

Direct `vault:...` references in container and init-container `env.value` and the `vault-from-path` annotation are tracked. `env.valueFrom` and `envFrom` are ignored: the operator does not read referenced Kubernetes Secrets or ConfigMaps, and Vault references contained in those resources are not tracked. A version-pinned reference does not follow `latest`. Unsupported indirect variables do not block tracking of direct references in the same workload.

Only Vault Kubernetes auth and explicit unversioned KV v2 references are supported. Workload settings use `secrets-webhook.security.bank-vaults.io/` annotations, with corresponding `vault.security.banzaicloud.io/` aliases. Defaults can come from `VAULT_ADDR`, `VAULT_NAMESPACE`, `VAULT_PATH` (auth mount) and `VAULT_AUTH_METHOD`. The operator's role is required in the Pod-template `secrets-reloader.security.bank-vaults.io/vault-role` annotation; a missing or blank value reports an error for that workload. The webhook's `vault-role` annotation and workload ServiceAccount are never used as a role fallback.

The chart projects a short-lived JWT for the operator's own Pod ServiceAccount with audience `vault` at `/var/run/secrets/vault/token`. The operator rereads that file and reauthenticates when the JWT changes. Each workload has one Vault, auth mount and operator role. Vault administrators must allow the operator ServiceAccount name/namespace and JWT audience in that role, with policies granting only required KV v2 metadata reads. The operator never requests tokens for application ServiceAccounts or reads their Pod tokens.

A platform CA file takes precedence over any workload `vault-tls-secret` annotation; no team Secret read is needed in that case. Without a platform CA file, optional CA Secret reads require `namespaced.allowCASecretReads=true`, exact `namespaced.caSecretAllowlist` entries and local `get` permission on the named Secret. No Secret list/watch is granted by the default chart. See the [optional CA RBAC example](../examples/namespaced/optional-auth-rbac.yaml).

The existing Vault reviewer token/TokenReview arrangement is unchanged. This operator creates no reviewer ServiceAccount, token Secret or `system:auth-delegator` binding. Vault's reviewer identity and the identity that logs in to Vault are distinct.

## Metadata-only polling and isolation

The poller uses ordinary periodic HTTP GET requests, with no Vault WebSocket, gRPC subscription or Enterprise dependency. A reference such as `vault:team/kv/data/payments/db#password` becomes `GET /v1/team/kv/metadata/payments/db`. Mounts can contain multiple path segments. References must contain one unambiguous `data` segment; ambiguous paths with multiple `data` segments, traversal, encoded separators and unsupported engines fail explicitly without guessing the mount or calling a privileged discovery API.

The poller never requests secret data. Its identity needs `read` on the **metadata** path; read permission on the data path is insufficient. For example:

```hcl
path "team/kv/metadata/payments/db" {
  capabilities = ["read"]
}
```

The application's own secret-reading identity still needs its data permissions. The operator's HTTP behaviour is metadata-only, even if a deployment gives its Vault role broader permissions; do not assume the implementation can narrow permissions already granted by Vault policy. There is no data-read fallback when metadata access fails.

Only the required version number, version creation time and key creation time are retained. Custom metadata, secret values, credentials and upstream error bodies are not logged or stored in CR status. Deleted/destroyed/missing/invalid versions produce a per-target error and preserve the last successful observation. An undelete of the same version resumes observation without forcing a rollout solely for the undelete. A recreated key with the same version number but a new creation identity is detected as changed. Metadata does not prove that the application received a value.

A process-wide cache and scheduler deduplicate by tenant, Vault origin/namespace, auth settings, operator role, TLS configuration, and path. Different namespaces or identities never share a cached observation even when the role/path matches. Within the same tenant and identity, workloads sharing a path use one in-flight request. Canceling one subscriber does not cancel a request still needed by another. Cache entries are removed when the last subscriber leaves; there is also a hard entry limit and stale observations cannot drive new actions.

Fair scheduling rotates between tenants, with global, per-origin and per-tenant concurrency limits, per-origin request-start rate limits, request deadlines, retry backoff and jitter. A `403` for team A does not block B–J at the same Vault. A shared Vault outage or a saturated global limit can affect all users of that service. Vault login sessions are independently locked by context, so a slow login for A does not hold a global authentication lock. Login tokens are renewed by reauthentication before expiry; an observed auth error invalidates the token. Vault policy revocation is observed on subsequent requests, not guaranteed instantaneously.

The default poll interval is one minute. Namespace reconciliation runs periodically at `namespaced.reconcileInterval` (default `1m`, for example `30s`) and on workload events. Vault polling is independent; its results are processed on the next namespace reconciliation, so detection and retries may be delayed by up to the reconcile interval, plus queueing and backoff. The interval must be a positive Go duration. Requests to the metadata API, login and optional Kubernetes credential reads have deadlines. Aggregate metrics are available at `/metrics`; labels do not contain paths, identities or workload names.

## Persistence and rollout

The namespace CR's status stores bounded state per workload UID. Paths and source settings are represented by hashes. Desired intent is persisted with conditional whole-status JSON Patch before any workload patch. Status conflicts or failed writes prevent the workload action. A workload patch tests both UID and resourceVersion and changes only the revision annotation. A timeout or a crash between patch and status persistence is recovered by rereading the marker; it does not blindly increment a counter.

RecordOnly adoption establishes a baseline without a rollout. A source configuration/identity change similarly establishes a new baseline. This avoids an automatic mass restart, but cannot prove the value used by an existing Pod. Actual observed version changes after adoption create a desired revision; read errors preserve previous versions. Independent consumers have independent pending state. Later complete version snapshots coalesce into the latest desired revision.

For Deployments, StatefulSets and DaemonSets, rollout observation uses generation and kind-specific replica/revision checks. Paused, OnDelete, partitioned and stalled workloads are not declared complete merely because the template was patched. A timeout reports a stalled state while observation continues. Kubernetes rollout completion and application secret delivery remain separate signals.

Defaults limit namespaces, cache entries, targets, paths and serialized status size. Exceeding the target/cache/status limit fails safely and does not patch workloads without persistent intent. This version uses one status object per namespace, not unlimited per-workload storage. Do not raise target/path counts above CRD schema limits. Status path hashes and revision metadata are visible through cluster-wide CR list/watch. No secret values or tokens belong in that API.

Configure GitOps to ignore only the operator's `secrets-reloader.security.bank-vaults.io/revision` Pod-template annotation. Removing or reverting that marker can request another rollout.

## Security limits

The namespaced Deployment uses `maxUnavailable: 1` and `maxSurge: 0`. Readiness requires leadership and synchronized discovery, so the old leader must terminate before its replacement can become Ready. The default surge-only strategy would deadlock a single-replica update. Allow for a short processing pause during upgrades; persisted rollout state is recovered after acquisition of the Lease.


ClusterRoleBinding is installed only for CR discovery. Optional leader-election permissions are local to the operator namespace. The shared operator nevertheless holds the union of permissions granted by all team RoleBindings. Compromising it can affect every connected tenant. RBAC `patch` is not field-level authorization and allows more than a rollout annotation; no ValidatingAdmissionPolicy enforcing that restriction is included.

## Local validation and remaining deployment checks

Local tests use client-go fake clients and HTTP test servers. They cover status/patch crash windows, unknown patch outcome, failed-consumer retry, read recovery, UID changes, opt-out, scoped informer requests, missing/restored/revoked access, suspend/shutdown, leader startup/cancellation, cache bounds, deduplication and ten-tenant fairness. Tests use the Kubernetes CRD validation/CEL libraries and render the Helm chart without contacting a cluster. These do not prove real API-server RBAC or admission semantics.

Before deployment, separately test actual authorization and denied cross-namespace access, RoleBinding creation by tenant GitOps, CRD installation/upgrade/admission, leader failover, API-server patch preconditions/status size, Vault auth and CA ACLs, dynamic informer scaling and Secrets Webhook end-to-end delivery. No supported-version or production-readiness claim follows from local tests alone.
