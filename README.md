# Vault Secrets Reloader

## Description

Vault Secrets Reloader can periodically check if a secret that is used in watched workloads has a new version in
Hashicorp Vault, and if so, automatically “reloads” them by incrementing an annotation value, initiating a rollout for
the workload’s pods. This controller is essentially a complementary to `vault-secrets-webhook`, relying on it for
actually injecting secrets into the pods of the affected workloads.

If you already use the [webhook](https://github.com/bank-vaults/vault-secrets-webhook), you are probably aware of it
only injecting secrets when the pods are created/recreated, and until now, there were no solution within the Bank-Vaults
ecosystem to inject secrets of these workloads in a continuous manner. Vault Secrets Reloader offers Vault Secrets
Webhook users an automated solution for this problem.

> \[!IMPORTANT\] This is an **early alpha version** and breaking changes are expected. As such, it is not recommended
> for usage in production.
>
> You can support us with your feedback, bug reports, and feature requests.

## Documentation

The official documentation for the Reloader will be available at
[https://bank-vaults.dev](https://bank-vaults.dev/docs/).

## Development

**For an optimal developer experience, it is recommended to install [Nix](https://nixos.org/download.html) and
[direnv](https://direnv.net/docs/installation.html).**

_Alternatively, install [Go](https://go.dev/dl/) on your computer then run `make deps` to install the rest of the
dependencies._

Make sure Docker is installed with Compose and Buildx.

Run project dependencies:

```shell
make deps

make up
```

Port-forward Vault:

```shell
export VAULT_TOKEN=$(kubectl get secrets vault-unseal-keys -o jsonpath={.data.vault-root} | base64 --decode)

kubectl get secret vault-tls -o jsonpath="{.data.ca\.crt}" | base64 --decode > $PWD/vault-ca.crt
export VAULT_CACERT=$PWD/vault-ca.crt

export VAULT_ADDR=https://127.0.0.1:8200

kubectl port-forward service/vault 8200 &
```

Run the reloader:

```shell
make run
```

Run the test suite:

```shell
make test

make container-image
make test-e2e-local
```

Run linters:

```shell
make lint # pass -j option to run them in parallel
```

Some linter violations can automatically be fixed:

```shell
make fmt
```

Build artifacts locally:

```shell
make artifacts
```

Once you are done either stop or tear down dependencies:

```shell
make down
```

### Running e2e tests

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

## License

The project is licensed under the [Apache 2.0 License](LICENSE).
