# Vault Secrets Reloader

Vault Secrets Reloader can periodically check if a secret that is used in watched workloads has a new version in
Hashicorp Vault, and if so, automatically “reloads” them by incrementing an annotation value, initiating a rollout for
the workload’s pods. This controller is essentially a complementary to `vault-secrets-webhook`, relying on it for
actually injecting secrets into the pods of the affected workloads.

If you already use the [webhook](https://github.com/bank-vaults/vault-secrets-webhook), you are probably aware of it
only injecting secrets when the pods are created/recreated, and until now, there were no solution within the Bank-Vaults
ecosystem to inject secrets into these workloads in a continuous manner. Vault Secrets Reloader offers Vault Secrets
Webhook users an automated solution for this problem.

> [!IMPORTANT]
> This is an **early alpha version** and breaking changes are expected. As such, it is not recommended
> for usage in production.
>
> You can support us with your feedback, bug reports, and feature requests.

## Features

Upon deployment, the Reloader spawns two “workers”, that run periodically at two different time intervals:

1. The `collector` collects and stores information about the workloads that are opted in via the
   `alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"` annotation in their pod template metadata and
   the Vault secrets they use.
1. The `reloader` iterates on the data collected by the `collector`, polling the configured Vault instance for the
   current version of the secrets, and if it finds that it differs from the stored one, adds the workloads where the
   secret is used to a list of workloads that needs reloading. In a following step, it modifies these workloads by
   incrementing the value of the `alpha.vault.security.banzaicloud.io/secret-reload-count` annotation in their pod
   template metadata, initiating a new rollout.

- The time interval can be set separately for these two workers, to limit resources they use and the number of requests
  sent to the Vault instance. The interval setting for the `collector` (`collectorSyncPeriod` in the Helm chart) should
  logically be the same, or lower than for the `reloader` (`reloaderRunPeriod`).
- Vault credentials can be set through environment variables in the Helm chart.

## Current limitations

- It can only check for updated versions of secrets in one specific instance of Hashicorp Vault, no other secret stores
are supported yet.

- It can only “reload” Deployments, DaemonSets and StatefulSets that have the
  `alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"` annotation set among their
  `spec.template.metadata.annotations`.

- It can only watch secrets put in the workload’s pod template environment variables directly, in the format the
  `vault-secrets-webhook` also uses, and are unversioned, for example:

  ```yaml
  env:
    - name: AWS_SECRET_ACCESS_KEY
      value: "vault:secret/data/accounts/aws#AWS_SECRET_ACCESS_KEY" # this will be collected for version check
    - name: MYSQL_PASSWORD
      value: "vault:secret/data/mysql#${.MYSQL_PASSWORD}#1" # versioned secrets will not be collected
  ```

- Data collected by the `reloader` is only stored in-memory (secret version updates during the controller is being
  recreated will not be acted upon, as it will rebuild its data store from scratch on start).

## Configuration

Reloader needs to access the Vault instance on its own, so make sure you set the correct environment variables through
the Helm chart (you can check the list of environmental variables accepted for creating a Vault client
[here](https://developer.hashicorp.com/vault/docs/commands#environment-variables)). Furthermore, configure the workload
data collection and reloading periods (using Go Duration format) that work best for your requirements and use-cases. For
example:

```shell
helm upgrade --install vault-secrets-reloader oci://ghcr.io/bank-vaults/helm-charts/vault-secrets-reloader \
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
file](https://github.com/bank-vaults/vault-secrets-reloader/blob/main/e2e/deploy/vault/vault.yaml#L102).

## Trying out Vault Secrets Reloader locally

Make sure Docker is installed with Compose and Buildx.

Prepare the environment:

```shell
# install dependencies
make deps

# start a kind cluster with Bank-Vaults operator, a Vault instance and Vault Secrets Webhook
make up

# build the Vault Secrets Reloader image
make container-image

# deploy Vault Secrets Reloader
make deploy-kind
```

The last command will install the Reloader Helm chart with the following configuration:

```shell
helm upgrade --install vault-secrets-reloader deploy/charts/vault-secrets-reloader \
    --set image.tag=dev \
    --set collectorSyncPeriod=30s \
    --set reloaderRunPeriod=1m \
    --namespace bank-vaults-infra
```

Now that we have the Bank-Vaults ecosystem running in our kind cluster, we can try out the Reloader in action:

```shell
# deploy some workloads
kubectl apply -f e2e/deploy/workloads

# watch reloader logs
kubectl logs -n bank-vaults-infra -l app.kubernetes.io/name=vault-secrets-reloader --follow
```

To trigger a new rollout for the affected workloads, change a secret in Vault! If you followed the previous steps,
export some environmental variables and port-forward the Vault pod first:

```shell
export VAULT_TOKEN=$(kubectl get secrets vault-unseal-keys -o jsonpath={.data.vault-root} | base64 --decode)

kubectl get secret vault-tls -o jsonpath="{.data.ca\.crt}" | base64 --decode > $PWD/vault-ca.crt
export VAULT_CACERT=$PWD/vault-ca.crt

export VAULT_ADDR=https://127.0.0.1:8200

kubectl port-forward service/vault 8200 &
```

Now we are ready to try some things out with the Reloader:

1. Change a secret, observe the affected workloads (`reloader-test-deployment-to-be-reloaded-...`, and
`reloader-test-statefulset-0`) restarting (this might take up to a minute), check their logs for the
updated secret.

    ```shell
    vault kv patch secret/mysql MYSQL_PASSWORD=totallydifferentsecret
    ```

    Also notice that there are two pods with the now changed `MYSQL_PASSWORD` injected into them not
being restarted, for the following reasons:

    - the pod `reloader-test-deployment-no-reload-...` does not have the
`alpha.vault.security.banzaicloud.io/reload-on-secret-change: "true"` annotation set
    - the pod `reloader-test-deployment-fixed-versions-no-reload-...` - although it does have the annotation - only uses
  versioned secrets, so they won't be reloaded for the latest version of the secret.

2. Change two secrets used in a workload, observe the previous pod to be recreated again, also that the pod
`reloader-test-daemonset-...` only restarted once, although it uses both of these secrets.

    ```shell
    vault kv patch secret/accounts/aws AWS_SECRET_ACCESS_KEY=s3cr3t2
    vault kv patch secret/dockerrepo DOCKER_REPO_PASSWORD=dockerrepopassword2
    ```

3. Update a workload to no longer have a secret, then change that secret, observe the workload not to be reloaded.
4. Remove a secret from Vault, observe the error message in the logs of the Reloader.

    ```shell
    vault kv metadata delete secret/mysql

    # watch reloader logs, there should be similar error message soon:
    # ... level=error msg="Vault secret path secret/data/mysql not found" app=vault-secrets-reloader worker=reloader
    kubectl logs -n bank-vaults-infra -l app.kubernetes.io/name=vault-secrets-reloader --follow
    ```

You can tear down the test cluster with `make down` once you finished.

## Development

**For an optimal developer experience, it is recommended to install [Nix](https://nixos.org/download.html) and
[direnv](https://direnv.net/docs/installation.html).**

_Alternatively, install [Go](https://go.dev/dl/) on your computer then run `make deps` to install the rest of the
dependencies._

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


