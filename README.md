# Vault Secrets Reloader  

[![Go Report Card](https://goreportcard.com/badge/github.com/DaveAlmR/vault-secrets-reloader-namespaced)](https://goreportcard.com/report/github.com/DaveAlmR/vault-secrets-reloader-namespaced)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/DaveAlmR/vault-secrets-reloader-namespaced/ci.yaml?branch=main&style=flat-square)](https://github.com/DaveAlmR/vault-secrets-reloader-namespaced/actions/workflows/ci.yaml?query=workflow%3ACI)

Vault Secrets Reloader can periodically check if a secret that is used in watched workloads has a new version in Hashicorp Vault, and if so, automatically “reloads” them by incrementing an annotation value, initiating a rollout for the workload’s pods. This controller is essentially a complementary to `secrets-webhook`, relying on it for actually injecting secrets into the pods of the affected workloads.

> [!IMPORTANT]
> This is an **early alpha version** and breaking changes are expected. As such, it is not recommended
> for usage in production.
>
> You can support us with your feedback, bug reports, and feature requests.

## Overview

Upon deployment, the Reloader spawns two “workers”, that run periodically at two different time intervals:

1. The `collector` collects and stores information about the workloads that are opted in via the `secrets-reloader.security.bank-vaults.io/reload-on-secret-change: "true"` annotation in their pod template metadata and the Vault secrets they use.

2. The `reloader` iterates on the data collected by the `collector`, polling the configured Vault instance for the current version of the secrets, and if it finds that it differs from the stored one, adds the workloads where the secret is used to a list of workloads that needs reloading. In a following step, it modifies these workloads by incrementing the value of the `secrets-reloader.security.bank-vaults.io/secret-reload-count` annotation in their pod template metadata, initiating a new rollout.

To get familiarized, check out [how Reloader fits in the Bank-Vaults ecosystem](https://github.com/davealmr/vault-secrets-reloader-namespaced/blob/main/examples/reloader-in-bank-vaults-ecosystem.md), and how can you [give Reloader a spin](https://github.com/davealmr/vault-secrets-reloader-namespaced/blob/main/examples/try-locally.md) on your local machine.

### Current features, limitations

- The time interval can be set separately for these two workers, to limit resources they use and the number of requests sent to the Vault instance. The interval setting for the `collector` (`collectorSyncPeriod` in the Helm chart) should logically be the same, or lower than for the `reloader` (`reloaderRunPeriod`).

- Vault credentials can be set through environment variables in the Helm chart.

- It can only check for updated versions of secrets in one specific instance of Hashicorp Vault, no other secret stores are supported yet.

- It can only “reload” Deployments, DaemonSets and StatefulSets that have the `secrets-reloader.security.bank-vaults.io/reload-on-secret-change: "true"` annotation set among their `spec.template.metadata.annotations`.

- The `collector` can only look for secrets in the workload’s pod template environment variables directly, and in their `secrets-webhook.security.bank-vaults.io/vault-from-path` annotation, in the format the `secrets-webhook` also uses, and are unversioned.

- Data collected by the `reloader` is only stored in-memory.

### Configuration

Reloader needs to access the Vault instance on its own, so make sure you set the correct environment variables through
the Helm chart (you can check the list of environmental variables accepted for creating a Vault client
[here](https://developer.hashicorp.com/vault/docs/commands#environment-variables)). Furthermore, configure the workload
data collection and reloading periods (using Go Duration format) that work best for your requirements and use-cases. For
example:

The chart also injects `POD_NAMESPACE` using the Downward API so the controller only watches workloads in its own
namespace. The controller logs an error and exits if `POD_NAMESPACE` is not set.

```shell
helm upgrade --install vault-secrets-reloader oci://ghcr.io/davealmr/helm-charts/vault-secrets-reloader-namespaced \
    --set collectorSyncPeriod=2h \
    --set reloaderRunPeriod=4h \
    --set env.VAULT_ADDR=[URL for Vault]
    --set env.VAULT_PATH=[Auth path]
    --set env.VAULT_ROLE=[Auth role]
    --set env.VAULT_AUTH_METHOD=[Auth method]
    # other environmental variables needed for the auth method of your choice
    --namespace bank-vaults-infra --create-namespace
```

Vault also needs to be configured with an auth method for the Reloader to use. Additionally, it is advised to create a
role and policy that allows the Reloader to `read` and `list` secrets from Vault. An example can be found in the
[example Bank-Vaults Operator CR
file](https://github.com/davealmr/vault-secrets-reloader-namespaced/blob/main/e2e/deploy/vault/vault.yaml#L102).

## Development

Install [Go](https://go.dev/dl/) on your computer then run `make deps` to install the rest of the
dependencies.

Make sure Docker is installed with Compose and Buildx.

### Install project dependencies locally

```shell
make deps

make up
```

### Port-forward Vault

```shell
export VAULT_TOKEN=$(kubectl get secrets vault-unseal-keys -o jsonpath={.data.vault-root} | base64 --decode)

kubectl get secret vault-tls -o jsonpath="{.data.ca\.crt}" | base64 --decode > $PWD/vault-ca.crt
export VAULT_CACERT=$PWD/vault-ca.crt

export VAULT_ADDR=https://127.0.0.1:8200

kubectl port-forward service/vault 8200 &
```

### Run the Reloader

```shell
make run
```

### Run unit tests

```shell
make test
```

### Run end-to-end tests

The project comes with an e2e test suite that is mostly self-contained, but at the very least, you need Docker
installed.

By default, the suite launches a [KinD](https://kind.sigs.k8s.io/) cluster, deploys all necessary components and runs
the test suite. This is a good option if you want to run the test suite to make sure everything works. This is also how
the CI runs the test suite (with a few minor differences).

You can run the test suite by running the following commands:

```shell
make container-image
make test-e2e-local
```

### Run linters

```shell
make lint # pass -j option to run them in parallel
```

Some linter violations can automatically be fixed:

```shell
make fmt
```

### Build artifacts locally

```shell
make artifacts
```

### Once you are done either stop or tear down dependencies

```shell
make down
```

## License

The project is licensed under the [Apache 2.0 License](LICENSE).
