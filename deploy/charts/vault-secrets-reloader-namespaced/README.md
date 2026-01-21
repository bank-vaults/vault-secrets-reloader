# vault-secrets-reloader-namespaced

This chart will install Vault Secrets Reloader Controller, that reloads workloads on a referenced secret change in HashiCorp Vault.

Reloader will collect (unversioned) secrets injected by the Webhook from watched workloads, periodically checks if their version has been updated in Vault and if so, "reloads" the workload with an annotation update, triggering a new rollout so the Webhook can inject the new version of the secret into the pod.

## Before you start

Reloader works in conjunction with the [Secrets Webhook](https://github.com/bank-vaults/secrets-webhook), therefore the prerequisites to start using it would be a Hashicorp Vault instance, and a working Webhook.

You will need to add the following annotations to the pod template spec of the workloads (i.e. Deployments, DaemonSets and StatefulSets) that you wish to reload:

```yaml
secrets-reloader.security.bank-vaults.io/reload-on-secret-change: "true"
```

## Installing the Chart

**Prepare Kubernetes namespace**

You can prepare a separate namespace for Vault Secrets Reloader beforehand, create it automatically if not yet exist with appending `--create-namespace` to the installation Helm command, or just use the one already created for Secrets Webhook.

**Install the chart**

1. Save Reloader default chart values:

```shell
helm show values oci://ghcr.io/davealmr/helm-charts/vault-secrets-reloader-namespaced > values.yaml
```

2. Check the configuration in `values.yaml` and update the required values if needed. Configure the time for periodic runs of the `collector` and `reloader` workers with a value in Go Duration format:

```yaml
collectorSyncPeriod: 30m
reloaderRunPeriod: 1h
```

Additionally, Reloader needs to be supplied with Vault credentials to be able to connect to Vault in order to get the secrets. You can check the list of environmental variables accepted for creating a Vault client [here](https://developer.hashicorp.com/vault/docs/commands#environment-variables). For example:

```yaml
env:
  # define env vars for Vault used for authentication
  VAULT_ROLE: "reloader"
  VAULT_ADDR: "https://vault.default.svc.cluster.local:8200"
  VAULT_NAMESPACE: "default"
  VAULT_TLS_SECRET: "vault-tls"
  VAULT_TLS_SECRET_NS: "bank-vaults-infra"
```

3. Install the chart:

```shell
helm upgrade --install --values values.yaml vault-secrets-reloader-namespaced oci://ghcr.io/davealmr/helm-charts/vault-secrets-reloader-namespaced --namespace bank-vaults-infra --create-namespace
```

## Values

The following table lists the configurable parameters of the Helm chart.

| Parameter | Type | Default | Description |
| --- | ---- | ------- | ----------- |
| `logLevel` | string | `"info"` | Log level |
| `enableJSONLog` | bool | `false` | Use JSON log format instead of text |
| `image.repository` | string | `"ghcr.io/davealmr/vault-secrets-reloader-namespaced"` | Container image repo that contains the Reloader Controller |
| `image.tag` | string | `""` | Container image tag |
| `image.pullPolicy` | string | `"IfNotPresent"` | Container image pull policy |
| `image.imagePullSecrets` | list | `[]` | Container image pull secrets for private repositories |
| `nameOverride` | string | `""` | Override app name |
| `fullnameOverride` | string | `""` | Override app full name |
| `collectorSyncPeriod` | string | `"30m"` | Time interval for the collector worker to run in Go Duration format |
| `reloaderRunPeriod` | string | `"1h"` | Time interval for the reloader worker to run in Go Duration format |
| `serviceAccount.create` | bool | `true` | Specifies whether a service account should be created |
| `serviceAccount.annotations` | object | `{}` | Annotations to add to the service account |
| `serviceAccount.name` | string | `""` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |
| `podAnnotations` | object | `{}` | Extra annotations to add to pod metadata |
| `podSecurityContext` | object | `{}` | Pod security context for Reloader deployment |
| `securityContext` | object | `{}` | Pod security context for Reloader containers |
| `service.name` | string | `"vault-secrets-reloader-namespaced"` | Reloader service name |
| `service.type` | string | `"ClusterIP"` | Reloader service type |
| `service.externalPort` | int | `443` | Reloader service external port |
| `service.internalPort` | int | `8443` | Reloader service internal port |
| `service.annotations` | object | `{}` | Reloader service annotations, e.g. if type is AWS LoadBalancer and you want to add security groups |
| `ingress.enabled` | bool | `false` | Enable Reloader ingress |
| `ingress.className` | string | `""` | Reloader IngressClass name |
| `ingress.annotations` | object | `{}` | Reloader ingress annotations |
| `ingress.hosts` | list | `[]` | Reloader ingress hosts |
| `ingress.tls` | list | `[]` | Reloader ingress tls |
| `env` | object | `{}` | Environment variables e.g. for Vault authentication |
| `volumes` | list | `[]` | Extra volume definitions for Reloader deployment |
| `volumeMounts` | list | `[]` | Extra volume mounts for Reloader deployment |
| `resources` | object | `{}` | Resources to request for the deployment and pods |
| `autoscaling.enabled` | bool | `false` | Enable Reloader horizontal pod autoscaling |
| `autoscaling.minReplicas` | int | `1` | Minimum number of replicas |
| `autoscaling.maxReplicas` | int | `100` | Maximum number of replicas |
| `nodeSelector` | object | `{}` | Node labels for pod assignment. Check: <https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector> |
| `tolerations` | list | `[]` | List of node tolerations for the pods. Check: <https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/> |
| `affinity` | object | `{}` | Node affinity settings for the pods. Check: <https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/> |

Specify each parameter using the `--set key=value[,key=value]` argument to `helm install`.

### Vault settings

Make sure to add the `read` and `list` capabilities for secrets to the Vault auth role the Reloader will use. An example can be found in the [example Bank-Vaults Operator CR file](https://github.com/davealmr/vault-secrets-reloader-namespaced/blob/main/e2e/deploy/vault/vault.yaml#L102).
